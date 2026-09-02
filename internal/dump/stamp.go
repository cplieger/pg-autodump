package dump

import "path/filepath"

// stampName is the cycle record at the DUMP_DIR root: one line recording
// when the last dump cycle completed and whether every database succeeded.
const stampName = ".pg-autodump-last-run"

// StampPath returns the cycle-record file for dumpDir. The record sits at
// the DUMP_DIR root so it survives a container recreate alongside the dumps
// themselves; root-level files are outside the per-server layout, so no dump
// scan, retention prune, or orphan reclaim ever touches it.
func StampPath(dumpDir string) string {
	return filepath.Join(dumpDir, stampName)
}
