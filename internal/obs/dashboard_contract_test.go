package obs

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/scheduler/v2"
	"github.com/cplieger/pg-autodump/internal/config"
	"github.com/cplieger/pg-autodump/internal/spec"
)

const (
	dashboardFile = "../../grafana-dashboard.json"
	promqlRules   = "../../alerts/promql.yaml"
)

type dashQuery struct {
	Spec struct {
		Query struct {
			Spec struct {
				Expr string `json:"expr"`
			} `json:"spec"`
		} `json:"query"`
	} `json:"spec"`
}

type dashPanel struct {
	Spec struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Data        struct {
			Spec struct {
				Queries         []dashQuery      `json:"queries"`
				Transformations []transformation `json:"transformations"`
				QueryOptions    struct {
					MaxDataPoints int `json:"maxDataPoints"`
				} `json:"queryOptions"`
			} `json:"spec"`
		} `json:"data"`
		VizConfig struct {
			Group string `json:"group"`
			Spec  struct {
				Options struct {
					Stacking string `json:"stacking"`
					XField   string `json:"xField"`
				} `json:"options"`
				FieldConfig struct {
					Defaults struct {
						NoValue string `json:"noValue"`
					} `json:"defaults"`
					Overrides []struct {
						Matcher struct {
							Options string `json:"options"`
						} `json:"matcher"`
						Properties []struct {
							ID    string `json:"id"`
							Value any    `json:"value"`
						} `json:"properties"`
					} `json:"overrides"`
				} `json:"fieldConfig"`
			} `json:"spec"`
		} `json:"vizConfig"`
	} `json:"spec"`
}

type transformation struct {
	Group string `json:"group"`
}

type conditionRule struct {
	Kind string `json:"kind"`
	Spec struct {
		Value    any    `json:"value"`
		Variable string `json:"variable"`
		Operator string `json:"operator"`
	} `json:"spec"`
}

type conditionGroup struct {
	Spec struct {
		Visibility string          `json:"visibility"`
		Condition  string          `json:"condition"`
		Items      []conditionRule `json:"items"`
	} `json:"spec"`
}

type gridItem struct {
	Spec struct {
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Element              struct {
			Name string `json:"name"`
		} `json:"element"`
	} `json:"spec"`
}

type layoutNode struct {
	Kind string `json:"kind"`
	Spec struct {
		Layout               *layoutNode     `json:"layout"`
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Title                string          `json:"title"`
		Rows                 []layoutNode    `json:"rows"`
		Items                []gridItem      `json:"items"`
		HideHeader           bool            `json:"hideHeader"`
	} `json:"spec"`
}

type dashVariable struct {
	Kind string `json:"kind"`
	Spec struct {
		Name        string `json:"name"`
		Definition  string `json:"definition"`
		Hide        string `json:"hide"`
		Refresh     string `json:"refresh"`
		Regex       string `json:"regex"`
		SkipURLSync bool   `json:"skipUrlSync"`
		Current     struct {
			Text  any `json:"text"`
			Value any `json:"value"`
		} `json:"current"`
		// Query is a data query on a QueryVariable and a string on a
		// CustomVariable.
		Query json.RawMessage `json:"query"`
	} `json:"spec"`
}

type dashboard struct {
	Spec struct {
		Elements  map[string]dashPanel `json:"elements"`
		Layout    layoutNode           `json:"layout"`
		Variables []dashVariable       `json:"variables"`
		Preload   bool                 `json:"preload"`
	} `json:"spec"`
}

func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardFile, err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decoding %s: %v", dashboardFile, err)
	}
	if len(d.Spec.Elements) == 0 {
		t.Fatalf("%s holds no panel", dashboardFile)
	}
	return d
}

func panelExprs(p *dashPanel) []string {
	var out []string
	for _, q := range p.Spec.Data.Spec.Queries {
		out = append(out, q.Spec.Query.Spec.Expr)
	}
	return out
}

func (d *dashboard) variables() map[string]dashVariable {
	out := map[string]dashVariable{}
	for _, v := range d.Spec.Variables {
		out[v.Spec.Name] = v
	}
	return out
}

