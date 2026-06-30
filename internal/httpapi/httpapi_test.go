package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// stubPG implements dump.PGTool for handler tests. Dump optionally blocks on
// release after signalling entered, so single-flight (429) can be exercised.
type stubPG struct {
	entered chan struct{}
	release chan struct{}
	exit    int
}

func (p *stubPG) Probe(context.Context, dump.Conn) (int, dump.FailKind, error) {
	return 18, dump.FailNone, nil
}

func (p *stubPG) Dump(_ context.Context, _ dump.Conn, w io.Writer) (int, string, error) {
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

func newTestServer(t *testing.T, pg dump.PGTool, token string) *http.Server {
	t.Helper()
	orch := dump.New(&dump.Params{
		PG:          pg,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	trigger := NewTrigger(&dump.Guard{}, orch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return NewServer(&Deps{
		ListenAddr: ":0",
		AuthToken:  token,
		Trigger:    trigger,
		Health:     okSignal{ok: true},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
// not the raw pg_dump stderr (l-f21 hardening): the bounded stderr can echo
// schema/object/role names, and the endpoint may run open, so the detail is
// routed to the logs only. stubPG with exit 1 writes stderr "boom" => pg_error.
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
	if rec := post(t, srv, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", rec.Code)
	}
	if rec := post(t, srv, "Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := post(t, srv, "Bearer sekret"); rec.Code == http.StatusUnauthorized {
		t.Fatalf("correct token rejected: status = %d", rec.Code)
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
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	trigger := NewTrigger(&dump.Guard{}, orch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return NewServer(&Deps{
		ListenAddr: ":0",
		Trigger:    trigger,
		Health:     okSignal{ok: true},
		Log:        logger,
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

	// nil logger is replaced with a non-nil default.
	trNil := NewTrigger(guard, nil, nil)
	if trNil.log == nil {
		t.Fatalf("NewTrigger(.., nil) left log nil; want it defaulted to a non-nil logger")
	}

	// a supplied logger is retained unchanged.
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	trCustom := NewTrigger(guard, nil, custom)
	if trCustom.log != custom {
		t.Fatalf("NewTrigger(.., custom) did not retain the supplied logger; want it stored as-is")
	}
}

// NewServer wires the server's timeout budget. IdleTimeout is 60s and both the
// header and full read timeouts are readHeaderTimeout; there is deliberately no
// WriteTimeout (a dump run holds the response open for minutes). A miscomputed
// IdleTimeout (e.g. 60/time.Second collapsing to 0) would silently remove the
// keep-alive idle bound.
func TestServerTimeoutsConfigured(t *testing.T) {
	srv := newTestServer(t, &stubPG{}, "")
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.ReadTimeout != readHeaderTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, readHeaderTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (a dump run holds the response open by design)", srv.WriteTimeout)
	}
}
