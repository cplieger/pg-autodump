package dump

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestDumpFileName(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 13, 22, 30, 5, 0, time.UTC)
	tests := []struct {
		name string
		want string
		keep int
	}{
		{name: "keep_default_stable", keep: 1, want: "app.dump"},
		{name: "keep_zero_treated_as_stable", keep: 0, want: "app.dump"},
		{name: "keep_many_timestamped", keep: 3, want: "app.20260613T223005Z.dump"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dumpFileName("app", tt.keep, ts); got != tt.want {
				t.Fatalf("dumpFileName(app, %d) = %q, want %q", tt.keep, got, tt.want)
			}
		})
	}
}

// dumpFileName must be sortable by name == sortable by time, so prune can sort
// lexically. Two timestamps an hour apart must keep their chronological order
// under a plain string sort.
func TestDumpFileNameLexicallySortable(t *testing.T) {
	t.Parallel()
	older := dumpFileName("app", 2, time.Date(2026, 6, 13, 1, 0, 0, 0, time.UTC))
	newer := dumpFileName("app", 2, time.Date(2026, 6, 13, 2, 0, 0, 0, time.UTC))
	names := []string{newer, older}
	sort.Strings(names)
	if names[0] != older || names[1] != newer {
		t.Fatalf("lexical sort did not order by time: %v", names)
	}
}

func TestPruneOldDumps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Five timestamped copies for "app", oldest first.
	stamps := []string{
		"app.20260101T000000Z.dump",
		"app.20260102T000000Z.dump",
		"app.20260103T000000Z.dump",
		"app.20260104T000000Z.dump",
		"app.20260105T000000Z.dump",
	}
	for _, s := range stamps {
		write(s)
	}
	// Decoys that must never be touched: a bare stable file, a prefix-colliding
	// db, and an unrelated file.
	write("app.dump")                      // bare stable file (keep<=1 scheme)
	write("appdata.20260101T000000Z.dump") // different db that shares a prefix
	write("notes.txt")

	removed, err := pruneOldDumps(dir, "app", 2)
	if err != nil {
		t.Fatalf("pruneOldDumps: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}

	mustExist := []string{
		"app.20260104T000000Z.dump", // newest two kept
		"app.20260105T000000Z.dump",
		"app.dump",                      // bare file untouched
		"appdata.20260101T000000Z.dump", // prefix-collision db untouched
		"notes.txt",
	}
	for _, n := range mustExist {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("expected %q to survive prune: %v", n, err)
		}
	}
	mustBeGone := []string{
		"app.20260101T000000Z.dump",
		"app.20260102T000000Z.dump",
		"app.20260103T000000Z.dump",
	}
	for _, n := range mustBeGone {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("expected %q to be pruned, stat err = %v", n, err)
		}
	}
}

func TestPruneOldDumpsNoopWhenUnderLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, s := range []string{"app.20260101T000000Z.dump", "app.20260102T000000Z.dump"} {
		if err := os.WriteFile(filepath.Join(dir, s), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := pruneOldDumps(dir, "app", 3)
	if err != nil {
		t.Fatalf("pruneOldDumps: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (under limit)", removed)
	}
}

func TestPruneOldDumpsSkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	// A directory whose name matches the "<dbname>.<ts>.dump" pattern and sorts
	// oldest. The IsDir guard must skip it, so prune never counts or removes a
	// directory as a dump artifact.
	matchingDir := filepath.Join(dir, "app.20260101T000000Z.dump")
	if err := os.Mkdir(matchingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"app.20260104T000000Z.dump", "app.20260105T000000Z.dump"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := pruneOldDumps(dir, "app", 1)
	if err != nil {
		t.Fatalf("pruneOldDumps: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the oldest file is pruned; the matching directory is skipped)", removed)
	}
	if _, statErr := os.Stat(matchingDir); statErr != nil {
		t.Fatalf("a directory matching the dump pattern was removed by prune: %v", statErr)
	}
}

// The matcher requires a name strictly longer than "<dbname>." + ".dump", so
// the degenerate "app..dump" (empty timestamp) is NOT counted as a retained
// copy. With two real copies at keep=2 the correct behaviour removes nothing.
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

func TestPruneOldDumpsReadDirError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	removed, err := pruneOldDumps(missing, "app", 2)
	if err == nil {
		t.Fatal("pruneOldDumps(missing dir) err = nil, want a ReadDir error")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 when the directory cannot be read", removed)
	}
}

func TestPruneOldDumpsRemoveError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write-permission bits, so os.Remove never returns EACCES")
	}
	dir := t.TempDir()
	for _, f := range []string{
		"app.20260101T000000Z.dump",
		"app.20260102T000000Z.dump",
		"app.20260103T000000Z.dump",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A read-only dir makes os.Remove of its entries fail (EACCES) for a non-root
	// process, exercising removeDumps's error arm and pruneOldDumps's error return.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so t.TempDir cleanup can recurse

	removed, err := pruneOldDumps(dir, "app", 1)
	if err == nil {
		t.Fatal("pruneOldDumps on a read-only dir err = nil, want a remove error")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 when every remove fails", removed)
	}
}
