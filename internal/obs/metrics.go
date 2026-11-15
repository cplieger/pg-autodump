package obs

import (
	"context"
	"slices"
	"time"

	"github.com/cplieger/go/measure/v2"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
)

const metricsPrefix = "pg_autodump"

// perDB are the labels of every per-database series.
var perDB = []string{"server", "db"}

// Metrics holds the series a dump cycle sets.
type Metrics struct {
	dumps     *measure.LabeledCounter
	dumpOK    *measure.LabeledGauge
	duration  *measure.LabeledGauge
	cycleTime *measure.Gauge
	cycleOK   *measure.Gauge
	deadline  *measure.Gauge
	gap       time.Duration
}

var _ dump.Recorder = (*Metrics)(nil)

// database is the label pair every per-database series carries: server is
// the folder its dumps land in, under DUMP_DIR.
type database struct{ server, db string }

func databaseOf(r *dump.Result) database {
	return database{server: spec.ServerDir(r.Host, r.Port), db: r.DBName}
}

// Boot is what the server knows at start.
type Boot struct {
	Start time.Time
	// Last is the last-run record read at boot; Known is false when there is
	// none.
	Last scheduler.RunRecord
	// Version is the running build's, served in build_info.
	Version string
	// Gap is how long after a fully successful cycle the next one is due; 0
	// while the built-in timer is off.
	Gap   time.Duration
	Known bool
}

// NewRegistry builds the registry GET /metrics serves and the Metrics every
// cycle records into. Every entry of specs has its counters and last-result
// gauge from the start, and boot.Last seeds the last-result and last-cycle
// gauges: the record does not say which database failed, so a failed record
// seeds every entry failed. A duration has no series until this process
// measures one, since 0 would read as a measured dump. The cycle deadline
// starts at the last success on record, or boot.Start when there is none,
// plus boot.Gap. The files under dumpDir are read at each scrape.
func NewRegistry(dumpDir string, specs []spec.DBSpec, boot *Boot) (*measure.Registry, *Metrics) {
	last, known := boot.Last, boot.Known
	dbs := make([]database, len(specs))
	seeds := make([][]string, 0, len(specs)*len(dump.Reasons()))
	for i := range specs {
		s := &specs[i]
		dbs[i] = database{server: spec.ServerDir(s.Host, s.Port), db: dump.ResultDB(s)}
		for _, r := range dump.Reasons() {
			seeds = append(seeds, []string{dbs[i].server, dbs[i].db, string(r)})
		}
	}
	m := &Metrics{
		dumps: measure.NewLabeledCounter("dumps_total",
			"Dumps by database and result.", measure.UnitCount, append(slices.Clone(perDB), "result"),
			measure.WithSeed(seeds...)),
		dumpOK: measure.NewLabeledGauge("dump_ok",
			"1 unless the database's latest dump failed; 0 for every database after a restart that read a failed last-run record.",
			measure.UnitNone, perDB),
		duration: measure.NewLabeledGauge("dump_duration_seconds",
			"How long the database's latest dump attempt took; absent until the first attempt since the process started.",
			measure.UnitSeconds, perDB),
		cycleTime: measure.NewGauge("last_cycle_timestamp_seconds",
			"Unix time the latest dump cycle ended; read from the last-run record at start.", measure.UnitSeconds),
		cycleOK: measure.NewGauge("last_cycle_ok",
			"1 when every database dumped and verified in the latest cycle, else 0.", measure.UnitNone),
		deadline: measure.NewGauge("cycle_deadline_timestamp_seconds",
			"Unix time by which the next fully successful dump cycle is due; 0 while DUMP_INTERVAL is off.", measure.UnitSeconds),
		gap: boot.Gap,
	}
	buildInfo := measure.NewInfo("build_info",
		"Always 1; version is the running build's release tag.", []string{"version"})
	buildInfo.Set(boot.Version)
	reg := measure.NewRegistry(metricsPrefix)
	reg.MustRegister(m.dumps, m.dumpOK, m.duration, m.cycleTime, m.cycleOK, m.deadline,
		buildInfo, newDumpDirCollector(dumpDir, specs))

	okSeed := 1.0
	if known && !last.OK {
		okSeed = 0
	}
	for _, d := range dbs {
		m.dumpOK.Set(okSeed, d.server, d.db)
	}
	if known {
		m.cycleTime.Set(unixSeconds(last.Time))
		m.cycleOK.Set(digit(last.OK))
	}
	lastSuccess := boot.Start
	if known && last.OK {
		lastSuccess = last.Time
	}
	m.setDeadline(lastSuccess)
	return reg, m
}

