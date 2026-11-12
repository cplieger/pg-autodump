package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// fakeDump is the archive fakePG writes; its length is the dump size the
// page must report.
const fakeDump = "PGDMP-fake"

// fakePG answers every database but those on host "down", whose probe is
// refused.
type fakePG struct{}

func (fakePG) Probe(_ context.Context, c dump.Conn) (int, dump.FailKind, error) {
	if c.Host == "down" {
		return 0, dump.FailConnect, nil
	}
	return 18, dump.FailNone, nil
}

func (fakePG) Dump(_ context.Context, _ dump.Conn, w io.Writer) (int, string, error) {
	_, _ = io.WriteString(w, fakeDump)
	return 0, "", nil
}

func (fakePG) VerifyTOC(context.Context, string) error { return nil }

// runCycle runs one cycle over specs through the real orchestrator, recording
// into m.
func runCycle(ctx context.Context, t *testing.T, dumpDir string, specs []spec.DBSpec, m *Metrics) {
	t.Helper()
	orch := dump.New(&dump.Params{
		PG:          fakePG{},
		Logger:      slog.New(slog.DiscardHandler),
		Metrics:     m,
		DumpDir:     dumpDir,
		Specs:       specs,
		DumpTimeout: 30 * time.Second,
		Concurrency: 2,
		Keep:        1,
	})
	orch.Run(ctx)
}

// series reads every sample on reg, keyed name{label values joined by ","}.
func series(t *testing.T, reg *measure.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string]float64)
	for _, f := range families {
		for _, s := range f.Samples {
			vals := make([]string, 0, len(s.Labels))
			for _, l := range s.Labels {
				vals = append(vals, l.Value)
			}
			out[f.Name+"{"+strings.Join(vals, ",")+"}"] = s.Value
		}
	}
	return out
}

func key(name string, labels ...string) string {
	return metricsPrefix + "_" + name + "{" + strings.Join(labels, ",") + "}"
}

func checkSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		g, ok := got[k]
		switch {
		case !ok:
			t.Errorf("%s is not on the page", k)
		case g != v:
			t.Errorf("%s = %v, want %v", k, g, v)
		}
	}
}

// A cycle in which one database fails sets that database's last-result gauge
// to 0 and leaves the others at 1, counts each result under its own reason,
// and records the cycle's time and its failed verdict.
func TestOneFailingDatabaseLeavesTheOthersOK(t *testing.T) {
	specs := spec.Parse("h1:5432:app:u down:5432:app:u h2:5433:billing:u")
	reg, m := NewRegistry(t.TempDir(), specs, Boot{})
	before := time.Now()

	runCycle(t.Context(), t, t.TempDir(), specs, m)

	got := series(t, reg)
	checkSeries(t, got, map[string]float64{
		key("dump_ok", "h1_5432", "app"):                        1,
		key("dump_ok", "down_5432", "app"):                      0,
		key("dump_ok", "h2_5433", "billing"):                    1,
		key("dumps_total", "h1_5432", "app", "ok"):              1,
		key("dumps_total", "down_5432", "app", "connect_error"): 1,
		key("dumps_total", "down_5432", "app", "ok"):            0,
		key("dumps_total", "h2_5433", "billing", "ok"):          1,
		key("dump_bytes", "h1_5432", "app"):                     float64(len(fakeDump)),
		key("dump_bytes", "down_5432", "app"):                   0,
		key("last_cycle_ok", ""):                                0,
	})
	if at := got[key("last_cycle_timestamp_seconds", "")]; at < float64(before.Unix()) {
		t.Errorf("last_cycle_timestamp_seconds = %v, want the cycle's end, at or after %d", at, before.Unix())
	}
	if _, ok := got[key("dump_duration_seconds", "down_5432", "app")]; !ok {
		t.Error("dump_duration_seconds has no series for the failed database")
	}

	runCycle(t.Context(), t, t.TempDir(), specs[:1], m)
	if got := series(t, reg)[key("last_cycle_ok", "")]; got != 1 {
		t.Errorf("last_cycle_ok after a fully successful cycle = %v, want 1", got)
	}
}

// A DB_SPECS entry that repeats an earlier one fails the cycle, so its
// database reads 0 for as long as the cycle reports failed, whatever order
// the two results arrive in.
func TestADuplicateEntryKeepsItsDatabaseFailed(t *testing.T) {
	specs := spec.Parse("h1:5432:app:u h1:5432:app:u")
	reg, m := NewRegistry(t.TempDir(), specs, Boot{})

	runCycle(t.Context(), t, t.TempDir(), specs, m)

	checkSeries(t, series(t, reg), map[string]float64{
		key("dump_ok", "h1_5432", "app"):                  0,
		key("dumps_total", "h1_5432", "app", "ok"):        1,
		key("dumps_total", "h1_5432", "app", "duplicate"): 1,
		key("dump_bytes", "h1_5432", "app"):               float64(len(fakeDump)),
	})
}

