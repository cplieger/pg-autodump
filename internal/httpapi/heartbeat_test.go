package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// heartbeatPG succeeds for any db whose name starts with "ok" and fails
// (exit 1) otherwise, so a single run yields an asymmetric mix of ok and
// failed results that discriminates the heartbeat tally.
type heartbeatPG struct{}

func (heartbeatPG) Probe(context.Context, dump.Conn) (int, dump.FailKind, error) {
	return 18, dump.FailNone, nil
}

func (heartbeatPG) Dump(_ context.Context, c dump.Conn, w io.Writer) (int, string, error) {
	if strings.HasPrefix(c.DBName, "ok") {
		_, _ = io.WriteString(w, "PGDMP")
		return 0, "", nil
	}
	return 1, "boom", nil
}

func (heartbeatPG) VerifyTOC(context.Context, string) error { return nil }

func TestTriggerRunHeartbeatTally(t *testing.T) {
	var buf bytes.Buffer
	orch := dump.New(&dump.Params{
		PG:      heartbeatPG{},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		DumpDir: t.TempDir(),
		Specs: []spec.DBSpec{
			{Host: "h", Port: 5432, DBName: "okdb1", User: "u"},
			{Host: "h", Port: 5432, DBName: "okdb2", User: "u"},
			{Host: "h", Port: 5432, DBName: "faildb", User: "u"},
		},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	tr := NewTrigger(&dump.Guard{}, orch, slog.New(slog.NewTextHandler(&buf, nil)))

	results, ok := tr.Run()
	if !ok {
		t.Fatal("Run ok = false, want true")
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	out := buf.String()
	if !strings.Contains(out, "dump cycle complete") {
		t.Fatalf("missing heartbeat log line, got %q", out)
	}
	for _, want := range []string{"total=3", "ok=2", "failed=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("heartbeat log missing %q, got %q", want, out)
		}
	}
}
