package obs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/atomicfile/v3"
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

	// The probe leaves nothing behind on the happy path; asserted by reading
	// the directory rather than globbing a probe name, since the name is now
	// atomicfile's temp shape.
	t.Run("probe file is removed after a successful check", func(t *testing.T) {
		dir := t.TempDir()
		if err := dirWritable(dir); err != nil {
			t.Fatalf("dirWritable: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("dirWritable left %d entr(ies) behind: %v", len(entries), entries)
		}
	})

	// The error names the stage so the log distinguishes "cannot create"
	// from the teardown stages below it.
	t.Run("unwritable directory fails and names the stage", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory write permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := dirWritable(dir)
		if err == nil {
			t.Fatal("dirWritable(read-only dir) = nil, want an error")
		}
		if !strings.Contains(err.Error(), atomicfile.ProbeStageCreate.String()) {
			t.Errorf("dirWritable() error = %v, want it to name the %q stage",
				err, atomicfile.ProbeStageCreate)
		}
		if strings.Contains(err.Error(), "not attempted") {
			t.Errorf("dirWritable() error = %v, a real refusal must not be reported as an unattempted probe", err)
		}
	})
}

// The preflight's probe file must be a name the app's own stale-temp sweep
// reclaims (atomicfile.TempName/IsPackageTemp), unlike the retired
// ".pg-autodump-writable-*" name, which the sweep never recognized. The
// reclaim half is pinned in internal/dump
// (TestReclaimOrphansReapsRootProbeLeftovers).
func TestDirWritableProbeNameIsReclaimable(t *testing.T) {
	name := atomicfile.TempName()
	if !atomicfile.IsPackageTemp(name) {
		t.Fatalf("atomicfile.TempName() = %q, which its own sweep does not recognise", name)
	}
	if atomicfile.IsPackageTemp(".pg-autodump-writable-123") {
		t.Error("the retired app-owned probe name is reclaimable after all; the leak this adoption fixes was not real")
	}
}
