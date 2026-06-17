package dump

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// fixedNow returns a clock pinned to a 2026 instant so timestamped dump
// filenames produced during a run sort AFTER any 2020-era fixtures a test
// pre-creates (newest-last), making prune outcomes deterministic.
func fixedNow() func() time.Time {
	t := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// --- clamp: L182 / L185 CONDITIONALS_NEGATION -----------------------------
//
// clamp(v,lo,hi) returns lo when v<lo, hi when v>hi, else v. The negation
// mutants (`v < lo` -> `v >= lo`, `v > hi` -> `v <= hi`) reorder the branches
// so an in-range value gets clamped to a bound. Asserting the exact in-range
// passthrough and both clamped ends pins every comparison.
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
	// A non-1 lower bound keeps `lo` from being a constant across all call
	// sites (kills a "lo hardcoded to 1" mutant and exercises the lo branch
	// independently of the production caller, which always passes lo=1).
	if got := clamp(1, 2, 10); got != 2 {
		t.Errorf("clamp(1, 2, 10) = %d, want 2 (below lo clamps to lo)", got)
	}
}

// --- invalidResult: L162 CONDITIONALS_NEGATION ----------------------------
//
// invalidResult uses the raw token as the DB label only when the parsed name
// is empty (`db == ""`). The negation (`db != ""`) inverts that, so a parsed
// name gets discarded for the raw token and an empty name is left blank.
func TestInvalidResultDBNameFallback(t *testing.T) {
	t.Parallel()

	empty := invalidResult(&spec.DBSpec{Raw: "bad-token", Invalid: "invalid format"})
	if empty.DBName != "bad-token" {
		t.Errorf("invalidResult(empty name) DBName = %q, want %q (raw fallback)", empty.DBName, "bad-token")
	}
	if empty.Reason != ReasonInvalid {
		t.Errorf("invalidResult reason = %q, want invalid", empty.Reason)
	}

	named := invalidResult(&spec.DBSpec{DBName: "named", Raw: "raw-token", Invalid: "duplicate host:port:dbname (kept first)"})
	if named.DBName != "named" {
		t.Errorf("invalidResult(named) DBName = %q, want %q (parsed name kept)", named.DBName, "named")
	}
	if named.Reason != ReasonDuplicate {
		t.Errorf("invalidResult reason = %q, want duplicate", named.Reason)
	}
}

// --- stderrDetail: L144 CONDITIONALS_NEGATION -----------------------------
//
// stderrDetail returns a generic line for empty stderr (`tail == ""`) and an
// annotated line otherwise. The negation (`tail != ""`) swaps the two arms.
func TestStderrDetail(t *testing.T) {
	t.Parallel()
	if got := stderrDetail(""); got != "dump failed (pg_dump exited non-zero)" {
		t.Errorf("stderrDetail(%q) = %q, want generic message", "", got)
	}
	if got := stderrDetail("FATAL: nope"); got != "dump failed: FATAL: nope" {
		t.Errorf("stderrDetail(%q) = %q, want annotated message", "FATAL: nope", got)
	}
}

// --- stageAndReplace dump error with exit 0: L113 CONDITIONALS_NEGATION ----
//
// A non-context dump error with a zero exit code classifies as ReasonOther
// carrying the error text (`dumpErr != nil && exitCode == 0`). Negating either
// operand falls through to the size/verify path, mislabelling the failure.
func TestStageAndReplaceDumpErrorExitZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pg := &fakePG{dump: func(_ context.Context, _ Conn, _ io.Writer) (int, string, error) {
		return 0, "", errors.New("pipe error")
	}}

	res := stageAndReplace(deadlineCtx(t), pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})

	if res.Reason != ReasonOther {
		t.Fatalf("reason = %q, want other (dump error with exit 0)", res.Reason)
	}
	if res.Detail != "pipe error" {
		t.Fatalf("detail = %q, want %q (the dump error text)", res.Detail, "pipe error")
	}
}

// --- pruneOldDumps minimal-name boundary: L55 BOUNDARY + ARITHMETIC_BASE ---
//
// The matcher requires a name strictly longer than prefix+suffix
// (`len(n) > len(prefix)+len(suffix)`) so the degenerate "app..dump" (empty
// timestamp) is NOT a retained copy. The boundary mutant (`>=`) and the
// arithmetic mutant (`+` -> `-`) both admit that degenerate name, which then
// sorts oldest-first and gets pruned. With two real copies at keep=2 the
// correct behaviour removes nothing; either mutant removes one.
func TestPruneOldDumpsIgnoresEmptyTimestampName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := []string{
		"app..dump", // degenerate: len == len("app.")+len(".dump"), must be ignored
		"app.20260103T000000Z.dump",
		"app.20260104T000000Z.dump",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := pruneOldDumps(dir, "app", 2)
	if err != nil {
		t.Fatalf("pruneOldDumps: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (the two real copies are within keep=2; the degenerate name must not count)", removed)
	}
	for _, f := range files {
		if _, statErr := os.Stat(filepath.Join(dir, f)); statErr != nil {
			t.Errorf("expected %q to survive prune: %v", f, statErr)
		}
	}
}

