package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v4"
)

func TestLocalAddr(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		want   string
	}{
		{"port_only_default", ":9847", "127.0.0.1:9847"},
		{"wildcard_v4", "0.0.0.0:9847", "127.0.0.1:9847"},
		{"wildcard_v6", "[::]:9847", "127.0.0.1:9847"},
		// An explicit host is preserved verbatim: localhost and ::1 both
		// resolve to loopback, and preserving an IPv6-only-loopback host is
		// the point of the fix.
		{"host_and_port", "localhost:5432", "localhost:5432"},
		{"bare_port_no_colon", "9847", "127.0.0.1:9847"},
		{"loopback_v6_only", "[::1]:9847", "[::1]:9847"},
		{"loopback_v4_explicit", "127.0.0.1:9847", "127.0.0.1:9847"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := localAddr(tc.listen)
			if got != tc.want {
				t.Errorf("localAddr(%q) = %q, want %q", tc.listen, got, tc.want)
			}
		})
	}
}

// triggerTimeout is pure but lives in main.go and has no direct test. Values
// traced against internal/dump.dumpOne's per-DB ceiling (min(ProbeTimeoutCap,
// DumpTimeout) probe + DumpTimeout dump), the pool's clamp(concurrency,1,len)
// wave count, and the (1 + cycleQueueCapacity) cycles the handler may run.
func TestTriggerTimeout(t *testing.T) {
	specs := func(n int) []spec.DBSpec {
		s := make([]spec.DBSpec, n)
		return s
	}
	cases := []struct {
		name        string
		dumpTimeout time.Duration
		concurrency int
		nSpecs      int
		want        time.Duration
	}{
		// 0 specs still bills one wave: 1 wave * 2 cycles * (300s+min(10s,300s)) + 30s.
		{"no specs floors at one wave", 300 * time.Second, 2, 0, 650 * time.Second},
		// ceil(2/2)=1 wave.
		{"specs fit one wave", 300 * time.Second, 2, 2, 650 * time.Second},
		// ceil(3/2)=2 waves.
		{"specs span two waves", 300 * time.Second, 2, 3, 1270 * time.Second},
		// concurrency<1 coerced to 1: 3 specs => 3 waves.
		{"zero concurrency coerced to one", 300 * time.Second, 0, 3, 1890 * time.Second},
		// DumpTimeout below the probe cap selects DumpTimeout for the probe.
		{"dump timeout below probe cap", 5 * time.Second, 2, 1, 50 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{DumpTimeout: tc.dumpTimeout, DumpConcurrency: tc.concurrency, Specs: specs(tc.nSpecs)}
			if got := triggerTimeout(&cfg); got != tc.want {
				t.Errorf("triggerTimeout(dumpTimeout=%s, concurrency=%d, specs=%d) = %s, want %s",
					tc.dumpTimeout, tc.concurrency, tc.nSpecs, got, tc.want)
			}
		})
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	got := run([]string{"pg-autodump", "bogus"}, func(string) string { return "" })
	if got != 2 {
		t.Errorf("run with unknown subcommand = %d, want 2", got)
	}
}

// A fatal DUMP_DIR (a ".." component) aborts startup with exit code 1 for
// all three subcommands, rather than silently relocating backups to the
// default directory.
func TestRunAbortsOnFatalDumpDir(t *testing.T) {
	env := func(k string) string {
		if k == "DUMP_DIR" {
			return "/dumps/../etc"
		}
		return ""
	}
	for _, sub := range []string{"serve", "run", "trigger"} {
		if got := run([]string{"pg-autodump", sub}, env); got != 1 {
			t.Errorf("run %q with traversal DUMP_DIR = %d, want 1", sub, got)
		}
	}
}

// exitForRun maps the one-shot cycle outcome to the process exit code: 0 iff
// the invocation's own run was fully ok OR its demand was queued/discarded
// for the active runner; a gated start or an infrastructure failure exits 1;
// a coordination error after a successful run does not fail the run.
func TestExitForRun(t *testing.T) {
	okRes := []dump.Result{{Reason: dump.ReasonOK}, {Reason: dump.ReasonOK}}
	mixedRes := []dump.Result{{Reason: dump.ReasonOK}, {Reason: dump.ReasonPGError}}
	infraErr := errors.New("queue file unusable")

	cases := []struct {
		name    string
		outcome scheduler.Outcome
		exErr   error
		results []dump.Result
		ran     bool
		want    int
	}{
		{"ran all ok", scheduler.OutcomeRan, nil, okRes, true, 0},
		{"ran one failed", scheduler.OutcomeRan, nil, mixedRes, true, 1},
		{"ran plus queued rerun all ok", scheduler.OutcomeRanQueued, nil, okRes, true, 0},
		{"ran ok with late coordination error", scheduler.OutcomeRan, infraErr, okRes, true, 0},
		{"queued behind in-flight cycle", scheduler.OutcomeQueued, nil, nil, false, 0},
		{"queued with re-probe error still success", scheduler.OutcomeQueued, infraErr, nil, false, 0},
		{"discarded (queue full) still success", scheduler.OutcomeDiscarded, nil, nil, false, 0},
		{"gated by shutdown", scheduler.OutcomeGated, nil, nil, false, 1},
		{"infrastructure failure ran nothing", scheduler.OutcomeNone, infraErr, nil, false, 1},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitForRun(tc.outcome, tc.exErr, tc.results, tc.ran, log); got != tc.want {
				t.Errorf("exitForRun(%v, %v, ran=%v) = %d, want %d",
					tc.outcome, tc.exErr, tc.ran, got, tc.want)
			}
		})
	}
}

// requireSetgidWidensMkdir makes the parent setgid and proves the kernel
// widens a mkdir's mode underneath it, skipping the caller otherwise: without
// this witness, a test cannot distinguish a verified create from an
// unverified one on a filesystem that honours every mode request.
func requireSetgidWidensMkdir(t *testing.T, parent string) {
	t.Helper()
	if err := os.Chmod(parent, 0o700|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(parent, "witness")
	if err := os.Mkdir(witness, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(witness)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Skipf("kernel did not widen a 0o700 mkdir under a setgid parent (got %v); "+
			"this test cannot distinguish a verified create from an unverified one here", fi.Mode())
	}
}

// The cycle directory holds the flock serializing the resident server against
// an exec'd `pg-autodump run`, so its mode must be owner-only as MEASURED,
// not merely requested — os.MkdirAll never checks what the filesystem stored.
func TestEnsureCycleDirVerifiesTheModeItCreated(t *testing.T) {
	parent := t.TempDir()
	requireSetgidWidensMkdir(t, parent)

	dir := filepath.Join(parent, "pg-autodump")
	if err := ensureCycleDir(dir, discardLogger()); err != nil {
		t.Fatalf("ensureCycleDir: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode(); got != os.ModeDir|0o700 {
		t.Fatalf("created dir mode = %v, want %v: the mode it created was not verified",
			got, os.ModeDir|0o700)
	}
}

// A symlink planted at the cycle-directory path is refused, not followed:
// os.MkdirAll would resolve it and relocate the cycle lock onto a file the
// planter controls.
func TestEnsureCycleDirRefusesSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "pg-autodump")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureCycleDir(link, discardLogger()); err == nil {
		t.Fatal("symlinked cycle dir accepted; want refusal")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
