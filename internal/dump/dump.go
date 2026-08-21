// Package dump is the core of pg-autodump: it orchestrates one dump run,
// drives the pg boundary to stream a network pg_dump per database, verifies
// each dump locally, atomically replaces the previous good file, and reports
// a typed result per database. It defines the narrow interface it consumes
// (PGTool) so the logic is testable against fakes with no network,
// no daemon, and no real filesystem dependencies beyond a temp dir.
package dump

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// ProbeTimeoutCap bounds the per-database reachability probe so a dead host
// classifies as connect_error quickly instead of consuming the dump budget.
// Exported so the trigger client's worst-case wait model (cmd/pg-autodump
// triggerTimeout) consumes the same constant the orchestrator enforces and the
// two can never drift.
const ProbeTimeoutCap = 10 * time.Second

// Params configures an Orchestrator. The pg boundary is injected; the rest are
// validated config values.
type Params struct {
	PG          PGTool
	Logger      *slog.Logger
	Now         func() time.Time // injectable clock; defaults to time.Now
	DumpDir     string
	Specs       []spec.DBSpec
	DumpTimeout time.Duration
	Concurrency int
	Keep        int
	FreeKBWarn  int64
}

// Orchestrator owns a dump run: split valid from invalid specs, drive the
// bounded worker pool, and return one Result per spec in spec order.
type Orchestrator struct {
	pg          PGTool
	log         *slog.Logger
	now         func() time.Time
	freeSpace   func(string) (int64, error) // injectable disk-space probe; defaults to statfsFreeKB
	dumpDir     string
	specs       []spec.DBSpec
	dumpTimeout time.Duration
	concurrency int
	keep        int
	freeKBWarn  int64
	// serverDir serializes the per-server custody sequence below; see
	// ensureServerDir for why the create and its mode repair must not interleave
	// across two workers naming the same directory.
	serverDir sync.Mutex
}

// New builds an Orchestrator from validated params.
func New(p *Params) *Orchestrator {
	now := p.Now
	if now == nil {
		now = time.Now
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		pg:          p.PG,
		log:         log,
		now:         now,
		freeSpace:   statfsFreeKB,
		dumpDir:     absDumpDir(p.DumpDir),
		specs:       p.Specs,
		dumpTimeout: p.DumpTimeout,
		concurrency: p.Concurrency,
		keep:        max(1, p.Keep),
		freeKBWarn:  p.FreeKBWarn,
	}
}

// absDumpDir resolves the configured dump directory to an absolute path once,
// here on the single-threaded construction path.
//
// DUMP_DIR is an operator string this app has never required to be absolute,
// and the custody check in ensureServerDir refuses a relative one on purpose: a
// verdict about "dumps/db_5432" is a statement about wherever the process
// happens to be standing, which nothing stops another goroutine from changing
// between the check and the write. Resolving at construction keeps a relative
// DUMP_DIR working exactly as it did while giving the check a path whose meaning
// cannot move under the worker pool. A failing os.Getwd leaves the value as
// configured rather than guessing; the custody check then reports it.
func absDumpDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// Run executes every spec and returns one Result per database in spec order.
// Invalid and duplicate specs yield a Result without being dispatched; valid
// specs run through the bounded worker pool. It never returns a partial slice.
//
// Run assumes it is the only dump run in flight (its production callers hold
// the in-process guard and the cross-process cycle lock), so it first reclaims
// crash-orphaned temp files under DumpDir — every temp visible at cycle start
// is an orphan. On completion it emits a single "dump cycle complete"
// heartbeat (total/ok/failed tallied from the results); the README's alerting
// section documents the Loki absence rule that keys on it to catch a silent
// non-run of this backup-critical sidecar, not just a loud per-DB failure.
func (o *Orchestrator) Run(ctx context.Context) []Result {
	ReclaimOrphans(o.dumpDir, o.log)
	o.checkDiskSpace()

	results := make([]Result, len(o.specs))
	valid := make([]spec.DBSpec, 0, len(o.specs))
	validPos := make([]int, 0, len(o.specs))

	for i, s := range o.specs {
		if s.Invalid != "" {
			results[i] = invalidResult(&s)
			continue
		}
		valid = append(valid, s)
		validPos = append(validPos, i)
	}

	if len(valid) > 0 {
		n := clamp(o.concurrency, 1, len(valid))
		sub := runPool(ctx, n, valid, o.dumpOne)
		for j, r := range sub {
			results[validPos[j]] = r
		}
	}

	var okN, failedN int
	for i := range results {
		if results[i].OK() {
			okN++
		} else {
			failedN++
		}
	}
	o.log.Info("dump cycle complete", "total", len(results), "ok", okN, "failed", failedN)

	return results
}

