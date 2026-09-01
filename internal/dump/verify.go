package dump

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v3"
)

// dumpTimeFormat is the UTC timestamp embedded in retained dump filenames. It
// is fixed-width and lexically sortable, so sorting names sorts by time.
const dumpTimeFormat = "20060102T150405Z"

// dumpFileName returns the artifact name for a database. With keep <= 1 it is
// the stable "<dbname>.dump" (overwritten each run, the default), so external
// collectors that expect a fixed path are unaffected. With keep > 1 each run
// writes a distinct "<dbname>.<UTC>.dump" so pruneOldDumps can retain the N
// newest.
func dumpFileName(dbname string, keep int, t time.Time) string {
	if keep <= 1 {
		return dbname + ".dump"
	}
	return dbname + "." + t.UTC().Format(dumpTimeFormat) + ".dump"
}

// pruneOldDumps keeps the newest keep timestamped dumps for dbname in dir and
// removes the rest, returning the number removed. It matches only
// "<dbname>.<ts>.dump" files, never the bare "<dbname>.dump" a keep<=1 run
// writes, so switching keep down never deletes the stable file out from
// under a collector. Best-effort: a remove error is returned for logging but
// does not undo the prior removals.
func pruneOldDumps(dir, dbname string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	names := timestampedDumpNames(entries, dbname)
	if len(names) <= keep {
		return 0, nil
	}
	slices.Sort(names) // ascending == oldest-first (timestamp format is lexically sortable)
	return removeDumps(dir, names[:len(names)-keep])
}

// timestampedDumpNames returns the names of the timestamped dump files for
// dbname in entries ("<dbname>.<ts>.dump"), skipping directories and the bare
// stable "<dbname>.dump". A name must be strictly longer than
// "<dbname>." + ".dump" so a degenerate empty-timestamp name never counts.
func timestampedDumpNames(entries []os.DirEntry, dbname string) []string {
	prefix := dbname + "."
	const suffix = ".dump"
	bare := dbname + ".dump"

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if n == bare {
			continue
		}
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) && len(n) > len(prefix)+len(suffix) {
			names = append(names, n)
		}
	}
	return names
}

// removeDumps deletes each named file under dir, returning the number
// removed and the first remove error. Best-effort: does not stop the loop or
// undo prior removals.
func removeDumps(dir string, names []string) (int, error) {
	removed := 0
	var firstErr error
	for _, n := range names {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// stageAndReplace is the load-bearing verify-before-replace invariant. It
// streams a network pg_dump into a temp file in dir (via atomicfile, so the
// temp shares dir's filesystem and the replace is an atomic rename), verifies
// the result locally, and only then commits to fileName. On any failure it
// discards the temp and leaves any existing file untouched: a ctx
// cancellation always wins over the gate's own failure (timeout/killed), and
// only a clean pg_dump exit, non-empty file, and readable TOC reach Commit.
func stageAndReplace(ctx context.Context, p PGTool, dir, fileName string, c Conn) Result {
	target := filepath.Join(dir, fileName)

	pending, err := atomicfile.NewPendingFile(ctx, target, atomicfile.WithMode(0o600))
	if err != nil {
		// A target occupied by a directory, FIFO, device, or socket is
		// refused here rather than at Commit; the prior dump is intact and
		// this classifies as rename_failed, not a generic temp-create fault
		// (ReasonOther would misname the destination problem as a temp one).
		if errors.Is(err, atomicfile.ErrNotRegular) {
			return Result{
				Reason: ReasonRenameFailed,
				Detail: "target is not a regular file: " + err.Error(),
			}
		}
		// A ctx cancel/deadline at temp-create time is killed/timeout, not a
		// generic fault; abortOr mirrors the VerifyTOC/Commit gates so every
		// gate classifies a cancellation uniformly.
		return abortOr(ctx, &Result{Reason: ReasonOther, Detail: "cannot create temp file: " + err.Error()})
	}
	committed := false
	defer func() {
		if !committed {
			_ = pending.Cleanup()
		}
	}()

	exitCode, stderrTail, dumpErr := p.Dump(ctx, c, pending.File)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxAbortResult(ctxErr)
	}
	if dumpErr != nil && exitCode == 0 {
		return Result{Reason: ReasonOther, Detail: dumpErr.Error()}
	}
	if exitCode != 0 {
		return Result{Reason: ReasonPGError, Detail: stderrDetail(stderrTail)}
	}

	info, statErr := os.Stat(pending.Name())
	if statErr != nil {
		return Result{Reason: ReasonOther, Detail: "stat temp file: " + statErr.Error()}
	}
	size := info.Size()
	if size == 0 {
		return Result{Reason: ReasonEmpty, Detail: "dump produced an empty file"}
	}

	if err := p.VerifyTOC(ctx, pending.Name()); err != nil {
		return abortOr(ctx, &Result{Reason: ReasonTruncated, Detail: "pg_restore --list failed (TOC unreadable): " + err.Error()})
	}

	if _, err := pending.Commit(ctx); err != nil {
		return abortOr(ctx, &Result{Reason: ReasonRenameFailed, Detail: "atomic replace failed: " + err.Error()})
	}
	committed = true

	return Result{Reason: ReasonOK, Bytes: size, Detail: fmt.Sprintf("ok (%d bytes)", size)}
}

// stderrDetail returns a short human detail for a failed pg_dump, falling back
// to a generic message when pg_dump wrote nothing to stderr.
func stderrDetail(tail string) string {
	if tail == "" {
		return "dump failed (pg_dump exited non-zero)"
	}
	return "dump failed: " + tail
}

// ctxAbortResult builds the Result for a context cancellation/deadline
// detected at a stageAndReplace gate, so every gate classifies a cancelled
// run uniformly as killed/timeout.
func ctxAbortResult(ctxErr error) Result {
	reason := classify(0, ctxErr, FailNone)
	return Result{Reason: reason, Detail: string(reason)}
}

// abortOr returns a ctx-abort Result when ctx has been cancelled or has
// expired, otherwise the supplied fallback Result. Collapses the shared
// "cancellation wins over the operation-specific failure" branch at the
// temp-create, verify, and commit gates.
func abortOr(ctx context.Context, fallback *Result) Result {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxAbortResult(ctxErr)
	}
	return *fallback
}
