package dump

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/slogx/capture"
)

// fixedNow pins the clock to a 2026 instant so dump filenames produced
// during a run sort after any 2020-era fixtures a test pre-creates.
func fixedNow() func() time.Time {
	t := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func dumpOrchestrator(t *testing.T, dir string, keep int) *Orchestrator {
	t.Helper()
	return New(&Params{
		PG:          &fakePG{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         fixedNow(),
		DumpDir:     dir,
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        keep,
	})
}

func captureOrchestrator(t *testing.T, dir string, keep int, buf *bytes.Buffer) *Orchestrator {
	t.Helper()
	return New(&Params{
		PG:          &fakePG{},
		Logger:      slog.New(slog.NewTextHandler(buf, nil)),
		Now:         fixedNow(),
		DumpDir:     dir,
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        keep,
	})
}

func TestOrchestratorRunReportsEverySpec(t *testing.T) {
	specs := []spec.DBSpec{
		{Host: "h", Port: 5432, DBName: "good", User: "u"},
		{Raw: "bad", Invalid: "invalid format"},
		{Host: "h", Port: 5432, DBName: "dupe", User: "u", Invalid: "duplicate host:port:dbname (kept first)", Duplicate: true},
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

// An untyped probe error classifies as ReasonOther with the probe's own
// error text as Detail, not the bare reason string "other".
func TestOrchestratorProbeOtherUsesErrorDetail(t *testing.T) {
	t.Parallel()
	orch := New(&Params{
		PG: &fakePG{probe: func(_ context.Context, _ Conn) (int, FailKind, error) {
			return 0, FailNone, errors.New("weird probe failure")
		}},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "db", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
	})

	res := orch.Run(deadlineCtx(t))
	if res[0].Reason != ReasonOther {
		t.Fatalf("reason = %q, want other", res[0].Reason)
	}
	if res[0].Detail != "weird probe failure" {
		t.Fatalf("detail = %q, want the probe error text %q", res[0].Detail, "weird probe failure")
	}
}

// With keep=2 and three old copies plus one fresh dump, the two oldest are pruned.
func TestOrchestratorPrunesAfterSuccessfulDump(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srv := filepath.Join(dir, "h_5432")
	if err := os.MkdirAll(srv, 0o700); err != nil {
		t.Fatal(err)
	}
	old := []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	}
	for _, f := range old {
		if err := os.WriteFile(filepath.Join(srv, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res := dumpOrchestrator(t, dir, 2).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("dump reason = %q, want ok", res[0].Reason)
	}

	for _, gone := range []string{"app.20200101T000000Z.dump", "app.20200102T000000Z.dump"} {
		if _, err := os.Stat(filepath.Join(srv, gone)); !os.IsNotExist(err) {
			t.Errorf("expected %q to be pruned after successful dump, stat err = %v", gone, err)
		}
	}
	for _, keep := range []string{"app.20200103T000000Z.dump", "app.20260615T000000Z.dump"} {
		if _, err := os.Stat(filepath.Join(srv, keep)); err != nil {
			t.Errorf("expected %q to survive prune: %v", keep, err)
		}
	}
}

// keep=1 uses the stable "app.dump" scheme; no prune runs.
func TestOrchestratorKeepOneDoesNotPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srv := filepath.Join(dir, "h_5432")
	if err := os.MkdirAll(srv, 0o700); err != nil {
		t.Fatal(err)
	}
	old := []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	}
	for _, f := range old {
		if err := os.WriteFile(filepath.Join(srv, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res := dumpOrchestrator(t, dir, 1).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("dump reason = %q, want ok", res[0].Reason)
	}

	for _, f := range old {
		if _, err := os.Stat(filepath.Join(srv, f)); err != nil {
			t.Errorf("keep=1 must not prune timestamped copies; %q was removed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(srv, "app.dump")); err != nil {
		t.Errorf("keep=1 should write the stable app.dump: %v", err)
	}
}

// A prune that removes at least one copy logs a "pruned old dumps" info line.
func TestOrchestratorLogsPrunedCountWhenRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srv := filepath.Join(dir, "h_5432")
	if err := os.MkdirAll(srv, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	} {
		if err := os.WriteFile(filepath.Join(srv, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	res := captureOrchestrator(t, dir, 2, &buf).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("dump reason = %q, want ok", res[0].Reason)
	}
	if !strings.Contains(buf.String(), "pruned old dumps") {
		t.Fatalf("expected a 'pruned old dumps' info log when copies were removed, got %q", buf.String())
	}
}

// A prune that removes nothing (copies within the keep window) logs no
// "pruned old dumps" line.
func TestOrchestratorNoPruneLogWhenNothingRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srv := filepath.Join(dir, "h_5432")
	if err := os.MkdirAll(srv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv, "app.20200101T000000Z.dump"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res := captureOrchestrator(t, dir, 2, &buf).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("dump reason = %q, want ok", res[0].Reason)
	}
	if strings.Contains(buf.String(), "pruned old dumps") {
		t.Fatalf("prune removed nothing; must not log 'pruned old dumps', got %q", buf.String())
	}
}

func TestClamp(t *testing.T) {
	t.Parallel()
	if got := clamp(5, 1, 10); got != 5 {
		t.Errorf("clamp(5, 1, 10) = %d, want 5 (in-range value must pass through)", got)
	}
	if got := clamp(0, 1, 10); got != 1 {
		t.Errorf("clamp(0, 1, 10) = %d, want 1 (below lo clamps to lo)", got)
	}
	if got := clamp(15, 1, 10); got != 10 {
		t.Errorf("clamp(15, 1, 10) = %d, want 10 (above hi clamps to hi)", got)
	}
	// Production always passes lo=1; a non-1 lower bound exercises the lo
	// branch independently of that caller.
	if got := clamp(1, 2, 10); got != 2 {
		t.Errorf("clamp(1, 2, 10) = %d, want 2 (below lo clamps to lo)", got)
	}
}

func TestInvalidResultDBNameFallback(t *testing.T) {
	t.Parallel()

	empty := invalidResult(&spec.DBSpec{Raw: "bad-token", Invalid: "invalid format"})
	if empty.DBName != "bad-token" {
		t.Errorf("invalidResult(empty name) DBName = %q, want %q (raw fallback)", empty.DBName, "bad-token")
	}
	if empty.Reason != ReasonInvalid {
		t.Errorf("invalidResult reason = %q, want invalid", empty.Reason)
	}

	named := invalidResult(&spec.DBSpec{DBName: "named", Raw: "raw-token", Invalid: "duplicate host:port:dbname (kept first)", Duplicate: true})
	if named.DBName != "named" {
		t.Errorf("invalidResult(named) DBName = %q, want %q (parsed name kept)", named.DBName, "named")
	}
	if named.Reason != ReasonDuplicate {
		t.Errorf("invalidResult reason = %q, want duplicate", named.Reason)
	}
}

func TestLevelFor(t *testing.T) {
	tests := []struct {
		reason Reason
		want   string
	}{
		{reason: ReasonOK, want: "INFO"},
		{reason: ReasonInvalid, want: "WARN"},
		{reason: ReasonDuplicate, want: "WARN"},
		{reason: ReasonSkipped, want: "WARN"},
		// A killed dump is a graceful shutdown, not a failure.
		{reason: ReasonKilled, want: "WARN"},
		{reason: ReasonConnectError, want: "ERROR"},
		{reason: ReasonTimeout, want: "ERROR"},
		{reason: ReasonPGError, want: "ERROR"},
		{reason: ReasonOther, want: "ERROR"},
		{reason: ReasonEmpty, want: "ERROR"},
		{reason: ReasonTruncated, want: "ERROR"},
		{reason: ReasonAuthError, want: "ERROR"},
		{reason: ReasonVersionMismatch, want: "ERROR"},
		{reason: ReasonMkdirFailed, want: "ERROR"},
		{reason: ReasonRenameFailed, want: "ERROR"},
	}
	for _, tt := range tests {
		if got := levelFor(tt.reason).String(); got != tt.want {
			t.Errorf("levelFor(%q).String() = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

// requireCompletionLine fails the test unless finish() logged msg, so an
// attr-ABSENCE assertion below cannot pass vacuously on a missing record.
func requireCompletionLine(t *testing.T, rec *capture.Recorder, msg string) {
	t.Helper()
	if !rec.Contains(msg) {
		t.Fatalf("no %q record captured; finish() must log its completion line", msg)
	}
}

func finishOrchestrator(logger *slog.Logger) *Orchestrator {
	return New(&Params{PG: &fakePG{}, Logger: logger})
}

// server_version is logged only when the probe resolved a positive major.
func TestFinishServerVersionAttr(t *testing.T) {
	t.Run("logged when the major is positive", func(t *testing.T) {
		logger, rec := capture.New()
		finishOrchestrator(logger).finish(&Result{Host: "h", DBName: "db", Reason: ReasonOK, ServerVersion: 18}, nil)
		got, ok := rec.AttrValue("dump ok", "server_version")
		if !ok {
			t.Fatal("finish omitted server_version for a positive major; want it logged")
		}
		if got != "18" {
			t.Fatalf("server_version = %s, want 18", got)
		}
	})

	t.Run("omitted when the major is zero", func(t *testing.T) {
		logger, rec := capture.New()
		finishOrchestrator(logger).finish(&Result{Host: "h", DBName: "db", Reason: ReasonConnectError, ServerVersion: 0, Detail: "connect_error"}, nil)
		requireCompletionLine(t, rec, "dump connect_error")
		if _, ok := rec.AttrValue("dump connect_error", "server_version"); ok {
			t.Fatalf("finish logged server_version for an unknown (zero) major; want it omitted")
		}
	})
}

// detail is logged only for a failure that carries one; a success keeps its
// "ok (N bytes)" detail in the body, not the log line.
func TestFinishDetailAttr(t *testing.T) {
	t.Run("logged for a failure carrying a detail", func(t *testing.T) {
		logger, rec := capture.New()
		finishOrchestrator(logger).finish(&Result{Host: "h", DBName: "db", Reason: ReasonPGError, Detail: "dump failed: boom"}, nil)
		got, ok := rec.AttrValue("dump pg_error", "detail")
		if !ok {
			t.Fatal("finish omitted detail for a failure carrying one; want it logged")
		}
		if got != "dump failed: boom" {
			t.Fatalf("detail = %q, want %q", got, "dump failed: boom")
		}
	})

	t.Run("omitted on success", func(t *testing.T) {
		logger, rec := capture.New()
		finishOrchestrator(logger).finish(&Result{Host: "h", DBName: "db", Reason: ReasonOK, Detail: "ok (5 bytes)"}, nil)
		requireCompletionLine(t, rec, "dump ok")
		if _, ok := rec.AttrValue("dump ok", "detail"); ok {
			t.Fatalf("finish logged detail on success; want it omitted (the ok detail belongs in the body, not the log)")
		}
	})
}

// The diagnostic error is logged only when it differs from Detail, so the
// same text is never recorded twice on one line.
func TestFinishDiagErrAttr(t *testing.T) {
	t.Run("logged when the diagnostic differs from detail", func(t *testing.T) {
		logger, rec := capture.New()
		finishOrchestrator(logger).finish(
			&Result{Host: "h", DBName: "db", Reason: ReasonConnectError, Detail: "connect_error"},
			errors.New("dial tcp 10.0.0.1:5432: connect: connection refused"),
		)
		if _, ok := rec.AttrValue("dump connect_error", "err"); !ok {
			t.Fatal("finish omitted err when the probe diagnostic differs from detail; want it logged for the operator")
		}
	})

	t.Run("omitted when the diagnostic equals detail", func(t *testing.T) {
		logger, rec := capture.New()
		finishOrchestrator(logger).finish(
			&Result{Host: "h", DBName: "db", Reason: ReasonOther, Detail: "boom"},
			errors.New("boom"),
		)
		requireCompletionLine(t, rec, "dump other")
		if _, ok := rec.AttrValue("dump other", "err"); ok {
			t.Fatalf("finish logged err when the diagnostic equals detail; want it omitted (no duplicate of the same text)")
		}
	})
}
