package dump

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/atomicfile/v2"
)

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

	ReclaimOrphans(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	ReclaimOrphans(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	ReclaimOrphans(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	ReclaimOrphans(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("reclaim created the missing dir %s", dir)
	}
}
