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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

func runServer(getenv func(string) string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, warns := config.Load(getenv)
	for _, w := range warns {
		log.Warn(string(w))
	}

	// At startup no dump is in flight, so any leftover temp is a crash orphan.
	if removed, err := atomicfile.CleanupStaleTemps(cfg.DumpDir, 0); err != nil {
		log.Warn("stale temp cleanup failed", "dir", cfg.DumpDir, "err", err)
	} else if removed > 0 {
		log.Info("reclaimed stale temp files", "count", removed)
	}

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
		go runTicker(ctx, cfg.DumpInterval, trigger, log)
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
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Warn("drain budget exceeded; cancelling in-flight dump", "grace", cfg.ShutdownGrace, "err", err)
		guard.CancelInFlight()
	}
	return 0
}

// runTicker drives the optional built-in scheduler (DUMP_INTERVAL). The homelab
// leaves this off and triggers externally via Ofelia.
func runTicker(ctx context.Context, interval time.Duration, trigger *httpapi.Trigger, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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
	cfg, _ := config.Load(getenv)
	url := "http://" + localAddr(cfg.ListenAddr) + "/dump"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, http.NoBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, "trigger:", err)
		return 1
	}
	if cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	resp, err := (&http.Client{}).Do(req) // no client timeout: a dump may run for minutes
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

// localAddr turns a listen address (":9847", "0.0.0.0:9847", "[::]:9847") into
// a loopback dial target on the same port.
func localAddr(listen string) string {
	port := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		port = listen[i+1:]
	}
	return "127.0.0.1:" + port
}