// variableQuery is a query variable's PromQL, or "" for any other variable.
// Grafana runs the query's __legacyStringValue, null as "", when that key is
// present (getDataQueryForVariable), and its query field otherwise.
func variableQuery(v *dashVariable) string {
	var q struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if v.Kind != "QueryVariable" || json.Unmarshal(v.Spec.Query, &q) != nil {
		return ""
	}
	raw, ok := q.Spec["__legacyStringValue"]
	if !ok {
		raw = q.Spec["query"]
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func row(t *testing.T, d *dashboard, title string) *layoutNode {
	t.Helper()
	for i := range d.Spec.Layout.Spec.Rows {
		if r := &d.Spec.Layout.Spec.Rows[i]; r.Spec.Title == title {
			return r
		}
	}
	t.Fatalf("no row titled %q", title)
	return nil
}

// servedSeries gathers a registry with a run on record and one database
// dumped since the start, and returns each series name it serves with its
// label names.
func servedSeries(t *testing.T) map[string][]string {
	t.Helper()
	dumpDir := t.TempDir()
	specs := spec.Parse("h1:5432:app:u")
	reg, m := NewRegistry(dumpDir, specs,
		&Boot{Last: scheduler.RunRecord{Time: time.Date(2026, 11, 3, 2, 1, 0, 0, time.UTC), OK: true}, Known: true})
	runCycle(t.Context(), t, dumpDir, specs, m)
	families, err := reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	served := map[string][]string{}
	for _, f := range families {
		labels := served[f.Name]
		for _, s := range f.Samples {
			for _, l := range s.Labels {
				if !slices.Contains(labels, l.Name) {
					labels = append(labels, l.Name)
				}
			}
		}
		if len(f.Samples) > 0 {
			served[f.Name] = labels
		}
	}
	if _, ok := served["pg_autodump_newest_dump_timestamp_seconds"]; !ok {
		t.Fatalf("Setup: the registry serves no pg_autodump_newest_dump_timestamp_seconds: %v", served)
	}
	return served
}

var (
	selectorRe    = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}`)
	matcherNameRe = regexp.MustCompile(`([a-zA-Z_]\w*)\s*(?:=~|!~|!=|=)`)
	matchersRe    = regexp.MustCompile(`\{[^}]*\}`)
	groupingRe    = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
	rangeRe       = regexp.MustCompile(`\[[^\]]*\]`)
	variableRe    = regexp.MustCompile(`\$\{?\w+(?::\w+)?\}?`)
	labelValuesRe = regexp.MustCompile(`label_values\(([^,]+),\s*\w+\)`)
	functionRe    = regexp.MustCompile(`\b[a-z_]+\s*\(`)
	identRe       = regexp.MustCompile(`\b[a-zA-Z_:][a-zA-Z0-9_:]*\b`)
	alertsRe      = regexp.MustCompile(`^ALERTS\{alertname="(\w+)"`)
	namedInBraces = regexp.MustCompile(`__name__|\{\s*["'` + "`" + `]|(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`" + `)\s*(?:=~|!~|!=|=)`)
	// bracedVariableRe matches ${name}, whose braces are no selector.
	bracedVariableRe = regexp.MustCompile(`\$\{\w+(?::\w+)?\}`)
	// absentCallRe matches a call of absent() or absent_over_time(); PromQL
	// lets whitespace stand before a call's parenthesis.
	absentCallRe = regexp.MustCompile(`(?:^|[^\w:])absent(?:_over_time)?\s*\(`)
)

var promqlKeywords = []string{"and", "or", "unless", "bool", "offset"}

// dashboardQueries returns every variable's and panel's query, keyed by where
// it sits; a variable that runs no query gives "".
func dashboardQueries(d *dashboard) map[string]string {
	queries := map[string]string{}
	for name, v := range d.variables() {
		queries["variable "+name] = variableQuery(&v)
	}
	for name, p := range d.Spec.Elements {
		for i, e := range panelExprs(&p) {
			queries[fmt.Sprintf("%s %s query %d", name, p.Spec.Title, i)] = e
		}
	}
	return queries
}

// skipString returns the index just past the string literal opening at s[i].
func skipString(s string, i int) int {
	q := s[i]
	for i++; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q != '`':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return len(s)
}

// promqlCode is expr with its string literals and # comments blanked, so a
// word inside one is not read as code.
func promqlCode(expr string) string {
	b := []byte(expr)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '"', '\'', '`':
			end := skipString(expr, i)
			for j := i; j < end; j++ {
				b[j] = ' '
			}
			i = end - 1
		case '#':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// hasComment reports whether expr holds a # outside a string literal, which
// opens a PromQL comment.
func hasComment(expr string) bool {
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '"', '\'', '`':
			i = skipString(expr, i) - 1
		case '#':
			return true
		}
	}
	return false
}

type selectorBlock struct {
	name, text string
}

// selectors returns each {...} block of expr with the series name written
// before it, empty when none, a keyword (x or {...}) or a variable ($v{...})
// is. Quoted strings are skipped, so a brace inside a label value ends no
// block.
func selectors(expr string) []selectorBlock {
	var out []selectorBlock
	start := -1
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; {
		case c == '"' || c == '\'' || c == '`':
			i = skipString(expr, i) - 1
		case c == '{':
			start = i
		case c == '}' && start >= 0:
			before := strings.TrimRight(expr[:start], " \t\r\n")
			name := before[len(strings.TrimRightFunc(before, func(r rune) bool {
				return r == '_' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
			})):]
			if slices.Contains(promqlKeywords, name) || strings.HasSuffix(before[:len(before)-len(name)], "$") {
				name = ""
			}
			out = append(out, selectorBlock{name: name, text: name + expr[start:i+1]})
			start = -1
		}
	}
	return out
}

// seriesIn returns the series names a panel or variable query reads;
// label_values names a label after its series, which is no series.
func seriesIn(expr string) []string {
	expr = promqlCode(labelValuesRe.ReplaceAllString(expr, "$1"))
	for _, re := range []*regexp.Regexp{matchersRe, groupingRe, rangeRe, variableRe, functionRe} {
		expr = re.ReplaceAllString(expr, " ")
	}
	var names []string
	for _, id := range identRe.FindAllString(expr, -1) {
		if !slices.Contains(promqlKeywords, id) && !slices.Contains(names, id) {
			names = append(names, id)
		}
	}
	return names
}

// TestDashboardReadsOnlySeriesPgAutodumpServes fails when a panel or a
// variable reads a series or a label pg-autodump does not serve, so a renamed
// series fails the build instead of blanking a panel. ALERTS is the ruler's
// series; job and instance are the scrape's labels. A query fails when it
// names a series or label inside quotes of any kind or through __name__, has a
// selector with no series name before its braces, or holds a # comment, since
// these regexes cannot read it.
func TestDashboardReadsOnlySeriesPgAutodumpServes(t *testing.T) {
	served := servedSeries(t)
	d := loadDashboard(t)
	scrape := []string{"job", "instance"}
	for where, expr := range dashboardQueries(&d) {
		if namedInBraces.MatchString(expr) {
			t.Errorf("%s names a series or label inside quotes or with __name__; write the metric and label names bare so this check can read them", where)
			continue
		}
		if hasComment(expr) {
			t.Errorf("%s holds a # comment; remove it so this check reads the query PromQL reads", where)
			continue
		}
		for _, sel := range selectors(bracedVariableRe.ReplaceAllString(expr, "$$v")) {
			if sel.name == "" {
				t.Errorf("%s has a selector with no series name before its braces, which this check cannot read; name the series: %s", where, sel.text)
			}
		}
		var labels []string
		for _, name := range seriesIn(expr) {
			switch have, ok := served[name]; {
			case name == "ALERTS":
			case !ok:
				t.Errorf("%s reads %s, which pg-autodump does not serve", where, name)
			default:
				labels = append(labels, have...)
			}
		}
		for _, sel := range selectorRe.FindAllStringSubmatch(promqlCode(expr), -1) {
			have, ok := served[sel[1]]
			if !ok {
				continue
			}
			for _, m := range matcherNameRe.FindAllStringSubmatch(sel[2], -1) {
				if !slices.Contains(have, m[1]) && !slices.Contains(scrape, m[1]) {
					t.Errorf("%s matches %s on %s, which carries no such label", where, m[1], sel[1])
				}
			}
		}
		for _, g := range groupingRe.FindAllStringSubmatch(expr, -1) {
			for l := range strings.SplitSeq(g[2], ",") {
				l = strings.TrimSpace(l)
				// A label the query names as a string is one count_values or label_replace makes.
				computed := strings.Contains(expr, `"`+l+`"`)
				if l != "" && !computed && !slices.Contains(labels, l) && !slices.Contains(scrape, l) {
					t.Errorf("%s groups by %s, which no series it reads carries", where, l)
				}
			}
		}
	}
}

// shippedRules returns each alert in alerts/promql.yaml with its expression,
// read as text: the module carries no YAML parser.
func shippedRules(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(promqlRules)
	if err != nil {
		t.Fatalf("reading %s: %v", promqlRules, err)
	}
	rules := map[string]string{}
	for _, block := range strings.Split(string(raw), "- alert: ")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		_, expr, found := strings.Cut(rest, "expr: ")
		if !found {
			t.Fatalf("%s: %s has no expr", promqlRules, name)
		}
		expr, _, _ = strings.Cut(expr, "for:")
		rules[strings.TrimSpace(name)] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(expr), "|"))
	}
	if len(rules) == 0 {
		t.Fatalf("%s defines no rule", promqlRules)
	}
	return rules
}