// dumpOne probes one database, then (on success) stages, verifies, and
// atomically replaces its dump file. It logs the outcome. Safe for concurrent
// use across the worker pool.
func (o *Orchestrator) dumpOne(ctx context.Context, s *spec.DBSpec) Result {
	conn := Conn{Host: s.Host, Port: s.Port, DBName: s.DBName, User: s.User}
	start := o.now()

	probeCtx, cancelProbe := context.WithTimeout(ctx, min(ProbeTimeoutCap, o.dumpTimeout))
	major, kind, perr := o.pg.Probe(probeCtx, conn)
	probeErr := probeCtx.Err()
	cancelProbe()

	if kind != FailNone || perr != nil {
		reason := classify(0, probeErr, kind)
		detail := string(reason)
		if reason == ReasonOther && perr != nil {
			detail = perr.Error()
		}
		return o.finish(&Result{
			Host: s.Host, DBName: s.DBName, Reason: reason,
			Detail: detail, ServerVersion: major, Duration: o.now().Sub(start),
		}, perr)
	}

	dumpCtx, cancelDump := context.WithTimeout(ctx, o.dumpTimeout)
	defer cancelDump()

	// Qualify the artifact by its server: DUMP_DIR/<host>_<port>/<dbname>.dump.
	// This makes the path honor the (host, port, dbname) identity the validator
	// dedups on, so two databases sharing a name on different servers can never
	// map to one file.
	dir := filepath.Join(o.dumpDir, spec.ServerDir(s.Host, s.Port))
	if err := o.ensureServerDir(dir); err != nil {
		return o.finish(&Result{
			Host: s.Host, DBName: s.DBName, Reason: ReasonMkdirFailed,
			Detail:        "cannot create server dir " + dir + ": " + err.Error(),
			ServerVersion: major, Duration: o.now().Sub(start),
		}, nil)
	}

	res := stageAndReplace(dumpCtx, o.pg, dir, dumpFileName(s.DBName, o.keep, start), conn)
	res.Host = s.Host
	res.DBName = s.DBName
	res.ServerVersion = major
	res.Duration = o.now().Sub(start)

	if res.Reason == ReasonOK && o.keep > 1 {
		if removed, err := pruneOldDumps(dir, s.DBName, o.keep); err != nil {
			o.log.Warn("dump retention prune failed", "db", s.DBName, "keep", o.keep, "err", err)
		} else if removed > 0 {
			o.log.Info("pruned old dumps", "db", s.DBName, "keep", o.keep, "removed", removed)
		}
	}
	return o.finish(&res, nil)
}

