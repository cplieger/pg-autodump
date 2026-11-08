package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/obs"
)

const (
	promqlRules = "../../alerts/promql.yaml"
	logqlRules  = "../../alerts/logql.yaml"
)

// downPG dumps every database except those on host "down", whose probe is
// refused.
type downPG struct{}

func (downPG) Probe(_ context.Context, c dump.Conn) (int, dump.FailKind, error) {
	if c.Host == "down" {
		return 0, dump.FailConnect, nil
	}
	return 18, dump.FailNone, nil
}

func (downPG) Dump(_ context.Context, _ dump.Conn, w io.Writer) (int, string, error) {
	_, _ = io.WriteString(w, "PGDMP-fake")
	return 0, "", nil
}

func (downPG) VerifyTOC(context.Context, string) error { return nil }

// ruleExprs returns each alert of a shipped rule file with its expression,
// read as text: the module carries no YAML parser.
func ruleExprs(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	rules := make(map[string]string)
	for _, block := range strings.Split(string(raw), "- alert: ")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		_, expr, found := strings.Cut(rest, "expr: ")
		if !found {
			t.Fatalf("%s: %s has no expr", path, name)
		}
		expr, _, _ = strings.Cut(expr, "for:")
		rules[strings.TrimSpace(name)] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(expr), "|"))
	}
	if len(rules) == 0 {
		t.Fatalf("%s defines no rule", path)
	}
	return rules
}

// cycleEnv is a DB_SPECS with one database that dumps and one whose server
// refuses the connection, on a fresh DUMP_DIR.
func cycleEnv(t *testing.T) config.Config {
	t.Helper()
	env := map[string]string{"DB_SPECS": "h1:5432:app:u down:5432:app:u", "DUMP_DIR": t.TempDir()}
	cfg, _, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// runCycle runs one cycle over cfg through the real orchestrator, logging
// through the production text handler, and returns the log.
func runCycle(t *testing.T, cfg *config.Config, metrics dump.Recorder) string {
	t.Helper()
	var buf bytes.Buffer
	handler, _ := slogx.NewHandler(slogx.Options{Output: &buf})
	orch := dump.New(&dump.Params{
		PG:          downPG{},
		Logger:      slog.New(handler),
		Metrics:     metrics,
		DumpDir:     cfg.DumpDir,
		Specs:       cfg.Specs,
		DumpTimeout: cfg.DumpTimeout,
		Concurrency: cfg.DumpConcurrency,
		Keep:        cfg.DumpKeep,
	})
	orch.Run(t.Context())
	return buf.String()
}

var (
	labelMatcherRe = regexp.MustCompile(`\{[^}]*\}`)
	groupingRe     = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promRangeRe    = regexp.MustCompile(`\[[^\]]*\]`)
	identRe        = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	selectorRe     = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}`)
	matcherNameRe  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)`)
)

// promqlWords are the PromQL keywords and functions the shipped rules may use;
// every other identifier in an expression is a series name.
var promqlWords = []string{
	"and", "or", "unless", "on", "ignoring", "group_left", "group_right", "by", "without", "bool",
	"sum", "min", "max", "avg", "count", "absent", "time", "rate", "increase", "changes", "resets",
	"min_over_time", "max_over_time", "avg_over_time", "sum_over_time", "count_over_time",
	"last_over_time", "present_over_time", "absent_over_time",
}

// seriesNames returns the series a PromQL expression reads.
func seriesNames(expr string) []string {
	expr = labelMatcherRe.ReplaceAllString(expr, " ")
	expr = groupingRe.ReplaceAllString(expr, " ")
	expr = promRangeRe.ReplaceAllString(expr, " ")
	var names []string
	for _, word := range identRe.FindAllString(expr, -1) {
		if !slices.Contains(promqlWords, word) && !slices.Contains(names, word) {
			names = append(names, word)
		}
	}
	return names
}

// exportedFamilies builds the registry runServer serves, from the same config
// and boot record, and runs one cycle through it so every labelled family has
// a sample. It returns each family's label names.
func exportedFamilies(t *testing.T) map[string][]string {
	t.Helper()
	cfg := cycleEnv(t)
	reg, metrics := obs.NewRegistry(cfg.DumpDir, cfg.Specs, scheduler.RunRecord{}, false)
	runCycle(t, &cfg, metrics)
	return familyLabels(t, reg)
}

func familyLabels(t *testing.T, reg *measure.Registry) map[string][]string {
	t.Helper()
	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather = %v", err)
	}
	labels := make(map[string][]string, len(families))
	for _, f := range families {
		if len(f.Samples) == 0 {
			t.Errorf("%s has no sample after a cycle, so a rule over it sees nothing", f.Name)
		}
		var names []string
		for _, s := range f.Samples {
			for _, l := range s.Labels {
				if !slices.Contains(names, l.Name) {
					names = append(names, l.Name)
				}
			}
		}
		labels[f.Name] = names
	}
	return labels
}

