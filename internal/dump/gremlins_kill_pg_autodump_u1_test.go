package dump

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"syscall"
	"testing"
)

// gk_pg_autodump_u1_capture is an in-memory slog.Handler that records whether
// the "low free disk space for dumps" warning fired and the free_kb value it
// carried, so disk-space assertions read structured attributes instead of
// parsing text.
type gk_pg_autodump_u1_capture struct {
	mu          sync.Mutex
	freeKB      int64
	lowSpaceHit bool
}

func (h *gk_pg_autodump_u1_capture) Enabled(context.Context, slog.Level) bool { return true }

func (h *gk_pg_autodump_u1_capture) Handle(_ context.Context, r slog.Record) error {
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

func (h *gk_pg_autodump_u1_capture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *gk_pg_autodump_u1_capture) WithGroup(string) slog.Handler      { return h }

// gk_pg_autodump_u1_statfsFreeKB recomputes checkDiskSpace's free-KB formula
// independently in test code (gremlins never mutates test files), so it is a
// stable oracle for the magnitude of the production value regardless of which
// L22 operator is mutated.
func gk_pg_autodump_u1_statfsFreeKB(t *testing.T, dir string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs(%q) = %v", dir, err)
	}
	return int64(st.Bavail) * st.Bsize / 1024
}

// --- diskspace.go L22:29 ARITHMETIC_BASE (`*`) ----------------------------
//
// freeKB := int64(st.Bavail) * st.Bsize / 1024. The `*` scales free space UP
// by the block size (~4096), so any usable temp dir reports far more than 1 MB
// of free KB and the advisory warning stays silent at a 1 MB threshold.
// Mutating `*`->`/` collapses freeKB to a handful of KB (Bavail / Bsize / 1024,
// effectively 0), which trips the threshold and fires the warning.
func TestGkPgAutodumpU1CheckDiskSpaceMulScalesUp(t *testing.T) {
	dir := t.TempDir()
	if free := gk_pg_autodump_u1_statfsFreeKB(t, dir); free < 2000 {
		t.Skipf("temp dir has only %d KB free; need >1 MB to assert the no-warn baseline", free)
	}

	h := &gk_pg_autodump_u1_capture{}
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     slog.New(h),
		DumpDir:    dir,
		FreeKBWarn: 1000, // 1 MB: far below any real temp dir's true free space
	})

	o.checkDiskSpace()

	if h.lowSpaceHit {
		t.Fatalf("checkDiskSpace warned at a 1 MB threshold (free_kb=%d); real free space is far above 1 MB, "+
			"so freeKB must scale up via `* Bsize`, not `/ Bsize`", h.freeKB)
	}
}

// --- diskspace.go L22:40 ARITHMETIC_BASE (`/`) ----------------------------
// (this test also independently kills the L22:29 `*` mutant)
//
// With the warning forced on (threshold MaxInt64) checkDiskSpace logs free_kb.
// The oracle recomputes the same formula in unmutated test code, so under
// either L22 arithmetic mutant the logged value diverges by orders of
// magnitude: `/`->`*` inflates it ~1024^2 (~1.05e6) times; `*`->`/` shrinks it
// ~Bsize^2 (~1.6e7) times. A generous +/-100% band absorbs benign free-space
// drift between the two statfs calls yet rejects any operator swap.
func TestGkPgAutodumpU1CheckDiskSpaceFreeKBMagnitude(t *testing.T) {
	dir := t.TempDir()
	want := gk_pg_autodump_u1_statfsFreeKB(t, dir)
	if want <= 0 {
		t.Skipf("temp dir reports %d KB free; cannot exercise freeKB magnitude", want)
	}

	h := &gk_pg_autodump_u1_capture{}
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
		t.Fatalf("checkDiskSpace logged free_kb = %d, want within [%d, %d]; an L22 operator swap (`*`<->`/`) "+
			"moves it by ~1e6x or more", h.freeKB, lo, hi)
	}
}