// attentionReads is every ALERTS selector a query may read the rule name's
// firing alerts with, for the rule expression expr, in its tile's order.
// absent() and absent_over_time() label their alert with only the selector's
// equality matchers, and a shipped rule's selector names no instance, so only
// such a rule's alert is also read with instance="":
// https://prometheus.io/docs/prometheus/latest/querying/functions/#absent
func attentionReads(name, expr string) []string {
	read := `ALERTS{alertname="` + name + `", alertstate="firing", instance=`
	if absentCallRe.MatchString(promqlCode(expr)) {
		return []string{read + `"$instance"}`, read + `""}`}
	}
	return []string{read + `"$instance"}`}
}

// attentionTile is the query of the Needs attention tile for the rule name
// with expression expr, which answers while any of its reads does.
func attentionTile(name, expr string) string {
	reads := attentionReads(name, expr)
	if len(reads) == 2 {
		return `0 * max((` + reads[0] + ` or ` + reads[1] + `))`
	}
	return `0 * max(` + reads[0] + `)`
}

// TestDashboardAttentionTilesMirrorEveryShippedRule fails when a rule in
// alerts/promql.yaml has no Needs attention tile, a tile names a rule the file
// does not hold or does not name it in its description, a tile's query is not
// exactly attentionTile's for its rule, the row's variable does not count
// exactly those tiles in layout order, a tile is not hidden until its query
// answers, any query reads an ALERTS selector that is not one of
// attentionReads' for a shipped rule, or a query reads a rule with
// instance="" but not with instance="$instance".
func TestDashboardAttentionTilesMirrorEveryShippedRule(t *testing.T) {
	d := loadDashboard(t)
	rules := shippedRules(t)
	for where, expr := range dashboardQueries(&d) {
		sels := selectors(bracedVariableRe.ReplaceAllString(expr, "$$v"))
		var texts []string
		for _, sel := range sels {
			texts = append(texts, sel.text)
		}
		for _, sel := range sels {
			if sel.name != "ALERTS" {
				continue
			}
			name := ""
			if m := alertsRe.FindStringSubmatch(sel.text); m != nil {
				name = m[1]
			}
			rule, ok := rules[name]
			switch {
			case name == "":
				t.Errorf("%s reads %s, which does not start with alertname=\"<rule>\": every ALERTS read is one of attentionReads' texts, alertname first", where, sel.text)
				continue
			case !ok:
				t.Errorf("%s reads %s, whose alertname names no rule in %s", where, sel.text, promqlRules)
				continue
			}
			reads := attentionReads(name, rule)
			switch {
			case !slices.Contains(reads, sel.text):
				t.Errorf("%s reads %s; a read of %s must be exactly one of these, so it shows only the picked instance's firing alerts:\n%s", where, sel.text, name, strings.Join(reads, "\n"))
			case strings.HasSuffix(sel.text, `instance=""}`) && !slices.Contains(texts, reads[0]):
				t.Errorf("%s reads %s without %s in the same query: a read of only instance=\"\" misses the rule's alerts for the picked instance", where, sel.text, reads[0])
			}
		}
	}
	attention := row(t, &d, "Needs attention")
	gate := attention.Spec.ConditionalRendering
	if gate == nil || gate.Spec.Visibility != "show" || len(gate.Spec.Items) != 1 ||
		gate.Spec.Items[0].Kind != "ConditionalRenderingVariable" ||
		gate.Spec.Items[0].Spec.Operator != "matches" || gate.Spec.Items[0].Spec.Value != "^[1-9]" {
		t.Fatal("Needs attention is not shown by a variable matching ^[1-9]")
	}
	var mirrored, counts []string
	for _, it := range attention.Spec.Layout.Spec.Items {
		p := d.Spec.Elements[it.Spec.Element.Name]
		cr := it.Spec.ConditionalRendering
		var conds []string
		if cr != nil {
			for _, c := range cr.Spec.Items {
				conds = append(conds, c.Kind)
			}
		}
		slices.Sort(conds)
		if cr == nil || cr.Spec.Visibility != "show" || cr.Spec.Condition != "or" ||
			!slices.Equal(conds, []string{"ConditionalRenderingData", "ConditionalRenderingTimeRangeSize"}) ||
			p.Spec.Data.Spec.QueryOptions.MaxDataPoints <= 0 {
			t.Errorf("%q does not show exactly while its query answers: want a shown group or'ing a has-data rule with a 0s range rule, and a maxDataPoints pin", p.Spec.Title)
		}
		exprs := panelExprs(&p)
		if len(exprs) != 1 {
			t.Errorf("%q holds %d queries, want 1", p.Spec.Title, len(exprs))
			continue
		}
		counts = append(counts, fmt.Sprintf(`label_replace(count(%s), "tile", "%s", "", "")`, exprs[0], strings.TrimPrefix(it.Spec.Element.Name, "panel-")))
		name := ""
		for _, sel := range selectors(exprs[0]) {
			if m := alertsRe.FindStringSubmatch(sel.text); m != nil && name == "" {
				name = m[1]
			}
		}
		if name == "" {
			t.Errorf("%q reads no ALERTS series by alertname: %s", p.Spec.Title, exprs[0])
			continue
		}
		mirrored = append(mirrored, name)
		if !strings.Contains(p.Spec.Description, name) {
			t.Errorf("%q reads %s, which its description does not name", p.Spec.Title, name)
		}
		expr, ok := rules[name]
		if !ok {
			t.Errorf("a tile mirrors %s, which %s does not hold", name, promqlRules)
			continue
		}
		if want := attentionTile(name, expr); exprs[0] != want {
			t.Errorf("%q must read exactly\n%s\nso it shows only the picked instance's firing %s alerts; it reads\n%s",
				p.Spec.Title, want, name, exprs[0])
		}
	}
	gateVar := d.variables()[gate.Spec.Items[0].Spec.Variable]
	if got, want := variableQuery(&gateVar), "query_result(count("+strings.Join(counts, " or ")+") or vector(0))"; got != want {
		t.Errorf("the row's variable must count exactly its tiles in layout order:\n%s\nit reads\n%s", want, got)
	}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		if !slices.Contains(mirrored, name) {
			t.Errorf("%s has no Needs attention tile reading its firing alerts", name)
		}
	}
}

