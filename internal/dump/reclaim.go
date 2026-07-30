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

// ReclaimOrphans removes crash-orphaned temp files under dumpDir: every
// first-level per-server subdirectory — where dump temps stage
// (stageAndReplace targets <host>_<port>/ and atomicfile creates its temp in
// the target's own directory) — and the DUMP_DIR root itself, which holds
// exactly one class of the app's own artifact: the writability probe
// obs.Preflight leaves behind when the directory accepts a write but denies the
// unlink. Both scans reap only what atomicfile recognises as its own temp
// (".atomicfile-<digits>.tmp", regular files), so an operator's own files at the
// root — dumps, notes, anything merely prefix-alike — are never touched.
//
// It MUST only be called while no dump can be in flight — at the start of a
// cycle with the cross-process cycle lock held, or at startup with the lock
// momentarily acquired — because every temp it sees is then a crash orphan
// (graceful failure paths run pending.Cleanup() themselves). A concurrently
// running preflight probe is the one benign exception: losing its own temp to
// this sweep reads as an already-removed file, which atomicfile counts as a
// clean removal. Best-effort: unreadable directories are skipped and per-file
// failures are handled inside CleanupStaleTemps; only the outcome is logged
// here.
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
	total := reclaimDir(dumpDir, log)
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