// Every configured entry has its series before any cycle: each result counted
// at 0, and its last result read from the last-run record, so a restart
// neither fires a failure for every database nor resolves a failure the
// record still holds.
func TestEverySeriesIsSeededBeforeTheFirstCycle(t *testing.T) {
	specs := spec.Parse("h1:5432:app:u not-a-spec")
	last := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		rec    scheduler.RunRecord
		known  bool
		wantOK float64
	}{
		{name: "no_record", wantOK: 1},
		{name: "successful_record", rec: scheduler.RunRecord{Time: last, OK: true}, known: true, wantOK: 1},
		{name: "failed_record", rec: scheduler.RunRecord{Time: last, OK: false}, known: true, wantOK: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := NewRegistry(t.TempDir(), specs, Boot{Last: tc.rec, Known: tc.known})
			got := series(t, reg)
			for _, db := range []struct{ server, name string }{{"h1_5432", "app"}, {"_5432", "not-a-spec"}} {
				for _, r := range dump.Reasons() {
					checkSeries(t, got, map[string]float64{key("dumps_total", db.server, db.name, string(r)): 0})
				}
				checkSeries(t, got, map[string]float64{
					key("dump_ok", db.server, db.name):               tc.wantOK,
					key("dump_bytes", db.server, db.name):            0,
					key("dump_duration_seconds", db.server, db.name): 0,
				})
			}
			wantTime, wantCycleOK := 0.0, 0.0
			if tc.known {
				wantTime = float64(last.Unix())
				if tc.rec.OK {
					wantCycleOK = 1
				}
			}
			checkSeries(t, got, map[string]float64{
				key("last_cycle_timestamp_seconds", ""): wantTime,
				key("last_cycle_ok", ""):                wantCycleOK,
			})
		})
	}
}

// A cycle shutdown cut short counts its results but leaves every gauge as the
// previous cycle set it, as it leaves the health verdict and the last-run
// record.
func TestACycleCutShortCountsButSetsNoVerdict(t *testing.T) {
	specs := spec.Parse("h1:5432:app:u")
	reg, m := NewRegistry(t.TempDir(), specs, Boot{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	runCycle(ctx, t, t.TempDir(), specs, m)

	checkSeries(t, series(t, reg), map[string]float64{
		key("dumps_total", "h1_5432", "app", "skipped"): 1,
		key("dump_ok", "h1_5432", "app"):                1,
		key("last_cycle_timestamp_seconds", ""):         0,
	})
}

// The cycle deadline at start counts the gap from the last success on record,
// or from the start when the record shows none, and reads 0 with the built-in
// timer off.
func TestCycleDeadlineAtStart(t *testing.T) {
	specs := spec.Parse("h1:5432:app:u")
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	last := time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC)
	const gap = 49 * time.Hour
	cases := []struct {
		name string
		boot Boot
		want time.Time
	}{
		{name: "no_record", boot: Boot{Start: start, Gap: gap}, want: start.Add(gap)},
		{name: "successful_record", boot: Boot{Last: scheduler.RunRecord{Time: last, OK: true}, Known: true, Start: start, Gap: gap}, want: last.Add(gap)},
		{name: "failed_record", boot: Boot{Last: scheduler.RunRecord{Time: last}, Known: true, Start: start, Gap: gap}, want: start.Add(gap)},
		{name: "timer_off", boot: Boot{Last: scheduler.RunRecord{Time: last, OK: true}, Known: true, Start: start}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := NewRegistry(t.TempDir(), specs, tc.boot)
			want := 0.0
			if !tc.want.IsZero() {
				want = float64(tc.want.Unix())
			}
			checkSeries(t, series(t, reg), map[string]float64{key("cycle_deadline_timestamp_seconds", ""): want})
		})
	}
}