// TestDashboardInstancePickerListsEveryInstance fails when the instance
// variable reads anything but pg_autodump_build_info, which every running
// pg-autodump serves from its start, filters what it reads with a regex, or
// never refreshes, so the picker lists every instance the query finds.
func TestDashboardInstancePickerListsEveryInstance(t *testing.T) {
	d := loadDashboard(t)
	v, ok := d.variables()["instance"]
	if !ok {
		t.Fatal("no instance variable")
	}
	if got, want := variableQuery(&v), "label_values(pg_autodump_build_info, instance)"; got != want {
		t.Errorf("the instance variable must read exactly\n%s\nso the picker lists every instance; it reads\n%s", want, got)
	}
	if refreshes := v.Spec.Refresh == "onDashboardLoad" || v.Spec.Refresh == "onTimeRangeChanged"; v.Spec.Regex != "" || !refreshes {
		t.Errorf("the instance variable has regex %q and refresh %q: a regex, or a refresh of never, which an omitted refresh defaults to, hides instances its query lists", v.Spec.Regex, v.Spec.Refresh)
	}
}

// TestDashboardVariableDefinitionsMatchTheirQueries fails when a query
// variable's definition, the text Grafana's variable editor shows, differs
// from the query it runs.
func TestDashboardVariableDefinitionsMatchTheirQueries(t *testing.T) {
	d := loadDashboard(t)
	for _, v := range d.Spec.Variables {
		if q := variableQuery(&v); v.Kind == "QueryVariable" && v.Spec.Definition != q {
			t.Errorf("variable %s shows definition\n%s\nbut runs\n%s", v.Spec.Name, v.Spec.Definition, q)
		}
	}
}

