package config

import (
	"strings"
	"testing"
	"time"
)

// envFunc builds a getenv from a map for injection into Load.
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// mustLoad calls Load and fails the test if it returns a fatal error. Every
// test here exercises a non-fatal configuration (the one fatal case, a
// traversal DUMP_DIR, has its own test), so the error is always expected nil.
func mustLoad(t *testing.T, m map[string]string) (Config, []Warning) {
	t.Helper()
	cfg, warns, err := Load(envFunc(m))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	return cfg, warns
}

func TestLoadDefaults(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if cfg.ListenAddr != DefaultListenAddr || cfg.DumpDir != DefaultDumpDir || cfg.PGPassFile != DefaultPGPassFile {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.DumpTimeout != DefaultDumpTimeout || cfg.DumpConcurrency != DefaultConcurrency {
		t.Fatalf("numeric defaults not applied: %+v", cfg)
	}
	if cfg.DumpKeep != DefaultDumpKeep || cfg.DumpInterval != DefaultDumpInterval {
		t.Fatalf("keep/interval defaults not applied: %+v", cfg)
	}
	if cfg.StmtTimeout <= cfg.DumpTimeout {
		t.Fatalf("StmtTimeout %s must exceed DumpTimeout %s", cfg.StmtTimeout, cfg.DumpTimeout)
	}
	if cfg.ShutdownGrace <= cfg.DumpTimeout {
		t.Fatalf("ShutdownGrace %s must exceed DumpTimeout %s", cfg.ShutdownGrace, cfg.DumpTimeout)
	}
	if cfg.AuthToken != "" {
		t.Fatalf("AuthToken should default empty")
	}
}

// TestLoadEmptyEnvAllDefaults is the hardening contract: with NOTHING set,
// Load yields a fully-populated, valid Config (every field a safe default) and
// no warnings. A missing environment never blocks startup.
func TestLoadEmptyEnvAllDefaults(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{})
	if len(warns) != 0 {
		t.Fatalf("empty env should produce no warnings, got %v", warns)
	}
	if cfg.ListenAddr != DefaultListenAddr || cfg.DumpDir != DefaultDumpDir ||
		cfg.PGPassFile != DefaultPGPassFile || cfg.DumpTimeout != DefaultDumpTimeout ||
		cfg.DumpConcurrency != DefaultConcurrency || cfg.DumpInterval != DefaultDumpInterval ||
		cfg.DumpKeep != DefaultDumpKeep || cfg.FreeKBWarn != DefaultFreeKBWarn {
		t.Fatalf("empty env did not yield all defaults: %+v", cfg)
	}
	if len(cfg.Specs) != 0 {
		t.Fatalf("expected no specs with empty env, got %d", len(cfg.Specs))
	}
}

func TestLoadNoSecretFields(t *testing.T) {
	// PGPASSWORD must never be read into Config (it is libpq-owned).
	cfg, _ := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "PGPASSWORD": "s3cr3t"})
	for _, v := range []string{cfg.ListenAddr, cfg.DumpDir, cfg.PGPassFile, cfg.AuthToken} {
		if v == "s3cr3t" {
			t.Fatal("PGPASSWORD leaked into a Config field")
		}
	}
}

// A DUMP_DIR with ".." is fatal: Load returns an error so startup aborts rather
// than silently relocating backups to the default directory (a backup tool must
// not guess where the operator's chosen destination went).
func TestLoadDumpDirTraversalIsFatal(t *testing.T) {
	cfg, warns, err := Load(envFunc(map[string]string{"DUMP_DIR": "/dumps/../etc"}))
	if err == nil {
		t.Fatalf("DUMP_DIR with \"..\" must return a fatal error; got nil (cfg %+v, warns %v)", cfg, warns)
	}
	if !strings.Contains(err.Error(), "DUMP_DIR") {
		t.Fatalf("error %q should name DUMP_DIR", err)
	}
}

