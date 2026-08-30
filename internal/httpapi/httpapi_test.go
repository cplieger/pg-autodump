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
// release after signalling entered, so single-flight (429) can be exercised;
// dumpCalls counts Dump invocations so rerun coalescing can be asserted.
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

// discard is a throwaway logger for wiring the pieces under test.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestServerInDir builds a server whose cross-process cycle lock lives in
// cycleDir, so tests can contend on the lock the way an exec'd `pg-autodump
// run` process would.
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

// On an execution-tool failure the response body carries only the reason word,
// not the raw pg_dump stderr: the bounded stderr can echo schema/object/role
// names, and the endpoint may run open, so the detail is routed to the logs
// only. stubPG with exit 1 writes stderr "boom" => pg_error.
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
	// One rejection class per named subtest: a regression in one arm must
	// not hide the others behind the first Fatal.
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
		// Status and body asserted on the SAME response: the pre-subtest test
		// coupled them, and the coupling is the point — the 401 must carry
		// http.Error's plain body, the same shape as before the webhttp
		// verifier switch.
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

// TestVerifierFailsClosedOnEmptyConfigured pins the contract the auth gate
// relies on: the wired verifier (webhttp.NewStaticTokenVerifier) never
// authorizes when the configured secret is empty — not even for an empty
// presented credential, where a bare hash-then-compare would match.
// pg-autodump's documented open mode (empty AUTH_TOKEN serves unauthenticated,
// TestDumpOKReturns200) is a bypass ABOVE this gate; the gate itself failing
// closed means removing that bypass could never fail open.
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

// newTestServerWithLog mirrors newTestServer but routes the server's logger to
// a caller-owned buffer so log output can be asserted.
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

// failingWriter is an http.ResponseWriter whose Write always errors, so the
// response-write failure branch in dumpHandler is exercised.
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

// The response body is the per-database Detail, not the bare Reason. A
// successful dump details "ok (N bytes)"; dumpHandler only substitutes the
// Reason string when Detail is empty (`detail == ""`). The negation
// (`detail != ""`) would replace a present detail with the reason, collapsing
// "ok (5 bytes)" to "ok".
func TestDumpResponseBodyUsesDetail(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "") // stub writes "PGDMP" => 5 bytes
	rec := post(t, srv, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "h/db: ok (5 bytes)\n" {
		t.Fatalf("body = %q, want %q", got, "h/db: ok (5 bytes)\n")
	}
}

// On a successful response write there must be NO write-failure warning
// (`err != nil`). The negation (`err == nil`) would log the warning on every
// successful request.
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

// When the response write fails, the warning is emitted through the logger the
// caller supplied to NewServer. The negation on NewServer's guard
// (`log == nil` -> `log != nil`) would discard the supplied logger for the
// default, so the message would never reach the caller's buffer.
func TestServerUsesSuppliedLoggerOnWriteFailure(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServerWithLog(t, &stubPG{}, &buf)

	req := httptest.NewRequest(http.MethodPost, "/dump", nil)
	srv.Handler.ServeHTTP(&failingWriter{}, req)

	if !strings.Contains(buf.String(), "write dump response failed") {
		t.Fatalf("expected write-failure warning in the supplied logger, got %q", buf.String())
	}
}

// NewTrigger defaults a nil logger to a non-nil one and keeps a supplied logger
// unchanged: exercise both the nil input (defaulted) and a non-nil input
// (retained) and assert the stored logger in each.
func TestNewTriggerLoggerDefaulting(t *testing.T) {
	guard := &dump.Guard{}
	cycle := scheduler.NewExclusive(t.TempDir(), nil)

	// nil logger is replaced with a non-nil default.
	trNil := NewTrigger(guard, cycle, nil, nil)
	if trNil.log == nil {
		t.Fatalf("NewTrigger(.., nil) left log nil; want it defaulted to a non-nil logger")
	}

	// a supplied logger is retained unchanged.
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	trCustom := NewTrigger(guard, cycle, nil, custom)
	if trCustom.log != custom {
		t.Fatalf("NewTrigger(.., custom) did not retain the supplied logger; want it stored as-is")
	}
}

// A trigger run whose cross-process cycle coordination went fine logs no
// coordination warning. That warning means the rerun queue-file bookkeeping
// degraded while the dump itself succeeded — the line an operator chasing a
// missed rerun looks for — so emitting it after every clean cycle would bury the
// real one.
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

// A cycle lock held by ANOTHER process (an exec'd `pg-autodump run`) makes
// POST /dump respond 429 exactly like an in-process contention: the server
// must never dump concurrently with a one-shot run. The test holds the flock
// through a second file description, which is what a separate process would do.
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

