package dump

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v4"
)

// healthOrchestrator wires an orchestrator over pgf whose cycle verdicts land
// on a real marker in a private directory, and returns both.
func healthOrchestrator(t *testing.T, pgf *fakePG, specs []spec.DBSpec) (*Orchestrator, *health.Marker) {
	t.Helper()
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
	orch := New(&Params{
		PG:          pgf,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Health:      marker,
		DumpDir:     t.TempDir(),
		Specs:       specs,
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        1,
	})
	return orch, marker
}

// A cycle in which every configured database dumped and verified marks the
// container healthy, from any prior state.
func TestRunFullSuccessSetsHealthy(t *testing.T) {
	orch, marker := healthOrchestrator(t, &fakePG{}, []spec.DBSpec{
		{Host: "h1", Port: 5432, DBName: "app", User: "u"},
		{Host: "h2", Port: 5432, DBName: "app", User: "u"},
	})
	if marker.CheckHealthy() {
		t.Fatal("marker present before any cycle ran; the fixture must start unhealthy")
	}

	res := orch.Run(deadlineCtx(t))
	for i, r := range res {
		if r.Reason != ReasonOK {
			t.Fatalf("spec[%d] reason = %q, want ok (detail %q)", i, r.Reason, r.Detail)
		}
	}
	if !marker.CheckHealthy() {
		t.Fatal("cycle completed with 0 failures; want the health marker set, got absent")
	}
}

// A cycle with any failed database marks the container unhealthy even though
// the other databases dumped fresh files: health means the last cycle fully
// succeeded, the same verdict the last-run record carries.
func TestRunPartialFailureMarksUnhealthy(t *testing.T) {
	pgf := &fakePG{probe: func(_ context.Context, c Conn) (int, FailKind, error) {
		if c.Host == "bad" {
			return 0, FailConnect, errors.New("connection refused")
		}
		return 18, FailNone, nil
	}}
	orch, marker := healthOrchestrator(t, pgf, []spec.DBSpec{
		{Host: "good", Port: 5432, DBName: "app", User: "u"},
		{Host: "bad", Port: 5432, DBName: "app", User: "u"},
	})
	marker.Set(true)

	res := orch.Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK || res[1].Reason != ReasonConnectError {
		t.Fatalf("reasons = %q, %q; want ok + connect_error (a partial failure)", res[0].Reason, res[1].Reason)
	}
	if marker.CheckHealthy() {
		t.Fatal("one database failed; want the health marker removed, got present")
	}
}

// A cycle over zero configured databases dumped nothing, so it is not a
// success: the marker the boot left behind is removed and the last-run record
// reads failed, keeping the next boot's startup dump due. The empty DB_SPECS
// the boot preflight refused must not turn healthy on the first cycle.
func TestRunNoDatabasesIsNotSuccess(t *testing.T) {
	orch, marker := healthOrchestrator(t, &fakePG{}, nil)
	marker.Set(true)

	if res := orch.Run(deadlineCtx(t)); len(res) != 0 {
		t.Fatalf("results = %d, want 0 for an empty spec set", len(res))
	}
	if marker.CheckHealthy() {
		t.Error("no database configured; want the health marker removed, got present")
	}
	rec, known := scheduler.NewStamp(StampPath(orch.dumpDir)).Last()
	if !known || rec.OK {
		t.Errorf("last-run record after an empty cycle = {known:%v ok:%v}, want {true false}", known, rec.OK)
	}
}

// A cycle cut short by shutdown writes no verdict at all: its killed dumps are
// an operator action, not a dump failure, so the previous cycle's record and
// marker both stand and a container recreated mid-cycle does not boot
// unhealthy with no failed dump to explain it.
func TestRunCancelledCycleRecordsNothing(t *testing.T) {
	started := make(chan struct{})
	pgf := &fakePG{dump: func(ctx context.Context, _ Conn, _ io.Writer) (int, string, error) {
		close(started)
		<-ctx.Done()
		return 1, "", ctx.Err()
	}}
	orch, marker := healthOrchestrator(t, pgf, []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}})
	marker.Set(true)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	res := orch.Run(ctx)

	if res[0].Reason != ReasonKilled {
		t.Fatalf("reason = %q, want killed; the cycle must be cut short for the rest to assert anything", res[0].Reason)
	}
	if !marker.CheckHealthy() {
		t.Error("marker removed by a shutdown-cancelled cycle; want the previous cycle's verdict to stand")
	}
	if _, known := scheduler.NewStamp(StampPath(orch.dumpDir)).Last(); known {
		t.Error("last-run record written by a shutdown-cancelled cycle; want none")
	}
}
