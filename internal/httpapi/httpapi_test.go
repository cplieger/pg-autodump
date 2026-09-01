package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/webhttp/v2"
)

// stubPG implements dump.PGTool for handler tests. Dump optionally blocks on
// release after signalling entered, so single-flight (429) can be exercised.
type stubPG struct {
	entered   chan struct{}
	release   chan struct{}
	exit      int
	dumpCalls atomic.Int32
}

func (p *stubPG) Probe(context.Context, dump.Conn) (int, dump.FailKind, error) {
	return 18, dump.FailNone, nil
}

func (p *stubPG) Dump(_ context.Context, _ dump.Conn, w io.Writer) (int, string, error) {
	p.dumpCalls.Add(1)
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.release != nil {
		<-p.release
	}
	_, _ = io.WriteString(w, "PGDMP")
	if p.exit != 0 {
		return p.exit, "boom", nil
	}
	return 0, "", nil
}

func (p *stubPG) VerifyTOC(context.Context, string) error { return nil }

type okSignal struct{ ok bool }

func (s okSignal) Healthy() bool { return s.ok }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestServerInDir puts the cross-process cycle lock in cycleDir, so tests
// can contend on it like an exec'd `pg-autodump run` process would.
func newTestServerInDir(t *testing.T, pg dump.PGTool, token, cycleDir string) *http.Server {
	t.Helper()
	orch := dump.New(&dump.Params{
		PG:          pg,
		Logger:      discard(),
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	cycle := scheduler.NewExclusive(cycleDir, discard())
	trigger := NewTrigger(&dump.Guard{}, cycle, orch, discard())
	return NewServer(&Deps{
		AuthToken: token,
		Trigger:   trigger,
		Health:    okSignal{ok: true},
		Log:       discard(),
	})
}

func newTestServer(t *testing.T, pg dump.PGTool, token string) *http.Server {
	t.Helper()
	return newTestServerInDir(t, pg, token, t.TempDir())
}

func post(t *testing.T, srv *http.Server, header string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dump", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestDumpOKReturns200(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	if rec := post(t, srv, ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestDumpFailureReturns500(t *testing.T) {
	srv := newTestServer(t, &stubPG{exit: 1}, "")
	if rec := post(t, srv, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// The response body carries only the reason word, never raw pg_dump stderr
// (which can echo schema/object/role names); stderr goes to logs only.
func TestDumpFailureBodyOmitsStderr(t *testing.T) {
	srv := newTestServer(t, &stubPG{exit: 1}, "")
	rec := post(t, srv, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pg_error") {
		t.Fatalf("body = %q, want it to name the reason 'pg_error'", body)
	}
	if strings.Contains(body, "boom") {
		t.Fatalf("body = %q leaked pg_dump stderr 'boom'; it must be logs-only", body)
	}
}

func TestAuthRequired(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "sekret")
	rejected := map[string]string{
		"missing token":         "",
		"non-bearer scheme":     "Basic sekret",
		"empty presented token": "Bearer ",
		"prefix-matching token": "Bearer sekret-with-suffix",
		"wrong token":           "Bearer wrong",
	}
	for name, header := range rejected {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, srv, header); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
	t.Run("rejection body is http.Error's plain unauthorized", func(t *testing.T) {
		rec := post(t, srv, "Bearer wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		if got := rec.Body.String(); got != "unauthorized\n" {
			t.Errorf("401 body = %q, want %q", got, "unauthorized\n")
		}
	})
	t.Run("correct token accepted", func(t *testing.T) {
		if rec := post(t, srv, "Bearer sekret"); rec.Code == http.StatusUnauthorized {
			t.Errorf("correct token rejected: status = %d", rec.Code)
		}
	})
}

// webhttp.NewStaticTokenVerifier must never authorize when the configured
// secret is empty, even for an empty presented credential. pg-autodump's open
// mode (empty AUTH_TOKEN) is a bypass above this gate, not a weakening of it.
func TestVerifierFailsClosedOnEmptyConfigured(t *testing.T) {
	verify := webhttp.NewStaticTokenVerifier("")
	for _, presented := range []string{"", "sekret", "Bearer "} {
		if verify.Verify(presented) {
			t.Errorf("NewStaticTokenVerifier(\"\").Verify(%q) = true, want false (empty configured must never authorize)", presented)
		}
	}
}

func TestNonPostReturns405(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	req := httptest.NewRequest(http.MethodGet, "/dump", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /dump status = %d, want 405", rec.Code)
	}
}

func TestSingleFlightReturns429(t *testing.T) {
	pg := &stubPG{entered: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServer(t, pg, "")

	done := make(chan int, 1)
	go func() {
		rec := post(t, srv, "")
		done <- rec.Code
	}()

	<-pg.entered // first request now holds the guard inside Dump

	if rec := post(t, srv, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent dump: status = %d, want 429", rec.Code)
	}

	close(pg.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
}

// newTestServerWithLog mirrors newTestServer but routes the logger to a
// caller-owned buffer.
func newTestServerWithLog(t *testing.T, pg dump.PGTool, buf *bytes.Buffer) *http.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(buf, nil))
	orch := dump.New(&dump.Params{
		PG:          pg,
		Logger:      discard(),
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	cycle := scheduler.NewExclusive(t.TempDir(), discard())
	trigger := NewTrigger(&dump.Guard{}, cycle, orch, discard())
	return NewServer(&Deps{
		Trigger: trigger,
		Health:  okSignal{ok: true},
		Log:     logger,
	})
}

// failingWriter is an http.ResponseWriter whose Write always errors.
type failingWriter struct {
	header http.Header
	code   int
}

func (f *failingWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}

func (f *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func (f *failingWriter) WriteHeader(code int) { f.code = code }

// The response body is the per-database Detail, not the bare Reason;
// dumpHandler substitutes Reason only when Detail is empty.
func TestDumpResponseBodyUsesDetail(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	rec := post(t, srv, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "h/db: ok (5 bytes)\n" {
		t.Fatalf("body = %q, want %q", got, "h/db: ok (5 bytes)\n")
	}
}

// A successful response write must log no write-failure warning.
func TestDumpSuccessLogsNoWriteWarning(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServerWithLog(t, &stubPG{}, &buf)

	if rec := post(t, srv, ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(buf.String(), "write dump response failed") {
		t.Fatalf("successful write should not log a failure warning, got %q", buf.String())
	}
}

// A response-write failure is logged through the logger supplied to NewServer,
// not the default.
func TestServerUsesSuppliedLoggerOnWriteFailure(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServerWithLog(t, &stubPG{}, &buf)

	req := httptest.NewRequest(http.MethodPost, "/dump", nil)
	srv.Handler.ServeHTTP(&failingWriter{}, req)

	if !strings.Contains(buf.String(), "write dump response failed") {
		t.Fatalf("expected write-failure warning in the supplied logger, got %q", buf.String())
	}
}

// NewTrigger defaults a nil logger to non-nil and keeps a supplied logger
// unchanged.
func TestNewTriggerLoggerDefaulting(t *testing.T) {
	guard := &dump.Guard{}
	cycle := scheduler.NewExclusive(t.TempDir(), nil)

	trNil := NewTrigger(guard, cycle, nil, nil)
	if trNil.log == nil {
		t.Fatalf("NewTrigger(.., nil) left log nil; want it defaulted to a non-nil logger")
	}

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	trCustom := NewTrigger(guard, cycle, nil, custom)
	if trCustom.log != custom {
		t.Fatalf("NewTrigger(.., custom) did not retain the supplied logger; want it stored as-is")
	}
}

// A clean cycle logs no cycle-coordination warning; that warning means the
// rerun queue-file bookkeeping degraded and must stay rare enough to notice.
func TestTriggerRunCleanCycleLogsNoCoordinationWarning(t *testing.T) {
	var buf bytes.Buffer
	orch := dump.New(&dump.Params{
		PG:          &stubPG{},
		Logger:      discard(),
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	trigger := NewTrigger(&dump.Guard{}, scheduler.NewExclusive(t.TempDir(), discard()), orch,
		slog.New(slog.NewTextHandler(&buf, nil)))

	results, ok, err := trigger.Run()

	if !ok || err != nil {
		t.Fatalf("Trigger.Run() = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	if len(results) != 1 || results[0].Reason != dump.ReasonOK {
		t.Fatalf("Trigger.Run() results = %+v, want one ok result", results)
	}
	if strings.Contains(buf.String(), "cycle coordination error after run") {
		t.Errorf("Trigger.Run() on a clean cycle logged a coordination warning, want none: %q", buf.String())
	}
}

// A cycle lock held by another process (an exec'd `pg-autodump run`) makes
// POST /dump respond 429, same as in-process contention.
func TestCycleLockHeldByOtherProcessReturns429(t *testing.T) {
	cycleDir := t.TempDir()
	srv := newTestServerInDir(t, &stubPG{}, "", cycleDir)

	lock, ok, err := scheduler.TryLock(filepath.Join(cycleDir, scheduler.ExclusiveLockName))
	if err != nil || !ok {
		t.Fatalf("TryLock(cycle.lock) = ok=%v err=%v, want held", ok, err)
	}
	defer lock.Unlock()

	if rec := post(t, srv, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("dump with foreign cycle lock held: status = %d, want 429", rec.Code)
	}
}

// Rerun demand queued while the server's cycle is in flight is executed by
// the server before the HTTP response is written; the requester never blocks
// and the handler body reports only the caller's own first run.
func TestQueuedRunDemandConsumedByServerCycle(t *testing.T) {
	cycleDir := t.TempDir()
	pg := &stubPG{entered: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServerInDir(t, pg, "", cycleDir)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(t, srv, "") }()

	<-pg.entered // server cycle now in flight, cycle lock held

	requester := scheduler.NewExclusive(cycleDir, discard())
	outcome, err := requester.Run(func() error {
		t.Error("requester executed the job; want it queued behind the in-flight cycle")
		return nil
	})
	if err != nil || outcome != scheduler.OutcomeQueued {
		t.Fatalf("requester.Run = (%v, %v), want (queued, nil)", outcome, err)
	}

	close(pg.release)
	<-pg.entered // the queued rerun entered Dump

	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "h/db: ok (5 bytes)\n" {
		t.Fatalf("body = %q, want the caller's own single-run report", got)
	}
	if got := pg.dumpCalls.Load(); got != 2 {
		t.Fatalf("Dump calls = %d, want 2 (own cycle + one coalesced rerun)", got)
	}
}

// NewServer's timeout budget: IdleTimeout 60s, ReadHeaderTimeout/ReadTimeout
// 10s, no WriteTimeout (a dump run holds the response open for minutes).
func TestServerTimeoutsConfigured(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s (the slow-header guard)", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != srv.ReadHeaderTimeout {
		t.Errorf("ReadTimeout = %v, want the same budget as ReadHeaderTimeout (%v)",
			srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (a dump run holds the response open by design)", srv.WriteTimeout)
	}
}

// webhttp's baseline hardening headers are present, but no CSP or HSTS: this
// is a plain-HTTP, non-browser control endpoint, and HSTS would make a
// browser refuse plain HTTP to the host.
func TestSecurityHeadersPresent(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	rec := post(t, srv, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("Content-Security-Policy = %q, want unset (non-browser endpoint)", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want unset (plain-HTTP endpoint)", got)
	}
}

// throttleProbeAttempts must exceed webhttp.FailedAuthRateLimit's burst.
const throttleProbeAttempts = 64

// Bad attempts inside the preset's burst pass to their 401s, the flood is
// then cut off with a 429 carrying Retry-After, a valid bearer is never
// throttled even mid-flood, and excluded routes (GET /dump, /healthz) draw no
// tokens.
func TestAuthFailureThrottle(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "sekrit")

	badBearer := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/dump", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		srv.Handler.ServeHTTP(rec, req)
		return rec
	}

	admitted := 0
	var throttled *httptest.ResponseRecorder
	for i := range throttleProbeAttempts {
		rec := badBearer()
		if rec.Code == http.StatusTooManyRequests {
			throttled = rec
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad attempt %d: status = %d, want 401 (admitted) or 429 (throttled)", i+1, rec.Code)
		}
		admitted++
	}
	if throttled == nil {
		t.Fatalf("%d bad bearer attempts were never throttled; the failed-auth limiter is not engaged", throttleProbeAttempts)
	}
	if admitted == 0 {
		t.Error("the first bad bearer attempt was throttled; the preset's burst must admit attempts to their 401s")
	}
	if throttled.Header().Get("Retry-After") == "" {
		t.Error("throttled 429 carries no Retry-After hint")
	}
	body := throttled.Body.String()
	if !strings.Contains(body, "too_many_auth_failures") {
		t.Errorf("throttled body = %q, want the too_many_auth_failures envelope", body)
	}
	if !strings.Contains(body, "too many failed bearer attempts") {
		t.Errorf("throttled body = %q, want this app's bearer-specific message", body)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/dump", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid bearer mid-flood: status = %d, want 200 (never throttled)", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dump", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /dump with empty bucket: status = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz with empty bucket: status = %d, want 200", rec.Code)
	}
}

// With no configured token the throttle must be disabled: unauthenticated
// POSTs are never 429'd (FailedAuthRateLimit has no non-positive "off").
func TestAuthFailureThrottle_openModeDisabled(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")

	for i := range throttleProbeAttempts {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dump", nil))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("open-mode POST %d was throttled; the limiter must be disabled without a token", i+1)
		}
	}
}
