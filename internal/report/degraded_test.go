package report_test

import (
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// repoAt keeps the fixture rows below readable. Paths are filled in because a
// configured repo always has one, and a row that omitted it would describe a
// shape the config loader cannot produce.
func repoAt(id int64, name string) store.Repo {
	return store.Repo{ID: id, Name: name, Path: "/repos/" + name}
}

// The columns of the every-repo table, in the order the template emits them.
// The name and the last-collected column are deliberately absent: a repo name
// may legitimately contain a digit and a timestamp is nothing but digits, so
// neither can be judged by the rule below.
var degradedColumns = [...]string{
	"coverage",
	"coverage change",
	"tests",
	"tests change",
	"packages without tests",
}

// cellSpec says what one table cell is allowed to be.
//
// An unmeasured cell is asserted to contain no ASCII digit at all rather than
// to match a particular phrase. One predicate then covers "not collected",
// "not measured" and "no baseline yet" alike, survives a rewording of any of
// them, and covers a degraded state nobody has invented yet. Matching the
// wording instead would pass the day someone adds a sixth degraded state that
// renders 0.0%.
type cellSpec struct {
	measured bool
	// want is the exact rendered text, and is only consulted for a measured
	// cell. Exact equality there is the anti-vacuity control: a test that only
	// ever asserted absence would pass on a template that printed nothing.
	want string
}

func unmeasured() cellSpec        { return cellSpec{} }
func measured(s string) cellSpec  { return cellSpec{measured: true, want: s} }
func (c cellSpec) hasDigit() bool { return strings.ContainsFunc(c.want, unicode.IsDigit) }
func hasDigit(cell string) bool   { return strings.ContainsFunc(cell, unicode.IsDigit) }
func trimCell(cell string) string { return strings.TrimSpace(cell) }
func sortedNames(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

type degradedRow struct {
	name string
	// why names the failure this row guards, so a failure message says what
	// broke rather than which index of a table did.
	why string
	in  delta.Input
	// cells lines up with degradedColumns.
	cells [len(degradedColumns)]cellSpec
	// mover is whether this repo may lead the report. Every degraded state
	// answers no, and two of them are marked movers by delta.Compute, so the
	// report layer is the only thing standing between them and the headline.
	mover bool
	// moverBeforeFiltering asserts the fixture actually reaches the filter.
	// Without it, "not in Movers" would pass for rows that were never
	// candidates, which is most of them.
	moverBeforeFiltering bool
}

const (
	repoNever        = "neverrepo"
	repoFailedOnly   = "failedrepo"
	repoFailedAfter  = "failedhistoryrepo"
	repoIngest       = "ingestrepo"
	repoFirstRun     = "firstrunrepo"
	repoEmptyProfile = "emptyprofilerepo"
	repoPartial      = "partialrepo2"
	repoHealthy      = "healthyrepo"
)

func degradedRows() []degradedRow {
	return []degradedRow{
		{
			name: repoNever,
			why:  "configured but never collected: every count on it is a Go zero value, not a measurement",
			in:   delta.Input{Repo: repoAt(1, repoNever)},
			cells: [len(degradedColumns)]cellSpec{
				unmeasured(), unmeasured(), unmeasured(), unmeasured(), unmeasured(),
			},
		},
		{
			name: repoFailedOnly,
			why:  "the run came back with nothing and there is no history either",
			in: delta.Input{
				Repo: repoAt(2, repoFailedOnly),
				Head: snap(21, 2, "go1.26.5", store.StatusFailed, "test command exited 2"),
			},
			cells: [len(degradedColumns)]cellSpec{
				unmeasured(), unmeasured(), unmeasured(), unmeasured(), unmeasured(),
			},
		},
		{
			name: repoFailedAfter,
			why:  "a failed run over a healthy baseline: subtracting the baseline from its zeros invents a cliff",
			in: delta.Input{
				Repo:        repoAt(3, repoFailedAfter),
				Head:        snap(31, 3, "go1.26.5", store.StatusFailed, "coverage profile was stale"),
				Base:        snap(30, 3, "go1.26.5", store.StatusOK, ""),
				BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0, testCount(pkgAlpha, 40))),
			},
			cells: [len(degradedColumns)]cellSpec{
				unmeasured(), unmeasured(), unmeasured(), unmeasured(), unmeasured(),
			},
			moverBeforeFiltering: true,
		},
		{
			name: repoIngest,
			why:  "ingest mode parses no test stream, so the test columns were never looked at",
			in: delta.Input{
				Repo:        repoAt(4, repoIngest),
				Head:        snap(41, 4, "go1.26.5", store.StatusOK, ""),
				HeadMetrics: cov(pkgAlpha, 60, 100),
				Base:        snap(40, 4, "go1.26.5", store.StatusOK, ""),
				BaseMetrics: cov(pkgAlpha, 50, 100),
			},
			cells: [len(degradedColumns)]cellSpec{
				measured("60.0%"), measured("+10.0 pts"), unmeasured(), unmeasured(), unmeasured(),
			},
			mover:                true,
			moverBeforeFiltering: true,
		},
		{
			name: repoFirstRun,
			why:  "first ever run: the numbers are real but there is nothing to compare them against",
			in: delta.Input{
				Repo:        repoAt(5, repoFirstRun),
				Head:        snap(51, 5, "go1.26.5", store.StatusOK, ""),
				HeadMetrics: metrics(cov(pkgAlpha, 10, 100), testStream(0, testCount(pkgAlpha, 1))),
			},
			cells: [len(degradedColumns)]cellSpec{
				measured("10.0%"), unmeasured(), measured("1"), unmeasured(), measured("0"),
			},
		},
		{
			// The fifth instance of this project's recurring bug. A coverage
			// profile carrying only a "mode: set" header parses clean and
			// produces no per-package metrics, and nothing downgrades the
			// status, so the snapshot is stored ok. Coverage.Pct() then returns
			// 0 on a zero total and the row reads 0.0%, and over a healthy
			// baseline the repo leads the report as the week's biggest drop.
			// That is the fabricated cliff the failed status was fixed for,
			// reached through a different door.
			name: repoEmptyProfile,
			why:  "a header-only coverage profile stores no coverage metrics, so 0.0% is a number nobody measured",
			in: delta.Input{
				Repo:        repoAt(6, repoEmptyProfile),
				Head:        snap(61, 6, "go1.26.5", store.StatusOK, ""),
				HeadMetrics: testStream(0, testCount(pkgAlpha, 5)),
				Base:        snap(60, 6, "go1.26.5", store.StatusOK, ""),
				BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0, testCount(pkgAlpha, 5))),
			},
			cells: [len(degradedColumns)]cellSpec{
				unmeasured(), unmeasured(), measured("5"), measured("+0"), measured("0"),
			},
			moverBeforeFiltering: true,
		},
		{
			// The control that stops the invariant being written as
			// "status != ok blanks the row". A partial run carries real
			// numbers and they stay in the report.
			name: repoPartial,
			why:  "a partial run collected real numbers and must keep showing them",
			in: delta.Input{
				Repo:        repoAt(7, repoPartial),
				Head:        snap(71, 7, "go1.26.5", store.StatusPartial, "test command exited 1"),
				HeadMetrics: metrics(cov(pkgAlpha, 33, 100), testStream(2, testCount(pkgAlpha, 7))),
			},
			cells: [len(degradedColumns)]cellSpec{
				measured("33.0%"), unmeasured(), measured("7"), unmeasured(), measured("2"),
			},
		},
		{
			name: repoHealthy,
			why:  "the healthy control: if this row blanks, the invariant is over-broad",
			in: delta.Input{
				Repo:        repoAt(8, repoHealthy),
				Head:        snap(81, 8, "go1.26.5", store.StatusOK, ""),
				HeadMetrics: metrics(cov(pkgAlpha, 80, 100), testStream(1, testCount(pkgAlpha, 9))),
				Base:        snap(80, 8, "go1.26.5", store.StatusOK, ""),
				BaseMetrics: metrics(cov(pkgAlpha, 75, 100), testStream(1, testCount(pkgAlpha, 8))),
			},
			cells: [len(degradedColumns)]cellSpec{
				measured("80.0%"), measured("+5.0 pts"), measured("9"), measured("+1"), measured("1"),
			},
			mover:                true,
			moverBeforeFiltering: true,
		},
	}
}

