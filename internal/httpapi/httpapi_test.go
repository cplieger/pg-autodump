package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	trigger := NewTrigger(&dump.Guard{}, orch, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
