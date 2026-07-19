// Package httpapi is the HTTP control surface: POST /dump (optional bearer
// auth) and GET /healthz (liveness, via the health library). It owns no domain
// logic; the dump run is driven through a Trigger that both the handler and the
// optional built-in ticker share, so single-flight lives in exactly one place.
package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/webhttp"
)

// readHeaderTimeout guards against slow-header (slowloris) clients. There is no
// write timeout: a dump run holds the response open for minutes by design.
const readHeaderTimeout = 10 * time.Second

// Trigger runs one dump under the in-process single-flight guard and the
// cross-process cycle lock (scheduler.Exclusive). The run context is derived
// from context.Background(), NOT the HTTP request, so a trigger client that
// disconnects (a short wget firing a long backup) never cancels the dump; only
// shutdown cancels it, via the guard. The cycle lock extends single-flight
// across processes: an exec'd `pg-autodump run` can never dump concurrently
// with the server, and a rerun request it queues while the server's cycle runs
// is executed by the server before Run returns (depth-1 coalescing — queued
// demand is owed a run that starts after it arrived).
type Trigger struct {
	guard *dump.Guard
	cycle *scheduler.Exclusive
	orch  *dump.Orchestrator
	log   *slog.Logger
}

// NewTrigger builds a Trigger.
func NewTrigger(guard *dump.Guard, cycle *scheduler.Exclusive, orch *dump.Orchestrator, log *slog.Logger) *Trigger {
	if log == nil {
		log = slog.Default()
	}
	return &Trigger{guard: guard, cycle: cycle, orch: orch, log: log}
}

// Run executes one dump run if none is in flight. It returns the per-database
// results of the caller's own run and ok=true. ok=false (with a nil error)
// means a run is already in progress: the in-process guard is held, or an
// exec'd `pg-autodump run` in another process holds the cycle lock. A non-nil
// error means the cross-process cycle coordination itself failed and nothing
// ran. Rerun demand queued by exec'd runs during the cycle is consumed before
// Run returns; the reported results are always the first (the caller's own)
// run.
func (t *Trigger) Run() (results []dump.Result, ok bool, err error) {
	runCtx, cancel := context.WithCancel(context.Background())
	release, acquired := t.guard.TryAcquire(cancel)
	if !acquired {
		cancel()
		return nil, false, nil
	}
	defer release()
	defer cancel()

	var out []dump.Result
	got := false
	_, exErr := t.cycle.RunOrSkip(func() error {
		r := t.orch.Run(runCtx)
		if !got {
			out, got = r, true
		}
		return nil
	})
	if !got {
		// Nothing ran: the cycle lock is held by another process (skip), the
		// shutdown gate closed first, or the lock could not be used (exErr).
		return nil, false, exErr
	}
	if exErr != nil {
		// The run itself completed; a queue-file error only degrades the
		// demand-coalescing bookkeeping, so it is logged rather than failing
		// the run the caller paid for.
		t.log.Warn("cycle coordination error after run", "err", exErr)
	}
	return out, true, nil
}

// Deps are the wiring NewServer needs.
type Deps struct {
	Trigger   *Trigger
	Health    health.Signal
	Log       *slog.Logger
	AuthToken string
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

	// Access logging (with request-id) + panic recovery + baseline security
	// headers all come from webhttp; the /healthz probe is skipped so routine
	// liveness checks do not flood the log. Chain is outermost-first, so this is
	// webhttp's canonical order: Logging outermost (its access line records the
	// final status), Recoverer inside it (a recovered panic is logged as its
	// 500), and SecurityHeaders innermost — its nosniff / X-Frame-Options: DENY /
	// Referrer-Policy baseline is set before the handler runs, so it survives
	// even onto a recovered 500. No CSP or HSTS: this is a plain-HTTP,
	// non-browser, text/plain control endpoint, so nosniff is the header that
	// earns its keep (the framing/referrer defaults are harmless standardization).
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.WithLogger(log), webhttp.WithSkipPaths("/healthz")),
		webhttp.Recoverer(webhttp.WithRecoverLogger(log)),
		webhttp.SecurityHeaders(),
	)

	// webhttp.NewServer supplies the streaming-safe defaults (MaxHeaderBytes
	// 1 MiB, WriteTimeout unset). WriteTimeout stays unset on purpose: a dump
	// run holds the response open for minutes. ReadHeaderTimeout (the slowloris
	// guard), ReadTimeout, and the 60s IdleTimeout are supplied explicitly to
	// pin the previous bounds. No Addr is set: webhttp.Run serves the listener
	// main binds, and http.Server.Serve ignores http.Server.Addr.
	return webhttp.NewServer(handler,
		webhttp.WithReadHeaderTimeout(readHeaderTimeout),
		webhttp.WithReadTimeout(readHeaderTimeout),
		webhttp.WithIdleTimeout(60*time.Second),
	)
}

// dumpHandler runs one dump and writes one text line per database. Status is
// 200 when every database produced "ok", else 500; a run already in progress
// (in this process or an exec'd `pg-autodump run`) is 429, and a cycle-lock
// infrastructure failure is a 500 with a generic body (the detail is logged).
// The method-aware mux pattern returns 405 for non-POST.
func dumpHandler(tr *Trigger, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		results, ok, err := tr.Run()
		if err != nil {
			log.Error("cycle coordination failed", "err", err)
			http.Error(w, "cycle coordination failed", http.StatusInternalServerError)
			return
		}
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
// when the token is empty (documented open mode). Verification is delegated to
// webhttp's static-token verifier, built ONCE here so the configured token is
// pre-hashed: each request hashes only the presented value and compares
// fixed-length SHA-256 digests in constant time, so neither the compare's
// short-circuit nor the per-call hashing leaks anything about the configured
// token (a raw ConstantTimeCompare short-circuits on length mismatch, making
// the token's LENGTH timing-observable even though its content is not). The
// verifier also fails closed on an empty configured secret, so if the open-mode
// bypass above it were ever removed, an unset token would reject every request
// rather than matching an empty presented credential.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	const prefix = "Bearer "
	verify := webhttp.NewStaticTokenVerifier(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) ||
			!verify.Verify(strings.TrimPrefix(h, prefix)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
