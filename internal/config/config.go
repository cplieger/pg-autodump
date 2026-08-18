// Package config is the single environment-reading layer. os.Getenv appears
// nowhere else in the codebase (per go.md): every tunable is a typed Config
// field populated once at startup by Load and never mutated. Typed reads go
// through envx's injectable Source (built over the getenv Load receives, so
// the run(args, getenv) test seam survives); the strict variants return the
// parse result to Load, which owns this app's warning policy — warnings
// accumulate into a slice the caller logs once at startup, they do not go to
// slog mid-parse. No database
// password is ever a Config field; pg_dump reads .pgpass (or the
// libpq-owned PGPASSWORD) itself, so DB passwords never transit memory
// this package logs or formats. The lone secret this package holds is
// AuthToken (the AUTH_TOKEN /dump bearer token); Load records no warning
// for it and no caller logs it, so it never reaches a log or error line.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/pathinside/v2"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/webhttp/v2"
)

// Defaults for every tunable. Exported so tests and docs share one source.
const (
	DefaultListenAddr   = ":9847"
	DefaultDumpDir      = "/dumps"
	DefaultPGPassFile   = "/secrets/.pgpass"
	DefaultDumpTimeout  = 300 * time.Second
	MinDumpTimeout      = 10 * time.Second
	DefaultConcurrency  = 2
	DefaultDumpInterval = 24 * time.Hour
	DefaultDumpKeep     = 7       // retained timestamped copies per DB; set 1 for a single stable <dbname>.dump (overwrite)
	DefaultFreeKBWarn   = 1 << 20 // 1 GiB
	shutdownSlack       = 15 * time.Second
	stmtTimeoutSlack    = 60 * time.Second
)

// Config is the fully-typed, validated runtime configuration.
type Config struct {
	ListenAddr      string
	DumpDir         string
	PGPassFile      string
	AuthToken       string // AUTH_TOKEN bearer for POST /dump; the one secret field -- never logged or formatted
	Specs           []spec.DBSpec
	DumpTimeout     time.Duration
	StmtTimeout     time.Duration // server-side statement_timeout, derived above DumpTimeout
	DumpConcurrency int
	DumpInterval    time.Duration // DUMP_INTERVAL, default 24h; "off" disables the built-in timer (external trigger only)
	DumpKeep        int           // DUMP_KEEP, default 7; 1 = single stable <dbname>.dump, >1 = N timestamped copies retained
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT, Go duration; default DUMP_TIMEOUT+15s
	FreeKBWarn      int64
}

// Warning is a non-fatal configuration note (e.g. a clamped value) for the
// caller to log. Warnings never abort startup.
type Warning string

// Load reads configuration from getenv (injected for testability). It returns
// the typed Config, a slice of non-fatal warnings, and an error. Almost every
// missing or malformed value falls back to a safe default with a warning, so
// Load succeeds; the sole fatal case is a DUMP_DIR containing ".." — refusing
// to start beats silently relocating backups to the default when the operator
// asked for a specific directory (the backup destination is too important to
// guess at). An empty DB_SPECS yields no specs (surfaced via the health probe,
// matching 1.x); malformed DB_SPECS tokens are validated per-token in
// internal/spec and reported per-DB by the orchestrator, never here.
func Load(getenv func(string) string) (Config, []Warning, error) {
	var w warnings
	src := envx.Source{Get: getenv}
	dumpDir, dumpDirErr := loadDumpDir(getenv("DUMP_DIR"))
	cfg := Config{
		ListenAddr: firstNonEmpty(getenv("LISTEN_ADDR"), DefaultListenAddr),
		DumpDir:    dumpDir,
		PGPassFile: firstNonEmpty(getenv("PGPASSFILE"), DefaultPGPassFile),
		AuthToken:  getenv("AUTH_TOKEN"),
	}

	// Each token is validated independently in internal/spec; a malformed one
	// (control characters, bad shape, traversal) becomes an Invalid spec the
	// orchestrator reports and skips, so one bad entry never blocks the rest.
	cfg.Specs = spec.Parse(getenv("DB_SPECS"))

	cfg.DumpTimeout = loadDumpTimeout(src, &w)
	// Server-side statement_timeout sits ABOVE the Go DumpTimeout so the Go
	// deadline fires first (clean "timeout" class); the server-side bound only
	// matters for an uncleanly-dropped network path, where it self-aborts the
	// backend rather than aborting a legitimate long COPY.
	cfg.StmtTimeout = cfg.DumpTimeout + stmtTimeoutSlack
	cfg.DumpConcurrency = loadPositiveInt(src, "DUMP_CONCURRENCY", DefaultConcurrency, &w)
	cfg.DumpInterval = loadInterval(src, &w)
	cfg.DumpKeep = loadPositiveInt(src, "DUMP_KEEP", DefaultDumpKeep, &w)
	cfg.FreeKBWarn = loadFreeKB(src, &w)
	cfg.ShutdownTimeout = loadShutdownTimeout(src, cfg.DumpTimeout, &w)

	return cfg, w, dumpDirErr
}

