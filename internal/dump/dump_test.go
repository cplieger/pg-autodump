package dump

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// fakePG is a configurable PGTool for tests: no network, no real pg binaries.
type fakePG struct {
	probe  func(ctx context.Context, c Conn) (int, FailKind, error)
	dump   func(ctx context.Context, c Conn, w io.Writer) (int, string, error)
	verify func(ctx context.Context, path string) error
}

func (f *fakePG) Probe(ctx context.Context, c Conn) (int, FailKind, error) {
	if f.probe != nil {
		return f.probe(ctx, c)
	}
	return 18, FailNone, nil
}

func (f *fakePG) Dump(ctx context.Context, c Conn, w io.Writer) (int, string, error) {
	if f.dump != nil {
		return f.dump(ctx, c, w)
	}
	_, _ = io.WriteString(w, "PGDMP-fake")
	return 0, "", nil
}

func (f *fakePG) VerifyTOC(ctx context.Context, path string) error {
	if f.verify != nil {
		return f.verify(ctx, path)
	}
	return nil
}

func deadlineCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestStageAndReplace(t *testing.T) {
	const dbname = "app"
	tests := []struct {
		name       string
		pg         *fakePG
		wantReason Reason
		wantFile   bool // should <dbname>.dump exist with new content afterward
	}{
		{
			name: "ok writes and replaces",
			pg: &fakePG{dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
				_, _ = io.WriteString(w, "newdump")
				return 0, "", nil
			}},
			wantReason: ReasonOK,
			wantFile:   true,
		},
		{
			name:       "empty dump is rejected",
			pg:         &fakePG{dump: func(_ context.Context, _ Conn, _ io.Writer) (int, string, error) { return 0, "", nil }},
			wantReason: ReasonEmpty,
		},
		{
			name: "truncated dump is rejected",
			pg: &fakePG{
				dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
					_, _ = io.WriteString(w, "partial")
					return 0, "", nil
				},
				verify: func(_ context.Context, _ string) error { return errors.New("TOC unreadable") },
			},
			wantReason: ReasonTruncated,
		},
		{
			name:       "non-zero exit is pg_error",
			pg:         &fakePG{dump: func(_ context.Context, _ Conn, _ io.Writer) (int, string, error) { return 1, "FATAL: nope", nil }},
			wantReason: ReasonPGError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, dbname+".dump")
			const known = "KNOWN-GOOD"
			if err := os.WriteFile(target, []byte(known), 0o600); err != nil {
				t.Fatal(err)
			}

			res := stageAndReplace(deadlineCtx(t), tt.pg, dir, dbname+".dump", Conn{Host: "h", Port: 5432, DBName: dbname, User: "u"})
			if res.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q (detail %q)", res.Reason, tt.wantReason, res.Detail)
			}

			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("target missing: %v", err)
			}
			if tt.wantFile {
				if string(got) == known {
					t.Fatal("target was not replaced on success")
				}
			} else if string(got) != known {
				// Property 1: a failed dump never overwrites the known-good file.
				t.Fatalf("known-good backup was clobbered on failure: got %q", got)
			}
		})
	}
}

func TestStageAndReplaceContextTimeout(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	pg := &fakePG{dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
		cancel() // simulate the run being cancelled mid-dump
		_, _ = io.WriteString(w, "partial")
		return 0, "", nil
	}}
	res := stageAndReplace(ctx, pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})
	if res.Reason != ReasonKilled {
		t.Fatalf("reason = %q, want killed", res.Reason)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		ctxErr   error
		name     string
		want     Reason
		exitCode int
		kind     FailKind
	}{
		{name: "deadline wins", exitCode: 1, ctxErr: context.DeadlineExceeded, kind: FailConnect, want: ReasonTimeout},
		{name: "cancel wins", exitCode: 1, ctxErr: context.Canceled, kind: FailAuth, want: ReasonKilled},
		{name: "connect", exitCode: 0, ctxErr: nil, kind: FailConnect, want: ReasonConnectError},
		{name: "auth", exitCode: 0, ctxErr: nil, kind: FailAuth, want: ReasonAuthError},
		{name: "version", exitCode: 0, ctxErr: nil, kind: FailVersion, want: ReasonVersionMismatch},
		{name: "generic exit", exitCode: 2, ctxErr: nil, kind: FailNone, want: ReasonPGError},
		{name: "clean none", exitCode: 0, ctxErr: nil, kind: FailNone, want: ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.exitCode, tt.ctxErr, tt.kind); got != tt.want {
				t.Fatalf("classify = %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestOrchestratorRunReportsEverySpec(t *testing.T) {
	specs := []spec.DBSpec{
		{Host: "h", Port: 5432, DBName: "good", User: "u"},
		{Raw: "bad", Invalid: "invalid format"},
		{Host: "h", Port: 5432, DBName: "dupe", User: "u", Invalid: "duplicate host:port:dbname (kept first)"},
	}
	orch := New(&Params{
		PG:          &fakePG{},
		DumpDir:     t.TempDir(),
		Specs:       specs,
		DumpTimeout: 30 * time.Second,
		Concurrency: 2,
	})
	results := orch.Run(deadlineCtx(t))

	if len(results) != len(specs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(specs))
	}
	if results[0].Reason != ReasonOK {
		t.Fatalf("valid spec reason = %q, want ok", results[0].Reason)
	}
	if results[1].Reason != ReasonInvalid {
		t.Fatalf("invalid spec reason = %q, want invalid", results[1].Reason)
	}
	if results[2].Reason != ReasonDuplicate {
		t.Fatalf("duplicate spec reason = %q, want duplicate", results[2].Reason)
	}
}

func TestOrchestratorProbeFailureClassified(t *testing.T) {
	orch := New(&Params{
		PG: &fakePG{probe: func(_ context.Context, _ Conn) (int, FailKind, error) {
			return 0, FailConnect, errors.New("connection refused")
		}},
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})
	results := orch.Run(deadlineCtx(t))
	if results[0].Reason != ReasonConnectError {
		t.Fatalf("reason = %q, want connect_error", results[0].Reason)
	}
}