// A control character in one DB_SPECS token does not abort startup: that token
// becomes an Invalid spec (reported per-DB later) while valid tokens parse.
func TestLoadControlCharSpecIsPerSpec(t *testing.T) {
	cfg, _ := mustLoad(t, map[string]string{"DB_SPECS": "good-host:db:user h:db:u\x01"})
	if len(cfg.Specs) != 2 {
		t.Fatalf("expected 2 specs (1 valid, 1 invalid), got %d", len(cfg.Specs))
	}
	if cfg.Specs[0].Invalid != "" {
		t.Fatalf("first spec should be valid, got invalid: %q", cfg.Specs[0].Invalid)
	}
	if cfg.Specs[1].Invalid == "" {
		t.Fatal("control-char spec should be marked Invalid")
	}
}

func TestLoadEmptySpecsNotFatal(t *testing.T) {
	cfg, _ := mustLoad(t, map[string]string{})
	if len(cfg.Specs) != 0 {
		t.Fatalf("expected no specs, got %d", len(cfg.Specs))
	}
}

func TestLoadClampsAndWarns(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{
		"DB_SPECS":         "h:db:u",
		"DUMP_TIMEOUT":     "3", // below MinDumpTimeout
		"DUMP_CONCURRENCY": "abc",
	})
	if cfg.DumpTimeout != MinDumpTimeout {
		t.Fatalf("DumpTimeout = %s, want clamped to %s", cfg.DumpTimeout, MinDumpTimeout)
	}
	if cfg.DumpConcurrency != DefaultConcurrency {
		t.Fatalf("bad DUMP_CONCURRENCY should fall back to default")
	}
	if len(warns) < 2 {
		t.Fatalf("expected warnings for clamp and bad concurrency, got %v", warns)
	}
}

func TestLoadDumpInterval(t *testing.T) {
	// Default (unset): the built-in timer is on.
	def, _ := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u"})
	if def.DumpInterval != DefaultDumpInterval {
		t.Fatalf("default DumpInterval = %s, want %s", def.DumpInterval, DefaultDumpInterval)
	}

	cfg, _ := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_INTERVAL": "6h"})
	if cfg.DumpInterval != 6*time.Hour {
		t.Fatalf("DumpInterval = %s, want 6h", cfg.DumpInterval)
	}

	// Every disable sentinel maps to 0 (built-in timer off), matching the
	// sibling schedulers.
	for _, off := range []string{"off", "disabled", "0", "0s", "OFF"} {
		got, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_INTERVAL": off})
		if got.DumpInterval != 0 {
			t.Fatalf("DUMP_INTERVAL=%q: DumpInterval = %s, want 0 (disabled)", off, got.DumpInterval)
		}
		// A disable sentinel is a SILENT 0 -- distinct from a negative duration,
		// which returns 0 *with* a warning (TestLoadDumpIntervalNegativeDisablesWithWarning).
		// Pin the no-warning half so a mutant that warns on the off/disabled or
		// d==0 branch of loadInterval is caught.
		if len(warns) != 0 {
			t.Fatalf("DUMP_INTERVAL=%q: want no warnings (silent disable), got %v", off, warns)
		}
	}
}

func TestLoadDumpKeep(t *testing.T) {
	// Default (unset): keep a rolling window.
	def, _ := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u"})
	if def.DumpKeep != DefaultDumpKeep {
		t.Fatalf("default DumpKeep = %d, want %d", def.DumpKeep, DefaultDumpKeep)
	}

	cfg, _ := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_KEEP": "5"})
	if cfg.DumpKeep != 5 {
		t.Fatalf("DumpKeep = %d, want 5", cfg.DumpKeep)
	}

	// Invalid values fall back to the default with a warning.
	for _, bad := range []string{"0", "-1", "two", "1.5"} {
		got, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_KEEP": bad})
		if got.DumpKeep != DefaultDumpKeep {
			t.Fatalf("DUMP_KEEP=%q: DumpKeep = %d, want default %d", bad, got.DumpKeep, DefaultDumpKeep)
		}
		if len(warns) == 0 {
			t.Fatalf("DUMP_KEEP=%q: expected a warning", bad)
		}
	}
}

// A valid DUMP_CONCURRENCY passes through unchanged and produces no warning.
// This pins both negations on the rejection guard `err != nil || n < 1`
// (loadPositiveInt): mutating `err != nil` -> `err == nil` or `n < 1` ->
// `n >= 1` would reject a perfectly valid value and fall back to the default.
func TestLoadValidConcurrencyPassesThrough(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_CONCURRENCY": "4"})
	if cfg.DumpConcurrency != 4 {
		t.Fatalf("DumpConcurrency = %d, want 4 (valid value must pass through)", cfg.DumpConcurrency)
	}
	if len(warns) != 0 {
		t.Fatalf("valid DUMP_CONCURRENCY should produce no warnings, got %v", warns)
	}
}

