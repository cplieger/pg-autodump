package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/httpapi"
)

// The boot verdict is healthy only when the preflight passes AND the last-run
// record allows it: in built-in timer mode the record must show a fully
// successful cycle within one interval (missing, failed, stale all boot
// unhealthy until a cycle fully succeeds); in external-trigger mode a
// recorded failure boots unhealthy and anything else boots healthy. A record
// the process cannot rewrite is ignored in both modes.
func TestBootHealthy(t *testing.T) {
	const interval = 24 * time.Hour
	now := time.Now()
	preErr := errors.New("pg_dump does not run")

	cases := []struct {
		name       string
		record     string // "" = no record, else the outcome word seeded at age
		preErr     error
		interval   time.Duration
		age        time.Duration
		unwritable bool
		want       bool
	}{
		{name: "preflight_failure_overrides_fresh_success", preErr: preErr, interval: interval, record: "ok", age: time.Hour, want: false},
		{name: "external_mode_no_record_healthy", interval: 0, want: true},
		{name: "external_mode_failed_record_unhealthy", interval: 0, record: "failed", age: time.Hour, want: false},
		{name: "external_mode_old_success_healthy", interval: 0, record: "ok", age: 30 * 24 * time.Hour, want: true},
		{name: "external_mode_unwritable_failed_record_ignored", interval: 0, record: "failed", age: time.Hour, unwritable: true, want: true},
		{name: "builtin_no_record_unhealthy", interval: interval, want: false},
		{name: "builtin_fresh_failed_record_unhealthy", interval: interval, record: "failed", age: time.Hour, want: false},
		{name: "builtin_stale_success_unhealthy", interval: interval, record: "ok", age: 48 * time.Hour, want: false},
		{name: "builtin_fresh_success_healthy", interval: interval, record: "ok", age: time.Hour, want: true},
		{name: "builtin_unwritable_fresh_success_unhealthy", interval: interval, record: "ok", age: time.Hour, unwritable: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.record != "" {
				seedRecord(t, dir, now.Add(-tc.age), tc.record)
			}
			if tc.unwritable {
				prev := openRecordForWrite
				openRecordForWrite = func(string) error { return errors.New("permission denied") }
				t.Cleanup(func() { openRecordForWrite = prev })
			}
			rec := readStartupRecord(dump.StampPath(dir), tc.interval, now, discardLogger())
			if got := bootHealthy(tc.preErr, tc.interval, rec); got != tc.want {
				t.Errorf("bootHealthy(preErr=%v, interval=%s, record=%q age=%s unwritable=%v) = %v, want %v",
					tc.preErr, tc.interval, tc.record, tc.age, tc.unwritable, got, tc.want)
			}
		})
	}
}

// A record the server cannot rewrite is not trusted for the startup dump
// either: the boot both reports unhealthy and dumps, instead of skipping the
// dump on a success it could never replace.
func TestReadStartupRecordIgnoresUnwritableRecord(t *testing.T) {
	dir := t.TempDir()
	seedRecord(t, dir, time.Now().Add(-time.Hour), "ok")
	prev := openRecordForWrite
	openRecordForWrite = func(string) error { return errors.New("permission denied") }
	t.Cleanup(func() { openRecordForWrite = prev })

	rec := readStartupRecord(dump.StampPath(dir), 24*time.Hour, time.Now(), discardLogger())
	if rec.known || rec.remaining != 0 {
		t.Errorf("readStartupRecord over an unwritable fresh success = {known:%v remaining:%s}, want {false 0} (the startup dump must fire)",
			rec.known, rec.remaining)
	}
}

// A one-shot run whose preconditions fail exits 1: a manual or scheduled run
// reports by its exit code.
func TestRunOnceExitsOneWhenPreflightFails(t *testing.T) {
	// DUMP_DIR is a regular file, so the dump-dir write probe fails whether or
	// not pg client binaries happen to be on PATH.
	notADir := filepath.Join(t.TempDir(), "dumps")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DB_SPECS": "h:5432:db:u", "DUMP_DIR": notADir}
	getenv := func(k string) string { return env[k] }

	if got := run([]string{"pg-autodump", "run"}, getenv); got != 1 {
		t.Fatalf("run with a failing preflight = %d, want 1", got)
	}
}

