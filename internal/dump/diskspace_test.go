package dump

import (
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// lowSpaceWarning extracts the "low free disk space for dumps" warning from a
// capture.Recorder: whether it fired and the free_kb / warn_below_kb values it
// carried, so disk-space assertions read structured attributes instead of
// parsing text.
func lowSpaceWarning(rec *capture.Recorder) (hit bool, freeKB, warnBelowKB int64) {
	for _, r := range rec.Records() {
		if r.Message != "low free disk space for dumps" {
			continue
		}
		hit = true
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "free_kb":
				freeKB = a.Value.Int64()
			case "warn_below_kb":
				warnBelowKB = a.Value.Int64()
			}
			return true
		})
	}
	return hit, freeKB, warnBelowKB
}

// fixedFreeSpace is an o.freeSpace stub that reports a controlled free-KB value,
// so the low-space decision is exercised at an exact threshold without the live
// filesystem's free space drifting between the probe and the assertion.
func fixedFreeSpace(freeKB int64) func(string) (int64, error) {
	return func(string) (int64, error) { return freeKB, nil }
}

// The strict guard `freeKB < freeKBWarn` is the whole point of the check, and
// its boundary was previously untestable without racing the live filesystem.
// With the disk-space probe injected, the reading is exact, so below / equal /
// above the threshold are all deterministic.
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

			hit, freeKB, warnBelowKB := lowSpaceWarning(rec)
			if hit != tc.wantWarn {
				t.Fatalf("free_kb=%d, threshold=%d: warned=%v, want warned=%v",
					tc.freeKB, warn, hit, tc.wantWarn)
			}
			if tc.wantWarn {
				if freeKB != tc.freeKB {
					t.Errorf("logged free_kb = %d, want %d", freeKB, tc.freeKB)
				}
				if warnBelowKB != warn {
					t.Errorf("logged warn_below_kb = %d, want %d", warnBelowKB, warn)
				}
			}
		})
	}
}

// A zero or negative threshold disables the check: checkDiskSpace must return
// before probing the filesystem at all (no statfs, no log). The injected probe
// records whether it was called so the "no probe" contract is asserted directly.
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

// When the probe fails, checkDiskSpace logs the probe-error warning and does NOT
// also emit the low-space warning (a failed reading is unknown, not low).
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
	if rec.Contains("low free disk space for dumps") {
		t.Errorf("probe failed; must not also emit the low-space warning, got %v", rec.Messages())
	}
}

// The default probe (statfsFreeKB) returns positive free space for a usable temp
// dir and errors for a missing path. This exercises the real syscall the
// Orchestrator wires in by default, which the injected tests above stub out.
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

// End-to-end through the DEFAULT probe: with the warning forced on (threshold
// MaxInt64) checkDiskSpace warns and logs the real free_kb. The oracle is the
// same production statfsFreeKB, compared within a generous band that absorbs
// benign drift between the two reads; an arithmetic error (e.g. dropping the
// block-size scaling) would move the value by orders of magnitude and fail.
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

	hit, freeKB, _ := lowSpaceWarning(rec)
	if !hit {
		t.Fatalf("checkDiskSpace did not warn at threshold MaxInt64; expected a low-space warning")
	}
	if lo, hi := want/2, want*2; freeKB < lo || freeKB > hi {
		t.Errorf("logged free_kb = %d, want within [%d, %d]", freeKB, lo, hi)
	}
}