// DUMP_CONCURRENCY=1 is the exact lower boundary: it is valid and must NOT be
// rejected. The boundary mutant `n < 1` -> `n <= 1` would reject 1.
func TestLoadConcurrencyBoundaryOneIsValid(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_CONCURRENCY": "1"})
	if cfg.DumpConcurrency != 1 {
		t.Fatalf("DumpConcurrency = %d, want 1 (the boundary value is valid)", cfg.DumpConcurrency)
	}
	if len(warns) != 0 {
		t.Fatalf("DUMP_CONCURRENCY=1 should produce no warnings, got %v", warns)
	}
}

// DUMP_KEEP=1 is the exact lower boundary: it selects the single-stable-file
// scheme and is valid. The boundary mutant `n < 1` -> `n <= 1` (loadPositiveInt)
// would reject 1 and fall back to the default of 7.
func TestLoadKeepBoundaryOneIsValid(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_KEEP": "1"})
	if cfg.DumpKeep != 1 {
		t.Fatalf("DumpKeep = %d, want 1 (the boundary value is valid)", cfg.DumpKeep)
	}
	if len(warns) != 0 {
		t.Fatalf("DUMP_KEEP=1 should produce no warnings, got %v", warns)
	}
}

func TestLoadFreeKBInvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "1.5"} {
		cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_FREE_KB_WARN": bad})
		if cfg.FreeKBWarn != DefaultFreeKBWarn {
			t.Errorf("DUMP_FREE_KB_WARN=%q: FreeKBWarn = %d, want default %d", bad, cfg.FreeKBWarn, DefaultFreeKBWarn)
		}
		if len(warns) == 0 {
			t.Errorf("DUMP_FREE_KB_WARN=%q: expected a warning, got none", bad)
		}
	}
}

func TestLoadFreeKBZeroDisablesAndValidPassesThrough(t *testing.T) {
	zero, zwarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_FREE_KB_WARN": "0"})
	if zero.FreeKBWarn != 0 {
		t.Errorf("DUMP_FREE_KB_WARN=0: FreeKBWarn = %d, want 0 (disabled)", zero.FreeKBWarn)
	}
	if len(zwarns) != 0 {
		t.Errorf("DUMP_FREE_KB_WARN=0: want no warnings, got %v", zwarns)
	}

	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_FREE_KB_WARN": "2048"})
	if cfg.FreeKBWarn != 2048 {
		t.Errorf("DUMP_FREE_KB_WARN=2048: FreeKBWarn = %d, want 2048", cfg.FreeKBWarn)
	}
	if len(warns) != 0 {
		t.Errorf("DUMP_FREE_KB_WARN=2048: want no warnings, got %v", warns)
	}
}

func TestLoadShutdownGrace(t *testing.T) {
	def, dwarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u"})
	if def.ShutdownGrace != def.DumpTimeout+15*time.Second {
		t.Errorf("default ShutdownGrace = %s, want DumpTimeout+15s = %s", def.ShutdownGrace, def.DumpTimeout+15*time.Second)
	}
	if len(dwarns) != 0 {
		t.Errorf("default ShutdownGrace: want no warnings, got %v", dwarns)
	}

	bad, bwarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "SHUTDOWN_GRACE": "nope"})
	if bad.ShutdownGrace != bad.DumpTimeout+15*time.Second {
		t.Errorf("invalid SHUTDOWN_GRACE: ShutdownGrace = %s, want derived %s", bad.ShutdownGrace, bad.DumpTimeout+15*time.Second)
	}
	if len(bwarns) == 0 {
		t.Error("invalid SHUTDOWN_GRACE: expected a warning, got none")
	}

	ok, owarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "SHUTDOWN_GRACE": "600"})
	if ok.ShutdownGrace != 600*time.Second {
		t.Errorf("valid SHUTDOWN_GRACE=600: ShutdownGrace = %s, want 600s", ok.ShutdownGrace)
	}
	if len(owarns) != 0 {
		t.Errorf("valid SHUTDOWN_GRACE=600: want no warnings, got %v", owarns)
	}

	low, lwarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_TIMEOUT": "300", "SHUTDOWN_GRACE": "30"})
	if low.ShutdownGrace != 30*time.Second {
		t.Errorf("below-timeout SHUTDOWN_GRACE=30: ShutdownGrace = %s, want 30s (value still honored)", low.ShutdownGrace)
	}
	if len(lwarns) == 0 {
		t.Error("below-timeout SHUTDOWN_GRACE: expected a warning that an in-flight dump may be killed")
	}
}