// TestDegradedStatesNeverRenderAnUnmeasuredNumber is the class-level guard, as
// opposed to the per-instance tests beside it.
//
// One bug has appeared five times in this project: something unmeasured
// presented as a measurement of zero. A never-collected repo at 0.0%, a repo
// whose every run failed rendered the same way, test counts of 0 for a repo
// nobody counted, untested packages at 0 under -coverpkg, and a header-only
// coverage profile stored ok at 0.0%. This asserts the shared rule instead of
// the five instances: no cell of the table may carry a digit unless something
// actually measured it.
func TestDegradedStatesNeverRenderAnUnmeasuredNumber(t *testing.T) {
	rows := degradedRows()

	// The table must not be able to lie about itself. A row that declares a
	// cell measured but gives an expected value with no digit in it would
	// satisfy both halves of the rule at once and prove nothing.
	for _, row := range rows {
		for i, c := range row.cells {
			if c.measured && !c.hasDigit() {
				t.Fatalf("fixture is wrong: %s declares the %s column measured but expects %q, which carries no number",
					row.name, degradedColumns[i], c.want)
			}
			if !c.measured && c.want != "" {
				t.Fatalf("fixture is wrong: %s declares the %s column unmeasured but also expects the text %q",
					row.name, degradedColumns[i], c.want)
			}
		}
	}

	inputs := make([]delta.Input, 0, len(rows))
	for _, row := range rows {
		inputs = append(inputs, row.in)
	}
	rep := delta.Compute(inputs, options(), fixedNow())
	md := mustMarkdown(t, rep)
	wire := jsonRepoRows(t, rep)

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cells := tableCells(t, md, row.name)
			for i, want := range row.cells {
				got := cells[i]
				switch {
				case want.measured && got != want.want:
					t.Errorf("%s (%s): %s column reads %q, want exactly %q\nfull row: %s",
						row.name, row.why, degradedColumns[i], got, want.want, repoRow(t, md, row.name))
				case !want.measured && hasDigit(got):
					t.Errorf("%s (%s): %s column reads %q, which is a number nobody measured\nfull row: %s",
						row.name, row.why, degradedColumns[i], got, repoRow(t, md, row.name))
				}
			}

			// The gates are the only thing a JSON consumer can key a blank off,
			// so they have to agree with what the markdown decided to print.
			gate := wire[row.name]
			if gate == nil {
				t.Fatalf("%s is missing from the json entirely", row.name)
			}
			if got := gate["coverage_measured"]; got != row.cells[0].measured {
				t.Errorf("%s coverage_measured: got %v, want %v to match the rendered coverage column",
					row.name, got, row.cells[0].measured)
			}
			if got := gate["tests_measured"]; got != row.cells[2].measured {
				t.Errorf("%s tests_measured: got %v, want %v to match the rendered tests column",
					row.name, got, row.cells[2].measured)
			}
		})
	}

	// Second surface: the same fabricated numbers reach the headline through
	// delta.Compute, which sets IsMover from a subtraction that has no opinion
	// about whether either side was measured.
	var wantMovers, mustBeCandidates []string
	for _, row := range rows {
		if row.mover {
			wantMovers = append(wantMovers, row.name)
		}
		if row.moverBeforeFiltering {
			mustBeCandidates = append(mustBeCandidates, row.name)
		}
	}

	var candidates []string
	for _, r := range rep.Movers() {
		candidates = append(candidates, r.Repo.Name)
	}
	// Without this the Movers assertion below would pass for rows that were
	// never candidates in the first place, which is most of them.
	if got, want := sortedNames(candidates), sortedNames(mustBeCandidates); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fixture is wrong: delta.Compute marks %v as movers, want %v; the degraded rows have to reach the report filter for its exclusion to mean anything", got, want)
	}

	var got []string
	for _, m := range report.Build(rep).Movers {
		got = append(got, m.Name)
	}
	if a, b := sortedNames(got), sortedNames(wantMovers); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("movers: got %v, want %v; a degraded repo leading the report is the fabricated cliff, and dropping a healthy one is the over-broad fix for it", a, b)
	}

	t.Logf("degraded-state report:\n%s", md)
}

