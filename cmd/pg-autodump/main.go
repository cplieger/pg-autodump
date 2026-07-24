// Command pg-autodump is the composition root. With no argument it runs the
// HTTP server; `pg-autodump run` performs exactly one dump cycle and exits
// (for scheduler-owned deployments); `pg-autodump health` runs the file-marker
// probe for the Docker HEALTHCHECK; `pg-autodump trigger` POSTs to the local
// server's /dump (for exec-based schedulers such as Ofelia). The two exec
// trigger paths deliberately differ on a busy cycle: `trigger` requires the
// resident server and inherits its skip-mode contract (429, the demand is
// dropped — the next tick provides freshness), while `run` requires no server
// and queues its demand (exit 0, the active runner owes it a cycle that
// starts after the request arrived). Pick per deployment; they are not
// interchangeable. main is the only place that calls config.Load, builds the
// slog handler, wires dependencies, and decides fatal-vs-recover.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/httpapi"
	"github.com/cplieger/pg-autodump/internal/obs"
	"github.com/cplieger/pg-autodump/internal/pg"
	"github.com/cplieger/scheduler/v3"
	"github.com/cplieger/slogx"
	"github.com/cplieger/webhttp"
)

func main() { os.Exit(run(os.Args, os.Getenv)) }

// run dispatches the subcommand. It returns a process exit code so main stays a
// one-liner and the dispatch is unit-testable.
func run(args []string, getenv func(string) string) int {
	var sub string
	if len(args) > 1 {
		sub = args[1]
	}
	switch sub {
	case "health":
		health.RunProbe(health.DefaultPath) // stats the marker and calls os.Exit
		return 0                            // unreachable
	case "trigger":
		return runTrigger(getenv)
	case "run":
		return runOnce(getenv)
	case "", "serve":
		return runServer(getenv)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want: serve | run | health | trigger)\n", sub)
		return 2
	}
}

// cycleDir holds the cross-process cycle-coordination files
// (scheduler.Exclusive's cycle.lock and cycle.queued). It lives under /tmp —
// already a required-writable tmpfs for the health marker — and is created
// 0700 by whichever entry point starts first; the server and any exec'd
// `pg-autodump run` share it because they run as the same container user.
const cycleDir = "/tmp/pg-autodump"

// cycleQueueCapacity is scheduler.Exclusive's rerun-queue depth (the library
// default of 1, restated as a named constant because the trigger client's
// worst-case wait model bills one extra coalesced cycle per queue slot).
const cycleQueueCapacity = 1

// newCycleExclusive builds the cross-process cycle coordinator shared by the
// resident server and exec'd one-shot runs: at most one dump cycle runs at a
// time per container, and a `run` request arriving mid-cycle queues (depth
// cycleQueueCapacity) for the active runner instead of overlapping. The gate
// stops queued reruns (and a not-yet-started initial run) once shutdown is
// signalled; an in-flight run is never interrupted by the gate — the drain
// path owns cancellation.
func newCycleExclusive(ctx context.Context, log *slog.Logger) (*scheduler.Exclusive, error) {
	if err := os.MkdirAll(cycleDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cycle dir %s: %w", cycleDir, err)
	}
	return scheduler.NewExclusive(cycleDir, log,
		scheduler.WithGate(func() bool { return ctx.Err() == nil })), nil
}

// newOrchestrator wires the dump orchestrator from validated config. Shared by
// the serve and run subcommands so the two entry points can never drift.
func newOrchestrator(cfg *config.Config, log *slog.Logger) *dump.Orchestrator {
	return dump.New(&dump.Params{
		PG:          pg.New(cfg.PGPassFile, cfg.StmtTimeout),
		Logger:      log,
		DumpDir:     cfg.DumpDir,
		Specs:       cfg.Specs,
		DumpTimeout: cfg.DumpTimeout,
		Concurrency: cfg.DumpConcurrency,
		Keep:        cfg.DumpKeep,
		FreeKBWarn:  cfg.FreeKBWarn,
	})
}

