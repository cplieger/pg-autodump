package dump

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeWithMtime(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func mkSubdir(t *testing.T, dir, sub string) string {
	t.Helper()
	d := filepath.Join(dir, sub)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDueForStartupDump(t *testing.T) {
	interval := 24 * time.Hour
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	t.Run("no dumps means due", func(t *testing.T) {
		if !DueForStartupDump(t.TempDir(), interval, now) {
			t.Fatal("empty dump dir: want due (no dump yet)")
		}
	})

	t.Run("missing dir means due", func(t *testing.T) {
		if !DueForStartupDump(filepath.Join(t.TempDir(), "nope"), interval, now) {
			t.Fatal("missing dump dir: want due")
		}
	})

	t.Run("fresh dump in a server subdir suppresses startup", func(t *testing.T) {
		dir := t.TempDir()
		writeWithMtime(t, filepath.Join(mkSubdir(t, dir, "h_5432"), "app.dump"), now.Add(-1*time.Hour))
		if DueForStartupDump(dir, interval, now) {
			t.Fatal("a dump 1h old (< 24h interval): want NOT due")
		}
	})

	t.Run("stale dump triggers startup", func(t *testing.T) {
		dir := t.TempDir()
		writeWithMtime(t, filepath.Join(mkSubdir(t, dir, "h_5432"), "app.dump"), now.Add(-48*time.Hour))
		if !DueForStartupDump(dir, interval, now) {
			t.Fatal("a dump 48h old (>= 24h interval): want due")
		}
	})

	t.Run("legacy flat dump at root is considered", func(t *testing.T) {
		dir := t.TempDir()
		writeWithMtime(t, filepath.Join(dir, "app.dump"), now.Add(-1*time.Hour))
		if DueForStartupDump(dir, interval, now) {
			t.Fatal("a fresh legacy flat dump: want NOT due")
		}
	})

	t.Run("newest across subdirs wins", func(t *testing.T) {
		dir := t.TempDir()
		writeWithMtime(t, filepath.Join(mkSubdir(t, dir, "h1_5432"), "app.dump"), now.Add(-48*time.Hour)) // stale
		writeWithMtime(t, filepath.Join(mkSubdir(t, dir, "h2_5432"), "app.dump"), now.Add(-1*time.Hour))  // fresh
		if DueForStartupDump(dir, interval, now) {
			t.Fatal("one fresh dump among stale ones: want NOT due (newest wins)")
		}
	})

	t.Run("non-dump files are ignored", func(t *testing.T) {
		dir := t.TempDir()
		// A recent non-".dump" file must not count as a recent dump.
		writeWithMtime(t, filepath.Join(dir, "notes.txt"), now.Add(-1*time.Hour))
		if !DueForStartupDump(dir, interval, now) {
			t.Fatal("only a non-dump file present: want due (no dump artifact)")
		}
	})

	t.Run("boundary: exactly one interval old is due", func(t *testing.T) {
		dir := t.TempDir()
		writeWithMtime(t, filepath.Join(dir, "app.dump"), now.Add(-interval))
		if !DueForStartupDump(dir, interval, now) {
			t.Fatal("a dump exactly one interval old: want due (>= boundary)")
		}
	})
}
