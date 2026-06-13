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