// runServer runs the serve subcommand (the default with no argument). It builds
// the slog handler, loads config, sets the health marker from the startup
// preflight, wires the cycle lock, dump orchestrator, and HTTP server,
// reclaims crash-orphaned temp dumps, optionally starts the built-in ticker,
// then serves until a signal and drains any in-flight dump within
// ShutdownGrace. It returns the process exit code.
func runServer(getenv func(string) string) int {
	slogx.Setup(slogx.Options{})
	log := slog.Default()

	cfg, warns, err := config.Load(getenv)
	for _, w := range warns {
		log.Warn(string(w))
	}
	if err != nil {
		log.Error("invalid configuration; refusing to start", "err", err)
		return 1
	}
	if config.ListenerOpenAndPublic(cfg.AuthToken, cfg.ListenAddr) {
		log.Warn("POST /dump is unauthenticated and bound to a non-loopback address; "+
			"publish the port to loopback only (127.0.0.1:<port>:<port>) or set AUTH_TOKEN if the network is untrusted",
			"listen_addr", cfg.ListenAddr)
	}

	marker := health.NewMarker(health.DefaultPath)
	defer marker.Cleanup()
	if preErr := obs.Preflight(cfg.DumpDir, cfg.Specs); preErr != nil {
		log.Error("health preconditions not met; serving but unhealthy", "err", preErr)
		marker.Set(false)
	} else {
		marker.Set(true)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cycle, err := newCycleExclusive(ctx, log)
	if err != nil {
		log.Error("cycle coordination unavailable; refusing to start", "err", err)
		return 1
	}
	reclaimAtStartup(cfg.DumpDir, log)

	guard := &dump.Guard{}
	orch := newOrchestrator(&cfg, log)
	trigger := httpapi.NewTrigger(guard, cycle, orch, log)
	srv := httpapi.NewServer(&httpapi.Deps{
		AuthToken: cfg.AuthToken,
		Trigger:   trigger,
		Health:    marker,
		Log:       log,
	})

	// Bind the listener up front so a port-in-use error surfaces synchronously
	// here rather than asynchronously after serving has started.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("bind failed", "addr", cfg.ListenAddr, "err", err)
		return 1
	}

	if cfg.DumpInterval > 0 {
		go runTicker(ctx, cfg.DumpDir, cfg.DumpInterval, trigger, log)
	}

	log.Info("pg-autodump listening",
		"addr", cfg.ListenAddr, "databases", len(cfg.Specs), "concurrency", cfg.DumpConcurrency)

	// webhttp.Run drains the HTTP server within ShutdownGrace, then invokes the
	// teardown below with a context bounded by the same deadline. A built-in
	// ticker dump holds no HTTP connection Shutdown can see, so drainGuard waits
	// for the single-flight guard to go idle within the remaining budget; if it
	// does not, it cancels the in-flight run and lets pg_dump unwind cleanly.
	// The pre-drain phase flips the health marker red strictly BEFORE the drain
	// begins, so a probe reports unready during the drain window (the marker is
	// a FILE read by the healthcheck CLI, which listener closure does not cover).
	if err := webhttp.Run(ctx, srv, ln, drainInFlightDump(guard, cfg.ShutdownGrace, log),
		webhttp.WithShutdownGrace(cfg.ShutdownGrace),
		webhttp.WithPreDrain(func(context.Context) {
			log.Info("shutting down", "cause", context.Cause(ctx))
			marker.Set(false)
		})); err != nil {
		log.Error("server failed", "err", err)
		return 1
	}
	return 0
}

