package dump

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/slogx/capture"
)

// reclaimedMsg is the message of ReclaimOrphans's reclaim line, the scope for
// the capture assertions below.
const reclaimedMsg = "reclaimed stale temp files"

// writeFile creates a file with marker content, failing the test on error.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Crash-orphaned atomicfile temps are reclaimed from the per-server
// subdirectories — where the app stages dump temps — while committed dumps are
// left alone. The pre-fix behavior scanned only the root, so a temp orphaned
// inside <host>_<port>/ accumulated forever on the backup volume.
func TestReclaimOrphansReapsServerSubdirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "dbhost_5432")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	subTemp := filepath.Join(sub, ".atomicfile-456.tmp")
	keepDump := filepath.Join(sub, "myapp.dump")
	writeFile(t, subTemp)
	writeFile(t, keepDump)

	ReclaimOrphans(t.Context(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := os.Stat(subTemp); !os.IsNotExist(err) {
		t.Errorf("%s still exists; want it reclaimed", subTemp)
	}
	if _, err := os.Stat(keepDump); err != nil {
		t.Errorf("committed dump %s was touched by reclaim: %v", keepDump, err)
	}
}

// The DUMP_DIR root is scanned too, because it is where obs.Preflight's
// writability probe lands and the probe is now an atomicfile-shaped temp: a
// directory that accepts a write but denies the unlink leaks one there, and
// this sweep is the only thing that can reclaim it. This REPLACES the earlier
// assertion that a root-level ".atomicfile-<digits>.tmp" is left alone — that
// rested on "files at the DUMP_DIR root are never the app's", which the probe
// made false. The narrowness the old assertion was really protecting is pinned
// instead by the operator's own root files below, which must survive: only
// atomicfile's exact shape is eligible, at the root as in a subdir.
func TestReclaimOrphansReapsRootProbeLeftovers(t *testing.T) {
	dir := t.TempDir()
	leakedProbe := filepath.Join(dir, atomicfile.TempName())
	operatorFile := filepath.Join(dir, "README-restore-steps.txt")
	operatorLookalike := filepath.Join(dir, ".atomicfile-notes.tmp")
	writeFile(t, leakedProbe)
	writeFile(t, operatorFile)
	writeFile(t, operatorLookalike)

	ReclaimOrphans(t.Context(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := os.Stat(leakedProbe); !os.IsNotExist(err) {
		t.Errorf("leaked preflight probe %s still exists; want it reclaimed", leakedProbe)
	}
	if _, err := os.Stat(operatorFile); err != nil {
		t.Errorf("operator's own root file %s was reclaimed: %v", operatorFile, err)
	}
	if _, err := os.Stat(operatorLookalike); err != nil {
		t.Errorf("prefix-alike root file %s was reclaimed: %v", operatorLookalike, err)
	}
}

// A file that merely resembles a temp (non-digit middle) and a DIRECTORY named
// like a temp are never reclaimed, even inside a scanned server subdir: only
// atomicfile's exact ".atomicfile-<digits>.tmp" shape for regular files is
// eligible.
func TestReclaimOrphansLeavesNonTemps(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "dbhost_5432")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	notTemp := filepath.Join(sub, ".atomicfile-notes.tmp")
	writeFile(t, notTemp)
	tempDir := filepath.Join(sub, ".atomicfile-789.tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ReclaimOrphans(t.Context(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := os.Stat(notTemp); err != nil {
		t.Errorf("non-temp %s was reclaimed: %v", notTemp, err)
	}
	if _, err := os.Stat(tempDir); err != nil {
		t.Errorf("directory %s was reclaimed: %v", tempDir, err)
	}
}

// A missing DUMP_DIR is tolerated: the scan is best-effort (the preflight and
// the dumps themselves surface a broken volume) and must not panic or create
// anything.
func TestReclaimOrphansMissingDirIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	ReclaimOrphans(t.Context(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("reclaim created the missing dir %s", dir)
	}
}

// The reclaim is reported through the logger the CALLER supplied, and the line
// carries how many temps were removed. That count is the operator's only record
// that orphans were accumulating in a directory this app stages a dump into
// every cycle, so it must reach the caller's own logger rather than whatever the
// process default happens to be.
func TestReclaimOrphansReportsTheReclaimToTheSuppliedLogger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "dbhost_5432")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(sub, ".atomicfile-456.tmp"))

	logger, rec := capture.New()
	ReclaimOrphans(t.Context(), dir, logger)

	got, ok := rec.AttrValue(reclaimedMsg, "count")
	if !ok {
		t.Fatalf("ReclaimOrphans(dir with 1 orphan) logged %v, want a %q line on the supplied logger",
			rec.Messages(), reclaimedMsg)
	}
	if got != "1" {
		t.Errorf("ReclaimOrphans(dir with 1 orphan) logged count = %q, want %q", got, "1")
	}
}

