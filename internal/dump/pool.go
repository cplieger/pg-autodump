package dump

import (
	"context"
	"sync"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// runPool dumps every spec with at most n workers (the caller clamps n to
// [1, len(specs)]) and returns exactly one Result per spec, indexed by spec
// position. On cancellation it waits for in-flight workers; undispatched
// specs keep a reasonSkipped result.
//
// There is no per-host limit: the usual setup is one server with several
// databases, which a per-host limit would make fully serial.
func runPool(ctx context.Context, n int, specs []spec.DBSpec, dumpOne func(context.Context, *spec.DBSpec) Result) []Result {
	results := make([]Result, len(specs))
	for i := range specs {
		results[i] = Result{Host: specs[i].Host, Port: specs[i].Port, DBName: specs[i].DBName, Reason: reasonSkipped}
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