// A fully successful cycle moves the deadline to its end plus the gap; a
// failed cycle leaves it, so a timer whose cycles keep failing crosses it.
func TestCycleDeadlineMovesOnlyOnASuccess(t *testing.T) {
	const gap = 49 * time.Hour
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	bootDeadline := float64(start.Add(gap).Unix())
	dumpDir := t.TempDir()
	good := spec.Parse("h1:5432:app:u")
	failed := spec.Parse("h1:5432:app:u down:5432:app:u")
	reg, m := NewRegistry(dumpDir, failed, Boot{Start: start, Gap: gap})

	runCycle(t.Context(), t, dumpDir, failed, m)
	if got := series(t, reg)[key("cycle_deadline_timestamp_seconds", "")]; got != bootDeadline {
		t.Errorf("deadline after a failed cycle = %v, want the start's %v", got, bootDeadline)
	}

	before := time.Now()
	runCycle(t.Context(), t, dumpDir, good, m)
	after := time.Now()
	got := series(t, reg)[key("cycle_deadline_timestamp_seconds", "")]
	if lo, hi := float64(before.Add(gap).Unix()), float64(after.Add(gap).Unix()+1); got < lo || got > hi {
		t.Errorf("deadline after a successful cycle = %v, want the cycle's end plus %s, in [%v, %v]", got, gap, lo, hi)
	}
}

// With the built-in timer off a successful cycle leaves the deadline at 0, the
// value the stall rule reads as "an outside scheduler runs the cycles".
func TestCycleDeadlineStaysZeroWithTheTimerOff(t *testing.T) {
	specs := spec.Parse("h1:5432:app:u")
	reg, m := NewRegistry(t.TempDir(), specs, Boot{Start: time.Now()})

	runCycle(t.Context(), t, t.TempDir(), specs, m)

	checkSeries(t, series(t, reg), map[string]float64{
		key("last_cycle_ok", ""):                    1,
		key("cycle_deadline_timestamp_seconds", ""): 0,
	})
}

// DB_SPECS is read at start, so a database removed from it leaves the page at
// the restart that applies the change: no family carries it any more.
func TestADatabaseRemovedFromDBSpecsHasNoSeries(t *testing.T) {
	dumpDir := t.TempDir()
	before := spec.Parse("h1:5432:app:u h1:5432:gone:u")
	_, m := NewRegistry(dumpDir, before, Boot{})
	runCycle(t.Context(), t, dumpDir, before, m)

	after := spec.Parse("h1:5432:app:u")
	reg, m := NewRegistry(dumpDir, after, Boot{Last: scheduler.RunRecord{Time: time.Now(), OK: true}, Known: true})
	runCycle(t.Context(), t, dumpDir, after, m)

	got := series(t, reg)
	for k := range got {
		if strings.Contains(k, "gone") {
			t.Errorf("%s is on the page after gone left DB_SPECS", k)
		}
	}
	checkSeries(t, got, map[string]float64{key("dump_ok", "h1_5432", "app"): 1})
	if _, ok := got[key("newest_dump_timestamp_seconds", "h1_5432", "app")]; !ok {
		t.Error("newest_dump_timestamp_seconds has no series for app, which is still configured and dumped")
	}
}

// writeDump creates a dump file under dumpDir/server with modification time
// at.
func writeDump(t *testing.T, dumpDir, server, name string, at time.Time) {
	t.Helper()
	dir := filepath.Join(dumpDir, server)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(fakeDump), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The newest verified dump's time is read from the files when Prometheus
// scrapes: the stable name or the newest retained one, never another
// database's file or a staging temp, and a database with no dump yet has no
// sample. Gather reports no dropped sample.
func TestScrapeReadsTheNewestDumpFromTheFiles(t *testing.T) {
	dumpDir := t.TempDir()
	stable := time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
	older := time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC)
	writeDump(t, dumpDir, "h1_5432", "app.dump", stable)
	writeDump(t, dumpDir, "h1_5432", "keep.20261104T020000Z.dump", older)
	writeDump(t, dumpDir, "h1_5432", "keep.20261105T020000Z.dump", newer)
	writeDump(t, dumpDir, "h1_5432", "keep2.dump", newer.Add(time.Hour))
	writeDump(t, dumpDir, "h1_5432", ".atomicfile-1.tmp", newer.Add(2*time.Hour))
	specs := spec.Parse("h1:5432:app:u h1:5432:keep:u h1:5432:never:u")

	reg, _ := NewRegistry(dumpDir, specs, Boot{})

	got := series(t, reg)
	checkSeries(t, got, map[string]float64{
		key("newest_dump_timestamp_seconds", "h1_5432", "app"):  float64(stable.Unix()),
		key("newest_dump_timestamp_seconds", "h1_5432", "keep"): float64(newer.Unix()),
	})
	if v, ok := got[key("newest_dump_timestamp_seconds", "h1_5432", "never")]; ok {
		t.Errorf("newest_dump_timestamp_seconds for a database with no dump = %v, want no sample", v)
	}
	if free := got[key("dump_dir_free_bytes", "")]; free <= 0 {
		t.Errorf("dump_dir_free_bytes = %v, want the free space of the temp dir's filesystem", free)
	}
}
