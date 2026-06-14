// Package obs wires startup observability to pg-autodump's domain: a preflight
// check used to decide the health-marker state at boot. It deliberately does
// not probe per-host database reachability (a per-dump concern), so a
// transiently-down database never flips the container unhealthy.
package obs

import (
	"errors"
	"os"

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

// dirWritable confirms dir exists and accepts a create+remove, which is what a
// dump needs (atomicfile stages a temp there).
func dirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".pg-autodump-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}