// The lease is two intervals plus one worst-case cycle, and a fresh verdict
// passes it: the sizing pins one cycle's runtime, not the trigger client's
// two-cycle wait.
func TestHealthzLeaseDuration(t *testing.T) {
	env := map[string]string{"DUMP_INTERVAL": "1h", "DB_SPECS": "h:5432:db:u", "DUMP_TIMEOUT": "300", "DUMP_CONCURRENCY": "2"}
	getenv := func(k string) string { return env[k] }
	cfg, _, err := config.Load(getenv)
	if err != nil {
		t.Fatal(err)
	}
	// 1 wave * (300s dump + 10s probe) + 30s slack = 340s; two 1h intervals on top.
	if got, want := healthzLease(&cfg).Duration(), 2*time.Hour+340*time.Second; got != want {
		t.Errorf("healthzLease(DUMP_INTERVAL=1h, one database).Duration() = %s, want %s", got, want)
	}
	env["DUMP_INTERVAL"] = "off"
	cfg, _, err = config.Load(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got := healthzLease(&cfg).Duration(); got != 0 {
		t.Errorf("healthzLease(DUMP_INTERVAL=off).Duration() = %s, want 0 (disabled)", got)
	}
}

// The page runServer serves dates the cycle deadline one health lease after
// the last success on record, so PgAutodumpDumpStale's deadline arm and
// /healthz's freshness check judge the same moment.
func TestServedCycleDeadlineIsTheHealthLease(t *testing.T) {
	env := map[string]string{"DUMP_INTERVAL": "1h", "DB_SPECS": "h:5432:db:u", "DUMP_DIR": t.TempDir()}
	cfg, _, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC)
	rec := startupRecord{last: scheduler.RunRecord{Time: last, OK: true}, known: true}

	reg, _ := newRegistry(&cfg, rec, last.Add(time.Hour))

	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 1 wave * (300s dump + 10s probe) + 30s slack = 340s; two 1h intervals on top.
	want := float64(last.Add(2*time.Hour + 340*time.Second).Unix())
	for _, f := range families {
		if f.Name == "pg_autodump_cycle_deadline_timestamp_seconds" {
			if len(f.Samples) != 1 || f.Samples[0].Value != want {
				t.Errorf("cycle deadline samples = %+v, want one at %v", f.Samples, want)
			}
			return
		}
	}
	t.Error("the served page has no pg_autodump_cycle_deadline_timestamp_seconds")
}

// The page runServer serves carries the binary's version in build_info, which
// an unstamped build reports as "dev".
func TestServedBuildInfoIsTheVersion(t *testing.T) {
	env := map[string]string{"DB_SPECS": "h:5432:db:u", "DUMP_DIR": t.TempDir()}
	cfg, _, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := newRegistry(&cfg, startupRecord{}, time.Now())

	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.Name == "pg_autodump_build_info" {
			if len(f.Samples) != 1 || f.Samples[0].Value != 1 ||
				len(f.Samples[0].Labels) != 1 || f.Samples[0].Labels[0].Value != "dev" {
				t.Errorf("build_info samples = %+v, want one {version=\"dev\"} at 1", f.Samples)
			}
			return
		}
	}
	t.Error("the served page has no pg_autodump_build_info")
}