// dumpOrchestrator builds an Orchestrator that dumps one database "app"
// successfully (fakePG default), at a fixed clock, with the given keep.
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

// --- prune-after-success gate: L135 CONDITIONALS_NEGATION (x2) -------------
//
// After a successful dump with keep>1 the orchestrator prunes old timestamped
// copies (`res.Reason == ReasonOK && o.keep > 1`). Negating either operand
// skips the prune, so stale copies beyond the window survive. With three 2020
// copies + one fresh 2026 copy at keep=2, the two oldest must be removed.
func TestOrchestratorPrunesAfterSuccessfulDump(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	}
	for _, f := range old {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res := dumpOrchestrator(t, dir, 2).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("dump reason = %q, want ok", res[0].Reason)
	}

	// keep=2: the freshest 2026 copy + the newest 2020 copy survive; the two
	// oldest 2020 copies are pruned.
	for _, gone := range []string{"app.20200101T000000Z.dump", "app.20200102T000000Z.dump"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("expected %q to be pruned after successful dump, stat err = %v", gone, err)
		}
	}
	for _, keep := range []string{"app.20200103T000000Z.dump", "app.20260615T000000Z.dump"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("expected %q to survive prune: %v", keep, err)
		}
	}
}

// --- prune-after-success gate: L135 CONDITIONALS_BOUNDARY ------------------
//
// With keep==1 the stable "app.dump" scheme is used and NO prune runs
// (`o.keep > 1` is false). The boundary mutant (`o.keep >= 1`) would run a
// keep=1 prune that deletes existing timestamped copies. They must survive.
func TestOrchestratorKeepOneDoesNotPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	}
	for _, f := range old {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res := dumpOrchestrator(t, dir, 1).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("dump reason = %q, want ok", res[0].Reason)
	}

	for _, f := range old {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("keep=1 must not prune timestamped copies; %q was removed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "app.dump")); err != nil {
		t.Errorf("keep=1 should write the stable app.dump: %v", err)
	}
}

// --- probe failure detail: L117 CONDITIONALS_NEGATION (x2) -----------------
//
// When a probe reports no typed FailKind but a raw error, the failure
// classifies as ReasonOther and the human detail is the probe error text
// (`reason == ReasonOther && perr != nil`). Negating either operand leaves the
// detail as the bare reason string ("other") instead of the error.
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

// --- checkDiskSpace threshold gate: L13 BOUNDARY + NEGATION ----------------
//
// A zero/negative threshold disables the check entirely (`o.freeKBWarn <= 0`
// returns before any syscall). The boundary (`< 0`) and negation (`> 0`)
// mutants both proceed past the guard; pointed at a missing directory the
// statfs fails and logs "cannot check free disk space". A disabled check must
// log nothing at all.
func TestCheckDiskSpaceDisabledLogsNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(slog.NewTextHandler(&buf, nil)),
		DumpDir:    filepath.Join(t.TempDir(), "does-not-exist"),
		FreeKBWarn: 0, // disabled
	})

	o.checkDiskSpace()

	if buf.Len() != 0 {
		t.Fatalf("disabled disk-space check logged %q, want no output", buf.String())
	}
}

// With a threshold above any real free space, the advisory warning fires
// (`freeKB < o.freeKBWarn`). The negation mutant on the enabling guard
// (`o.freeKBWarn > 0`) would return early and emit nothing.
func TestCheckDiskSpaceWarnsBelowThreshold(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(slog.NewTextHandler(&buf, nil)),
		DumpDir:    t.TempDir(),
		FreeKBWarn: math.MaxInt64, // every real volume is below this
	})

	o.checkDiskSpace()

	if !strings.Contains(buf.String(), "low free disk space for dumps") {
		t.Fatalf("expected a low-space warning, got %q", buf.String())
	}
}

// captureOrchestrator builds an Orchestrator that dumps one database "app"
// successfully (fakePG default), at a fixed clock, with the given keep, routing
// its log to buf so the prune-outcome log lines can be asserted.
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

// --- prune-outcome log gate: L138 CONDITIONALS_NEGATION --------------------
//
// After a successful prune that removed at least one copy, the orchestrator
// emits an info line (`removed > 0`). The negation (`removed <= 0`) suppresses
// that line whenever something was actually pruned. With three 2020 copies + a
// fresh 2026 copy at keep=2, two copies are removed, so the line must appear.
func TestOrchestratorLogsPrunedCountWhenRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, f := range []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
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

// --- prune-outcome log gate: L138 CONDITIONALS_BOUNDARY --------------------
//
// When the prune ran but removed nothing (the copies are within the keep
// window), NO info line is emitted (`removed > 0` is false). The boundary
// mutant (`removed >= 0`) would log "pruned old dumps ... removed 0" on every
// successful keep>1 dump; the negation mutant (`removed <= 0`) would likewise
// log on the removed==0 case. One existing 2020 copy + the fresh 2026 copy at
// keep=2 prunes nothing, so the line must be absent.
func TestOrchestratorNoPruneLogWhenNothingRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.20200101T000000Z.dump"), []byte("x"), 0o600); err != nil {
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
