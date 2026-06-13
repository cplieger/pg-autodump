package dump

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
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
// "<dbname>.<ts>.dump" files (never the bare "<dbname>.dump" a keep<=1 run
// writes), so switching keep down never deletes the stable file out from under
// a collector. Best-effort: a remove error is returned for logging but does not
// undo the prior removals.
func pruneOldDumps(dir, dbname string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
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
	if len(names) <= keep {
		return 0, nil
	}
	sort.Strings(names) // ascending == oldest-first (timestamp format is lexically sortable)

	removed := 0
	var firstErr error
	for _, n := range names[:len(names)-keep] {
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
// discards the temp and leaves any existing file untouched.
//
// Steps:
//  1. open an atomicfile pending write (mode 0600) in dir
//  2. pg.Dump(ctx, conn, pending)            — network pg_dump, local child
//  3. ctx timeout/cancel                      -> discard, timeout/killed
//  4. exit != 0                               -> discard, classify -> pg_error
//  5. size == 0                               -> discard, empty
//  6. pg.VerifyTOC(temp) fails                -> discard, truncated  (local)
//  7. pending.Commit fails                    -> discard, rename_failed (prior intact)
//  8. otherwise                               -> ok (bytes)
func stageAndReplace(ctx context.Context, p PGTool, dir, fileName string, c Conn) Result {
	target := filepath.Join(dir, fileName)

	pending, err := atomicfile.NewPendingFile(ctx, target, atomicfile.WithMode(0o600))
	if err != nil {
		return Result{Reason: ReasonOther, Detail: "cannot create temp file: " + err.Error()}
	}
	committed := false
	defer func() {
		if !committed {
			_ = pending.Cleanup()
		}
	}()

	exitCode, stderrTail, dumpErr := p.Dump(ctx, c, pending.File)

	if ctxErr := ctx.Err(); ctxErr != nil {
		reason := classify(exitCode, ctxErr, FailNone)
		return Result{Reason: reason, Detail: string(reason)}
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
		return Result{Reason: ReasonTruncated, Detail: "pg_restore --list failed (TOC unreadable)"}
	}

	if _, err := pending.Commit(ctx); err != nil {
		return Result{Reason: ReasonRenameFailed, Detail: "atomic replace failed: " + err.Error()}
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