func TestLoadDumpTimeout(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-5"} {
		cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_TIMEOUT": bad})
		if cfg.DumpTimeout != DefaultDumpTimeout {
			t.Errorf("DUMP_TIMEOUT=%q: DumpTimeout = %s, want default %s", bad, cfg.DumpTimeout, DefaultDumpTimeout)
		}
		if len(warns) == 0 {
			t.Errorf("DUMP_TIMEOUT=%q: expected a warning, got none", bad)
		}
	}

	boundary, bwarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_TIMEOUT": "10"})
	if boundary.DumpTimeout != MinDumpTimeout {
		t.Errorf("DUMP_TIMEOUT=10: DumpTimeout = %s, want MinDumpTimeout %s (boundary, not clamped)", boundary.DumpTimeout, MinDumpTimeout)
	}
	if len(bwarns) != 0 {
		t.Errorf("DUMP_TIMEOUT=10 (at minimum): want no warnings, got %v", bwarns)
	}

	ok, owarns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_TIMEOUT": "600"})
	if ok.DumpTimeout != 600*time.Second {
		t.Errorf("DUMP_TIMEOUT=600: DumpTimeout = %s, want 600s", ok.DumpTimeout)
	}
	if len(owarns) != 0 {
		t.Errorf("DUMP_TIMEOUT=600: want no warnings, got %v", owarns)
	}
}

func TestLoadDumpIntervalInvalidFallsBack(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_INTERVAL": "notaduration"})
	if cfg.DumpInterval != DefaultDumpInterval {
		t.Errorf("DUMP_INTERVAL=notaduration: DumpInterval = %s, want default %s", cfg.DumpInterval, DefaultDumpInterval)
	}
	if len(warns) == 0 {
		t.Error("DUMP_INTERVAL=notaduration: expected a warning, got none")
	}
}

// A parseable NEGATIVE duration disables the timer (returns 0) and must warn --
// distinct from "off"/"0" (silent 0) and an unparseable value (default 24h).
func TestLoadDumpIntervalNegativeDisablesWithWarning(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_INTERVAL": "-5m"})
	if cfg.DumpInterval != 0 {
		t.Fatalf("DUMP_INTERVAL=-5m: DumpInterval = %s, want 0 (negative disables the built-in timer)", cfg.DumpInterval)
	}
	if len(warns) == 0 {
		t.Fatal("DUMP_INTERVAL=-5m: expected a warning that the value is negative, got none")
	}
}

func TestLoadDumpDirValidPassesThrough(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_DIR": "/custom/dumps"})
	if cfg.DumpDir != "/custom/dumps" {
		t.Errorf("DUMP_DIR=/custom/dumps: DumpDir = %q, want /custom/dumps", cfg.DumpDir)
	}
	if len(warns) != 0 {
		t.Errorf("valid DUMP_DIR: want no warnings, got %v", warns)
	}
}

func TestLoadCustomListenAddrAndPGPassFile(t *testing.T) {
	cfg, warns := mustLoad(t, map[string]string{
		"DB_SPECS":    "h:db:u",
		"LISTEN_ADDR": "127.0.0.1:5555",
		"PGPASSFILE":  "/etc/custom/.pgpass",
	})
	if cfg.ListenAddr != "127.0.0.1:5555" {
		t.Errorf("LISTEN_ADDR: ListenAddr = %q, want 127.0.0.1:5555", cfg.ListenAddr)
	}
	if cfg.PGPassFile != "/etc/custom/.pgpass" {
		t.Errorf("PGPASSFILE: PGPassFile = %q, want /etc/custom/.pgpass", cfg.PGPassFile)
	}
	if len(warns) != 0 {
		t.Errorf("custom addr/pgpass: want no warnings, got %v", warns)
	}
}

