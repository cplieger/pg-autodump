package dump

import (
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// lowSpaceMsg is checkDiskSpace's low-space warning message.
const lowSpaceMsg = "low free disk space for dumps"

// fixedFreeSpace stubs o.freeSpace to report a fixed free-KB value, avoiding
// drift from the live filesystem.
func fixedFreeSpace(freeKB int64) func(string) (int64, error) {
	return func(string) (int64, error) { return freeKB, nil }
}

// The injected probe makes the freeKB < freeKBWarn boundary exact and
// deterministic instead of racing the live filesystem.
func TestCheckDiskSpaceThresholdBoundary(t *testing.T) {
	t.Parallel()
	const warn = 1_000_000 // 1 GB threshold, in KB

	tests := []struct {
		name     string
		freeKB   int64
		wantWarn bool
	}{
		{"one KB below threshold warns", warn - 1, true},
		{"exactly at threshold stays silent", warn, false},
		{"one KB above threshold stays silent", warn + 1, false},
		{"far below threshold warns", warn / 4, true},
		{"far above threshold stays silent", warn * 4, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, rec := capture.New()
			o := New(&Params{
				PG:         &fakePG{},
				Logger:     logger,
				DumpDir:    t.TempDir(),
				FreeKBWarn: warn,
			})
			o.freeSpace = fixedFreeSpace(tc.freeKB)

			o.checkDiskSpace()

			if hit := rec.Contains(lowSpaceMsg); hit != tc.wantWarn {
				t.Fatalf("free_kb=%d, threshold=%d: warned=%v, want warned=%v",
					tc.freeKB, warn, hit, tc.wantWarn)
			}
			if tc.wantWarn {
				if got, ok := rec.AttrValue(lowSpaceMsg, "free_kb"); !ok || got != strconv.FormatInt(tc.freeKB, 10) {
					t.Errorf("logged free_kb = %q (found=%v), want %d", got, ok, tc.freeKB)
				}
				if got, ok := rec.AttrValue(lowSpaceMsg, "warn_below_kb"); !ok || got != strconv.FormatInt(warn, 10) {
					t.Errorf("logged warn_below_kb = %q (found=%v), want %d", got, ok, warn)
				}
			}
		})
	}
}

// A zero or negative threshold disables the check: checkDiskSpace must return
// before probing the filesystem at all (no statfs, no log).
func TestCheckDiskSpaceDisabledSkipsProbe(t *testing.T) {
	t.Parallel()
	for _, warn := range []int64{0, -1} {
		t.Run("threshold="+strconv.FormatInt(warn, 10), func(t *testing.T) {
			t.Parallel()
			logger, rec := capture.New()
			probed := false
			o := New(&Params{
				PG:         &fakePG{},
				Logger:     logger,
				DumpDir:    t.TempDir(),
				FreeKBWarn: warn,
			})
			o.freeSpace = func(string) (int64, error) {
				probed = true
				return 0, nil
			}

			o.checkDiskSpace()

			if probed {
				t.Errorf("disabled check (threshold=%d) probed the filesystem; it must return first", warn)
			}
			if rec.Len() != 0 {
				t.Errorf("disabled check logged %v, want no output", rec.Messages())
			}
		})
	}
}

// A failed probe logs the probe-error warning, not the low-space warning (a
// failed reading is unknown, not low).
func TestCheckDiskSpaceProbeError(t *testing.T) {
	t.Parallel()
	logger, rec := capture.New()
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     logger,
		DumpDir:    t.TempDir(),
		FreeKBWarn: 1000,
	})
	o.freeSpace = func(string) (int64, error) { return 0, errors.New("statfs boom") }

	o.checkDiskSpace()

	if !rec.Contains("cannot check free disk space") {
		t.Errorf("expected a probe-error warning, got %v", rec.Messages())
	}
	if rec.Contains(lowSpaceMsg) {
		t.Errorf("probe failed; must not also emit the low-space warning, got %v", rec.Messages())
	}
}

// statfsFreeKB is the real syscall the Orchestrator wires in by default; the
// injected tests above stub it out.
func TestStatfsFreeKB(t *testing.T) {
	t.Parallel()

	free, err := statfsFreeKB(t.TempDir())
	if err != nil {
		t.Fatalf("statfsFreeKB(tempdir) unexpected error: %v", err)
	}
	if free <= 0 {
		t.Errorf("statfsFreeKB(tempdir) = %d, want > 0", free)
	}

	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if _, err := statfsFreeKB(missing); err == nil {
		t.Errorf("statfsFreeKB(%q) = nil error, want statfs failure", missing)
	}
}

// A kilobyte figure must fall in [bavail, bavail*bsize]; a figure in blocks or
// bytes would sit outside this band by orders of magnitude.
func TestStatfsFreeKBReportsKilobytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("Statfs(%q) failed: %v", dir, err)
	}
	if st.Bsize < 1024 || st.Bavail == 0 {
		t.Skipf("filesystem backing %q reports bsize=%d bavail=%d; the kilobyte band needs blocks of "+
			"at least 1KB and some free space", dir, st.Bsize, st.Bavail)
	}
	atLeast, atMost := int64(st.Bavail), int64(st.Bavail)*st.Bsize

	freeKB, err := statfsFreeKB(dir)
	if err != nil {
		t.Fatalf("statfsFreeKB(%q) unexpected error: %v", dir, err)
	}

	if freeKB < atLeast || freeKB > atMost {
		t.Errorf("statfsFreeKB(%q) = %d, want a kilobyte figure in [%d, %d] (bavail=%d blocks of %d bytes)",
			dir, freeKB, atLeast, atMost, st.Bavail, st.Bsize)
	}
}

// End-to-end through the default probe: forcing the warning on (threshold
// MaxInt64) must log the real free_kb from statfsFreeKB, within a band that
// absorbs drift between the two reads.
func TestCheckDiskSpaceLogsRealFreeKB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want, err := statfsFreeKB(dir)
	if err != nil || want <= 0 {
		t.Skipf("temp dir statfs = (%d, %v); cannot exercise freeKB magnitude", want, err)
	}

	logger, rec := capture.New()
	o := New(&Params{
		PG:         &fakePG{},
		Logger:     logger,
		DumpDir:    dir,
		FreeKBWarn: math.MaxInt64, // every real volume is below this, so it warns
	})

	o.checkDiskSpace()

	rendered, ok := rec.AttrValue(lowSpaceMsg, "free_kb")
	if !ok {
		t.Fatalf("checkDiskSpace did not warn at threshold MaxInt64; expected a low-space warning")
	}
	freeKB, err := strconv.ParseInt(rendered, 10, 64)
	if err != nil {
		t.Fatalf("logged free_kb = %q, want an integer: %v", rendered, err)
	}
	if lo, hi := want/2, want*2; freeKB < lo || freeKB > hi {
		t.Errorf("logged free_kb = %d, want within [%d, %d]", freeKB, lo, hi)
	}
}
