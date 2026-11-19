package obs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/scheduler/v2"
	"github.com/cplieger/pg-autodump/internal/dump"
	"github.com/cplieger/pg-autodump/internal/spec"
)

const metricsGolden = "testdata/metrics.golden"

// freeBytesLine is the one sample whose value is the host's free disk space.
var freeBytesLine = regexp.MustCompile(`(?m)^(` + metricsPrefix + `_dump_dir_free_bytes) \S+$`)

// The /metrics page, minus the Go and process families whose values move and
// with the free-space sample's value masked, matches the committed golden byte
// for byte after a fixed boot, two recorded cycles and a scrape of fixed dump
// files, so a change to any other value, or to any family's name, help, type,
// labels or seeding, fails here.
// Regenerate with UPDATE_GOLDEN=1 go test ./internal/obs/ -run TestMetricsPageMatchesGolden.
func TestMetricsPageMatchesGolden(t *testing.T) {
	dumpDir := t.TempDir()
	at := time.Date(2026, 11, 4, 2, 0, 0, 250_000_000, time.UTC)
	writeDump(t, dumpDir, "h1_5432", "app.dump", "PGDMP-app", at)
	writeDump(t, dumpDir, "h2_5433", "billing.dump", "PGDMP-billing-dump", at.Add(90*time.Second))
	specs := spec.Parse("h1:5432:app:u down:5432:app:u h2:5433:billing:u not-a-spec")
	boot := &Boot{
		Start:   time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		Last:    scheduler.RunRecord{Time: time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC), OK: false},
		Known:   true,
		Version: "v1.2.3",
		Gap:     49 * time.Hour,
	}
	reg, m := NewRegistry(dumpDir, specs, boot)

	result := func(i int, r dump.Reason, d time.Duration) dump.Result {
		s := &specs[i]
		return dump.Result{Host: s.Host, Port: s.Port, DBName: dump.ResultDB(s), Reason: r, Duration: d}
	}
	good := []dump.Result{result(0, dump.ReasonOK, 1500*time.Millisecond)}
	m.CountDumps(good)
	m.RecordVerdict(good, time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC))
	failed := []dump.Result{
		result(0, dump.ReasonOK, 2*time.Second),
		result(1, dump.ReasonPGError, 250*time.Millisecond),
		result(2, dump.ReasonOK, 3*time.Second+125*time.Millisecond),
		result(3, dump.ReasonInvalid, 0),
	}
	m.CountDumps(failed)
	m.RecordVerdict(failed, time.Date(2026, 11, 5, 14, 30, 0, 500_000_000, time.UTC))

	got := appPage(t, reg.Handler())
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(metricsGolden), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metricsGolden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(metricsGolden)
	if err != nil {
		t.Fatalf("reading %s: %v; regenerate with UPDATE_GOLDEN=1 go test ./internal/obs/ -run TestMetricsPageMatchesGolden", metricsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("/metrics differs from %s; regenerate with UPDATE_GOLDEN=1 go test ./internal/obs/ -run TestMetricsPageMatchesGolden\ngot:\n%s", metricsGolden, got)
	}
}

// appPage scrapes h and returns its page without the Go and process families,
// with the free-space value masked.
func appPage(t *testing.T, h http.Handler) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	var out bytes.Buffer
	for line := range strings.Lines(rec.Body.String()) {
		name := strings.TrimPrefix(strings.TrimPrefix(line, "# HELP "), "# TYPE ")
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		out.WriteString(line)
	}
	masked := freeBytesLine.ReplaceAll(out.Bytes(), []byte("$1 @FREE_BYTES@"))
	if bytes.Equal(masked, out.Bytes()) {
		t.Errorf("the page has no %s_dump_dir_free_bytes sample to mask", metricsPrefix)
	}
	return masked
}