// Depth-1 rerun coalescing end to end: demand queued (by what would be an
// exec'd `pg-autodump run`) while the server's cycle is in flight is executed
// by the server before the HTTP response is written. The requester never
// blocks (OutcomeQueued returns immediately), the handler's body reports only
// the caller's own first run, and the orchestrator runs exactly twice (one
// spec, two cycles).
func TestQueuedRunDemandConsumedByServerCycle(t *testing.T) {
	cycleDir := t.TempDir()
	pg := &stubPG{entered: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServerInDir(t, pg, "", cycleDir)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(t, srv, "") }()

	<-pg.entered // server cycle now in flight, cycle lock held

	// A second Exclusive on the same dir is the requester side of another
	// process. Its job must never run here: the lock is busy, so the demand
	// queues for the active runner.
	requester := scheduler.NewExclusive(cycleDir, discard())
	outcome, err := requester.Run(func() error {
		t.Error("requester executed the job; want it queued behind the in-flight cycle")
		return nil
	})
	if err != nil || outcome != scheduler.OutcomeQueued {
		t.Fatalf("requester.Run = (%v, %v), want (queued, nil)", outcome, err)
	}

	close(pg.release) // let the first cycle finish; the consume loop reruns
	<-pg.entered      // the queued rerun entered Dump: demand was consumed

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

// NewServer wires the server's timeout budget. IdleTimeout is 60s and both the
// header and full read timeouts are 10s; there is deliberately no WriteTimeout
// (a dump run holds the response open for minutes). Each budget is asserted
// against its intended duration rather than against the constant it is built
// from: a miscomputed constant (e.g. 60/time.Second or 10/time.Second
// collapsing to 0) would silently remove the keep-alive idle bound or the
// slow-header guard while still matching itself.
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

// SecurityHeaders is wired into the middleware chain, so every response carries
// webhttp's baseline hardening headers. nosniff is the one that matters for a
// text/plain dump listing (it stops a browser MIME-sniffing the body); the
// X-Frame-Options and Referrer-Policy defaults ride along as standardization.
// There is deliberately no CSP or HSTS: this is a plain-HTTP, non-browser
// control endpoint, and HSTS would make a browser refuse plain HTTP to the host.
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

// throttleProbeAttempts bounds the failed-bearer probe loops below. It only
// has to EXCEED the preset's burst, which webhttp.FailedAuthRateLimit now owns
// along with the refill cadence: these tests assert the throttle's BEHAVIOUR
// (attempts admitted inward to their 401s, then a 429 carrying Retry-After and
// the shared code) instead of re-copying the numbers the adoption deleted.
const throttleProbeAttempts = 64

// TestAuthFailureThrottle pins the failed-bearer throttle end to end through
// the full middleware chain: bad attempts inside the preset's burst pass
// inward to their 401s, the flood is then cut off with a 429 carrying a
// Retry-After hint before reaching the handler, a VALID bearer is never
// throttled even mid-flood, and requests the predicate excludes (GET /dump,
// /healthz) draw no tokens.
func TestAuthFailureThrottle(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "sekrit")

	badBearer := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/dump", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		srv.Handler.ServeHTTP(rec, req)
		return rec
	}

	// Drive bad bearers until the shared bucket empties. Every attempt the
	// limiter admits must reach authMiddleware's 401; the first refusal is the
	// throttle engaging before the handler. How MANY are admitted is the
	// preset's tuning rather than this app's, so what is asserted is that some
	// are (an operator retrying a rotated credential by hand is not refused on
	// the first try) and that the flood is cut off within the probe bound.
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
	// The code above is the preset's (shared across services so log queries
	// and alert rules key on one string); the message stays this app's, so it
	// must still name the credential a caller presented.
	if !strings.Contains(body, "too many failed bearer attempts") {
		t.Errorf("throttled body = %q, want this app's bearer-specific message", body)
	}

	// A valid bearer never draws from the bucket: it passes even mid-flood.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/dump", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid bearer mid-flood: status = %d, want 200 (never throttled)", rec.Code)
	}

	// Excluded surfaces are untouched by the empty bucket: GET /dump still
	// answers the mux's 405, and the liveness probe stays 200.
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

// TestAuthFailureThrottle_openModeDisabled pins the off contract: with no
// configured token the limiter is the identity, so unauthenticated POSTs are
// never 429'd no matter the volume (open mode is documented as unthrottled).
// The bypass carries the whole contract now that the preset owns the tuning —
// FailedAuthRateLimit has no non-positive "off" and the fail-closed verifier
// would read every open-mode request as a failed attempt.
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
