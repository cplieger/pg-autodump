package dump

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
)

func TestRunPoolParallelismAndOrder(t *testing.T) {
	const n = 3
	specs := make([]spec.DBSpec, 8)
	for i := range specs {
		specs[i] = spec.DBSpec{Host: "h", Port: 5432, DBName: "db" + string(rune('a'+i)), User: "u"}
	}

	var active, maxActive atomic.Int64
	var mu sync.Mutex
	results := runPool(deadlineCtx(t), n, specs, func(_ context.Context, s *spec.DBSpec) Result {
		cur := active.Add(1)
		mu.Lock()
		if cur > maxActive.Load() {
			maxActive.Store(cur)
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return Result{DBName: s.DBName, Reason: ReasonOK}
	})

	if len(results) != len(specs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(specs))
	}
	for i, s := range specs {
		if results[i].DBName != s.DBName {
			t.Fatalf("result[%d] db = %q, want %q (order not preserved)", i, results[i].DBName, s.DBName)
		}
	}
	if mx := maxActive.Load(); mx > n {
		t.Fatalf("max concurrency %d exceeded cap %d", mx, n)
	}
	if maxActive.Load() < 2 {
		t.Fatalf("expected real parallelism, max concurrency was %d", maxActive.Load())
	}
}

func TestRunPoolCancelSkips(t *testing.T) {
	specs := []spec.DBSpec{
		{Host: "h", Port: 5432, DBName: "a", User: "u"},
		{Host: "h", Port: 5432, DBName: "b", User: "u"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: nothing should dispatch

	results := runPool(ctx, 2, specs, func(_ context.Context, s *spec.DBSpec) Result {
		t.Errorf("dumpOne called for %q despite cancelled context", s.DBName)
		return Result{Reason: ReasonOK}
	})
	for i, r := range results {
		if r.Reason != ReasonSkipped {
			t.Fatalf("result[%d] reason = %q, want skipped", i, r.Reason)
		}
	}
}

// runPool's in-dispatch cancel arm: with concurrency 1, spec[0]'s worker holds the only
// semaphore slot and blocks; cancelling while spec[1]'s dispatch is parked in the select
// must leave spec[1] ReasonSkipped (never dumped), distinct from the already-cancelled
// pre-check that TestRunPoolCancelSkips covers.
func TestRunPoolCancelMidDispatch(t *testing.T) {
	specs := []spec.DBSpec{
		{Host: "h", Port: 5432, DBName: "a", User: "u"},
		{Host: "h", Port: 5432, DBName: "b", User: "u"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	var dumped sync.Map

	results := runPool(ctx, 1, specs, func(_ context.Context, s *spec.DBSpec) Result {
		dumped.Store(s.DBName, true)
		if s.DBName == "a" {
			close(entered) // signal the slot is held
			cancel()       // cancel while spec[1]'s dispatch select is parked
			<-ctx.Done()   // hold the slot until cancellation has propagated
		}
		return Result{DBName: s.DBName, Reason: ReasonOK}
	})

	<-entered
	if results[0].Reason != ReasonOK {
		t.Fatalf("result[0] reason = %q, want ok (it ran before cancel)", results[0].Reason)
	}
	if results[1].Reason != ReasonSkipped {
		t.Fatalf("result[1] reason = %q, want skipped (cancelled mid-dispatch)", results[1].Reason)
	}
	if _, ran := dumped.Load("b"); ran {
		t.Fatal("spec b was dumped despite cancellation before its dispatch")
	}
}
