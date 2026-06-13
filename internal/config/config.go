// Package config is the single environment-reading layer. os.Getenv appears
// nowhere else in the codebase (per go.md): every tunable is a typed Config
// field populated once at startup by Load and never mutated. No secret is ever
// a Config field; pg_dump reads .pgpass (or the libpq-owned PGPASSWORD) itself,
// so passwords never transit memory this package logs or formats.
package config

import (
	"fmt"
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
	AuthToken       string
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
// the typed Config and a slice of non-fatal warnings. Load never fails: every
// missing or malformed value falls back to a safe default, recording a warning
// when it does. An empty DB_SPECS yields no specs (surfaced via the health
// probe, matching 1.x); malformed DB_SPECS tokens are validated per-token in
// internal/spec and reported per-DB by the orchestrator, never here.
func Load(getenv func(string) string) (Config, []Warning) {
	var w warnings
	cfg := Config{
		ListenAddr: firstNonEmpty(getenv("LISTEN_ADDR"), DefaultListenAddr),
		DumpDir:    loadDumpDir(getenv("DUMP_DIR"), &w),
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
	cfg.DumpConcurrency = loadConcurrency(getenv("DUMP_CONCURRENCY"), &w)
	cfg.DumpInterval = loadInterval(getenv("DUMP_INTERVAL"), &w)
	cfg.DumpKeep = loadKeep(getenv("DUMP_KEEP"), &w)
	cfg.FreeKBWarn = loadFreeKB(getenv("DUMP_FREE_KB_WARN"), &w)
	cfg.ShutdownGrace = loadShutdownGrace(getenv("SHUTDOWN_GRACE"), cfg.DumpTimeout, &w)

	return cfg, w
}

// warnings accumulates non-fatal notes; the addf helper keeps call sites terse.
type warnings []Warning

func (w *warnings) addf(format string, args ...any) {
	*w = append(*w, Warning(fmt.Sprintf(format, args...)))
}

func loadDumpDir(v string, w *warnings) string {
	if v == "" {
		return DefaultDumpDir
	}
	// A ".." component could let dumps escape the intended volume. Rather than
	// abort startup, reject the value and fall back to the default with a
	// warning (graceful degradation; the traversal path is never used).
	if strings.Contains(v, "..") {
		w.addf("DUMP_DIR %q must not contain \"..\"; using default %s", v, DefaultDumpDir)
		return DefaultDumpDir
	}
	return v
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

func loadConcurrency(v string, w *warnings) int {
	if v == "" {
		return DefaultConcurrency
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		w.addf("DUMP_CONCURRENCY %q is not a positive integer; using default %d", v, DefaultConcurrency)
		return DefaultConcurrency
	}
	return n
}

func loadInterval(v string, w *warnings) time.Duration {
	// Matches the sibling schedulers (SYNC_INTERVAL / FCLONES_INTERVAL /
	// SCHED_INTERVAL): the built-in timer runs by default; "off", "disabled",
	// or a zero duration ("0"/"0s") hands scheduling to an external trigger
	// (the homelab uses Ofelia). Unparseable values fall back to the default.
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
	case d <= 0:
		return 0
	default:
		return d
	}
}

func loadKeep(v string, w *warnings) int {
	if v == "" {
		return DefaultDumpKeep
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		w.addf("DUMP_KEEP %q is not a positive integer; using default %d", v, DefaultDumpKeep)
		return DefaultDumpKeep
	}
	return n
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
