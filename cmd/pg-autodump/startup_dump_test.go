package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/httpapi"
	"github.com/cplieger/pg-autodump/internal/spec"
	"github.com/cplieger/scheduler/v4"
)

// tickerPG is an always-successful dump.PGTool counting dump calls, so a test
// can assert whether the startup dump ran without parsing logs.
type tickerPG struct{ dumps atomic.Int32 }

func (p *tickerPG) Probe(context.Context, dump.Conn) (int, dump.FailKind, error) {
	return 18, dump.FailNone, nil
}

func (p *tickerPG) Dump(_ context.Context, _ dump.Conn, w io.Writer) (int, string, error) {
	p.dumps.Add(1)
	_, _ = io.WriteString(w, "PGDMP-fake")
	return 0, "", nil
}

func (p *tickerPG) VerifyTOC(context.Context, string) error { return nil }

// syncBuffer lets the test poll log output while runTicker writes it from
// its own goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// tickerFixture wires a real Trigger (guard + cycle lock + orchestrator) over
// a fake PGTool dumping into dir, logging into buf.
func tickerFixture(t *testing.T, dir string, buf *syncBuffer) (*httpapi.Trigger, *tickerPG, *slog.Logger) {
	t.Helper()
	pgf := &tickerPG{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	orch := dump.New(&dump.Params{
		PG:          pgf,
		Logger:      log,
		DumpDir:     dir,
		Specs:       []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}},
		DumpTimeout: 30 * time.Second,
		Concurrency: 1,
		Keep:        1,
	})
	cycle := scheduler.NewExclusive(t.TempDir(), log)
	return httpapi.NewTrigger(&dump.Guard{}, cycle, orch, log), pgf, log
}

// startTicker runs runTicker in a goroutine with a 24h interval (so no tick
// fires during the test) and returns a stop func that cancels and joins it.
func startTicker(t *testing.T, stamp *scheduler.Stamp, trig *httpapi.Trigger, log *slog.Logger) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTicker(ctx, stamp, 24*time.Hour, trig, log)
	}()
	return func() {
		cancel()
		<-done
	}
}

// waitFor polls cond with a deadline so a broken startup decision fails with
// a diagnostic instead of hanging.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedRecord writes a last-run record in the stamp's documented on-disk form.
func seedRecord(t *testing.T, dir string, at time.Time, outcome string) {
	t.Helper()
	line := at.UTC().Format(time.RFC3339Nano) + " " + outcome + "\n"
	if err := os.WriteFile(dump.StampPath(dir), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With no record (first boot, or an unpersisted volume) the startup dump
// fires, and the completed cycle writes a successful record.
func TestRunTickerFiresStartupDumpWithNoRecord(t *testing.T) {
	dir := t.TempDir()
	var buf syncBuffer
	trig, pgf, log := tickerFixture(t, dir, &buf)
	stamp := scheduler.NewStamp(dump.StampPath(dir))

	stop := startTicker(t, stamp, trig, log)
	waitFor(t, func() bool {
		rec, known := stamp.Last()
		return known && rec.OK
	}, "the startup cycle to record a successful run")
	stop()

	if got := pgf.dumps.Load(); got != 1 {
		t.Errorf("dump calls = %d, want 1 (startup dump exactly once)", got)
	}
	if !strings.Contains(buf.String(), "startup dump complete") {
		t.Errorf("log = %q, want a 'startup dump complete' line", buf.String())
	}
}

// A record older than one interval no longer answers the freshness question:
// the startup dump fires and refreshes it.
func TestRunTickerFiresStartupDumpWithStaleRecord(t *testing.T) {
	dir := t.TempDir()
	seeded := time.Now().Add(-48 * time.Hour)
	seedRecord(t, dir, seeded, "ok")
	var buf syncBuffer
	trig, pgf, log := tickerFixture(t, dir, &buf)
	stamp := scheduler.NewStamp(dump.StampPath(dir))

	stop := startTicker(t, stamp, trig, log)
	waitFor(t, func() bool {
		rec, known := stamp.Last()
		return known && rec.Time.After(seeded.Add(time.Hour))
	}, "the startup cycle to replace the stale record")
	stop()

	if got := pgf.dumps.Load(); got != 1 {
		t.Errorf("dump calls = %d, want 1 (a 48h-old record with a 24h interval is due)", got)
	}
}

// A fresh fully-successful record inherited from the previous container
// suppresses the startup dump, and the skip is logged with the inherited
// record time.
func TestRunTickerSkipsStartupDumpAfterFreshSuccess(t *testing.T) {
	dir := t.TempDir()
	stamp := scheduler.NewStamp(dump.StampPath(dir))
	if err := stamp.Record(true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dump.StampPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var buf syncBuffer
	trig, pgf, log := tickerFixture(t, dir, &buf)

	stop := startTicker(t, stamp, trig, log)
	waitFor(t, func() bool {
		return strings.Contains(buf.String(), "startup dump skipped; the last cycle fully succeeded within one interval")
	}, "the startup-skip log line")
	stop()

	if got := pgf.dumps.Load(); got != 0 {
		t.Errorf("dump calls = %d, want 0 (a fresh successful record suppresses the startup dump)", got)
	}
	if !strings.Contains(buf.String(), "last_success=") {
		t.Errorf("skip line lacks the inherited record time; log = %q", buf.String())
	}
	after, err := os.ReadFile(dump.StampPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("record changed from %q to %q; a skipped startup must not rewrite it", before, after)
	}
}

// A fresh but FAILED record does not suppress the startup dump: a partially
// failed cycle leaves fresh dump files behind, but the boot retries the whole
// cycle until one fully succeeds.
func TestRunTickerFiresStartupDumpAfterFailedCycleRecord(t *testing.T) {
	dir := t.TempDir()
	stamp := scheduler.NewStamp(dump.StampPath(dir))
	if err := stamp.Record(false); err != nil {
		t.Fatal(err)
	}
	var buf syncBuffer
	trig, pgf, log := tickerFixture(t, dir, &buf)

	stop := startTicker(t, stamp, trig, log)
	waitFor(t, func() bool {
		rec, known := stamp.Last()
		return known && rec.OK
	}, "the startup cycle to replace the failed record with a successful one")
	stop()

	if got := pgf.dumps.Load(); got != 1 {
		t.Errorf("dump calls = %d, want 1 (a fresh failed record must not suppress the retry)", got)
	}
	if !strings.Contains(buf.String(), "startup dump complete") {
		t.Errorf("log = %q, want a 'startup dump complete' line", buf.String())
	}
}