// runOnce implements `pg-autodump run`: exactly one signal-aware dump cycle,
// coordinated with any resident server (or concurrent run) through the
// cross-process cycle lock, exiting 0 only when every configured database
// dumped ok. When a cycle is already in flight the run's demand is queued
// (depth cycleQueueCapacity) and the process exits 0 immediately — the active
// runner executes the queued cycle when its current run finishes, and the
// per-database results land in that process's log stream. No HTTP listener is
// bound and the health marker is not touched; DUMP_INTERVAL, LISTEN_ADDR,
// AUTH_TOKEN, and SHUTDOWN_GRACE are ignored, because scheduling, transport,
// and drain belong to the invoking scheduler.
func runOnce(getenv func(string) string) int {
	slogx.Setup(slogx.Options{})
	log := slog.Default()

	cfg, warns, err := config.Load(getenv)
	for _, w := range warns {
		log.Warn(string(w))
	}
	if err != nil {
		log.Error("invalid configuration; refusing to run", "err", err)
		return 1
	}
	if preErr := obs.Preflight(cfg.DumpDir, cfg.Specs); preErr != nil {
		log.Error("run preconditions not met", "err", preErr)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cycle, err := newCycleExclusive(ctx, log)
	if err != nil {
		log.Error("cycle coordination unavailable", "err", err)
		return 1
	}
	orch := newOrchestrator(&cfg, log)

	// Capture the first execution's results: they are this invocation's own
	// run. The closure can run again for demand queued by OTHER processes
	// (consume loop / handoff); those cycles report through their own log
	// lines and never change this process's exit code.
	var results []dump.Result
	ran := false
	outcome, exErr := cycle.Run(func() error {
		r := orch.Run(ctx)
		if !ran {
			results, ran = r, true
		}
		return nil
	})
	return exitForRun(outcome, exErr, results, ran, log)
}

// exitForRun maps a one-shot cycle's outcome to the process exit code: 0 iff
// the invocation's own run reported ok for every configured database, or its
// demand was queued/discarded behind an in-flight cycle (the active runner
// owes it a run that starts after it arrived; log-and-exit-0 is the intended
// requester behavior). A gated start (shutdown signalled first) and a cycle
// infrastructure failure exit 1.
func exitForRun(outcome scheduler.Outcome, exErr error, results []dump.Result, ran bool, log *slog.Logger) int {
	switch outcome {
	case scheduler.OutcomeQueued, scheduler.OutcomeDiscarded:
		if exErr != nil {
			log.Warn("cycle coordination error after queueing; demand stands", "err", exErr)
		}
		log.Info("dump cycle already in flight; demand queued for the active runner",
			"outcome", outcome.String())
		return 0
	case scheduler.OutcomeGated:
		log.Warn("shutdown signalled before the run started; nothing dumped")
		return 1
	case scheduler.OutcomeNone, scheduler.OutcomeRan, scheduler.OutcomeRanQueued, scheduler.OutcomeSkipped:
		// Fall through to the ran/results accounting below. OutcomeSkipped is
		// unreachable from queue-mode Run; it is listed for switch completeness.
	}
	if !ran {
		log.Error("cycle coordination failed; nothing ran", "err", exErr)
		return 1
	}
	if exErr != nil {
		log.Warn("cycle coordination error after run", "err", exErr)
	}
	for i := range results {
		if !results[i].OK() {
			return 1
		}
	}
	return 0
}

// drainInFlightDump returns a webhttp.Run teardown callback that waits for any
// in-flight ticker dump to finish within the remaining drain budget and, if it
// does not, cancels it and lets it unwind so pg_dump is killed cleanly and the
// staged temp is removed. HTTP requests are already drained by the time it runs.
func drainInFlightDump(guard *dump.Guard, grace time.Duration, log *slog.Logger) func(context.Context) {
	return func(drainCtx context.Context) {
		if !guard.WaitIdle(drainCtx) {
			log.Warn("drain budget exceeded; cancelling in-flight dump", "grace", grace)
			guard.CancelInFlight()
			unwindCtx, unwindCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer unwindCancel()
			if !guard.WaitIdle(unwindCtx) {
				log.Warn("shutdown complete; in-flight dump did not unwind within the cancel budget", "grace", grace)
				return
			}
		}
		log.Info("shutdown complete", "grace", grace)
	}
}

// reclaimAtStartup reclaims crash-orphaned temp dumps before serving, but only
// while briefly holding the cross-process cycle lock: an exec'd `pg-autodump
// run` may already be dumping in another process, and its live temp must never
// be reaped. A busy (or unusable) lock skips the reclaim — every dump cycle
// reclaims again with the lock held (dump.Orchestrator.Run), so skipping here
// defers the cleanup, never leaks it.
func reclaimAtStartup(dumpDir string, log *slog.Logger) {
	lock, ok, err := scheduler.TryLock(filepath.Join(cycleDir, scheduler.ExclusiveLockName))
	if err != nil || !ok {
		return
	}
	defer lock.Unlock()
	dump.ReclaimOrphans(dumpDir, log)
}

// runTicker drives the optional built-in scheduler (DUMP_INTERVAL). A deployment
// can leave this off and trigger externally via a scheduler (e.g. Ofelia).
func runTicker(ctx context.Context, dumpDir string, interval time.Duration, trigger *httpapi.Trigger, log *slog.Logger) {
	// The ticker's first fire is one full interval after start and its clock
	// resets on every restart, so a deployment that restarts more often than
	// DUMP_INTERVAL could go a long time with no backups. Fire once at startup
	// to close that gap, but only when no existing dump is newer than one
	// interval, so a restart that already has a fresh dump does not re-dump (a
	// crash/restart loop must not become a dump loop). The run goes through the
	// shared single-flight guard, so it is cancelled by the same drain path as
	// any other run on shutdown.
	if dump.DueForStartupDump(dumpDir, interval, time.Now()) && ctx.Err() == nil {
		switch _, ok, err := trigger.Run(); {
		case err != nil:
			log.Error("startup dump failed; cycle coordination error", "err", err)
		case ok:
			log.Info("startup dump complete (no dump within one interval at boot)")
		default:
			log.Warn("startup dump skipped; a run is already in progress")
		}
	}

	// scheduler.RunLoop drives the recurring ticks. No FireOnStart: the startup
	// dump above is conditional (DueForStartupDump), unlike an unconditional
	// fire-on-start. RunLoop re-checks ctx before each tick — so a pending tick
	// racing a fresh SIGTERM never launches a run the drain then abandons — and
	// returns when ctx is cancelled.
	scheduler.RunLoop(ctx, func(context.Context) {
		if _, ok, err := trigger.Run(); err != nil {
			log.Error("scheduled dump failed; cycle coordination error", "err", err)
		} else if !ok {
			log.Warn("scheduled dump skipped; a run is already in progress")
		}
	}, scheduler.LoopOptions{Interval: interval})
}

// runTrigger POSTs to the local server's /dump and mirrors its body to stdout,
// exiting non-zero if the run reported any failure. Used by exec-based
// schedulers that prefer `docker exec <c> pg-autodump trigger` over an HTTP call.
func runTrigger(getenv func(string) string) int {
	cfg, warns, err := config.Load(getenv)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "trigger: config warning:", string(w))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trigger: invalid configuration:", err)
		return 1
	}
	url := "http://" + localAddr(cfg.ListenAddr) + "/dump"
	// Bound the trigger on the server's worst-case total dump time, NOT on
	// SHUTDOWN_GRACE (a drain knob the operator may set low for unrelated
	// reasons). The server dumps Specs in ceil(len/concurrency) serial waves,
	// each database bounded by DumpTimeout plus the reachability probe cap, so
	// a flat DumpTimeout+slack would falsely time out a multi-database run and
	// make `trigger` report exit 1 while the dump is still succeeding.
	timeout := triggerTimeout(&cfg)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, "trigger:", err)
		return 1
	}
	if cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "trigger failed:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// triggerHTTPSlack covers connection setup, the handler's bookkeeping, and the