// rawValue extracts the offending raw string from a strict-getter error for
// this package's warning lines; envx's *ParseError carries the trimmed value.
func rawValue(err error) string {
	var perr *envx.ParseError
	if errors.As(err, &perr) {
		return perr.Value
	}
	return ""
}

// warnings accumulates non-fatal notes; the addf helper keeps call sites terse.
type warnings []Warning

func (w *warnings) addf(format string, args ...any) {
	*w = append(*w, Warning(fmt.Sprintf(format, args...)))
}

// loadDumpDir resolves DUMP_DIR. An unset value uses the default. A value with
// a ".." path component is fatal (returns an error): a traversal component
// could let dumps escape the intended volume, and silently relocating backups
// to the default would hide that the operator's chosen directory was ignored —
// for a backup tool, failing to start is safer than backing up to the wrong
// place. main surfaces the error and aborts startup. The check is per path
// component (pathinside.HasDotDot, examining the value AS WRITTEN so a
// traversal that would normalize away is still refused), so a legal name that
// merely contains consecutive dots (e.g. "/dumps/a..b") is accepted.
func loadDumpDir(v string) (string, error) {
	if v == "" {
		return DefaultDumpDir, nil
	}
	if pathinside.HasDotDot(v) {
		return "", fmt.Errorf("DUMP_DIR %q must not contain a %q path component (refusing to start; set a directory without path traversal)", v, "..")
	}
	return v, nil
}

func loadDumpTimeout(src envx.Source, w *warnings) time.Duration {
	secs, ok, err := src.IntStrict("DUMP_TIMEOUT")
	switch {
	case err != nil:
		w.addf("DUMP_TIMEOUT %q is not a positive integer; using default %s", rawValue(err), DefaultDumpTimeout)
		return DefaultDumpTimeout
	case !ok:
		return DefaultDumpTimeout
	case secs <= 0:
		w.addf("DUMP_TIMEOUT %q is not a positive integer; using default %s", strconv.Itoa(secs), DefaultDumpTimeout)
		return DefaultDumpTimeout
	case time.Duration(secs)*time.Second < MinDumpTimeout:
		w.addf("DUMP_TIMEOUT %ds below minimum; clamped to %s", secs, MinDumpTimeout)
		return MinDumpTimeout
	default:
		return time.Duration(secs) * time.Second
	}
}

// loadPositiveInt parses a strictly-positive integer env var, falling
// back to def (with a warning) on an empty, malformed, or non-positive
// value. Shared by DUMP_CONCURRENCY and DUMP_KEEP, which carry the
// identical "positive int or default" contract.
func loadPositiveInt(src envx.Source, key envx.Key, def int, w *warnings) int {
	n, ok, err := src.IntStrict(key)
	switch {
	case err != nil:
		w.addf("%s %q is not a positive integer; using default %d", key, rawValue(err), def)
		return def
	case !ok:
		return def
	case n < 1:
		w.addf("%s %q is not a positive integer; using default %d", key, strconv.Itoa(n), def)
		return def
	default:
		return n
	}
}

