package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
)

// The boot marker is healthy only when the preflight passes AND the last-run
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

// A one-shot run whose preconditions fail counts as a failed cycle: the marker
// a healthy server left behind is removed, so an exec'd `run` that cannot even
// start its dumps moves the container's health the way a failed cycle would.
func TestRunOnceUnhealthyWhenPreflightFails(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), ".healthy")
	prev := healthMarkerPath
	healthMarkerPath = markerPath
	t.Cleanup(func() { healthMarkerPath = prev })
	health.NewMarker(markerPath).Set(true)

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
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker after a failed preflight: stat err = %v, want not-exist (a cycle that cannot start is a failed cycle)", err)
	}
}

// The health subcommand's freshness lease is armed in built-in timer mode
// only: a marker two intervals plus one cycle old fails the probe there, the
// same marker passes in external-trigger mode (no cadence to judge), and a
// config the daemon would refuse to start on disarms the lease rather than
// inventing a cadence.
func TestProbeOptionsFreshness(t *testing.T) {
	staleMarker := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".healthy")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name string
		env  map[string]string
		want int
	}{
		{name: "builtin_mode_stale_marker_unhealthy", env: map[string]string{"DUMP_INTERVAL": "1h", "DB_SPECS": "h:5432:db:u"}, want: 1},
		{name: "external_mode_stale_marker_healthy", env: map[string]string{"DUMP_INTERVAL": "off", "DB_SPECS": "h:5432:db:u"}, want: 0},
		{name: "fatal_config_disarms", env: map[string]string{"DUMP_INTERVAL": "1h", "DUMP_DIR": "/dumps/../etc"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := staleMarker(t)
			getenv := func(k string) string { return tc.env[k] }
			if got := health.ProbeCheck(path, probeOptions(getenv)...); got != tc.want {
				t.Errorf("ProbeCheck(72h-old marker, env=%v) = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// The lease is two intervals plus one worst-case cycle, and a fresh marker
// passes it: the sizing pins one cycle's runtime, not the trigger client's
// two-cycle wait.
func TestProbeLeaseDuration(t *testing.T) {
	env := map[string]string{"DUMP_INTERVAL": "1h", "DB_SPECS": "h:5432:db:u", "DUMP_TIMEOUT": "300", "DUMP_CONCURRENCY": "2"}
	getenv := func(k string) string { return env[k] }
	cfg, _, err := config.Load(getenv)
	if err != nil {
		t.Fatal(err)
	}
	// 1 wave * (300s dump + 10s probe) + 30s slack = 340s; two 1h intervals on top.
	if got, want := probeLease(&cfg).Duration(), 2*time.Hour+340*time.Second; got != want {
		t.Errorf("probeLease(DUMP_INTERVAL=1h, one database).Duration() = %s, want %s", got, want)
	}
	env["DUMP_INTERVAL"] = "off"
	cfg, _, err = config.Load(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeLease(&cfg).Duration(); got != 0 {
		t.Errorf("probeLease(DUMP_INTERVAL=off).Duration() = %s, want 0 (disabled)", got)
	}
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

// The server's boot writes its verdict to the marker the Docker healthcheck
// stats, before any cycle runs: a fresh success in built-in timer mode boots
// with the marker present, a recorded failure in external-trigger mode boots
// with it removed, and the record the ticker shares is the same reading.
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
			marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

			rec, healthy := bootHealth(&cfg, health.NewLatch(marker), discardLogger())

			if healthy != tc.want {
				t.Errorf("bootHealth(interval=%s, record=%q) healthy = %v, want %v",
					tc.interval, tc.record, healthy, tc.want)
			}
			if !rec.known {
				t.Error("bootHealth reported an unknown record over a seeded one; the ticker shares this reading")
			}
			if got := marker.CheckHealthy(); got != tc.want {
				t.Errorf("marker present after bootHealth(interval=%s, record=%q) = %v, want %v",
					tc.interval, tc.record, got, tc.want)
			}
		})
	}
}

// The pre-drain hook LATCHES unhealthy: a cycle completing inside the drain
// cannot restore the marker, which a plain unhealthy write would allow.
func TestBeginDrainLatchesUnhealthy(t *testing.T) {
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
	latch := health.NewLatch(marker)
	latch.Set(true)

	beginDrain(t.Context(), latch, discardLogger())(t.Context())
	latch.Set(true) // a cycle finishing during the drain

	if marker.CheckHealthy() {
		t.Error("marker restored by a cycle completing during the drain; want it to stay removed")
	}
}
