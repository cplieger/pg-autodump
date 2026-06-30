package dump

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// lowSpaceCapture is an in-memory slog.Handler that records whether the "low
// free disk space for dumps" warning fired and the free_kb value it carried, so
// disk-space assertions read structured attributes instead of parsing text.
type lowSpaceCapture struct {
	mu          sync.Mutex
	freeKB      int64
	lowSpaceHit bool
}

func (h *lowSpaceCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *lowSpaceCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "low free disk space for dumps" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lowSpaceHit = true
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "free_kb" {
			h.freeKB = a.Value.Int64()
			return false
		}
		return true
	})
	return nil
}

func (h *lowSpaceCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lowSpaceCapture) WithGroup(string) slog.Handler      { return h }

// statfsFreeKB recomputes checkDiskSpace's free-KB formula independently in test
// code, so it is a stable oracle for the magnitude of the production value.
func statfsFreeKB(t *testing.T, dir string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs(%q) = %v", dir, err)
	}
	return int64(st.Bavail) * st.Bsize / 1024
}

// A zero/negative threshold disables the check entirely: checkDiskSpace returns
// before any syscall and logs nothing at all, even when pointed at a missing
// directory (where an enabled check would log a statfs error).
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

// With a threshold above any real free space, the advisory warning fires.
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

// When statfs fails (a missing directory) the check logs the statfs-error
// warning and does NOT also emit the low-space warning.
func TestCheckDiskSpaceStatfsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	var buf bytes.Buffer
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(slog.NewTextHandler(&buf, nil)),
		DumpDir:    missing,
		FreeKBWarn: 1, // enabled, so statfs runs and then fails on the missing dir
	})

	o.checkDiskSpace()

	out := buf.String()
	if !strings.Contains(out, "cannot check free disk space") {
		t.Fatalf("expected a statfs-error warning, got %q", out)
	}
	if strings.Contains(out, "low free disk space for dumps") {
		t.Fatalf("statfs failed; must not also emit the low-space warning, got %q", out)
	}
}

// checkDiskSpace scales raw filesystem blocks UP by the block size, so any usable
// temp dir reports far more than 1 MB of free KB and the advisory warning stays
// silent at a 1 MB threshold. A miscomputed (un-scaled) free-KB value would trip
// the threshold and warn spuriously.
func TestCheckDiskSpaceNoWarnWhenSpaceAmple(t *testing.T) {
	dir := t.TempDir()
	if free := statfsFreeKB(t, dir); free < 2000 {
		t.Skipf("temp dir has only %d KB free; need >1 MB to assert the no-warn baseline", free)
	}

	h := &lowSpaceCapture{}
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(h),
		DumpDir:    dir,
		FreeKBWarn: 1000, // 1 MB: far below any real temp dir's true free space
	})

	o.checkDiskSpace()

	if h.lowSpaceHit {
		t.Fatalf("checkDiskSpace warned at a 1 MB threshold (free_kb=%d); real free space is far above 1 MB, "+
			"so freeKB must scale up by the block size", h.freeKB)
	}
}

// With the warning forced on (threshold MaxInt64) checkDiskSpace logs free_kb.
// The oracle recomputes the same formula in test code, so the logged value must
// match the real free space within a generous band that absorbs benign drift
// between the two statfs calls; an arithmetic error in the formula would move it
// by orders of magnitude.
func TestCheckDiskSpaceLogsAccurateFreeKB(t *testing.T) {
	dir := t.TempDir()
	want := statfsFreeKB(t, dir)
	if want <= 0 {
		t.Skipf("temp dir reports %d KB free; cannot exercise freeKB magnitude", want)
	}

	h := &lowSpaceCapture{}
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(h),
		DumpDir:    dir,
		FreeKBWarn: math.MaxInt64, // force the advisory warning so free_kb is logged
	})

	o.checkDiskSpace()

	if !h.lowSpaceHit {
		t.Fatalf("checkDiskSpace did not warn at threshold MaxInt64; expected a low-space warning with free_kb")
	}
	lo, hi := want/2, want*2
	if h.freeKB < lo || h.freeKB > hi {
		t.Fatalf("checkDiskSpace logged free_kb = %d, want within [%d, %d]", h.freeKB, lo, hi)
	}
}

// The low-space guard is `freeKB < freeKBWarn` (strict): free space exactly
// equal to the threshold is NOT low and must stay silent. Setting the threshold
// to the directory's current free space puts the comparison on its boundary, so
// a relaxed guard (free space <= threshold) would warn spuriously here. Both
// reads target the same temp dir back to back, so the value is stable.
func TestCheckDiskSpaceNoWarnAtExactThreshold(t *testing.T) {
	dir := t.TempDir()
	free := statfsFreeKB(t, dir)
	if free <= 0 {
		t.Skipf("temp dir reports %d KB free; cannot exercise the exact-threshold boundary", free)
	}

	h := &lowSpaceCapture{}
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(h),
		DumpDir:    dir,
		FreeKBWarn: free, // threshold == current free space: on the boundary
	})

	o.checkDiskSpace()

	if h.lowSpaceHit {
		t.Fatalf("checkDiskSpace warned with free space (free_kb=%d) exactly at the threshold (%d); "+
			"the guard is strict (`<`), so equal free space must not warn", h.freeKB, free)
	}
}