func loadInterval(src envx.Source, w *warnings) time.Duration {
	// Matches the sibling schedulers (SYNC_INTERVAL / SCAN_INTERVAL /
	// RUN_INTERVAL): the built-in timer runs by default; "off", "disabled",
	// or a zero duration ("0"/"0s") hands scheduling to an external trigger
	// (e.g. Ofelia). Unparseable values fall back to the default. The
	// off/disabled sentinels and the negative-disables policy are this app's
	// (scheduler.ParseInterval maps negative to the default, not to
	// disabled), so the sentinel check reads the raw value and only the
	// duration parse goes through envx.
	switch strings.ToLower(strings.TrimSpace(src.String("DUMP_INTERVAL"))) {
	case "off", "disabled":
		return 0
	}
	d, ok, err := src.DurationStrict("DUMP_INTERVAL")
	switch {
	case err != nil:
		w.addf("DUMP_INTERVAL %q is not a valid duration; using default %s (set \"off\" to disable)", rawValue(err), DefaultDumpInterval)
		return DefaultDumpInterval
	case !ok:
		return DefaultDumpInterval
	case d < 0:
		w.addf("DUMP_INTERVAL %q is negative; built-in timer disabled (use a positive duration or 'off')", d.String())
		return 0
	case d == 0:
		return 0
	default:
		return d
	}
}

// loadFreeKB reads DUMP_FREE_KB_WARN via IntStrict; int is 64-bit on both
// fleet platforms (amd64, arm64), so the old ParseInt(v, 10, 64) range is
// unchanged.
func loadFreeKB(src envx.Source, w *warnings) int64 {
	kb, ok, err := src.IntStrict("DUMP_FREE_KB_WARN")
	switch {
	case err != nil:
		w.addf("DUMP_FREE_KB_WARN %q is not a non-negative integer; using default %d", rawValue(err), DefaultFreeKBWarn)
		return DefaultFreeKBWarn
	case !ok:
		return DefaultFreeKBWarn
	case kb < 0:
		w.addf("DUMP_FREE_KB_WARN %q is not a non-negative integer; using default %d", strconv.Itoa(kb), DefaultFreeKBWarn)
		return DefaultFreeKBWarn
	default:
		return int64(kb)
	}
}

// loadShutdownTimeout reads SHUTDOWN_TIMEOUT as a Go duration (e.g. "315s",
// "5m"). Unset falls back to the derived DumpTimeout+shutdownSlack; a
// malformed or non-positive value warns and uses the same derived value. A
// value below DUMP_TIMEOUT is honoured but warned about, since it lets the
// drain kill an in-flight dump.
func loadShutdownTimeout(src envx.Source, dumpTimeout time.Duration, w *warnings) time.Duration {
	derived := dumpTimeout + shutdownSlack
	timeout, ok, err := src.DurationStrict("SHUTDOWN_TIMEOUT")
	switch {
	case err != nil:
		w.addf("SHUTDOWN_TIMEOUT %q is not a positive duration; using derived %s", rawValue(err), derived)
		return derived
	case !ok:
		return derived
	case timeout <= 0:
		w.addf("SHUTDOWN_TIMEOUT %q is not a positive duration; using derived %s", timeout.String(), derived)
		return derived
	}
	if timeout < dumpTimeout {
		w.addf("SHUTDOWN_TIMEOUT %s is below DUMP_TIMEOUT %s; an in-flight dump may be killed on shutdown", timeout, dumpTimeout)
	}
	return timeout
}

func firstNonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ListenerOpenAndPublic reports whether POST /dump would accept unauthenticated
// requests on a non-loopback bind: AUTH_TOKEN is empty AND LISTEN_ADDR resolves
// to something other than a loopback address. main emits a one-line startup
// WARN when it is true, so a deployment that takes the open default without
// restricting the published port to loopback (the documented 127.0.0.1: publish)
// is surfaced at boot. Open mode stays supported — this only flags it, never
// blocks startup.
func ListenerOpenAndPublic(authToken, listenAddr string) bool {
	if authToken != "" {
		return false
	}
	return listenIsPublic(listenAddr)
}

// listenIsPublic reports whether a listen address binds a non-loopback
// interface, via webhttp's bind classifier under the fail-public recipe: a
// wildcard bind (":9847", "0.0.0.0:9847", "[::]:9847") is public, an explicit
// loopback ("127.0.0.1", "::1", "localhost") is not, and an address that
// cannot be parsed is treated as public — a spurious warning is preferable to
// a silently unflagged open endpoint.
func listenIsPublic(addr string) bool {
	return webhttp.ClassifyBind(addr) != webhttp.BindLoopback
}
