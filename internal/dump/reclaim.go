package dump

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cplieger/atomicfile/v3"
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
//
// Two outcomes are reported, because they are different operator problems.
// Removed is the reclaim, at Info. Unreclaimed (atomicfile's SweepResult.Failed)
// is a temp the sweep SAW and could not unlink, which means orphans are
// accumulating in a directory this app stages a dump into every cycle. It does
// not self-clear — a benign race is not counted, since atomicfile treats ENOENT
// on either the lstat or the remove as neither removed nor failed — and nothing
// else here reports it: obs.Preflight's ladder probes the DUMP_DIR ROOT only, so
// a per-server subdirectory that takes a create and refuses an unlink is outside
// its reach, and dumps keep succeeding because a dump commits with a rename.
// SweepResult.Unreadable is deliberately not read: it is only ever incremented
// below the swept directory and every sweep here is flat, so it is a structural
// zero.
func ReclaimOrphans(ctx context.Context, dumpDir string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		// A missing or unreadable DUMP_DIR is surfaced by the preflight and by
		// the dumps themselves; the reclaim scan stays best-effort.
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
