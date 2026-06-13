package dump

import (
	"context"
	"sync"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// runPool dumps every valid spec using a bounded pool of n workers
// (n is clamped to [1, len(specs)] by the caller). Results are written into a
// fixed-size slice indexed by spec position: no shared map, no lock on the
// result set, and deterministic ordering regardless of completion order.
//
// There is no per-host serialization. The common topology is one Postgres
// server with several databases; serializing per host would force that case
// fully serial and defeat the feature. Postgres handles concurrent dumps of
// distinct databases fine, and the global cap is the operator's single knob
// for the load placed on the server(s) and the backup volume during the run.
//
// Cancellation: when ctx is done the dispatcher stops handing out work and
// waits for in-flight workers to unwind. Indices never dispatched keep their
// pre-filled ReasonSkipped result, so the taxonomy stays closed and every spec
// yields exactly one Result.
func runPool(ctx context.Context, n int, specs []spec.DBSpec, dumpOne func(context.Context, *spec.DBSpec) Result) []Result {
	results := make([]Result, len(specs))
	for i := range specs {
		results[i] = Result{Host: specs[i].Host, DBName: specs[i].DBName, Reason: ReasonSkipped}
	}

	sem := make(chan struct{}, n)
	var wg sync.WaitGroup

	for i := range specs {
		// Priority cancel check: select picks randomly among ready cases, so
		// without this an already-cancelled run could still dispatch work.
		if ctx.Err() != nil {
			wg.Wait()
			return results
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return results
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = dumpOne(ctx, &specs[i])
		}(i)
	}

	wg.Wait()
	return results
}
