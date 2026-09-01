// Package obs wires startup observability to pg-autodump's domain: a preflight
// check used to decide the health-marker state at boot.
package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/pg-autodump/internal/pg"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// Preflight reports whether the liveness preconditions hold: the client
// binaries resolve on PATH, the dump directory is writable, and DB_SPECS lists
// at least one entry. It deliberately does NOT probe per-host database
// reachability (that is a per-dump, per-DB concern), so a transiently-down
// database never flips the container unhealthy. Returns nil when healthy, else
// a reason for the log.
func Preflight(dumpDir string, specs []spec.DBSpec) error {
	if err := pg.BinariesPresent(); err != nil {
		return err
	}
	if err := dirWritable(dumpDir); err != nil {
		return err
	}
	if len(specs) == 0 {
		return errEmptySpecs
	}
	return nil
}

var errEmptySpecs = errors.New("DB_SPECS is empty")

// dirWritable confirms dir accepts atomicfile's create/write/sync/close/unlink
// ladder (the same probe every dump temp uses), so a leftover is reclaimable by
// dump.ReclaimOrphans.
//
// Policy: any failure up to and including Close fails the preflight (nothing
// durable was written, or the filesystem never confirmed the write reached
// disk — stricter than ProbeResult.Writable, which treats a Close failure as
// an accepted write). A Remove failure is a WARN only: a dump commits by
// rename, never by unlink, so a directory that wrote real bytes but refused
// the unlink is still dump-ready; the leftover is reclaimed by the next
// cycle's ReclaimOrphans.
func dirWritable(dir string) error {
	// ctx is checked once before ProbeWritable creates anything; Preflight is a
	// boot-time gate with no caller that could cancel it.
	res, err := atomicfile.ProbeWritable(context.Background(), dir)
	if err != nil {
		return fmt.Errorf("dump dir write probe not attempted: %w", err)
	}
	if res.OK() {
		return nil
	}
	if res.Stage == atomicfile.ProbeStageRemove {
		slog.Warn("dump dir refuses to remove its writability probe; reclaimed by the stale-temp sweep",
			"dir", res.Dir, "probe", res.Name, "err", res.Err)
		return nil
	}
	return fmt.Errorf("dump dir %q failed the write probe at %q: %w", res.Dir, res.Stage, res.Err)
}
