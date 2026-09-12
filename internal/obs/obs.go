// Package obs wires startup observability to pg-autodump's domain: a preflight
// check used to decide the health-marker state at boot.
package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/pg-autodump/internal/pg"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// preflightTimeout bounds the whole preflight: the three client --version runs
// plus the write probe. Long enough for a loaded host, short enough that a
// wedged client binary cannot hold up the boot indefinitely.
const preflightTimeout = 30 * time.Second

// Preflight reports whether the liveness preconditions hold: the client
// binaries run, the dump directory is writable, and DB_SPECS lists at least one
// entry. It deliberately does NOT probe per-host database reachability (that is
// a per-dump, per-DB concern), so a transiently-down database never flips the
// container unhealthy. Returns nil when healthy, else a reason for the log.
//
// It owns its own deadline: both callers are boot gates that run before the
// process installs its signal handler, so there is no caller context to
// inherit.
func Preflight(dumpDir string, specs []spec.DBSpec) error {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	if err := pg.BinariesRunnable(ctx); err != nil {
		return err
	}
	if err := dirWritable(ctx, dumpDir); err != nil {
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
func dirWritable(ctx context.Context, dir string) error {
	// ProbeWritable checks ctx once before it creates anything, so this bounds
	// when the probe starts rather than how long a wedged write may take.
	res, err := atomicfile.ProbeWritable(ctx, dir)
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
