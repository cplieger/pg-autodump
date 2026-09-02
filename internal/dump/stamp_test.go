package dump

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx/capture"
)

// A cycle in which every configured database dumped and verified records a
// successful run, which is what suppresses the next boot's startup dump.
func TestRunRecordsFullySuccessfulCycle(t *testing.T) {
	dir := t.TempDir()
	specs := []spec.DBSpec{
		{Host: "h1", Port: 5432, DBName: "app", User: "u"},
		{Host: "h2", Port: 5432, DBName: "app", User: "u"},
	}
	res := orchestratorFor(t, dir, 1, specs).Run(deadlineCtx(t))
	for i, r := range res {
		if r.Reason != ReasonOK {
			t.Fatalf("spec[%d] reason = %q, want ok (detail %q)", i, r.Reason, r.Detail)
		}
	}

	rec, known := scheduler.NewStamp(StampPath(dir)).Last()
	if !known {
		t.Fatal("cycle completed with 0 failures; want a readable last-run record, got none")
	}
	if !rec.OK {
		t.Fatal("cycle completed with 0 failures; want the record to read ok, got failed")
	}
	if since := time.Since(rec.Time); since < 0 || since > time.Minute {
		t.Fatalf("record time = %v (%v ago), want the cycle's own completion time", rec.Time, since)
	}
}

// A cycle with any failed database records failed, even though other
// databases dumped fresh files: only a fully successful cycle may suppress
// the next boot's startup dump, so a restart retries the whole cycle.
func TestRunRecordsPartialFailureAsFailed(t *testing.T) {
	dir := t.TempDir()
	pgf := &fakePG{probe: func(_ context.Context, c Conn) (int, FailKind, error) {
		if c.Host == "bad" {
			return 0, FailConnect, errors.New("connection refused")
		}
		return 18, FailNone, nil
	}}
	orch := New(&Params{
		PG:      pgf,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		DumpDir: dir,
		Specs: []spec.DBSpec{
			{Host: "good", Port: 5432, DBName: "app", User: "u"},
			{Host: "bad", Port: 5432, DBName: "app", User: "u"},
		},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        1,
	})
	res := orch.Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK || res[1].Reason != ReasonConnectError {
		t.Fatalf("reasons = %q, %q; want ok + connect_error (a partial failure)", res[0].Reason, res[1].Reason)
	}

	rec, known := scheduler.NewStamp(StampPath(dir)).Last()
	if !known {
		t.Fatal("cycle completed; want a readable last-run record, got none")
	}
	if rec.OK {
		t.Fatal("one database failed; want the record to read failed, got ok (a partial cycle must not suppress the startup dump)")
	}
}

// A record that cannot be written warns and names the consequence; the cycle
// result and heartbeat are untouched. The stamp path is occupied by a
// directory so the write fails for any uid, root included.
func TestRunRecordFailureWarnsWithoutFailingCycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(StampPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	logger, rec := capture.New()
	orch := New(&Params{
		PG:          &fakePG{},
		Logger:      logger,
		DumpDir:     dir,
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        1,
	})
	res := orch.Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("reason = %q, want ok (a record failure must not affect the cycle)", res[0].Reason)
	}
	if !rec.Contains("dump cycle complete") {
		t.Fatal("heartbeat missing; a record failure must not suppress the cycle-complete line")
	}
	if !rec.Contains("cannot record the cycle outcome; next boot fires a startup dump") {
		t.Fatal("record write failed; want a warning naming the consequence, got none")
	}
}