// newDumpDirCollector reads the dump directory at each scrape, so its series
// survive a restart and count dumps an exec'd `pg-autodump run` wrote. Only a
// verified dump is ever committed under its final name, so the newest file's
// size is the latest verified dump's.
func newDumpDirCollector(dumpDir string, specs []spec.DBSpec) *measure.Collector {
	newest := measure.NewGaugeDesc("newest_dump_timestamp_seconds",
		"Modification time of the database's newest verified dump under DUMP_DIR; absent until its first dump.",
		measure.UnitSeconds, perDB)
	size := measure.NewGaugeDesc("dump_bytes",
		"Size in bytes of the database's newest verified dump under DUMP_DIR; absent until its first dump.",
		measure.UnitBytes, perDB)
	free := measure.NewGaugeDesc("dump_dir_free_bytes",
		"Bytes free to the container user on the filesystem holding DUMP_DIR.", measure.UnitBytes, nil)
	return measure.NewCollector(func(_ context.Context, e *measure.Emitter) {
		for i := range specs {
			s := &specs[i]
			if s.Invalid != "" {
				continue
			}
			if info, found, err := dump.NewestDump(dumpDir, s); err == nil && found {
				server := spec.ServerDir(s.Host, s.Port)
				e.Emit(newest, unixSeconds(info.ModTime()), server, s.DBName)
				e.Emit(size, float64(info.Size()), server, s.DBName)
			}
		}
		if bytes, err := dump.FreeBytes(dumpDir); err == nil {
			e.Emit(free, float64(bytes))
		}
	}, newest, size, free)
}

// CountDumps counts every result, a cycle shutdown cut short included.
func (m *Metrics) CountDumps(results []dump.Result) {
	for i := range results {
		d := databaseOf(&results[i])
		m.dumps.Inc(d.server, d.db, string(results[i].Reason))
	}
}

// RecordVerdict sets the gauges from a cycle with a verdict. A database is ok
// only when every result for it is, so a duplicate entry keeps it failed as
// long as the cycle reports failed.
func (m *Metrics) RecordVerdict(results []dump.Result, end time.Time) {
	ok := make(map[database]bool, len(results))
	for i := range results {
		r := &results[i]
		d := databaseOf(r)
		prev, seen := ok[d]
		ok[d] = r.OK() && (prev || !seen)
		if r.Reason == dump.ReasonInvalid || r.Reason == dump.ReasonDuplicate {
			continue
		}
		m.duration.Set(r.Duration.Seconds(), d.server, d.db)
	}
	for d, v := range ok {
		m.dumpOK.Set(digit(v), d.server, d.db)
	}
	m.cycleTime.Set(unixSeconds(end))
	cycleOK := dump.CycleOK(results)
	m.cycleOK.Set(digit(cycleOK))
	if cycleOK {
		m.setDeadline(end)
	}
}

// setDeadline dates the cycle deadline one gap after a success at
// lastSuccess; with no gap it stays 0.
func (m *Metrics) setDeadline(lastSuccess time.Time) {
	if m.gap > 0 {
		m.deadline.Set(unixSeconds(lastSuccess.Add(m.gap)))
	}
}

// unixSeconds avoids UnixNano, which overflows past 2262 and a saturated gap
// reaches it.
func unixSeconds(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/float64(time.Second)
}

func digit(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