// tableCells splits one repo's table row into its cells, minus the name and the
// last-collected column. Those two are exempt from the digit rule: a repo name
// may carry a digit and a timestamp is nothing but digits.
func tableCells(t *testing.T, md, name string) [len(degradedColumns)]string {
	t.Helper()
	row := repoRow(t, md, name)
	parts := strings.Split(row, "|")
	// A leading and a trailing empty element from the outer pipes, plus name,
	// five columns under test, and last collected.
	if len(parts) != len(degradedColumns)+4 {
		t.Fatalf("%s row has %d cells, want %d; the table shape changed and this test has to be told about it:\n%s",
			name, len(parts)-2, len(degradedColumns)+2, row)
	}
	var out [len(degradedColumns)]string
	for i := range out {
		out[i] = trimCell(parts[i+2])
	}
	return out
}

// fieldKind is how a consumer of the JSON has to treat one key.
type fieldKind string

const (
	// kindMeasurement is a number that may or may not have been measured. Every
	// one of them has to name the gate a consumer reads first.
	kindMeasurement fieldKind = "measurement"
	// kindGate is a boolean saying whether some measurement happened.
	kindGate fieldKind = "gate"
	// kindContext is everything else: names, statuses, lists, error text.
	kindContext fieldKind = "context"
)

type fieldClass struct {
	kind fieldKind
	// gates are the boolean keys that say whether this number means anything.
	// A consumer that charts the number without reading these plots a zero
	// nobody measured, which is the whole bug class this file exists for.
	gates []string
}

