package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/httpapi"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// heldPG is a dump.PGTool whose one Dump stays in flight until release is
// closed or its context ends.
type heldPG struct {
	started chan struct{}
	release chan struct{}
}

func (p *heldPG) Probe(context.Context, dump.Conn) (int, dump.FailKind, error) {
	return 18, dump.FailNone, nil
}

func (p *heldPG) Dump(ctx context.Context, _ dump.Conn, w io.Writer) (int, string, error) {
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		return 1, "", ctx.Err()
	}
	_, _ = io.WriteString(w, "PGDMP-fake")
	return 0, "", nil
}

func (p *heldPG) VerifyTOC(context.Context, string) error { return nil }

var errAcceptFailed = errors.New("accept failed")

// failingListener makes http.Server.Serve return on its own, as on a fatal
// accept error, once fail is closed.
type failingListener struct{ fail chan struct{} }

func (l *failingListener) Accept() (net.Conn, error) {
	<-l.fail
	return nil, errAcceptFailed
}

func (l *failingListener) Close() error { return nil }

func (l *failingListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// A server that exits on its own gets no signal, so nothing else stops the
// timer, and the process exits once serve returns: serve must stop the timer
// and outlast a dump a request started.
func TestServe_ServeExitStopsTheTimerAndWaitsForTheInFlightDump(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		pgf := &heldPG{started: make(chan struct{}), release: make(chan struct{})}
		guard := &dump.Guard{}
		orch := dump.New(&dump.Params{
			PG:          pgf,
			Logger:      log,
			DumpDir:     t.TempDir(),
			Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}},
			DumpTimeout: time.Hour,
			Concurrency: 1,
			Keep:        1,
		})
		trig := httpapi.NewTrigger(guard, scheduler.NewExclusive(t.TempDir(), log), orch, log)
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		fresh := startupRecord{known: true, remaining: time.Hour}
		tickerDone := startTicker(ctx, fresh, 24*time.Hour, trig, log)

		dumped := make(chan scheduler.Outcome, 1)
		go func() {
			_, outcome, _ := trig.Run()
			dumped <- outcome
		}()
		<-pgf.started

		ln := &failingListener{fail: make(chan struct{})}
		latch := health.NewLatch(health.NewMarker(filepath.Join(t.TempDir(), "healthy")))
		served := make(chan error, 1)
		go func() {
			served <- serve(ctx, stop, &http.Server{}, ln, guard, tickerDone, latch, testGrace, log)
		}()
		close(ln.fail)
		synctest.Wait()

		if ctx.Err() == nil {
			t.Error("ctx still live after the server exited on its own, want it cancelled to stop the timer")
		}
		select {
		case err := <-served:
			close(pgf.release)
			t.Fatalf("serve returned %v while a dump was in flight; log = %q", err, buf.String())
		default:
		}

		close(pgf.release)
		if err := <-served; !errors.Is(err, errAcceptFailed) {
			t.Errorf("serve() = %v, want the serve error %v", err, errAcceptFailed)
		}
		if got := <-dumped; got != scheduler.OutcomeRan {
			t.Errorf("in-flight dump outcome = %v, want %v (finished, not cancelled)", got, scheduler.OutcomeRan)
		}
		if !closed(tickerDone) {
			t.Error("timer goroutine still running after serve returned")
		}
		out := buf.String()
		if !strings.Contains(out, `level=INFO msg="shutdown complete"`) {
			t.Errorf("log = %q, want an INFO 'shutdown complete' line", out)
		}
		if strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want no WARN line", out)
		}
	})
}
