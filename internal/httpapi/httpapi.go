// Package httpapi is the HTTP control surface: POST /dump (optional bearer
// auth) and GET /healthz (liveness, via the health library). It owns no domain
// logic; the dump run is driven through a Trigger that both the handler and the
// optional built-in ticker share, so single-flight lives in exactly one place.
package httpapi

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/pg-autodump/internal/dump"
)

// readHeaderTimeout guards against slow-header (slowloris) clients. There is no
// write timeout: a dump run holds the response open for minutes by design.
const readHeaderTimeout = 10 * time.Second

// Trigger runs one dump under the single-flight guard. The run context is
// derived from context.Background(), NOT the HTTP request, so a trigger client
// that disconnects (a short wget firing a long backup) never cancels the dump;
// only shutdown cancels it, via the guard.
type Trigger struct {
	guard *dump.Guard
	orch  *dump.Orchestrator
	log   *slog.Logger
}

// NewTrigger builds a Trigger.
func NewTrigger(guard *dump.Guard, orch *dump.Orchestrator, log *slog.Logger) *Trigger {
	if log == nil {
		log = slog.Default()
	}
	return &Trigger{guard: guard, orch: orch, log: log}
}

// Run executes one dump run if none is in flight. It returns the per-database
// results and ok=true, or (nil, false) when a run is already in progress. On
// completion it emits a single "dump cycle complete" heartbeat (total/ok/failed
// tallied from the results) so a Loki absence alert can catch a silent non-run
// of this backup-critical sidecar, not just a loud per-DB failure.
func (t *Trigger) Run() (results []dump.Result, ok bool) {
	runCtx, cancel := context.WithCancel(context.Background())
	release, acquired := t.guard.TryAcquire(cancel)
	if !acquired {
		cancel()
		return nil, false
	}
	defer release()
	defer cancel()

	results = t.orch.Run(runCtx)

	var okN, failedN int
	for _, r := range results {
		if r.OK() {
			okN++
		} else {
			failedN++
		}
	}
	t.log.Info("dump cycle complete", "total", len(results), "ok", okN, "failed", failedN)

	return results, true
}

// Deps are the wiring NewServer needs.
type Deps struct {
	Trigger    *Trigger
	Health     health.Signal
	Log        *slog.Logger
	ListenAddr string
	AuthToken  string
}

// NewServer wires the routes and returns a configured *http.Server.
func NewServer(d *Deps) *http.Server {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.Handle("POST /dump", authMiddleware(d.AuthToken, dumpHandler(d.Trigger, log)))
	mux.Handle("GET /healthz", health.Handler(d.Health))
	return &http.Server{
		Addr:              d.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		// No WriteTimeout: a dump run holds the response open for minutes by
		// design. ReadTimeout/IdleTimeout are safe to bound (they cover the
		// request-read and idle-keepalive phases, not the long dump response).
		ReadTimeout: readHeaderTimeout,
		IdleTimeout: 60 * time.Second,
	}
}

// dumpHandler runs one dump and writes one text line per database. Status is
// 200 when every database produced "ok", else 500; a run already in progress
// is 429. The method-aware mux pattern returns 405 for non-POST.
func dumpHandler(tr *Trigger, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		results, ok := tr.Run()
		if !ok {
			http.Error(w, "dump already in progress", http.StatusTooManyRequests)
			return
		}

		var b strings.Builder
		allOK := true
		for _, r := range results {
			fmt.Fprintf(&b, "%s/%s: %s\n", r.Host, r.DBName, r.BodyDetail())
			if !r.OK() {
				allOK = false
			}
		}

		status := http.StatusOK
		if !allOK {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, b.String()); err != nil {
			log.Warn("write dump response failed", "err", err)
		}
	})
}

// authMiddleware enforces a bearer token when one is configured; it is a no-op
// when the token is empty (documented open mode). The comparison is
// constant-time to avoid leaking the token via timing.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	const prefix = "Bearer "
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
