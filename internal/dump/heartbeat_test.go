package dump

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// Orchestrator.Run emits exactly one "dump cycle complete" heartbeat per cycle
// with an accurate total/ok/failed tally, whichever entry point (server
// trigger or one-shot run) drove it. The README's alerting section documents a
// Loki absence rule keyed on this line, so its text and tally are a contract.
// The fake succeeds for db names starting with "ok" and fails otherwise, so
// one run yields an asymmetric mix that discriminates the tally.
func TestOrchestratorRunHeartbeatTally(t *testing.T) {
	var buf bytes.Buffer
	fake := &fakePG{
		dump: func(_ context.Context, c Conn, w io.Writer) (int, string, error) {
			if strings.HasPrefix(c.DBName, "ok") {
				_, _ = io.WriteString(w, "PGDMP")
				return 0, "", nil
			}
			return 1, "boom", nil
		},
	}
	orch := New(&Params{
		PG:      fake,
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
		DumpDir: t.TempDir(),
		Specs: []spec.DBSpec{
			{Host: "h", Port: 5432, DBName: "okdb1", User: "u"},
			{Host: "h", Port: 5432, DBName: "okdb2", User: "u"},
			{Host: "h", Port: 5432, DBName: "faildb", User: "u"},
		},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})

	results := orch.Run(deadlineCtx(t))
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	out := buf.String()
	if got := strings.Count(out, "dump cycle complete"); got != 1 {
		t.Fatalf("heartbeat count = %d, want exactly 1 (log %q)", got, out)
	}
	for _, want := range []string{"total=3", "ok=2", "failed=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("heartbeat log missing %q, got %q", want, out)
		}
	}
}
