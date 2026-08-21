// Package obs wires startup observability to pg-autodump's domain: a preflight
// check used to decide the health-marker state at boot. It deliberately does
// not probe per-host database reachability (a per-dump concern), so a
// transiently-down database never flips the container unhealthy.
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

// dirWritable confirms dir accepts the whole ladder a dump needs — create a
// temp, write and flush bytes, close it, and unlink it — via
// atomicfile.ProbeWritable, the same package that stages every dump temp. Two
// properties come from the library rather than from this app: the probe file
// carries atomicfile's own temp-name shape, so a leftover is reclaimable by the
// sweep dump.ReclaimOrphans already runs (the old ".pg-autodump-writable-*"
// name was reclaimable by nothing), and no stage outcome is discarded (the old
// probe threw away both the close and the remove error, so a directory that
// took a create and refused cleanup passed this preflight silently).
//
// Policy — every stage up to and including Close fails the preflight; a Remove
// failure does not:
//
//   - Mkdir/Create/Write/Sync: nothing durable was written, so the destination
//     is not usable for a dump. Fatal, as before.
//   - Close: fatal, and this is the stage the old probe silently dropped. Close
//     is where a filesystem reports a write that never reached disk, and this
//     preflight gates a BACKUP run: a destination that cannot prove it durably
//     took one byte must not be reported as ready to take a dump. This is
//     deliberately stricter than ProbeResult.Writable, which counts a Close
//     failure as "the directory accepted a real write".
//   - Remove: a WARN, not a failure. The directory created, wrote and flushed
//     real bytes, which is all a dump needs — a dump COMMITS with a rename and
//     never with an unlink — so refusing the unlink costs leaked temps, not
//     backups. It is also not clearable by a restart, and this app's health
//     contract keeps restart-unclearable states out of the marker (the same
//     reasoning that keeps an unreadable input out of health). The leftover is
//     named in the WARN and is reclaimed by the next cycle's ReclaimOrphans.
func dirWritable(dir string) error {
	// context.Background is honest here: ProbeWritable checks ctx once, before
	// it creates anything, and Preflight is a boot-time gate with no caller
	// that could cancel it. Surviving a wedged mount needs a timeout AROUND the
	// call (the library says so), which this app does not do today.
	res, err := atomicfile.ProbeWritable(context.Background(), dir)
	if err != nil {
		// A non-nil error means the probe was never attempted. It carries no
		// verdict on writability, so it must not be reported as one.
		return fmt.Errorf("dump dir write probe not attempted: %w", err)
	}
	if res.OK() {
		return nil
	}
	if res.Stage == atomicfile.ProbeStageRemove {
		slog.Warn("dump dir refuses to remove its writability probe; the leftover is reclaimed by the stale-temp sweep",
			"dir", res.Dir, "probe", res.Name, "err", res.Err)
		return nil
	}
	return fmt.Errorf("dump dir %q failed the write probe at %q: %w", res.Dir, res.Stage, res.Err)
}