// TestPromQLRulesReadOnlyExportedSeries fails when a shipped PromQL rule reads
// a series, or matches a label, that the registry runServer serves does not
// export, so a renamed or dropped series fails the build instead of silencing
// an alert.
func TestPromQLRulesReadOnlyExportedSeries(t *testing.T) {
	exported := exportedFamilies(t)
	scrapeLabels := []string{"job", "instance"}
	for alert, expr := range ruleExprs(t, promqlRules) {
		names := seriesNames(expr)
		if len(names) == 0 {
			t.Errorf("%s reads no series: %s", alert, expr)
		}
		for _, name := range names {
			if _, ok := exported[name]; !ok && name != "up" {
				t.Errorf("%s reads %s, which the registry runServer serves does not export", alert, name)
			}
		}
		for _, sel := range selectorRe.FindAllStringSubmatch(expr, -1) {
			labels, ok := exported[sel[1]]
			if !ok {
				continue
			}
			for _, m := range matcherNameRe.FindAllStringSubmatch(sel[2], -1) {
				if !slices.Contains(labels, m[1]) && !slices.Contains(scrapeLabels, m[1]) {
					t.Errorf("%s matches label %s on %s, which carries only %v", alert, m[1], sel[1], labels)
				}
			}
		}
	}
}

// oneSided names the alerts that exist in one rule file only, with the alert
// that covers the same problem in the other file.
var oneSided = map[string]string{
	"PgAutodumpDumpStale":    "PgAutodumpCycleMissing",
	"PgAutodumpDown":         "PgAutodumpCycleMissing",
	"PgAutodumpCycleMissing": "PgAutodumpDumpStale",
}

// releasedAlerts are the alert names an earlier release shipped, each with the
// name that carries it now, so dropping or renaming one is a decision in the
// diff.
var releasedAlerts = map[string]string{
	"PgAutodumpDumpFailed":   "PgAutodumpDumpFailed",
	"PgAutodumpCycleMissing": "PgAutodumpCycleMissing",
}

// TestAlertFilesHoldTheSameAlerts keeps the two alternative files in parity:
// an alert in one has a partner of the same name in the other, except the
// one-sided alerts named with their cover, and every released alert name
// still maps to a shipped alert.
func TestAlertFilesHoldTheSameAlerts(t *testing.T) {
	promql := ruleExprs(t, promqlRules)
	logql := ruleExprs(t, logqlRules)
	for _, pair := range []struct {
		name        string
		rules, peer map[string]string
	}{
		{name: promqlRules, rules: promql, peer: logql},
		{name: logqlRules, rules: logql, peer: promql},
	} {
		for alert := range pair.rules {
			if _, ok := pair.peer[alert]; ok {
				if _, named := oneSided[alert]; named {
					t.Errorf("%s is in both files but oneSided names it", alert)
				}
				continue
			}
			cover, named := oneSided[alert]
			if !named {
				t.Errorf("%s holds %s, which the other file lacks and oneSided does not name", pair.name, alert)
				continue
			}
			if _, ok := pair.peer[cover]; !ok {
				t.Errorf("%s exists only in %s, and its cover %s is missing from the other file", alert, pair.name, cover)
			}
		}
	}
	for released, now := range releasedAlerts {
		_, inProm := promql[now]
		_, inLog := logql[now]
		if !inProm && !inLog {
			t.Errorf("released alert %s maps to %s, which neither file defines", released, now)
		}
	}
}

// TestHeadersNameTheOneSidedAlerts keeps each one-sided alert and its cover in
// both file headers, where a user deciding which file to load reads them.
func TestHeadersNameTheOneSidedAlerts(t *testing.T) {
	for _, path := range []string{promqlRules, logqlRules} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		header, _, _ := strings.Cut(string(raw), "groups:")
		for alert, cover := range oneSided {
			if !strings.Contains(header, alert) || !strings.Contains(header, cover) {
				t.Errorf("%s header does not name %s and its cover %s", path, alert, cover)
			}
		}
	}
}

// windowRe reads a LogQL range in hours; ageRe a PromQL age written as
// `> N * 3600`.
var (
	windowRe = regexp.MustCompile(`\[(\d+)h\]`)
	ageRe    = regexp.MustCompile(`>\s*(\d+)\s*\*\s*3600`)
)