// TestDashboardDatabasesTableShowsEachDatabase fails when the Databases table
// stops reading a database's latest result, dump age, size, duration or
// failures, when a filter hides the held size, when a short range or a
// restart blanks its duration, or when its dump age turns yellow at other than
// twice the default DUMP_INTERVAL.
func TestDashboardDatabasesTableShowsEachDatabase(t *testing.T) {
	d := loadDashboard(t)
	var databases *dashPanel
	for _, it := range row(t, &d, "Latest run").Spec.Layout.Spec.Items {
		if p := d.Spec.Elements[it.Spec.Element.Name]; p.Spec.Title == "Databases" {
			databases = &p
		}
	}
	if databases == nil || databases.Spec.VizConfig.Group != "table" {
		t.Fatal("no Databases table in the Latest run row")
	}
	exprs := strings.Join(panelExprs(databases), "\n")
	read := []string{
		"pg_autodump_dump_ok", "pg_autodump_newest_dump_timestamp_seconds", "pg_autodump_dump_bytes",
		"pg_autodump_dump_duration_seconds", "pg_autodump_dumps_total",
	}
	for _, series := range read {
		if !strings.Contains(exprs, series) {
			t.Errorf("Databases reads no %s", series)
		}
	}
	// dump_bytes holds the newest dump's size, so a filter on it only hides
	// a real value. NewRegistry seeds dump_ok 1 at each start, which no dump
	// has measured, and a restart clears dump_duration_seconds until the next
	// attempt, which can be older than a short selected range.
	seeds := map[string]*regexp.Regexp{
		"Size":        regexp.MustCompile(`(?m)^max by \(server, db\) \(pg_autodump_dump_bytes\{[^}]*\}\)$`),
		"Duration":    regexp.MustCompile(`last_over_time\(pg_autodump_dump_duration_seconds\{[^}]*\}\[\$__range\]\)\s+or\s+last_over_time\(pg_autodump_dump_duration_seconds\{[^}]*\}\[30d\]\)`),
		"Latest dump": regexp.MustCompile(`pg_autodump_dump_ok\{[^}]*\}\s*==\s*0\s+or\s+\(\s*pg_autodump_dump_ok\{[^}]*\}\s+and\s+on\s*\(\s*server,\s*db\s*\)\s*pg_autodump_newest_dump_timestamp_seconds\{`),
	}
	for column, want := range seeds {
		if !want.MatchString(exprs) {
			t.Errorf("%s shows a value no dump measured, or none after a restart, want a query matching %v", column, want)
		}
	}
	var yellow []float64
	for _, o := range databases.Spec.VizConfig.Spec.FieldConfig.Overrides {
		if o.Matcher.Options != "Newest dump age" {
			continue
		}
		for _, prop := range o.Properties {
			if prop.ID != "thresholds" {
				continue
			}
			raw, _ := json.Marshal(prop.Value)
			var th struct {
				Steps []struct {
					Value *float64 `json:"value"`
					Color string   `json:"color"`
				} `json:"steps"`
			}
			if err := json.Unmarshal(raw, &th); err != nil {
				t.Fatalf("Newest dump age thresholds: %v", err)
			}
			for _, st := range th.Steps {
				if st.Color == "yellow" && st.Value != nil {
					yellow = append(yellow, *st.Value)
				}
			}
		}
	}
	if want := 2 * config.DefaultDumpInterval.Seconds(); !slices.Equal(yellow, []float64{want}) {
		t.Errorf("Newest dump age turns yellow at %v s, want %v s, twice the default DUMP_INTERVAL", yellow, want)
	}
}

