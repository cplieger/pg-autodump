package dump

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cplieger/atomicfile/v2"
)

// reclaimAllOrphans is the maxAge passed to atomicfile.CleanupStaleTemps when
// every leftover temp is known to be an orphan: the smallest positive age
// ("older than ~now") reaps them all, while a non-positive age would make
// CleanupStaleTemps no-op.
const reclaimAllOrphans = time.Nanosecond

// ReclaimOrphans removes crash-orphaned temp dumps under dumpDir, scanning
// each first-level per-server subdirectory — the only place temps ever stage
// (stageAndReplace targets <host>_<port>/ and atomicfile creates its temp in
// the target's own directory). Files at the DUMP_DIR root are never the app's
// and are left alone.
//
// It MUST only be called while no dump can be in flight — at the start of a
// cycle with the cross-process cycle lock held, or at startup with the lock
// momentarily acquired — because every temp it sees is then a crash orphan
// (graceful failure paths run pending.Cleanup() themselves). Best-effort:
// unreadable directories are skipped and per-file failures are handled inside
// CleanupStaleTemps; only the outcome is logged here.
func ReclaimOrphans(dumpDir string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		// A missing or unreadable DUMP_DIR is surfaced by the preflight and by
		// the dumps themselves; the reclaim scan stays best-effort.
		return
	}
	total := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		total += reclaimDir(filepath.Join(dumpDir, e.Name()), log)
	}
	if total > 0 {
		log.Info("reclaimed stale temp files", "dir", dumpDir, "count", total)
	}
}

// reclaimDir reaps the package-recognized stale temps in one directory,
// returning the count removed. Failures are logged at Warn and count as zero.
func reclaimDir(dir string, log *slog.Logger) int {
	removed, err := atomicfile.CleanupStaleTemps(dir, reclaimAllOrphans)
	if err != nil {
		log.Warn("stale temp cleanup failed", "dir", dir, "err", err)
		return 0
	}
	return removed
}
