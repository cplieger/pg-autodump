package dump

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DueForStartup reports whether the built-in scheduler should run one dump at
// startup: true when no dump artifact under dumpDir is newer than one
// interval (including when there are none at all). This closes the
// restart-starvation gap — the ticker's first fire is one interval after
// start and its clock resets on every restart — without letting a
// crash/restart loop become a dump loop. now is the wall clock; a dump with a
// future mtime (a backward clock step) reads as fresh and suppresses the
// startup dump, the safe direction.
func DueForStartup(dumpDir string, interval time.Duration, now time.Time) bool {
	newest, found := newestDumpModTime(dumpDir)
	if !found {
		return true
	}
	return now.Sub(newest) >= interval
}

// newestDumpModTime returns the modification time of the most recently
// modified "*.dump" file under dumpDir and whether any was found. It scans
// one level of per-server subdirectories (<host>_<port>/, the only place
// dumps are written); files at the DUMP_DIR root are ignored. Best-effort:
// unreadable directories and entries are skipped rather than failing the
// scan, since the caller only needs a recency signal.
func newestDumpModTime(dumpDir string) (time.Time, bool) {
	var newest time.Time
	found := false

	top, err := os.ReadDir(dumpDir)
	if err != nil {
		return newest, found
	}
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(dumpDir, e.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			mt, ok := dumpEntryModTime(entry)
			if ok && (!found || mt.After(newest)) {
				newest, found = mt, true
			}
		}
	}
	return newest, found
}

// dumpEntryModTime returns the modification time of a directory entry when
// it is a regular "*.dump" file, and false otherwise.
func dumpEntryModTime(e os.DirEntry) (time.Time, bool) {
	if e.IsDir() || !strings.HasSuffix(e.Name(), ".dump") {
		return time.Time{}, false
	}
	info, err := e.Info()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