// TestDashboardSizeChartStacksOneBarPerCycle fails when the Dump size per
// database chart stops being one stacked bar per dump cycle: dump_bytes read
// with no filter, since the gauge holds the newest dump's size between dumps,
// at the step whose slot holds the last cycle's end, so a restart, which reads
// the same end back, draws no bar, joined on time so each cycle's databases
// stack in one bar.
func TestDashboardSizeChartStacksOneBarPerCycle(t *testing.T) {
	d := loadDashboard(t)
	const want = `pg_autodump_dump_bytes{instance="$instance"} and on (instance) ` +
		`(pg_autodump_last_cycle_timestamp_seconds{instance="$instance"} > time() - ${__interval_ms} / 1000)`
	charts := 0
	for name, p := range d.Spec.Elements {
		if p.Spec.Title != "Dump size per database" {
			continue
		}
		charts++
		if exprs := panelExprs(&p); !slices.Equal(exprs, []string{want}) {
			t.Errorf("%s reads %q, want only\n%s", name, exprs, want)
		}
		if g := p.Spec.VizConfig.Group; g != "barchart" {
			t.Errorf("%s is a %s, want a barchart", name, g)
		}
		if o := p.Spec.VizConfig.Spec.Options; o.Stacking != "normal" || o.XField != "Time" {
			t.Errorf("%s stacks %q along %q, want normal stacking along Time", name, o.Stacking, o.XField)
		}
		if tr := p.Spec.Data.Spec.Transformations; len(tr) != 1 || tr[0].Group != "joinByField" {
			t.Errorf("%s transforms with %v, want one joinByField, so each cycle is one row", name, tr)
		}
	}
	if charts != 1 {
		t.Fatalf("found %d Dump size per database charts, want 1", charts)
	}
}

var aggregationRe = regexp.MustCompile(`^(?:sum|min|max|avg|count|group)\b\s*`)

var byClauseRe = regexp.MustCompile(`^by\s*\(([^)]*)\)\s*`)