// A sweep with nothing to reclaim says nothing at all. Both lines are operator
// signals — the Info one says orphans were accumulating, the Warn one says they
// could not be removed and the volume is filling — so a cycle that reclaimed
// zero temps and failed on none must stay silent, or every clean run reports a
// problem it does not have.
func TestReclaimOrphansCleanSweepIsSilent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "dbhost_5432")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(sub, "app.dump"))

	logger, rec := capture.New()
	ReclaimOrphans(t.Context(), dir, logger)

	if rec.Len() != 0 {
		t.Errorf("ReclaimOrphans(clean dir) logged %v, want no output", rec.Messages())
	}
}

// A directory the sweep could read reports no cleanup failure, and the counts it
// returns are the outcome the caller sums into the reclaim line. reclaimDir's
// Warn names a dump directory the sweep could not walk, which is a volume-level
// fault an operator is expected to act on; a readable one must not produce it.
func TestReclaimDirReadableDirReportsCleanCounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".atomicfile-456.tmp"))

	logger, rec := capture.New()
	removed, unreclaimed := reclaimDir(t.Context(), dir, logger)

	if removed != 1 || unreclaimed != 0 {
		t.Errorf("reclaimDir(readable dir with 1 orphan) = (%d, %d), want (1, 0)", removed, unreclaimed)
	}
	if rec.Len() != 0 {
		t.Errorf("reclaimDir(readable dir) logged %v, want no output", rec.Messages())
	}
}

// A temp the sweep SEES and cannot unlink is reported, not silently dropped.
// This is the only outcome of the three that reaches an operator here:
// obs.Preflight's ladder probes the DUMP_DIR root only, so a per-server
// subdirectory that takes a create and refuses an unlink is outside its reach,
// and dumps keep succeeding because a dump commits with a rename. It also does
// not self-clear: atomicfile counts ENOENT on either the lstat or the remove as
// neither removed nor failed, so a benign race with a concurrent preflight
// probe cannot produce this count.
//
// The sticky bit is what separates "cannot unlink" from "cannot write": on a +t
// directory only the file's owner may unlink, while anyone with write
// permission may still create. Root bypasses that check, so the test skips
// rather than asserting a condition the kernel will not produce for it.
func TestReclaimOrphansReportsUnreclaimableTemps(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the sticky-bit unlink restriction this case needs")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "dbhost_5432")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orphan := filepath.Join(sub, ".atomicfile-789.tmp")
	writeFile(t, orphan)
	// r-x only: the entry is listable, so the temp is a candidate, and the
	// unlink is denied.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	removed, unreclaimed := reclaimDir(t.Context(), sub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if removed != 0 {
		t.Errorf("removed = %d, want 0: the unlink is denied", removed)
	}
	if unreclaimed != 1 {
		t.Errorf("unreclaimed = %d, want 1: the orphan was seen and could not be unlinked", unreclaimed)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan %s should still be there: %v", orphan, err)
	}
}

// The flat sweep can never report Unreadable, which is why ReclaimOrphans does
// not read it: atomicfile increments that counter only for a path BELOW the
// swept directory, and every sweep here is one directory deep. A subdirectory
// nobody can enter is therefore invisible to this sweep rather than counted,
// and the guard against that is the per-server loop visiting each one directly.
func TestReclaimDirUnreadableSubdirIsNotCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory-mode restriction this case needs")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(locked, ".atomicfile-321.tmp"))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	removed, unreclaimed := reclaimDir(t.Context(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if removed != 0 || unreclaimed != 0 {
		t.Errorf("reclaimDir(flat) = (%d, %d), want (0, 0): a subdirectory is out of a flat sweep's reach",
			removed, unreclaimed)
	}
}
