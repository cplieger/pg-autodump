package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/pg-autodump/internal/dump"
)

const testGrace = 30 * time.Second

// runTeardown runs the teardown in its own goroutine with a drain context of
// drainBudget, and returns a channel closed when it returns.
func runTeardown(teardown func(context.Context), drainBudget time.Duration) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		ctx, cancel := context.WithTimeout(context.Background(), drainBudget)
		defer cancel()
		teardown(ctx)
	}()
	return returned
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// holdGuard takes the guard as a dump would. The hold ends when release is
// closed or, if honourCancel, when the teardown cancels it.
func holdGuard(t *testing.T, guard *dump.Guard, release <-chan struct{}, honourCancel bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	free, ok := guard.TryAcquire(cancel)
	if !ok {
		t.Fatal("guard.TryAcquire on an idle guard = false, want true")
	}
	cancelled := ctx.Done()
	if !honourCancel {
		cancelled = nil
	}
	go func() {
		defer free()
		select {
		case <-release:
		case <-cancelled:
		}
	}()
}

// The teardown is the last step before the process exits, so it must not log
// shutdown complete while the timer goroutine can still log.
func TestDrainInFlightDump_WaitsForTheTimerGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		tickerDone := make(chan struct{})

		returned := runTeardown(drainInFlightDump(&dump.Guard{}, tickerDone, testGrace, log), testGrace)
		synctest.Wait()
		if closed(returned) {
			t.Fatalf("teardown returned while the timer goroutine was still running; log = %q", buf.String())
		}

		close(tickerDone)
		synctest.Wait()
		if !closed(returned) {
			t.Fatal("teardown still blocked after the timer goroutine returned")
		}
		if out := buf.String(); !strings.Contains(out, `level=INFO msg="shutdown complete"`) {
			t.Errorf("log = %q, want an INFO 'shutdown complete' line", out)
		}
	})
}

// A drain that spent its whole budget hands the teardown an expired context;
// a timer goroutine that returns just after is still waited for, not reported
// as stuck.
func TestDrainInFlightDump_WaitsForTheTimerAfterAnExhaustedDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		tickerDone := make(chan struct{})
		time.AfterFunc(time.Millisecond, func() { close(tickerDone) })

		<-runTeardown(drainInFlightDump(&dump.Guard{}, tickerDone, testGrace, log), 0)

		out := buf.String()
		if !strings.Contains(out, `level=INFO msg="shutdown complete"`) {
			t.Errorf("log = %q, want an INFO 'shutdown complete' line", out)
		}
		if strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want no WARN line: the timer goroutine returned", out)
		}
	})
}

// A timer goroutine that never returns cannot hold the process past its stop:
// the teardown gives up with a warning.
func TestDrainInFlightDump_GivesUpOnATimerThatNeverStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		<-runTeardown(drainInFlightDump(&dump.Guard{}, make(chan struct{}), testGrace, log), testGrace)

		out := buf.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "built-in timer did not stop") {
			t.Errorf("log = %q, want a WARN line saying the built-in timer did not stop", out)
		}
		if strings.Contains(out, `level=INFO msg="shutdown complete"`) {
			t.Errorf("log = %q, want no INFO 'shutdown complete' line", out)
		}
	})
}

// A dump still running when the drain budget ends is cancelled, and the
// teardown completes once it unwinds.
func TestDrainInFlightDump_CancelsADumpThatOutlivesTheDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		guard := &dump.Guard{}
		holdGuard(t, guard, nil, true)
		tickerDone := make(chan struct{})
		close(tickerDone)

		<-runTeardown(drainInFlightDump(guard, tickerDone, testGrace, log), time.Second)

		out := buf.String()
		if !strings.Contains(out, "drain budget exceeded; cancelling in-flight dump") {
			t.Errorf("log = %q, want a 'drain budget exceeded; cancelling in-flight dump' line", out)
		}
		if !strings.Contains(out, `level=INFO msg="shutdown complete"`) {
			t.Errorf("log = %q, want an INFO 'shutdown complete' line after the dump unwound", out)
		}
		if !guard.WaitIdle(t.Context()) {
			t.Error("guard still held after the teardown returned")
		}
	})
}

// A cancelled dump that does not unwind cannot hold the process past its
// stop: the teardown gives up with a warning.
func TestDrainInFlightDump_GivesUpOnADumpThatIgnoresCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		release := make(chan struct{})
		defer close(release)
		guard := &dump.Guard{}
		holdGuard(t, guard, release, false)
		tickerDone := make(chan struct{})
		close(tickerDone)

		<-runTeardown(drainInFlightDump(guard, tickerDone, testGrace, log), time.Second)

		out := buf.String()
		if !strings.Contains(out, "in-flight dump did not unwind") {
			t.Errorf("log = %q, want a warning that the in-flight dump did not unwind", out)
		}
		if strings.Contains(out, `level=INFO msg="shutdown complete"`) {
			t.Errorf("log = %q, want no INFO 'shutdown complete' line", out)
		}
	})
}

// With the built-in timer off there is no goroutine, so the teardown's wait
// must resolve at once.
func TestStartTicker_OffReturnsAClosedChannel(t *testing.T) {
	done := startTicker(t.Context(), startupRecord{}, 0, nil, slog.New(slog.DiscardHandler))
	if !closed(done) {
		t.Error("startTicker(interval=0) channel not closed, want closed")
	}
}

// The channel startTicker returns closes only once the timer goroutine has
// returned, which happens when its context ends.
func TestStartTicker_DoneClosesWhenTheTimerReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf syncBuffer
		trig, pgf, log := cycleTrigger(t, &buf, func(log *slog.Logger) *scheduler.Exclusive {
			return scheduler.NewExclusive(t.TempDir(), scheduler.WithLogger(log))
		})
		ctx, cancel := context.WithCancel(context.Background())
		fresh := startupRecord{known: true, remaining: time.Hour}

		done := startTicker(ctx, fresh, 24*time.Hour, trig, log)
		synctest.Wait()
		if closed(done) {
			t.Fatalf("done closed while the timer was waiting for its first tick; log = %q", buf.String())
		}

		cancel()
		synctest.Wait()
		if !closed(done) {
			t.Fatal("done still open after the timer's context ended")
		}
		if got := pgf.dumps.Load(); got != 0 {
			t.Errorf("dump calls = %d, want 0 (a fresh record skips the startup dump)", got)
		}
	})
}
