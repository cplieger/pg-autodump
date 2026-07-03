// Command pg-autodump is the composition root. With no argument it runs the
// HTTP server; `pg-autodump health` runs the file-marker probe for the Docker
// HEALTHCHECK; `pg-autodump trigger` POSTs to the local server's /dump (for
// exec-based schedulers such as Ofelia). It is the only place that calls
// config.Load, builds the slog handler, wires dependencies, and decides
// fatal-vs-recover.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embed the IANA tz database so TZ (default Europe/Paris) is honored regardless
	// of the base image's zoneinfo; without it, on a base that ships no
	// /usr/share/zoneinfo, time.Local silently falls back to UTC.
	_ "time/tzdata"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/health"
	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/httpapi"
	"github.com/cplieger/pg-autodump/internal/obs"
	"github.com/cplieger/pg-autodump/internal/pg"
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
	case "", "serve":
		return runServer(getenv)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want: serve | health | trigger)\n", sub)
		return 2
	}
}

// runServer runs the serve subcommand (the default with no argument). It builds the
// slog handler, loads config, reclaims crash-orphaned temp dumps, sets the health
// marker from the startup preflight, wires the dump orchestrator and HTTP server,
// optionally starts the built-in ticker, then serves until a signal and drains any
// in-flight dump within ShutdownGrace. It returns the process exit code.
func runServer(getenv func(string) string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

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

	reclaimStartupOrphans(cfg.DumpDir, log)

	marker := health.NewMarker(health.DefaultPath)
	defer marker.Cleanup()
	if err := obs.Preflight(cfg.DumpDir, cfg.Specs); err != nil {
		log.Error("health preconditions not met; serving but unhealthy", "err", err)
		marker.Set(false)
	} else {
		marker.Set(true)
	}

	guard := &dump.Guard{}
	orch := dump.New(&dump.Params{
		PG:          pg.New(cfg.PGPassFile, cfg.StmtTimeout),
		Logger:      log,
		DumpDir:     cfg.DumpDir,
		Specs:       cfg.Specs,
		DumpTimeout: cfg.DumpTimeout,
		Concurrency: cfg.DumpConcurrency,
		Keep:        cfg.DumpKeep,
		FreeKBWarn:  cfg.FreeKBWarn,
	})
	trigger := httpapi.NewTrigger(guard, orch, log)
	srv := httpapi.NewServer(&httpapi.Deps{
		ListenAddr: cfg.ListenAddr,
		AuthToken:  cfg.AuthToken,
		Trigger:    trigger,
		Health:     marker,
		Log:        log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.DumpInterval > 0 {
		go runTicker(ctx, cfg.DumpDir, cfg.DumpInterval, trigger, log)
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("pg-autodump listening",
			"addr", cfg.ListenAddr, "databases", len(cfg.Specs), "concurrency", cfg.DumpConcurrency)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			log.Error("server failed", "err", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		log.Info("shutting down", "cause", context.Cause(ctx))
		marker.Set(false)
	}

	gracefulShutdown(srv, guard, cfg.ShutdownGrace, log)
	return 0
}

// reclaimStartupOrphans removes crash-orphaned temp dumps left in dumpDir. At
// startup nothing is in flight, so every leftover temp is a crash orphan:
// CleanupStaleTemps removes temps older than maxAge and no-ops on a
// non-positive maxAge, so the smallest positive age ("older than ~now") reaps
// them all.
func reclaimStartupOrphans(dumpDir string, log *slog.Logger) {
	const reclaimAllOrphans = time.Nanosecond
	if removed, err := atomicfile.CleanupStaleTemps(dumpDir, reclaimAllOrphans); err != nil {
		log.Warn("stale temp cleanup failed", "dir", dumpDir, "err", err)
	} else if removed > 0 {
		log.Info("reclaimed stale temp files", "count", removed)
	}
}

// gracefulShutdown drains the HTTP server and any in-flight ticker dump within
// grace. It stops accepting new requests (srv.Shutdown); because a
// built-in-ticker dump holds no HTTP connection that Shutdown can see, it then
// waits for any in-flight run to finish within the remaining budget and, if it
// does not, cancels the run and lets it unwind so pg_dump is killed cleanly and
// the staged temp is removed.
func gracefulShutdown(srv *http.Server, guard *dump.Guard, grace time.Duration, log *slog.Logger) {
	drainCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Warn("HTTP drain budget exceeded", "grace", grace, "err", err)
	}
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
		if _, ok := trigger.Run(); ok {
			log.Info("startup dump complete (no dump within one interval at boot)")
		} else {
			log.Warn("startup dump skipped; a run is already in progress")
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A tick that fired (or sat buffered in t.C) while a run was in
			// flight can be selected in the SAME iteration ctx is cancelled:
			// select chooses among ready cases at random, so a pending tick
			// and a fresh ctx.Done() are a coin flip. Re-check the stop
			// request first so SIGTERM never lets the ticker launch a new run
			// that races the drain and is then abandoned by os.Exit.
			if ctx.Err() != nil {
				return
			}
			if _, ok := trigger.Run(); !ok {
				log.Warn("scheduled dump skipped; a run is already in progress")
			}
		}
	}
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

// triggerProbeCap mirrors the orchestrator's per-database reachability probe
// cap (internal/dump.probeTimeoutCap) so the trigger's wait estimate matches
// the server's actual per-database ceiling.
const triggerProbeCap = 10 * time.Second

// triggerHTTPSlack covers connection setup, the handler's bookkeeping, and the
// retention prune that runs after the dumps, on top of the modeled dump time.
const triggerHTTPSlack = 30 * time.Second

// triggerTimeout estimates the worst-case time POST /dump can legitimately take
// so `trigger` blocks long enough for a real dump to finish but is never
// unbounded. The server dumps cfg.Specs in ceil(len/concurrency) serial waves;
// each database is bounded by min(probeCap, DumpTimeout) for the probe plus
// DumpTimeout for the dump itself. The bound is the wave count times that
// per-database ceiling, plus fixed HTTP/handler slack.
func triggerTimeout(cfg *config.Config) time.Duration {
	concurrency := max(cfg.DumpConcurrency, 1)
	// At least one wave even when no specs are configured, so the trigger
	// still waits out the handler's own work.
	waves := 1
	if n := len(cfg.Specs); n > 0 {
		waves = (n + concurrency - 1) / concurrency
	}
	perDB := cfg.DumpTimeout + min(triggerProbeCap, cfg.DumpTimeout)
	return time.Duration(waves)*perDB + triggerHTTPSlack
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