func TestLoadPositiveIntWarningNamesVariable(t *testing.T) {
	// loadPositiveInt backs both DUMP_CONCURRENCY and DUMP_KEEP; the only thing
	// distinguishing their fallback warnings is the name threaded from the call
	// site. Pin that each warning names its own variable and renders the matching
	// default (2 for concurrency, 7 for keep), so a swapped name argument is caught.
	_, cw := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_CONCURRENCY": "bad"})
	wantC := Warning(`DUMP_CONCURRENCY "bad" is not a positive integer; using default 2`)
	if len(cw) != 1 || cw[0] != wantC {
		t.Errorf("DUMP_CONCURRENCY=bad warnings = %v, want exactly [%q]", cw, wantC)
	}

	_, kw := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_KEEP": "bad"})
	wantK := Warning(`DUMP_KEEP "bad" is not a positive integer; using default 7`)
	if len(kw) != 1 || kw[0] != wantK {
		t.Errorf("DUMP_KEEP=bad warnings = %v, want exactly [%q]", kw, wantK)
	}
}

func TestLoadShutdownGraceZeroFallsBackToDerived(t *testing.T) {
	// SHUTDOWN_GRACE="0" is non-positive: it must fall back to the derived
	// DumpTimeout+15s grace with a warning, not be honored as a zero-second
	// drain budget (a 0 grace expires the shutdown drain context immediately
	// and skips graceful drain).
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "SHUTDOWN_GRACE": "0"})
	if cfg.ShutdownGrace != cfg.DumpTimeout+15*time.Second {
		t.Errorf("SHUTDOWN_GRACE=0: ShutdownGrace = %s, want derived %s", cfg.ShutdownGrace, cfg.DumpTimeout+15*time.Second)
	}
	if len(warns) == 0 {
		t.Error("SHUTDOWN_GRACE=0: expected a warning that the value is not a positive integer, got none")
	}
}

func TestLoadShutdownGraceEqualToTimeoutNoWarning(t *testing.T) {
	// At grace == DumpTimeout the dump keeps its full budget on shutdown, so the
	// "below DUMP_TIMEOUT" warning must fire only when grace is STRICTLY less.
	cfg, warns := mustLoad(t, map[string]string{"DB_SPECS": "h:db:u", "DUMP_TIMEOUT": "300", "SHUTDOWN_GRACE": "300"})
	if cfg.ShutdownGrace != 300*time.Second {
		t.Errorf("SHUTDOWN_GRACE=300 (== DUMP_TIMEOUT): ShutdownGrace = %s, want 300s", cfg.ShutdownGrace)
	}
	if len(warns) != 0 {
		t.Errorf("SHUTDOWN_GRACE == DUMP_TIMEOUT: want no warnings, got %v", warns)
	}
}

// ListenerOpenAndPublic is true only when the endpoint is unauthenticated
// (AUTH_TOKEN empty) AND bound to a non-loopback address. A set token, or a
// loopback/localhost bind, makes it false. main turns a true result into a
// one-line startup WARN.
func TestListenerOpenAndPublic(t *testing.T) {
	tests := []struct {
		name      string
		authToken string
		listen    string
		want      bool
	}{
		{"open wildcard", "", ":9847", true},
		{"open 0.0.0.0", "", "0.0.0.0:9847", true},
		{"open ipv6 wildcard", "", "[::]:9847", true},
		{"open specific non-loopback", "", "192.168.1.5:9847", true},
		{"open loopback v4", "", "127.0.0.1:9847", false},
		{"open loopback v6", "", "[::1]:9847", false},
		{"open localhost", "", "localhost:9847", false},
		{"open portless addr (unparseable -> fail-safe public)", "", "justahost", true},
		{"open non-localhost named host", "", "db.internal:9847", true},
		{"token set, portless addr", "secret", "justahost", false},
		{"token set, wildcard", "secret", ":9847", false},
		{"token set, public", "secret", "0.0.0.0:9847", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ListenerOpenAndPublic(tt.authToken, tt.listen); got != tt.want {
				t.Errorf("ListenerOpenAndPublic(%q, %q) = %v, want %v", tt.authToken, tt.listen, got, tt.want)
			}
		})
	}
}