// fakeClientPATH stages a stand-in for every PostgreSQL client binary the
// preflight runs and puts that directory on PATH, so a boot can be driven
// without the real clients installed.
func fakeClientPATH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, bin := range []string{"pg_dump", "pg_restore", "psql"} {
		body := []byte("#!/bin/sh\nprintf 'fake (PostgreSQL) 18.6\\n'\n")
		if err := os.WriteFile(filepath.Join(dir, bin), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// The server's boot records its verdict before any cycle runs: a fresh success
// in built-in timer mode boots healthy, a recorded failure in external-trigger
// mode boots unhealthy, and the record the ticker shares is the same reading.
func TestBootHealthWritesTheBootVerdict(t *testing.T) {
	cases := []struct {
		name     string
		interval string
		record   string
		want     bool
	}{
		{name: "builtin_fresh_success_boots_healthy", interval: "24h", record: "ok", want: true},
		{name: "external_failed_record_boots_unhealthy", interval: "off", record: "failed", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClientPATH(t)
			dumpDir := t.TempDir()
			seedRecord(t, dumpDir, time.Now().Add(-time.Hour), tc.record)
			env := map[string]string{"DB_SPECS": "h:5432:db:u", "DUMP_DIR": dumpDir, "DUMP_INTERVAL": tc.interval}
			cfg, _, err := config.Load(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			st := health.NewStatus(health.WithLogger(discardLogger()))

			rec, healthy := bootHealth(&cfg, st, discardLogger())

			if healthy != tc.want {
				t.Errorf("bootHealth(interval=%s, record=%q) healthy = %v, want %v",
					tc.interval, tc.record, healthy, tc.want)
			}
			if !rec.known {
				t.Error("bootHealth reported an unknown record over a seeded one; the ticker shares this reading")
			}
			if got := st.Healthy(); got != tc.want {
				t.Errorf("verdict after bootHealth(interval=%s, record=%q) = %v, want %v",
					tc.interval, tc.record, got, tc.want)
			}
		})
	}
}

// The pre-drain hook turns the verdict unhealthy for good: a cycle completing
// inside the drain cannot restore it, which a plain unhealthy write would allow.
func TestBeginDrainLatchesUnhealthy(t *testing.T) {
	st := health.NewStatus(health.WithLogger(discardLogger()))
	st.Set(true)

	beginDrain(t.Context(), st, discardLogger())(t.Context())
	st.Set(true) // a cycle finishing during the drain

	if st.Healthy() {
		t.Error("verdict restored by a cycle completing during the drain; want it to stay unhealthy")
	}
}

// GET /healthz answers from the verdict and the lease: a fresh healthy verdict
// is 200 ok; an unhealthy one (a failed or unfinished cycle, a drain) is 503
// naming that; in timer mode a healthy verdict older than two intervals plus
// one cycle is 503 naming the stopped timer; in external-trigger mode, with no
// cadence to judge, an old healthy verdict stays 200.
func TestHealthzServesTheFleetBody(t *testing.T) {
	cases := []struct {
		name     string
		interval string
		age      time.Duration // age of the healthy verdict; 0 means unhealthy
		wantCode int
		wantBody healthzBody
	}{
		{name: "timer_fresh_verdict", interval: "1h", age: time.Minute, wantCode: http.StatusOK, wantBody: healthzBody{Status: "ok"}},
		{
			name: "timer_unhealthy", interval: "1h", wantCode: http.StatusServiceUnavailable,
			wantBody: healthzBody{Status: "unhealthy", Reasons: []string{reasonNoVerdict}},
		},
		{
			name: "timer_verdict_older_than_the_lease", interval: "1h", age: 3 * time.Hour, wantCode: http.StatusServiceUnavailable,
			wantBody: healthzBody{Status: "unhealthy", Reasons: []string{reasonTimerStopped}},
		},
		{name: "external_old_verdict", interval: "off", age: 30 * 24 * time.Hour, wantCode: http.StatusOK, wantBody: healthzBody{Status: "ok"}},
		{
			name: "external_unhealthy", interval: "off", wantCode: http.StatusServiceUnavailable,
			wantBody: healthzBody{Status: "unhealthy", Reasons: []string{reasonNoVerdict}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			synctest.Test(t, func(t *testing.T) {
				srv := newHealthzServer(t, tc.interval, tc.age)
				rec = httptest.NewRecorder()
				srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			})

			if rec.Code != tc.wantCode {
				t.Errorf("GET /healthz status = %d, want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			var got healthzBody
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("GET /healthz body %q is not JSON: %v", rec.Body.String(), err)
			}
			if got.Status != tc.wantBody.Status || !slices.Equal(got.Reasons, tc.wantBody.Reasons) {
				t.Errorf("GET /healthz body = %+v, want %+v", got, tc.wantBody)
			}
		})
	}
}

// `pg-autodump health` asks GET /healthz on the listen port and follows its
// status code: a 503 fails it, a 200 passes it, and a listener that does not
// answer fails it.
func TestHealthSubcommandAsksTheEndpoint(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusServiceUnavailable)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(int(answer.Load()))
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = endpoint.Serve(ln) }()
	t.Cleanup(func() { _ = endpoint.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		return map[string]string{"LISTEN_ADDR": ":" + port, "DB_SPECS": "h:5432:db:u", "DUMP_INTERVAL": "1h"}[k]
	}

	var stderr bytes.Buffer
	if got := probeHealth(&stderr, getenv, time.Second); got != 1 {
		t.Errorf("health with /healthz answering 503 = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "/healthz") || !strings.Contains(stderr.String(), "503") {
		t.Errorf("health's diagnostic = %q, want it to name /healthz and the 503", stderr.String())
	}

	answer.Store(http.StatusOK)
	if got := run([]string{"pg-autodump", "health"}, getenv); got != 0 {
		t.Errorf("health with /healthz answering 200 = %d, want 0", got)
	}

	_ = endpoint.Close()
	stderr.Reset()
	if got := probeHealth(&stderr, getenv, time.Second); got != 1 {
		t.Errorf("health with nothing listening = %d, want 1", got)
	}
}

// A LISTEN_ADDR the probe cannot turn into a URL fails the probe with a reason
// naming it, instead of asking a guessed address.
func TestHealthSubcommandRejectsAnUnusableListenAddr(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{"LISTEN_ADDR": "9472", "DB_SPECS": "h:5432:db:u"}[k]
	}
	var stderr bytes.Buffer
	if got := probeHealth(&stderr, getenv, time.Second); got != 1 {
		t.Errorf("health with LISTEN_ADDR=9472 = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "LISTEN_ADDR") {
		t.Errorf("health's diagnostic = %q, want it to name LISTEN_ADDR", stderr.String())
	}
}

type healthzBody struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

// newHealthzServer serves /healthz as runServer wires it, over a verdict set
// healthy age ago (0: never set) and DUMP_INTERVAL. Run it inside a synctest
// bubble so the age passes on the fake clock.
func newHealthzServer(t *testing.T, interval string, age time.Duration) *http.Server {
	t.Helper()
	env := map[string]string{"DB_SPECS": "h:5432:db:u", "DUMP_INTERVAL": interval}
	cfg, _, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	st := health.NewStatus(health.WithLogger(discardLogger()))
	if age > 0 {
		st.Set(true)
		time.Sleep(age)
	}
	return httpapi.NewServer(&httpapi.Deps{
		Healthz: health.CheckHandler(healthzChecks(st, &cfg)...),
		Metrics: http.NotFoundHandler(),
		Log:     discardLogger(),
	})
}