// repoViewFields is written by hand on purpose.
//
// Deriving it from the struct would make it agree with the code automatically,
// and automatic agreement is exactly what must not happen here: the point is
// that a human has to decide what a new number means before it can ship. A new
// field lands on the wire, fails this test, and the author has to come here and
// say whether it is a measurement and what gates it.
var repoViewFields = map[string]fieldClass{
	"name":         {kind: kindContext},
	"status":       {kind: kindContext},
	"collected_at": {kind: kindContext},
	"error":        {kind: kindContext},
	"env_changed":  {kind: kindContext},
	// Culprits and package churn are lists. An empty list is not a claim about
	// a number, so they need no gate.
	"culprits":         {kind: kindContext},
	"added_packages":   {kind: kindContext},
	"removed_packages": {kind: kindContext},

	"has_snapshot":      {kind: kindGate},
	"has_baseline":      {kind: kindGate},
	"tests_measured":    {kind: kindGate},
	"coverage_measured": {kind: kindGate},

	"coverage_pct":       {kind: kindMeasurement, gates: []string{"coverage_measured"}},
	"covered_statements": {kind: kindMeasurement, gates: []string{"coverage_measured"}},
	"total_statements":   {kind: kindMeasurement, gates: []string{"coverage_measured"}},
	// A delta needs both a measured head and something to compare it against,
	// so it names two gates rather than one.
	"coverage_delta_points":  {kind: kindMeasurement, gates: []string{"coverage_measured", "has_baseline"}},
	"tests":                  {kind: kindMeasurement, gates: []string{"tests_measured"}},
	"tests_delta":            {kind: kindMeasurement, gates: []string{"tests_measured", "has_baseline"}},
	"packages_without_tests": {kind: kindMeasurement, gates: []string{"tests_measured"}},
}

// TestEveryNumericFieldIsClassified is the fail-closed half of this file, and
// the load-bearing one.
//
// The table above it asserts today's fields render honestly. That catches the
// five instances of this bug that have already happened and would miss the
// sixth, because the sixth will almost certainly be a new number on RepoView
// that nobody classified, and a table of today's columns simply would not
// mention it. This does: any key that reaches the wire without an entry here
// fails, and the failure asks the author the one question that matters.
func TestEveryNumericFieldIsClassified(t *testing.T) {
	rows := degradedRows()
	inputs := make([]delta.Input, 0, len(rows))
	for _, row := range rows {
		inputs = append(inputs, row.in)
	}
	// The union across every state, because error is omitempty and only a
	// failed row carries it. A census taken from one healthy row would declare
	// a real key rot.
	wire := map[string]bool{}
	for name, row := range jsonRepoRows(t, delta.Compute(inputs, options(), fixedNow())) {
		if len(row) == 0 {
			t.Fatalf("%s rendered an empty json object", name)
		}
		for key := range row {
			wire[key] = true
		}
	}

	for key := range wire {
		if _, ok := repoViewFields[key]; !ok {
			t.Errorf("RepoView puts %q on the wire and nothing here says what it is. Add it to repoViewFields: is it a measurement, and if so which gate tells a consumer it was actually measured?", key)
		}
	}
	for key := range repoViewFields {
		if !wire[key] {
			t.Errorf("repoViewFields classifies %q but no rendered row carries it, so the classification is rot. Either the field was removed or the fixture no longer reaches the state that emits it.", key)
		}
	}

	for key, class := range repoViewFields {
		if class.kind != kindMeasurement {
			if len(class.gates) != 0 {
				t.Errorf("%q is classified %s but names gates; only a measurement needs one", key, class.kind)
			}
			continue
		}
		if len(class.gates) == 0 {
			t.Errorf("%q is a measurement with no gate, so a consumer has no way to tell a measured zero from an unmeasured one", key)
		}
		for _, gate := range class.gates {
			g, ok := repoViewFields[gate]
			if !ok {
				t.Errorf("%q names the gate %q, which is not a field at all", key, gate)
				continue
			}
			if g.kind != kindGate {
				t.Errorf("%q names %q as its gate, but %q is classified %s", key, gate, gate, g.kind)
			}
		}
	}

	// Pin the count as well as the membership. RepoView carries exactly seven
	// numbers today; the assertion above would still pass if a future edit
	// reclassified one of them as context to quiet a failure, and this would
	// not.
	var measurements []string
	for key, class := range repoViewFields {
		if class.kind == kindMeasurement {
			measurements = append(measurements, key)
		}
	}
	want := []string{
		"coverage_delta_points", "coverage_pct", "covered_statements",
		"packages_without_tests", "tests", "tests_delta", "total_statements",
	}
	if got := strings.Join(sortedNames(measurements), ","); got != strings.Join(want, ",") {
		t.Errorf("measurement fields: got %v, want %v. A number moved buckets, which is a decision worth making on purpose rather than to quiet a test.", sortedNames(measurements), want)
	}
}
