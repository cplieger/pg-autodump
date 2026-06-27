package obs

import (
	"path/filepath"
	"testing"
)

func TestDirWritable(t *testing.T) {
	t.Run("writable directory returns nil", func(t *testing.T) {
		if err := dirWritable(t.TempDir()); err != nil {
			t.Errorf("dirWritable(tempdir) = %v, want nil", err)
		}
	})

	t.Run("missing directory returns an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if err := dirWritable(missing); err == nil {
			t.Errorf("dirWritable(%q) = nil, want an error for a missing directory", missing)
		}
	})

	t.Run("probe file is removed after a successful check", func(t *testing.T) {
		dir := t.TempDir()
		if err := dirWritable(dir); err != nil {
			t.Fatalf("dirWritable: %v", err)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, ".pg-autodump-writable-*"))
		if len(matches) != 0 {
			t.Errorf("dirWritable left %d probe file(s) behind: %v", len(matches), matches)
		}
	})
}
