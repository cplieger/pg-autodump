// Package obs wires the shared observability libraries to pg-autodump's domain:
// a metrics registry (github.com/cplieger/metrics/v2) exposed at /metrics, and
// a startup preflight used to decide the health-marker state. The orchestrator
// records through the narrow dump.Recorder interface it defines; this package
// supplies the concrete implementation, so the core stays testable against a
// fake or nil recorder.
package obs

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/cplieger/metrics/v2"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/pg"
	"github.com/cplieger/pg-autodump/internal/spec"
)

// dumpDurationBuckets bound logical-dump latency. DefaultBuckets (<=1s) and
// APIBuckets (<=30s) both saturate well below real dump times, so custom
// seconds-scale buckets are used.
var dumpDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600}

// Metrics holds the registry and the pg-autodump metric set. It implements
// dump.Recorder. All names are registered without the "pg_autodump_" prefix;
// the registry prepends it (e.g. dump_db_total -> pg_autodump_dump_db_total).
type Metrics struct {
	reg         *metrics.Registry
	runs        *metrics.Counter
	dbTotal     *metrics.LabeledCounter
	failures    *metrics.LabeledCounter
	duration    *metrics.LabeledHistogram
	bytes       *metrics.LabeledGauge
	inFlight    *metrics.Gauge
	lastSuccess *metrics.LabeledGauge
	now         func() time.Time
}

var _ dump.Recorder = (*Metrics)(nil)

// NewMetrics constructs and registers the metric set under the "pg_autodump"
// namespace.
func NewMetrics() *Metrics {
	reg := metrics.NewRegistry("pg_autodump")
	labels := []string{"host", "db"}
	reasonLabels := []string{"host", "db", "reason"}

	m := &Metrics{
		reg:         reg,
		runs:        metrics.NewCounter("dump_runs_total", "Total dump runs triggered."),
		dbTotal:     metrics.NewLabeledCounter("dump_db_total", "Per-database dump outcomes by reason.", reasonLabels),
		failures:    metrics.NewLabeledCounter("dump_failures_total", "Per-database dump failures by reason.", reasonLabels),
		duration:    metrics.NewLabeledHistogram("dump_duration_seconds", "Per-database dump duration.", labels, metrics.WithBuckets(dumpDurationBuckets)),
		bytes:       metrics.NewLabeledGauge("dump_bytes", "Size of the last successful dump per database.", labels),
		inFlight:    metrics.NewGauge("dump_in_flight", "Dumps currently running."),
		lastSuccess: metrics.NewLabeledGauge("dump_last_success_timestamp_seconds", "Unix time of the last successful dump per database.", labels),
		now:         time.Now,
	}

	reg.RegisterCounter(m.runs)
	reg.RegisterLabeledCounter(m.dbTotal)
	reg.RegisterLabeledCounter(m.failures)
	reg.RegisterLabeledHistogram(m.duration)
	reg.RegisterLabeledGauge(m.bytes)
	reg.RegisterGauge(m.inFlight)
	reg.RegisterLabeledGauge(m.lastSuccess)
	return m
}

// Handler serves the Prometheus text exposition for /metrics.
func (m *Metrics) Handler() http.Handler { return m.reg.Handler() }

// IncRun records that a dump run started. Called once per run by the trigger,
// not per database.
func (m *Metrics) IncRun() { m.runs.Inc() }

// RecordResult records one completed per-database outcome.
func (m *Metrics) RecordResult(r *dump.Result) {
	reason := string(r.Reason)
	m.dbTotal.Inc(r.Host, r.DBName, reason)
	if !r.OK() {
		m.failures.Inc(r.Host, r.DBName, reason)
		return
	}
	if r.Duration > 0 {
		m.duration.Observe(r.Duration.Seconds(), r.Host, r.DBName)
	}
	m.bytes.Set(float64(r.Bytes), r.Host, r.DBName)
	m.lastSuccess.Set(float64(m.now().Unix()), r.Host, r.DBName)
}

// SetInFlight reports the current number of dumps actively running.
func (m *Metrics) SetInFlight(n int) { m.inFlight.Set(float64(n)) }

// Preflight reports whether the liveness preconditions hold: the client
// binaries resolve on PATH, the dump directory is writable, and DB_SPECS lists
// at least one entry. It deliberately does NOT probe per-host database
// reachability (that is a per-dump, per-DB concern), so a transiently-down
// database never flips the container unhealthy. Returns nil when healthy, else
// a reason for the log.
func Preflight(dumpDir string, specs []spec.DBSpec) error {
	if err := pg.BinariesPresent(); err != nil {
		return err
	}
	if err := dirWritable(dumpDir); err != nil {
		return err
	}
	if len(specs) == 0 {
		return errEmptySpecs
	}
	return nil
}

var errEmptySpecs = errors.New("DB_SPECS is empty")

// dirWritable confirms dir exists and accepts a create+remove, which is what a
// dump needs (atomicfile stages a temp there).
func dirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".pg-autodump-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}
