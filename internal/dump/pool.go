package dump

import (
	"context"
	"sync"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// runPool dumps every valid spec using a bounded pool of n workers (n is
// clamped to [1, len(specs)] by the caller). Results are written into a
// fixed-size slice indexed by spec position, so ordering is deterministic
// regardless of completion order.
//
// No per-host serialization: the common topology is one Postgres server with
// several databases, and serializing per host would force that case fully
// serial. The global cap n is the operator's single knob for load.
//
// On cancellation the dispatcher stops handing out work and waits for
// in-flight workers to unwind; undispatched indices keep their pre-filled
// ReasonSkipped result, so every spec yields exactly one Result.
func runPool(ctx context.Context, n int, specs []spec.DBSpec, dumpOne func(context.Context, *spec.DBSpec) Result) []Result {
	results := make([]Result, len(specs))
	for i := range specs {
		results[i] = Result{Host: specs[i].Host, DBName: specs[i].DBName, Reason: ReasonSkipped}
	}

	sem := make(chan struct{}, n)
	var wg sync.WaitGroup

	for i := range specs {
		// select picks randomly among ready cases, so without this priority
		// check an already-cancelled run could still dispatch work.
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
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = dumpOne(ctx, &specs[i])
		})
	}

	wg.Wait()
	return results
}