// TestWindowsAreTwoDefaultIntervalsPlusARun keeps every window at two runs of
// the default DUMP_INTERVAL plus two hours for the run: above the health
// lease at the defaults, so a timer that stops turns the container unhealthy
// before the stale and missing-cycle alerts fire.
func TestWindowsAreTwoDefaultIntervalsPlusARun(t *testing.T) {
	want := 2*config.DefaultDumpInterval + 2*time.Hour
	env := map[string]string{"DB_SPECS": "h1:5432:app:u"}
	cfg, _, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if lease := probeLease(&cfg).Duration(); want <= lease {
		t.Fatalf("window %s is not above the default health lease %s", want, lease)
	}
	for alert, expr := range ruleExprs(t, logqlRules) {
		m := windowRe.FindStringSubmatch(expr)
		if m == nil {
			t.Errorf("%s has no range in hours: %s", alert, expr)
			continue
		}
		hours, _ := strconv.Atoi(m[1])
		if got := time.Duration(hours) * time.Hour; got != want {
			t.Errorf("%s ranges over %s, want %s", alert, got, want)
		}
	}
	m := ageRe.FindStringSubmatch(ruleExprs(t, promqlRules)["PgAutodumpDumpStale"])
	if m == nil {
		t.Fatal("PgAutodumpDumpStale compares no age in hours")
	}
	hours, _ := strconv.Atoi(m[1])
	if got := time.Duration(hours) * time.Hour; got != want {
		t.Errorf("PgAutodumpDumpStale waits %s, want %s", got, want)
	}
}

var (
	lineFilterRe = regexp.MustCompile("\\|= `([^`]+)`")
	logfmtRe     = regexp.MustCompile(`\| logfmt (\w+) \| unwrap (\w+)`)
	logfmtPairRe = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)
)

// logfmtFields parses one slog text line.
func logfmtFields(line string) map[string]string {
	fields := make(map[string]string)
	for _, m := range logfmtPairRe.FindAllStringSubmatch(line, -1) {
		v := m[2]
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		}
		fields[m[1]] = v
	}
	return fields
}

// TestLogQLRulesReadTheRecordTheCycleWrites binds every LogQL rule to the line
// a real cycle writes: each selects the `dump cycle complete` record by its
// message, which appears once per cycle, and the field PgAutodumpDumpFailed
// unwraps is a number on it.
func TestLogQLRulesReadTheRecordTheCycleWrites(t *testing.T) {
	cfg := cycleEnv(t)
	var records []string
	for line := range strings.Lines(runCycle(t, &cfg, nil)) {
		if strings.Contains(line, "dump cycle complete") {
			records = append(records, line)
		}
	}
	if len(records) != 1 {
		t.Fatalf("a cycle wrote %d dump cycle complete lines, want 1", len(records))
	}
	for alert, expr := range ruleExprs(t, logqlRules) {
		filters := lineFilterRe.FindAllStringSubmatch(expr, -1)
		if len(filters) != 1 || !strings.Contains(records[0], filters[0][1]) {
			t.Errorf("%s filters on %v, which does not select the record %q", alert, filters, records[0])
		}
		m := logfmtRe.FindStringSubmatch(expr)
		if m == nil {
			continue
		}
		if m[1] != m[2] {
			t.Errorf("%s extracts %s but unwraps %s", alert, m[1], m[2])
		}
		v, err := strconv.ParseFloat(logfmtFields(records[0])[m[2]], 64)
		if err != nil {
			t.Errorf("%s unwraps %s, which the record %q does not carry as a number", alert, m[2], records[0])
		}
		if v != 1 {
			t.Errorf("record %s = %v after a cycle with one refused database, want 1", m[2], v)
		}
	}
}

// TestLogQLDumpFailedHoldsUntilACycleSucceeds is the state rule's fixture: the
// latest record in the window decides, so a failed cycle fires it until a
// good cycle follows, and the good one resolves it.
func TestLogQLDumpFailedHoldsUntilACycleSucceeds(t *testing.T) {
	expr := ruleExprs(t, logqlRules)["PgAutodumpDumpFailed"]
	if !strings.HasPrefix(expr, "last_over_time(") || !strings.Contains(expr, ") by (container) > 0") {
		t.Fatalf("PgAutodumpDumpFailed is not last_over_time by container > 0: %s", expr)
	}
	m := windowRe.FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("PgAutodumpDumpFailed has no range: %s", expr)
	}
	hours, _ := strconv.Atoi(m[1])
	window := time.Duration(hours) * time.Hour
	type record struct {
		at     time.Duration
		failed float64
	}
	// firing evaluates last_over_time(...)[window] > 0 at `at`.
	firing := func(records []record, at time.Duration) bool {
		last, seen := 0.0, false
		for _, r := range records {
			if r.at <= at && at-r.at < window {
				last, seen = r.failed, true
			}
		}
		return seen && last > 0
	}
	recs := []record{{at: 0, failed: 1}}
	if !firing(recs, time.Hour) {
		t.Error("does not fire an hour after a failed cycle")
	}
	if !firing(recs, 23*time.Hour) {
		t.Error("resolved 23 hours after a failed cycle with no cycle since; a count window would, a state rule must not")
	}
	recs = append(recs, record{at: 24 * time.Hour, failed: 0})
	if firing(recs, 25*time.Hour) {
		t.Error("still fires after a good cycle followed the failed one")
	}
}