// ensureServerDir establishes the per-server subdirectory that will receive this
// database's dump, and PROVES it came out owner-only.
//
// The mode passed to mkdir is a request, not a result, and os.MkdirAll never
// looks at what it got: on a filesystem carrying an inheritable group-write ACL
// the kernel stores 0770 for a 0o700 mkdir (measured on a ZFS nfs4acl dataset),
// and the call still returns nil. What lands in this directory is
// pg_dump --format=custom output — every row of every dumped database, schema
// included — so a silently-widened 0770 makes the full contents of each Postgres
// this sidecar reaches readable by anything sharing the container's group, with
// nothing logged and nothing at the call site to suggest a tighter mode was ever
// asked for. It also gets the pre-existing case right where MkdirAll cannot: a
// symlink planted at this path is refused by the kernel rather than followed, so
// a dump is never staged into a directory the planter chose, and a directory
// owned by another uid or already carrying group access is refused instead of
// adopted.
//
// EnsurePrivateDir is a single level, so DUMP_DIR itself keeps the plain
// MkdirAll it has always had at the same 0700. That is deliberate: DUMP_DIR is
// the operator's mount point, this app has never claimed custody of it, and
// refusing to start because a bind-mounted /dumps came in group-readable would
// break deployments that are working today.
//
// The lock is what makes this safe for the worker pool, which is the whole point
// of running the pool over one server's databases. Without it the concurrent
// case is worse than the bug being fixed: on exactly the widening filesystem
// this exists for, the worker that wins the mkdir owns the directory and repairs
// its mode, while a worker that loses arrives on the pre-existing path — and if
// it stats between the winner's mkdir and the winner's repair it sees the
// widened mode and correctly refuses to adopt it, failing that database's dump
// with mkdir_failed. Serializing create-then-repair closes that window; the lock
// spans a mkdir and an fstat/fchmod pair, never a dump, so the parallelism that
// matters is untouched.
//
// WithRepairOwnedDir is what makes the FIRST run after this change survive.
// These per-server directories are this app's own past output: an earlier
// release created them with a plain MkdirAll(0o700), so on a widening dataset
// they are already sitting at 0770, and the library's default rule — never
// repair a pre-existing directory — would refuse every one of them and fail
// every database with mkdir_failed. The option narrows them instead, and it is
// sound here rather than a loosening: EnsurePrivateDir has already proved the
// directory is owned by our own euid, and a directory cannot be planted under
// someone else's ownership. The repair logs once per directory, so the one-time
// migration is visible rather than silent.
func (o *Orchestrator) ensureServerDir(dir string) error {
	o.serverDir.Lock()
	defer o.serverDir.Unlock()

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	_, err := atomicfile.EnsurePrivateDir(dir,
		atomicfile.WithLogger(o.log), atomicfile.WithRepairOwnedDir())
	return err
}

// finish logs a result and returns it (by value) so callers can
// `return o.finish(&res, nil)`. diagErr is an optional probe diagnostic (the dial error or
// psql stderr behind a connect_error/auth_error); it is recorded in the log line only,
// never in r.Detail, so the HTTP response body is unchanged while operators keep the root
// cause in Loki.
func (o *Orchestrator) finish(r *Result, diagErr error) Result {
	attrs := []any{
		"host", r.Host, "db", r.DBName, "reason", string(r.Reason),
		"bytes", r.Bytes, "duration_s", r.Duration.Seconds(),
	}
	if r.ServerVersion > 0 {
		attrs = append(attrs, "server_version", r.ServerVersion)
	}
	if r.Reason != ReasonOK && r.Detail != "" {
		attrs = append(attrs, "detail", r.Detail)
	}
	if diagErr != nil && diagErr.Error() != r.Detail {
		attrs = append(attrs, "err", diagErr)
	}
	o.log.Log(context.Background(), levelFor(r.Reason), "dump "+string(r.Reason), attrs...)
	return *r
}

// invalidResult builds the Result for a spec that failed validation, using the
// raw token as the database label when the parsed name is empty.
func invalidResult(s *spec.DBSpec) Result {
	reason := ReasonInvalid
	if s.Duplicate {
		reason = ReasonDuplicate
	}
	db := s.DBName
	if db == "" {
		db = s.Raw
	}
	return Result{Host: s.Host, DBName: db, Reason: reason, Detail: s.Invalid}
}

// levelFor maps a Reason to the slog level its log line should use.
func levelFor(r Reason) slog.Level {
	switch r {
	case ReasonOK:
		return slog.LevelInfo
	case ReasonInvalid, ReasonDuplicate, ReasonSkipped, ReasonKilled:
		// ReasonKilled (context.Canceled) is only produced by a graceful
		// shutdown cancelling an in-flight dump (Guard.CancelInFlight on the
		// SIGTERM drain path), so it is an expected operator action, not a
		// dump failure. Logging it at Error would false-fire the Loki
		// dump-failure alert on every clean shutdown; Warn records the
		// cut-off without tripping the error-rate alert.
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// clamp constrains v to [lo, hi]; the sole caller passes lo=1 <= hi. Expressed
// with the min/max builtins (matching their use elsewhere in this file) rather
// than a hand-rolled comparison chain.
func clamp(v, lo, hi int) int {
	return min(hi, max(lo, v))
}