// closingParen is the index of the parenthesis closing the one s starts with,
// or -1.
func closingParen(s string) int {
	if !strings.HasPrefix(s, "(") {
		return -1
	}
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// outerGrouping is the sorted by-labels of expr's outermost aggregation, or
// false when the outermost operation is not an aggregation with a by clause.
// A round() around the whole expression is looked through, since it keeps
// every label.
func outerGrouping(expr string) ([]string, bool) {
	rest := strings.TrimSpace(expr)
	if inner, ok := strings.CutPrefix(rest, "round"); ok {
		inner = strings.TrimSpace(inner)
		if end := closingParen(inner); end >= 0 && strings.TrimSpace(inner[end+1:]) == "" {
			return outerGrouping(inner[1:end])
		}
	}
	agg := aggregationRe.FindString(rest)
	if agg == "" {
		return nil, false
	}
	rest = rest[len(agg):]
	var by []string
	readBy := func() {
		if g := byClauseRe.FindStringSubmatch(rest); g != nil {
			for l := range strings.SplitSeq(g[1], ",") {
				if l = strings.TrimSpace(l); l != "" {
					by = append(by, l)
				}
			}
			rest = rest[len(g[0]):]
		}
	}
	readBy()
	end := closingParen(rest)
	if end < 0 {
		return nil, false
	}
	rest = strings.TrimSpace(rest[end+1:])
	if by == nil {
		readBy()
	}
	if by == nil || rest != "" {
		return nil, false
	}
	slices.Sort(by)
	return by, true
}

// TestDashboardMergedTablesJoinEachRowOnItsKey fails when a table that merges
// its queries has a query that is not an aggregation by the same labels as the
// others. Grafana's merge joins rows only on the fields every frame shares and
// splits a row whose other shared fields differ, so a bare selector's __name__
// gives one database several rows:
// https://github.com/grafana/grafana/blob/main/packages/grafana-data/src/transformations/transformers/merge.ts
func TestDashboardMergedTablesJoinEachRowOnItsKey(t *testing.T) {
	d := loadDashboard(t)
	merged := 0
	for name, p := range d.Spec.Elements {
		if !slices.ContainsFunc(p.Spec.Data.Spec.Transformations, func(tr transformation) bool { return tr.Group == "merge" }) {
			continue
		}
		merged++
		var key []string
		for i, e := range panelExprs(&p) {
			by, ok := outerGrouping(e)
			switch {
			case !ok:
				t.Errorf("%s %q query %d is not an aggregation by the table's key: %s", name, p.Spec.Title, i, e)
			case key == nil:
				key = by
			case !slices.Equal(by, key):
				t.Errorf("%s %q query %d aggregates by %v, an earlier query by %v", name, p.Spec.Title, i, by, key)
			}
		}
	}
	if merged == 0 {
		t.Error("no table merges its queries; the check no longer reads the dashboard")
	}
}

// TestDashboardRowHeadersFoldTwoPanels fails when a row shows a header over
// fewer than two always-shown panels, which costs a click and hides nothing.
// A row of tiles shown only with data needs two of them, since it lights
// several.
func TestDashboardRowHeadersFoldTwoPanels(t *testing.T) {
	d := loadDashboard(t)
	for _, r := range d.Spec.Layout.Spec.Rows {
		if r.Spec.HideHeader {
			continue
		}
		shown, gated := 0, 0
		for _, it := range r.Spec.Layout.Spec.Items {
			if it.Spec.ConditionalRendering == nil {
				shown++
			} else {
				gated++
			}
		}
		if shown < 2 && (shown > 0 || gated < 2) {
			t.Errorf("row %q shows a header over %d always-shown panels and %d shown only with data, want two of one kind or hideHeader",
				r.Spec.Title, shown, gated)
		}
	}
}

func TestDashboardPanelsExplainThemselves(t *testing.T) {
	d := loadDashboard(t)
	if !d.Spec.Preload {
		t.Error("spec.preload = false, want true so a hidden tile below the fold still queries")
	}
	if d.Spec.Layout.Kind != "RowsLayout" || len(d.Spec.Layout.Spec.Rows) == 0 {
		t.Errorf("layout is %s, want one RowsLayout", d.Spec.Layout.Kind)
	}
	for name, p := range d.Spec.Elements {
		if strings.TrimSpace(p.Spec.Description) == "" {
			t.Errorf("%s %q has no description", name, p.Spec.Title)
		}
		switch p.Spec.VizConfig.Group {
		case "stat", "gauge", "bargauge", "table":
			if strings.TrimSpace(p.Spec.VizConfig.Spec.FieldConfig.Defaults.NoValue) == "" {
				t.Errorf("%s %q has no no-value text", name, p.Spec.Title)
			}
		}
	}
}

const spanTitle = "${span} ${spanunit}"

// rangeWordVars are the variables a title names the range length with.
var rangeWordVars = []string{"span", "spanunit", "spanthe"}

// customText is the text Grafana gives a custom variable whose query is query,
// which titles render. The parse follows CustomVariable.transformCsvStringToOptions,
// https://github.com/grafana/scenes/blob/main/packages/scenes/src/variables/variants/CustomVariable.tsx.
// More than one option would let Grafana keep a stale choice, so it fails.
func customText(t *testing.T, query string) string {
	t.Helper()
	entries := regexp.MustCompile(`(?:\\,|[^,])+`).FindAllString(query, -1)
	if len(entries) != 1 {
		t.Errorf("custom query %q gives %d options, want 1", query, len(entries))
		return ""
	}
	if m := regexp.MustCompile(`^\s*(.+)\s:\s(.+)$`).FindStringSubmatch(entries[0]); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(entries[0])
}

// rangeWords is the text of each range word variable once spanread holds the
// option Grafana makes of row, or none at all (""), as on a failed first read.
func rangeWords(t *testing.T, vars map[string]dashVariable, row string) map[string]string {
	t.Helper()
	read := vars["spanread"]
	rx, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(read.Spec.Regex, "/"), "/"))
	if err != nil || rx.SubexpIndex("value") < 0 || rx.SubexpIndex("text") < 0 {
		t.Fatalf("spanread regex %q: %v, want value and text groups", read.Spec.Regex, err)
	}
	var value, text string
	if row != "" {
		m := rx.FindStringSubmatch(row)
		if m == nil {
			t.Fatalf("spanread regex %v does not match %s", rx, row)
		}
		value, text = m[rx.SubexpIndex("value")], m[rx.SubexpIndex("text")]
	}
	fill := strings.NewReplacer("${spanread:text}", text, "${spanread}", value)
	words := map[string]string{}
	for _, name := range rangeWordVars {
		var query string
		_ = json.Unmarshal(vars[name].Spec.Query, &query)
		words[name] = customText(t, fill.Replace(query))
	}
	return words
}

