package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v3"
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
		// resolve to loopback, so dialing them is correct, and preserving an
		// IPv6-only-loopback host is the whole point of the fix.
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

// triggerTimeout is pure (cfg -> duration) but lives in main.go (the
// untestable composition root) and has no test. Pin its wave math so a
// regression in the ceil-division, the max(concurrency,1) guard, the
// min(probeCap, DumpTimeout) selection, or the coalesced-cycle multiplier is
// caught: an off-by-one wave or a flipped min/max makes `trigger` either
// falsely time out a real multi-database dump (exit 1 while the dump still
// succeeds) or wait far too long. Values traced against
// internal/dump.dumpOne's per-DB ceiling (min(dump.ProbeTimeoutCap,
// DumpTimeout) probe + DumpTimeout dump), the pool's clamp(concurrency,1,len)
// wave count, and the (1 + cycleQueueCapacity) cycles the handler may run
// before responding (its own cycle plus queued rerun demand).
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
		// 0 specs still bills one wave so the trigger waits out the handler's
		// own work: 1 wave * 2 cycles * (300s+min(10s,300s)) + 30s slack.
		{"no specs floors at one wave", 300 * time.Second, 2, 0, 650 * time.Second},
		// ceil(2/2)=1 wave: 1*2*310s + 30s.
		{"specs fit one wave", 300 * time.Second, 2, 2, 650 * time.Second},
		// ceil(3/2)=2 waves: 2*2*310s + 30s.
		{"specs span two waves", 300 * time.Second, 2, 3, 1270 * time.Second},
		// concurrency<1 is coerced to 1, so 3 specs => 3 waves: 3*2*310s + 30s.
		{"zero concurrency coerced to one", 300 * time.Second, 0, 3, 1890 * time.Second},
		// DumpTimeout below the probe cap selects DumpTimeout for the probe:
		// perDB = 5s + min(10s,5s) = 10s; 1*2*10s + 30s.
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

// A fatal configuration (a DUMP_DIR with a ".." component) aborts startup with
// exit code 1 before anything runs, rather than silently relocating backups to
// the default directory. The serve, run, and trigger subcommands all load
// config, so all three must refuse to start.
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

// exitForRun maps the one-shot cycle outcome to the process exit code. The
// contract: 0 iff the invocation's own run was fully ok OR its demand was
// queued/discarded for the active runner; a gated start (shutdown first) and
// an infrastructure failure that ran nothing exit 1; a coordination error
// AFTER a successful run does not fail the run.
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
