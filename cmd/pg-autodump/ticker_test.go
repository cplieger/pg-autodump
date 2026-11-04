package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/httpapi"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v4"
)

// cycleTrigger wires a real Trigger over cycle, so a test can close its gate
// or hold its lock, with a fake PGTool dumping into a temp dir and logging
// into buf.
func cycleTrigger(t *testing.T, buf *syncBuffer, cycle func(*slog.Logger) *scheduler.Exclusive) (*httpapi.Trigger, *tickerPG, *slog.Logger) {
	t.Helper()
	pgf := &tickerPG{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	orch := dump.New(&dump.Params{
		PG:          pgf,
		Logger:      log,
		DumpDir:     t.TempDir(),
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        1,
	})
	return httpapi.NewTrigger(&dump.Guard{}, cycle(log), orch, log), pgf, log
}

// closedGate is the cycle coordinator once shutdown is signalled: its gate
// refuses every run start.
func closedGate(t *testing.T) func(*slog.Logger) *scheduler.Exclusive {
	dir := t.TempDir()
	return func(log *slog.Logger) *scheduler.Exclusive {
		return scheduler.NewExclusive(dir, log, scheduler.WithGate(func() bool { return false }))
	}
}

// runTickerUntil runs runTicker until the log holds want, then cancels it and
// waits for it to return.
func runTickerUntil(t *testing.T, rec startupRecord, trig *httpapi.Trigger, log *slog.Logger, buf *syncBuffer, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTicker(ctx, rec, 24*time.Hour, trig, log)
	}()
	waitFor(t, func() bool { return strings.Contains(buf.String(), want) }, "a "+want+" line")
	cancel()
	<-done
}

// A startup dump the closed shutdown gate refuses ran nothing because the
// server is stopping; reporting it as a run already in progress names a cause
// that does not exist.
func TestRunTicker_GatedStartupDumpLogsShutdown(t *testing.T) {
	var buf syncBuffer
	trig, pgf, log := cycleTrigger(t, &buf, closedGate(t))

	runTickerUntil(t, startupRecord{}, trig, log, &buf, `msg="startup dump`)

	out := buf.String()
	if !strings.Contains(out, "startup dump skipped; shutting down") {
		t.Errorf("log = %q, want a 'startup dump skipped; shutting down' line", out)
	}
	if strings.Contains(out, "already in progress") {
		t.Errorf("log = %q, want no 'already in progress' line: no other run held the guard or the cycle lock", out)
	}
	if got := pgf.dumps.Load(); got != 0 {
		t.Errorf("dump calls = %d, want 0 (the gate refuses the run)", got)
	}
}

// A scheduled tick the closed shutdown gate refuses is reported as shutdown,
// not as a run already in progress.
func TestRunTicker_GatedScheduledTickLogsShutdown(t *testing.T) {
	var buf syncBuffer
	trig, pgf, log := cycleTrigger(t, &buf, closedGate(t))
	fresh := startupRecord{known: true, remaining: time.Millisecond}

	runTickerUntil(t, fresh, trig, log, &buf, `msg="scheduled dump`)

	out := buf.String()
	if !strings.Contains(out, "scheduled dump skipped; shutting down") {
		t.Errorf("log = %q, want a 'scheduled dump skipped; shutting down' line", out)
	}
	if strings.Contains(out, "already in progress") {
		t.Errorf("log = %q, want no 'already in progress' line: no other run held the guard or the cycle lock", out)
	}
	if got := pgf.dumps.Load(); got != 0 {
		t.Errorf("dump calls = %d, want 0 (the gate refuses the run)", got)
	}
}

// A tick that finds the cycle lock held by another process (an exec'd
// `pg-autodump run`) is skipped as a run already in progress.
func TestRunTicker_ScheduledTickBehindAForeignCycleLogsInProgress(t *testing.T) {
	cycleDir := t.TempDir()
	lock, ok, err := scheduler.TryLock(filepath.Join(cycleDir, scheduler.ExclusiveLockName))
	if err != nil || !ok {
		t.Fatalf("TryLock(cycle.lock) = ok=%v err=%v, want held", ok, err)
	}
	defer lock.Unlock()
	var buf syncBuffer
	trig, pgf, log := cycleTrigger(t, &buf, func(log *slog.Logger) *scheduler.Exclusive {
		return scheduler.NewExclusive(cycleDir, log)
	})
	fresh := startupRecord{known: true, remaining: time.Millisecond}

	runTickerUntil(t, fresh, trig, log, &buf, `msg="scheduled dump`)

	out := buf.String()
	if !strings.Contains(out, "scheduled dump skipped; a run is already in progress") {
		t.Errorf("log = %q, want a 'scheduled dump skipped; a run is already in progress' line", out)
	}
	if strings.Contains(out, "shutting down") {
		t.Errorf("log = %q, want no 'shutting down' line: the gate is open", out)
	}
	if got := pgf.dumps.Load(); got != 0 {
		t.Errorf("dump calls = %d, want 0 (another process holds the cycle lock)", got)
	}
}
