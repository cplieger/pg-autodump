package dump

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cplieger/atomicfile/v3"
)

// reclaimAllOrphans is the maxAge for atomicfile.CleanupStaleTemps when every
// leftover temp is known to be an orphan: the smallest positive age reaps
// them all, while a non-positive age would make CleanupStaleTemps a no-op.
const reclaimAllOrphans = time.Nanosecond

// ReclaimOrphans removes crash-orphaned temp files under dumpDir: every
// first-level per-server subdirectory (where dump temps stage) and the
// DUMP_DIR root itself (where obs.Preflight's writability probe can leave one
// behind). Both scans reap only atomicfile's own temp shape
// (".atomicfile-<digits>.tmp", regular files), so an operator's own files are
// never touched.
//
// MUST only be called while no dump can be in flight — at the start of a
// cycle with the cross-process cycle lock held, or at startup with the lock
// momentarily acquired — because every temp it sees is then a crash orphan.
// Best-effort: unreadable directories are skipped.
//
// Unreclaimed (SweepResult.Failed) means a temp was seen but could not be
// unlinked, so orphans are accumulating in a directory this app stages a
// dump into every cycle; nothing else reports this, since obs.Preflight's
// ladder probes only the DUMP_DIR root. SweepResult.Unreadable is not read:
// every sweep here is flat, so it is a structural zero.
func ReclaimOrphans(ctx context.Context, dumpDir string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		return
	}
	total, unreclaimed := reclaimDir(ctx, dumpDir, log)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		removed, failed := reclaimDir(ctx, filepath.Join(dumpDir, e.Name()), log)
		total += removed
		unreclaimed += failed
	}
	if total > 0 {
		log.Info("reclaimed stale temp files", "dir", dumpDir, "count", total)
	}
	if unreclaimed > 0 {
		log.Warn("stale temp files could not be reclaimed; orphans are accumulating under the dump dir",
			"dir", dumpDir, "unreclaimed", unreclaimed,
			"remediation", "check ownership and mode on the dump dir and its per-server subdirectories")
	}
}

// reclaimDir reaps the package-recognized stale temps in one directory,
// returning the count removed and the count seen but not unlinked. A walk
// failure is logged at Warn and still contributes whatever the sweep
// accumulated before it stopped.
func reclaimDir(ctx context.Context, dir string, log *slog.Logger) (removed, unreclaimed int) {
	res, err := atomicfile.CleanupStaleTemps(ctx, dir, reclaimAllOrphans)
	if err != nil {
		log.Warn("stale temp cleanup failed", "dir", dir, "err", err)
	}
	return res.Removed, res.Failed
}