// retention prune that runs after the dumps, on top of the modeled dump time.
const triggerHTTPSlack = 30 * time.Second

// triggerTimeout estimates the worst-case time POST /dump can legitimately take
// so `trigger` blocks long enough for a real dump to finish but is never
// unbounded. The server dumps cfg.Specs in ceil(len/concurrency) serial waves;
// each database is bounded by min(dump.ProbeTimeoutCap, DumpTimeout) for the
// probe plus DumpTimeout for the dump itself. Before the response is written
// the server also executes any rerun demand exec'd runs queued during the
// cycle (at most cycleQueueCapacity), so the modeled dump time is billed
// (1 + cycleQueueCapacity) times, plus fixed HTTP/handler slack.
func triggerTimeout(cfg *config.Config) time.Duration {
	concurrency := max(cfg.DumpConcurrency, 1)
	// At least one wave even when no specs are configured, so the trigger
	// still waits out the handler's own work.
	waves := 1
	if n := len(cfg.Specs); n > 0 {
		waves = (n + concurrency - 1) / concurrency
	}
	perDB := cfg.DumpTimeout + min(dump.ProbeTimeoutCap, cfg.DumpTimeout)
	cycles := 1 + cycleQueueCapacity
	return time.Duration(waves*cycles)*perDB + triggerHTTPSlack
}

// localAddr turns a listen address into the dial target for the trigger's POST /dump on
// the same port. A wildcard or unspecified bind (":9847", "0.0.0.0:9847", "[::]:9847")
// and an unparseable value map to 127.0.0.1; an explicit host (loopback or otherwise) is
// preserved, so a listener bound to a specific address is dialed where it actually bound.
func localAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// No port (e.g. a bare "9847") or otherwise unparseable: treat the
		// whole string as the port and dial IPv4 loopback.
		return net.JoinHostPort("127.0.0.1", listen)
	}
	// A wildcard or unspecified bind is reachable on loopback, so dial IPv4
	// loopback. An explicit loopback host is preserved as-is, so an
	// IPv6-only-loopback listener (LISTEN_ADDR="[::1]:port") is dialed on ::1
	// rather than on 127.0.0.1 (which it never bound).
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