// panelNames are a panel's title and the display names its overrides give
// its fields, such as a table column's header.
func panelNames(p *dashPanel) []string {
	names := []string{p.Spec.Title}
	for _, o := range p.Spec.VizConfig.Spec.FieldConfig.Overrides {
		for _, prop := range o.Properties {
			if s, ok := prop.Value.(string); ok && prop.ID == "displayName" {
				names = append(names, s)
			}
		}
	}
	return names
}

// A figure over the time range reads only with the range's length, so every
// panel outside the charts whose query reads $__range names it through span
// and spanunit, in its title or a column header. Every range word is a custom
// variable over spanread's one answer, and reads "the range" until spanread
// has read a length.
func TestDashboardRangeFiguresNameTheRangeLength(t *testing.T) {
	d := loadDashboard(t)
	vars := d.variables()
	read, ok := vars["spanread"]
	if !ok {
		t.Fatal("variable spanread missing, want one read of the range length for every range word")
	}
	if read.Kind != "QueryVariable" || read.Spec.Hide != "hideVariable" || read.Spec.Refresh != "onTimeRangeChanged" || !read.Spec.SkipURLSync ||
		read.Spec.Current.Text != "" || read.Spec.Current.Value != "" {
		t.Errorf("spanread is %s hide %q refresh %q, want a hidden QueryVariable refreshed on time range change, saved empty, not in the URL",
			read.Kind, read.Spec.Hide, read.Spec.Refresh)
	}
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		if name != "spanread" && strings.Contains(string(vars[name].Spec.Query), "$__range_s") {
			t.Errorf("variable %s reads $__range_s, want spanread alone to read the range length", name)
		}
	}
	for _, name := range rangeWordVars {
		if v := vars[name]; v.Kind != "CustomVariable" || v.Spec.Hide != "hideVariable" || !v.Spec.SkipURLSync {
			t.Errorf("variable %s is %q hide %q, want a hidden CustomVariable kept out of the URL", name, v.Kind, v.Spec.Hide)
		}
	}
	if got, want := rangeWords(t, vars, ""), map[string]string{"span": "the", "spanunit": "range", "spanthe": "the"}; !maps.Equal(got, want) {
		t.Errorf("range words before spanread reads a length are %v, want %v", got, want)
	}
	query := variableQuery(&read)
	for _, c := range []struct {
		rangeS     int
		span, unit string
	}{
		{45, "45", "seconds"},
		{120, "2", "minutes"},
		{7199, "120", "minutes"},
		{7200, "2", "hours"},
		{19980, "5.6", "hours"},
		{172799, "48", "hours"},
		{172800, "2", "days"},
		{2592000, "30", "days"},
	} {
		got := rangeWords(t, vars, queryResultRow(t, query, c.rangeS))
		if want := map[string]string{"span": c.span, "spanunit": c.unit, "spanthe": "the " + c.span}; !maps.Equal(got, want) {
			t.Errorf("range words at a %d s range are %v, want %v", c.rangeS, got, want)
		}
	}
	figures := 0
	for name, p := range d.Spec.Elements {
		if g := p.Spec.VizConfig.Group; g == "timeseries" || g == "state-timeline" {
			continue
		}
		if !slices.ContainsFunc(panelExprs(&p), func(e string) bool { return strings.Contains(e, "$__range") }) {
			continue
		}
		figures++
		if !slices.ContainsFunc(panelNames(&p), func(n string) bool { return strings.Contains(n, spanTitle) }) {
			t.Errorf("%s %q reads $__range, want its title or a column to name %s", name, p.Spec.Title, spanTitle)
		}
	}
	if figures == 0 {
		t.Error("no range figure found; the check no longer reads the dashboard")
	}
}
