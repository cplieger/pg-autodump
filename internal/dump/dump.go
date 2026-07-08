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
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// probeTimeoutCap bounds the per-database reachability probe so a dead host
// classifies as connect_error quickly instead of consuming the dump budget.
const probeTimeoutCap = 10 * time.Second

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
		dumpDir:     p.DumpDir,
		specs:       p.Specs,
		dumpTimeout: p.DumpTimeout,
		concurrency: p.Concurrency,
		keep:        max(1, p.Keep),
		freeKBWarn:  p.FreeKBWarn,
	}
}

// Run executes every spec and returns one Result per database in spec order.
// Invalid and duplicate specs yield a Result without being dispatched; valid
// specs run through the bounded worker pool. It never returns a partial slice.
func (o *Orchestrator) Run(ctx context.Context) []Result {
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
	return results
}

// dumpOne probes one database, then (on success) stages, verifies, and
// atomically replaces its dump file. It logs the outcome. Safe for concurrent
// use across the worker pool.
func (o *Orchestrator) dumpOne(ctx context.Context, s *spec.DBSpec) Result {
	conn := Conn{Host: s.Host, Port: s.Port, DBName: s.DBName, User: s.User}
	start := o.now()

	probeCtx, cancelProbe := context.WithTimeout(ctx, min(probeTimeoutCap, o.dumpTimeout))
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
	// map to one file. MkdirAll is idempotent and safe for concurrent workers
	// (same or different subdirs); 0700 matches the unprivileged, read-only,
	// non-root runtime (only this process traverses it).
	dir := filepath.Join(o.dumpDir, spec.ServerDir(s.Host, s.Port))
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
