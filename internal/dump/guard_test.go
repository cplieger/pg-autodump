package dump

import (
	"context"
	"testing"
)

func TestGuardSingleFlightAndCancel(t *testing.T) {
	t.Run("acquire is single-flight and frees up after release", func(t *testing.T) {
		var g Guard
		release, ok := g.TryAcquire(func() {})
		if !ok {
			t.Fatal("first TryAcquire ok = false, want true")
		}
		if _, ok2 := g.TryAcquire(func() {}); ok2 {
			t.Fatal("second TryAcquire ok = true while a run is held, want false")
		}
		release()
		if _, ok3 := g.TryAcquire(func() {}); !ok3 {
			t.Fatal("TryAcquire after release ok = false, want true (the slot must free up)")
		}
	})

	t.Run("CancelInFlight cancels the in-flight run context", func(t *testing.T) {
		var g Guard
		ctx, cancel := context.WithCancel(context.Background())
		release, ok := g.TryAcquire(cancel)
		if !ok {
			t.Fatal("TryAcquire ok = false, want true")
		}
		defer release()

		g.CancelInFlight()

		if ctx.Err() == nil {
			t.Fatal("CancelInFlight did not cancel the in-flight context")
		}
	})

	t.Run("CancelInFlight is a no-op when idle", func(t *testing.T) {
		var g Guard
		g.CancelInFlight() // must not panic with no run in flight
	})

	t.Run("release clears the cancel pointer", func(t *testing.T) {
		var g Guard
		cancelled := false
		release, _ := g.TryAcquire(func() { cancelled = true })
		release()
		g.CancelInFlight()
		if cancelled {
			t.Fatal("CancelInFlight ran the cancel func after release; want the pointer cleared")
		}
	})
}

func TestGuardWaitIdle(t *testing.T) {
	t.Run("idle guard returns true even when ctx is already done", func(t *testing.T) {
		var g Guard
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // drain budget already expired
		if !g.WaitIdle(ctx) {
			t.Fatal("WaitIdle on an idle guard = false, want true (no run in flight, so the idle check precedes the ctx check)")
		}
	})

	t.Run("returns true when the in-flight run finishes", func(t *testing.T) {
		var g Guard
		// A run whose done channel is already closed: the run finished, so
		// WaitIdle must observe the closed channel and report idle.
		ch := make(chan struct{})
		close(ch)
		g.done.Store(&ch)
		if !g.WaitIdle(deadlineCtx(t)) {
			t.Fatal("WaitIdle = false when the run's done channel is closed, want true (the run finished)")
		}
	})

	t.Run("returns false when ctx fires before the run finishes", func(t *testing.T) {
		var g Guard
		release, ok := g.TryAcquire(func() {})
		if !ok {
			t.Fatal("TryAcquire ok = false, want true")
		}
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // drain budget expires while the run is still in flight
		if g.WaitIdle(ctx) {
			t.Fatal("WaitIdle = true while a run is held and ctx is done, want false")
		}
	})
}
