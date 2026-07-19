// Package config is the single environment-reading layer. os.Getenv appears
// nowhere else in the codebase (per go.md): every tunable is a typed Config
// field populated once at startup by Load and never mutated. No database
// password is ever a Config field; pg_dump reads .pgpass (or the
// libpq-owned PGPASSWORD) itself, so DB passwords never transit memory
// this package logs or formats. The lone secret this package holds is
// AuthToken (the AUTH_TOKEN /dump bearer token); Load records no warning
// for it and no caller logs it, so it never reaches a log or error line.
package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
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
	ShutdownGrace   time.Duration
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
	cfg.Specs = spec.ParseSpecs(getenv("DB_SPECS"))

	cfg.DumpTimeout = loadDumpTimeout(getenv("DUMP_TIMEOUT"), &w)
	// Server-side statement_timeout sits ABOVE the Go DumpTimeout so the Go
	// deadline fires first (clean "timeout" class); the server-side bound only
	// matters for an uncleanly-dropped network path, where it self-aborts the
	// backend rather than aborting a legitimate long COPY.
	cfg.StmtTimeout = cfg.DumpTimeout + stmtTimeoutSlack
	cfg.DumpConcurrency = loadPositiveInt(getenv("DUMP_CONCURRENCY"), "DUMP_CONCURRENCY", DefaultConcurrency, &w)
	cfg.DumpInterval = loadInterval(getenv("DUMP_INTERVAL"), &w)
	cfg.DumpKeep = loadPositiveInt(getenv("DUMP_KEEP"), "DUMP_KEEP", DefaultDumpKeep, &w)
	cfg.FreeKBWarn = loadFreeKB(getenv("DUMP_FREE_KB_WARN"), &w)
	cfg.ShutdownGrace = loadShutdownGrace(getenv("SHUTDOWN_GRACE"), cfg.DumpTimeout, &w)

	return cfg, w, dumpDirErr
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
// component, so a legal name that merely contains consecutive dots (e.g.
// "/dumps/a..b") is accepted.
func loadDumpDir(v string) (string, error) {
	if v == "" {
		return DefaultDumpDir, nil
	}
	if hasDotDotComponent(v) {
		return "", fmt.Errorf("DUMP_DIR %q must not contain a %q path component (refusing to start; set a directory without path traversal)", v, "..")
	}
	return v, nil
}

// hasDotDotComponent reports whether p contains ".." as a full path component
// (the traversal form), as opposed to ".." merely appearing inside a longer
// name. Paths here are Linux container paths, so '/' is the only separator.
func hasDotDotComponent(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}

func loadDumpTimeout(v string, w *warnings) time.Duration {
	if v == "" {
		return DefaultDumpTimeout
	}
	secs, err := strconv.Atoi(v)
	switch {
	case err != nil || secs <= 0:
		w.addf("DUMP_TIMEOUT %q is not a positive integer; using default %s", v, DefaultDumpTimeout)
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
func loadPositiveInt(v, name string, def int, w *warnings) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		w.addf("%s %q is not a positive integer; using default %d", name, v, def)
		return def
	}
	return n
}

func loadInterval(v string, w *warnings) time.Duration {
	// Matches the sibling schedulers (SYNC_INTERVAL / FCLONES_INTERVAL /
	// SCHED_INTERVAL): the built-in timer runs by default; "off", "disabled",
	// or a zero duration ("0"/"0s") hands scheduling to an external trigger
	// (e.g. Ofelia). Unparseable values fall back to the default.
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultDumpInterval
	}
	switch strings.ToLower(v) {
	case "off", "disabled":
		return 0
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		w.addf("DUMP_INTERVAL %q is not a valid duration; using default %s (set \"off\" to disable)", v, DefaultDumpInterval)
		return DefaultDumpInterval
	case d < 0:
		w.addf("DUMP_INTERVAL %q is negative; built-in timer disabled (use a positive duration or 'off')", v)
		return 0
	case d == 0:
		return 0
	default:
		return d
	}
}

func loadFreeKB(v string, w *warnings) int64 {
	if v == "" {
		return DefaultFreeKBWarn
	}
	kb, err := strconv.ParseInt(v, 10, 64)
	if err != nil || kb < 0 {
		w.addf("DUMP_FREE_KB_WARN %q is not a non-negative integer; using default %d", v, DefaultFreeKBWarn)
		return DefaultFreeKBWarn
	}
	return kb
}

func loadShutdownGrace(v string, dumpTimeout time.Duration, w *warnings) time.Duration {
	derived := dumpTimeout + shutdownSlack
	if v == "" {
		return derived
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		w.addf("SHUTDOWN_GRACE %q is not a positive integer; using derived %s", v, derived)
		return derived
	}
	grace := time.Duration(secs) * time.Second
	if grace < dumpTimeout {
		w.addf("SHUTDOWN_GRACE %s is below DUMP_TIMEOUT %s; an in-flight dump may be killed on shutdown", grace, dumpTimeout)
	}
	return grace
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
// interface. A wildcard bind (":9847", "0.0.0.0:9847", "[::]:9847") is public;
// an explicit loopback ("127.0.0.1", "::1", "localhost") is not. An address that
// cannot be parsed is treated as public — a spurious warning is preferable to a
// silently unflagged open endpoint.
func listenIsPublic(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "" {
		return true // wildcard bind, e.g. ":9847"
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() // 0.0.0.0 and :: are unspecified => not loopback => public
	}
	return !strings.EqualFold(host, "localhost")
}
