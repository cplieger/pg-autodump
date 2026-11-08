package obs

import (
	"context"
	"slices"
	"time"

	"github.com/cplieger/go/measure"
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
	bytes     *measure.LabeledGauge
	duration  *measure.LabeledGauge
	cycleTime *measure.Gauge
	cycleOK   *measure.Gauge
}

var _ dump.Recorder = (*Metrics)(nil)

// database is the label pair every per-database series carries: server is
// the folder its dumps land in, under DUMP_DIR.
type database struct{ server, db string }

func databaseOf(r *dump.Result) database {
	return database{server: spec.ServerDir(r.Host, r.Port), db: r.DBName}
}

// NewRegistry builds the registry GET /metrics serves and the Metrics every
// cycle records into. Every entry of specs has its series from the start, and
// last, the last-run record read at boot (known is false when there is none),
// seeds the last-result and last-cycle gauges: the record does not say which
// database failed, so a failed record seeds every entry failed. The files
// under dumpDir are read at each scrape.
func NewRegistry(dumpDir string, specs []spec.DBSpec, last scheduler.RunRecord, known bool) (*measure.Registry, *Metrics) {
	m := &Metrics{
		dumps: measure.NewLabeledCounter("dumps_total",
			"Dumps by database and result.", append(slices.Clone(perDB), "result")),
		dumpOK: measure.NewLabeledGauge("dump_ok",
			"1 unless the database's latest dump failed; 0 for every database after a restart that read a failed last-run record.", perDB),
		bytes: measure.NewLabeledGauge("dump_bytes",
			"Size of the database's latest verified dump in bytes.", perDB),
		duration: measure.NewLabeledGauge("dump_duration_seconds",
			"How long the database's latest dump attempt took.", perDB),
		cycleTime: measure.NewGauge("last_cycle_timestamp_seconds",
			"Unix time the latest dump cycle ended; read from the last-run record at start."),
		cycleOK: measure.NewGauge("last_cycle_ok",
			"1 when every database dumped and verified in the latest cycle, else 0."),
	}
	reg := measure.NewRegistry(metricsPrefix)
	reg.MustRegister(m.dumps, m.dumpOK, m.bytes, m.duration, m.cycleTime, m.cycleOK, newDumpDirCollector(dumpDir, specs))

	okSeed := 1.0
	if known && !last.OK {
		okSeed = 0
	}
	for i := range specs {
		s := &specs[i]
		d := database{server: spec.ServerDir(s.Host, s.Port), db: dump.ResultDB(s)}
		for _, r := range dump.Reasons() {
			m.dumps.Add(0, d.server, d.db, string(r))
		}
		m.dumpOK.Set(okSeed, d.server, d.db)
		m.bytes.Set(0, d.server, d.db)
		m.duration.Set(0, d.server, d.db)
	}
	if known {
		m.cycleTime.Set(unixSeconds(last.Time))
		m.cycleOK.Set(digit(last.OK))
	}
	return reg, m
}

// newDumpDirCollector reads the dump directory at each scrape, so its series
// survive a restart and count dumps an exec'd `pg-autodump run` wrote.
func newDumpDirCollector(dumpDir string, specs []spec.DBSpec) *measure.Collector {
	newest := measure.NewGaugeDesc("newest_dump_timestamp_seconds",
		"Modification time of the database's newest verified dump under DUMP_DIR; absent until its first dump.",
		perDB)
	free := measure.NewGaugeDesc("dump_dir_free_bytes",
		"Bytes free to the container user on the filesystem holding DUMP_DIR.", nil)
	return measure.NewCollector(func(_ context.Context, e *measure.Emitter) {
		for i := range specs {
			s := &specs[i]
			if s.Invalid != "" {
				continue
			}
			if at, found, err := dump.NewestDump(dumpDir, s); err == nil && found {
				e.Emit(newest, unixSeconds(at), spec.ServerDir(s.Host, s.Port), s.DBName)
			}
		}
		if bytes, err := dump.FreeBytes(dumpDir); err == nil {
			e.Emit(free, float64(bytes))
		}
	}, newest, free)
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
		if r.OK() {
			m.bytes.Set(float64(r.Bytes), d.server, d.db)
		}
	}
	for d, v := range ok {
		m.dumpOK.Set(digit(v), d.server, d.db)
	}
	m.cycleTime.Set(unixSeconds(end))
	m.cycleOK.Set(digit(dump.CycleOK(results)))
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Second)
}

func digit(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
