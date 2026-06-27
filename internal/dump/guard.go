package dump

import (
	"context"
	"sync/atomic"
)

// Guard enforces one dump run at a time in-process. The resident HTTP server
// owns a single Guard; the trigger subcommand routes through the server, so
// there is exactly one coordination point and no lock file (the 1.x flock and
// its "crashed child holds the lock" failure mode are gone). The Guard also
// holds the in-flight run's cancel func so the shutdown path can reach and
// cancel it.
type Guard struct {
	cancel  atomic.Pointer[context.CancelFunc]
	done    atomic.Pointer[chan struct{}]
	running atomic.Bool
}

// TryAcquire marks a run in flight and returns a release closure if no run is
// active; it returns ok=false otherwise (the caller responds 429). The caller
// passes the run's cancel func so shutdown can reach it. The release closure
// clears both the flag and the cancel pointer and is safe to defer.
func (g *Guard) TryAcquire(cancel context.CancelFunc) (release func(), ok bool) {
	if !g.running.CompareAndSwap(false, true) {
		return nil, false
	}
	done := make(chan struct{})
	g.cancel.Store(&cancel)
	g.done.Store(&done)
	return func() {
		close(done)
		g.done.Store(nil)
		g.cancel.Store(nil)
		g.running.Store(false)
	}, true
}

// WaitIdle blocks until the in-flight run finishes or ctx is done. It returns
// true if no run was active or the run finished, false if ctx fired first. The
// shutdown path uses it to drain a ticker-triggered run that holds no HTTP
// connection for srv.Shutdown to see.
func (g *Guard) WaitIdle(ctx context.Context) bool {
	p := g.done.Load()
	if p == nil {
		return true
	}
	select {
	case <-*p:
		return true
	case <-ctx.Done():
		return false
	}
}

// CancelInFlight cancels the in-flight run's context if one is active, else is
// a no-op. The shutdown goroutine calls it after the drain budget expires;
// cancelling the run context cascades to every worker's per-database context.
func (g *Guard) CancelInFlight() {
	if p := g.cancel.Load(); p != nil {
		(*p)()
	}
}
