package config

import (
	"testing"
	"time"
)

// envFunc builds a getenv from a map for injection into Load.
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, warns := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u"}))
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
	cfg, warns := Load(envFunc(map[string]string{}))
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
	cfg, _ := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u", "PGPASSWORD": "s3cr3t"}))
	for _, v := range []string{cfg.ListenAddr, cfg.DumpDir, cfg.PGPassFile, cfg.AuthToken} {
		if v == "s3cr3t" {
			t.Fatal("PGPASSWORD leaked into a Config field")
		}
	}
}

// A DUMP_DIR with ".." is rejected gracefully: the value is dropped, the
// default is used, and a warning records the fallback (no startup abort).
func TestLoadDumpDirTraversalFallsBack(t *testing.T) {
	cfg, warns := Load(envFunc(map[string]string{"DUMP_DIR": "/dumps/../etc"}))
	if cfg.DumpDir != DefaultDumpDir {
		t.Fatalf("DumpDir = %q, want fallback to %q", cfg.DumpDir, DefaultDumpDir)
	}
	if len(warns) == 0 {
		t.Fatal("expected a warning for the rejected DUMP_DIR")
	}
}

// A control character in one DB_SPECS token does not abort startup: that token
// becomes an Invalid spec (reported per-DB later) while valid tokens parse.
func TestLoadControlCharSpecIsPerSpec(t *testing.T) {
	cfg, _ := Load(envFunc(map[string]string{"DB_SPECS": "good-host:db:user h:db:u\x01"}))
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
	cfg, _ := Load(envFunc(map[string]string{}))
	if len(cfg.Specs) != 0 {
		t.Fatalf("expected no specs, got %d", len(cfg.Specs))
	}
}

func TestLoadClampsAndWarns(t *testing.T) {
	cfg, warns := Load(envFunc(map[string]string{
		"DB_SPECS":         "h:db:u",
		"DUMP_TIMEOUT":     "3", // below MinDumpTimeout
		"DUMP_CONCURRENCY": "abc",
	}))
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
	def, _ := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u"}))
	if def.DumpInterval != DefaultDumpInterval {
		t.Fatalf("default DumpInterval = %s, want %s", def.DumpInterval, DefaultDumpInterval)
	}

	cfg, _ := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u", "DUMP_INTERVAL": "6h"}))
	if cfg.DumpInterval != 6*time.Hour {
		t.Fatalf("DumpInterval = %s, want 6h", cfg.DumpInterval)
	}

	// Every disable sentinel maps to 0 (built-in timer off), matching the
	// sibling schedulers.
	for _, off := range []string{"off", "disabled", "0", "0s", "OFF"} {
		got, _ := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u", "DUMP_INTERVAL": off}))
		if got.DumpInterval != 0 {
			t.Fatalf("DUMP_INTERVAL=%q: DumpInterval = %s, want 0 (disabled)", off, got.DumpInterval)
		}
	}
}

func TestLoadDumpKeep(t *testing.T) {
	// Default (unset): keep a rolling window.
	def, _ := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u"}))
	if def.DumpKeep != DefaultDumpKeep {
		t.Fatalf("default DumpKeep = %d, want %d", def.DumpKeep, DefaultDumpKeep)
	}

	cfg, _ := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u", "DUMP_KEEP": "5"}))
	if cfg.DumpKeep != 5 {
		t.Fatalf("DumpKeep = %d, want 5", cfg.DumpKeep)
	}

	// Invalid values fall back to the default with a warning.
	for _, bad := range []string{"0", "-1", "two", "1.5"} {
		got, warns := Load(envFunc(map[string]string{"DB_SPECS": "h:db:u", "DUMP_KEEP": bad}))
		if got.DumpKeep != DefaultDumpKeep {
			t.Fatalf("DUMP_KEEP=%q: DumpKeep = %d, want default %d", bad, got.DumpKeep, DefaultDumpKeep)
		}
		if len(warns) == 0 {
			t.Fatalf("DUMP_KEEP=%q: expected a warning", bad)
		}
	}
}
