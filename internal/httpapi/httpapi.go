// Package httpapi is the HTTP control surface: POST /dump (optional bearer
// auth) and GET /healthz (the health marker, via the health library). It owns no domain
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

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/pg-autodump/internal/dump"
)

// readHeaderTimeout guards against slow-header (slowloris) clients. There is no
// write timeout: a dump run holds the response open for minutes by design.
const readHeaderTimeout = 10 * time.Second

// Trigger runs one dump under the in-process single-flight guard and the
// cross-process cycle lock (scheduler.Exclusive). The run context is derived
// from context.Background(), not the HTTP request, so a client disconnect
// never cancels the dump — only shutdown does, via the guard. The cycle lock
// extends single-flight across processes: an exec'd `pg-autodump run` can
// never dump concurrently with the server, and a rerun it queues mid-cycle is
// executed by the server before Run returns (depth-1 coalescing).
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

// Run executes one dump run if none is in flight. OutcomeRan or
// OutcomeRanQueued carries the caller's own run's results; rerun demand that
// exec'd runs queue meanwhile is consumed before Run returns. OutcomeSkipped
// means a run is in progress here or in an exec'd `pg-autodump run`, and
// OutcomeGated that shutdown was signalled first. A non-nil error means the
// cross-process cycle coordination failed and nothing ran.
func (t *Trigger) Run() ([]dump.Result, scheduler.Outcome, error) {
	runCtx, cancel := context.WithCancel(context.Background())
	release, acquired := t.guard.TryAcquire(cancel)
	if !acquired {
		cancel()
		return nil, scheduler.OutcomeSkipped, nil
	}
	defer release()
	defer cancel()

	var out []dump.Result
	got := false
	outcome, exErr := t.cycle.RunOrSkip(func() error {
		r := t.orch.Run(runCtx)
		if !got {
			out, got = r, true
		}
		return nil
	})
	if !got {
		return nil, outcome, exErr
	}
	if exErr != nil {
		// The run completed; a queue-file error only degrades rerun
		// coalescing, so it is logged rather than failing the caller's run.
		t.log.Warn("cycle coordination error after run", "err", exErr)
	}
	return out, outcome, nil
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

	// Outermost first: the auth-failure throttle sits outside Logging so a
	// bad-bearer flood gets 429s without an access line each, and
	// SecurityHeaders sits innermost so its baseline survives onto a recovered
	// 500. client_ip is the bare socket peer: no proxy fronts this sidecar.
	handler := httpserver.Chain(mux,
		authFailureLimiter(d.AuthToken),
		httpserver.Logging(httpserver.WithLogger(log), httpserver.ProbeLogLevel("/healthz"), httpserver.WithClientIP()),
		httpserver.Recoverer(httpserver.WithRecoverLogger(log)),
		httpserver.SecurityHeaders(),
	)

	return httpserver.New(handler,
		httpserver.WithReadHeaderTimeout(readHeaderTimeout),
		httpserver.WithReadTimeout(readHeaderTimeout),
		httpserver.WithIdleTimeout(60*time.Second),
	)
}

// dumpHandler runs one dump and writes one text line per database. Status is
// 200 when the cycle fully succeeded (dump.CycleOK), else 500; a run already
// in progress (in this process or an exec'd `pg-autodump run`) is 429, a
// request that arrives once shutdown has begun is 503, and a cycle-lock
// infrastructure failure is a 500 with a generic body (the detail is logged).
// The method-aware mux pattern returns 405 for non-POST.
func dumpHandler(tr *Trigger, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		results, outcome, err := tr.Run()
		switch {
		case err != nil:
			log.Error("cycle coordination failed", "err", err)
			http.Error(w, "cycle coordination failed", http.StatusInternalServerError)
			return
		case outcome == scheduler.OutcomeGated:
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		case outcome == scheduler.OutcomeSkipped:
			http.Error(w, "dump already in progress", http.StatusTooManyRequests)
			return
		}

		var b strings.Builder
		for _, r := range results {
			fmt.Fprintf(&b, "%s/%s: %s\n", r.Host, r.DBName, r.BodyDetail())
		}
		if len(results) == 0 {
			b.WriteString("no databases configured\n")
		}

		status := http.StatusOK
		if !dump.CycleOK(results) {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, b.String()); err != nil {
			log.Warn("write dump response failed", "err", err)
		}
	})
}

// authFailureLimiter throttles repeated failed bearer attempts against
// POST /dump with httpserver's FailedAuthRateLimit preset. A valid bearer is never
// throttled; over-budget bad-bearer requests get 429 before the access logger
// or handler, bounding the log-flood vector. Open mode (no token) disables it:
// the verifier fails closed on an empty secret, so every request to a
// deliberately open endpoint would otherwise read as a failed attempt.
func authFailureLimiter(token string) httpserver.Middleware {
	if token == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	verify := httpserver.NewStaticTokenVerifier(token)
	return httpserver.FailedAuthRateLimit(func(r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/dump" {
			return false
		}
		return !presentsValidBearer(verify, r)
	}, "too many failed bearer attempts")
}

// presentsValidBearer reports whether r carries a well-formed Authorization
// bearer that verifies against the configured token. Shared by the
// auth-failure throttle's predicate and authMiddleware so the two can never
// drift on what counts as a valid credential.
func presentsValidBearer(verify httpserver.StaticTokenVerifier, r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, prefix) && verify.Verify(strings.TrimPrefix(h, prefix))
}

// authMiddleware enforces a bearer token when one is configured; a no-op when
// the token is empty (documented open mode). The verifier is built once so
// the configured token is pre-hashed: each request hashes only the presented
// value and compares fixed-length digests in constant time, so neither the
// compare nor the per-call hashing leaks the configured token's length or
// content. It also fails closed on an empty configured secret.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	verify := httpserver.NewStaticTokenVerifier(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !presentsValidBearer(verify, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
