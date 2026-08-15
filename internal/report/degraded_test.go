package report_test

import (
	"fmt"
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
			// delta.Compute now excludes this itself: a failed head stored no
			// coverage, so CoverageChangeMeaningful is false and IsMover never
			// fires. It used to reach the report and be dropped there. The
			// report filter is now a backstop rather than the only guard, and
			// TestBuildDropsAMoverThatMeasuredNothing exercises it directly so
			// the backstop does not rot untested.
			moverBeforeFiltering: false,
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
			// Same as the failed row above: delta stopped marking this a mover
			// once the coverage half of IsMover got the guard the test half
			// already had. See TestBuildDropsAMoverThatMeasuredNothing.
			moverBeforeFiltering: false,
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

			// The two renderers have to have made the same call. The markdown
			// blanks a cell; the JSON drops the whole group. If those ever come
			// apart, one format is publishing a number the other refused to.
			//
			// This used to compare against the coverage_measured and
			// tests_measured booleans. They are gone: the group being null says
			// it structurally, and a boolean beside it was a second source of
			// truth with nothing keeping the two in step.
			wireRow := wire[row.name]
			if wireRow == nil {
				t.Fatalf("%s is missing from the json entirely", row.name)
			}
			for _, group := range []struct {
				key    string
				column int
			}{{"coverage", 0}, {"tests", 2}} {
				v, present := wireRow[group.key]
				if !present {
					t.Errorf("%s: the %s key is missing from the json rather than null. A consumer that never sees the key defaults it, and the zeros come straight back.",
						row.name, group.key)
					continue
				}
				if got := v != nil; got != row.cells[group.column].measured {
					t.Errorf("%s %s: json group present=%v, but the markdown %s column reads %q. The two renderers disagree about whether anything was measured.",
						row.name, group.key, got, degradedColumns[group.column], cells[group.column])
				}
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

// fieldKind is how a consumer of the JSON has to treat one path.
type fieldKind string

const (
	// kindGroup is a nullable object that holds measurements and everything
	// derived from them. It is the only place a number is allowed to be, and it
	// renders as null whenever nothing measured it.
	kindGroup fieldKind = "group"
	// kindMeasurement is a number. Every one of them has to be inside a group,
	// so a consumer cannot reach it at all unless something measured it.
	kindMeasurement fieldKind = "measurement"
	// kindContext is everything else: names, statuses, booleans, lists, error
	// text. A context path may never render as a number.
	kindContext fieldKind = "context"
)

// repoWireFields is the census, and it is written by hand on purpose.
//
// Deriving it from the struct would make it agree with the code automatically,
// and automatic agreement is exactly what must not happen here: the point is
// that a human has to decide what a new number means before it can ship. A new
// field lands on the wire, fails this test, and the author has to come here and
// say what it is.
//
// It replaces a flat map in which each measurement named the boolean gate that
// governed it. The gates are gone and the numbers moved inside nullable groups,
// so the question the census asks got stricter rather than merely different:
// not "does every number name a gate that exists" but "is every number inside a
// nullable group, and is every group actually nullable". A number that ends up
// ungrouped now fails the build instead of needing a reviewer to notice.
//
// Paths use a dot for an object key and [] for a list element.
var repoWireFields = map[string]fieldKind{
	"name":         kindContext,
	"status":       kindContext,
	"collected_at": kindContext,
	"error":        kindContext,
	// These three are booleans, and none of them gates a number any more. They
	// say which kind of nothing a null group is, alongside status.
	"has_snapshot": kindContext,
	"has_baseline": kindContext,
	"env_changed":  kindContext,

	"coverage":              kindGroup,
	"coverage.pct":          kindMeasurement,
	"coverage.covered":      kindMeasurement,
	"coverage.total":        kindMeasurement,
	"coverage.delta_points": kindMeasurement,
	// The culprits and the churn lists live inside the coverage group because
	// they are coverage findings. Keeping them outside would put four numbers
	// (a culprit's two percentages, its contribution and its statement count)
	// beyond the reach of the rule below, and the rule would have needed an
	// exception clause for lists of objects. An exception clause is how this
	// guard gets quietly relaxed.
	"coverage.culprits":                       kindContext,
	"coverage.culprits[].package":             kindContext,
	"coverage.culprits[].state":               kindContext,
	"coverage.culprits[].from_pct":            kindMeasurement,
	"coverage.culprits[].to_pct":              kindMeasurement,
	"coverage.culprits[].contribution_points": kindMeasurement,
	"coverage.culprits[].statements":          kindMeasurement,
	"coverage.added_packages":                 kindContext,
	"coverage.removed_packages":               kindContext,

	"tests":                        kindGroup,
	"tests.count":                  kindMeasurement,
	"tests.delta":                  kindMeasurement,
	"tests.packages_without_tests": kindMeasurement,
}

// wireCensus is what walking the rendered JSON learned.
type wireCensus struct {
	// paths is every key path that reached the wire, across every degraded
	// state, so a classification that no longer matches anything can be spotted.
	paths map[string]bool
	// nulled and filled are the anti-vacuity control for groups. A group that no
	// fixture ever renders as null has not been shown to be nullable, and one no
	// fixture ever fills in means the fixture never reaches the measured state,
	// so neither half of the rule would have been tested.
	nulled map[string]bool
	filled map[string]bool
	// problems are phrased as the question the author has to answer.
	problems []string
}

func newWireCensus() *wireCensus {
	return &wireCensus{
		paths:  map[string]bool{},
		nulled: map[string]bool{},
		filled: map[string]bool{},
	}
}

func (c *wireCensus) problem(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// walk descends the rendered value, carrying whether it is currently inside a
// nullable group. Descending rather than reading only the top level is what
// stops the rule being dodged one level down: a number tucked into a new nested
// object, or into a list of objects, is still a number a consumer can default.
func (c *wireCensus) walk(path string, v any, inGroup bool) {
	switch val := v.(type) {
	case map[string]any:
		for key, child := range val {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			c.paths[childPath] = true
			kind, ok := repoWireFields[childPath]
			if !ok {
				c.problem("%q reaches the wire and nothing in repoWireFields says what it is. Add it: is it a number, and if so which nullable group is it inside?", childPath)
				continue
			}
			switch kind {
			case kindGroup:
				if inGroup {
					c.problem("%q is classified a group but sits inside another group. Nesting groups makes the null ambiguous: a consumer cannot tell which level was not measured.", childPath)
				}
				if child == nil {
					c.nulled[childPath] = true
					continue
				}
				if _, isObject := child.(map[string]any); !isObject {
					c.problem("%q is classified a group but rendered as %T. A group is an object or null, and nothing else.", childPath, child)
					continue
				}
				c.filled[childPath] = true
				c.walk(childPath, child, true)
			case kindMeasurement:
				if !inGroup {
					c.problem("%q is a number sitting outside any nullable group. A consumer reading it with a default turns an absent measurement straight back into a measured zero, which is the bug this whole package exists to refuse. Move it inside coverage or tests, or into a new nullable group.", childPath)
				}
				switch child.(type) {
				case float64, nil:
				default:
					c.problem("%q is classified a measurement but rendered as %T (%v).", childPath, child, child)
				}
			case kindContext:
				c.walk(childPath, child, inGroup)
			}
		}
	case []any:
		for _, item := range val {
			c.walk(path+"[]", item, inGroup)
		}
	case float64:
		// Reached only through a path classified as context, or through a list
		// element. Either way it is a number nobody declared as one.
		c.problem("%q renders as the number %v, but it is not classified a measurement. Every number on the wire has to be declared, and has to live inside a nullable group.", path, val)
		if !inGroup {
			c.problem("%q is also outside any nullable group, so nothing stops a consumer defaulting it to zero.", path)
		}
	}
}

// TestEveryNumberIsInsideANullableGroup is the fail-closed half of this file,
// and the load-bearing one.
//
// The table above it asserts today's columns render honestly. That catches the
// five instances of this bug that have already happened and would miss the
// sixth, because the sixth will almost certainly be a new number on RepoView
// that nobody thought about, and a table of today's columns simply would not
// mention it. This does, in four ways that each close a different door:
//
//  1. every key on the wire has to be classified, so a new field fails here
//     before a reviewer has to notice it;
//  2. no number may render outside a nullable group, asserted from the rendered
//     value's type rather than from what the census claims, so an ungrouped
//     number fails even if someone classified it as context;
//  3. every declared group has to be observed both null and filled, so
//     "nullable" is demonstrated rather than asserted;
//  4. the walk recurses, so the rule cannot be dodged by burying a number one
//     level further down.
func TestEveryNumberIsInsideANullableGroup(t *testing.T) {
	rows := degradedRows()
	inputs := make([]delta.Input, 0, len(rows))
	for _, row := range rows {
		inputs = append(inputs, row.in)
	}

	// The union across every state, because error is omitempty and only a
	// failed row carries it, and because no single row shows a group both null
	// and filled. A census taken from one healthy row would declare a real key
	// rot and would never see a null at all.
	census := newWireCensus()
	for name, row := range jsonRepoRows(t, delta.Compute(inputs, options(), fixedNow())) {
		if len(row) == 0 {
			t.Fatalf("%s rendered an empty json object", name)
		}
		census.walk("", row, false)
	}

	for _, problem := range census.problems {
		t.Error(problem)
	}

	for path := range repoWireFields {
		if !census.paths[path] {
			t.Errorf("repoWireFields classifies %q but no rendered row carries it, so the classification is rot. Either the field was removed or the fixture no longer reaches the state that emits it.", path)
		}
	}

	for path, kind := range repoWireFields {
		if kind != kindGroup {
			continue
		}
		if !census.nulled[path] {
			t.Errorf("%q is declared a group but no degraded state renders it as null, so nothing here proves it is nullable at all. A group that is always an object is a flat struct wearing a namespace, and the numbers inside it are as defaultable as they ever were.", path)
		}
		if !census.filled[path] {
			t.Errorf("%q is declared a group but no fixture ever fills it in, so the measured case is untested and the null above proves nothing.", path)
		}
	}

	// Pin the set as well as the membership rules. The assertions above would
	// still pass if a future edit reclassified a number as context to quiet a
	// failure, or dropped one, and this would not.
	var measurements []string
	for path, kind := range repoWireFields {
		if kind == kindMeasurement {
			measurements = append(measurements, path)
		}
	}
	want := []string{
		"coverage.covered",
		"coverage.culprits[].contribution_points",
		"coverage.culprits[].from_pct",
		"coverage.culprits[].statements",
		"coverage.culprits[].to_pct",
		"coverage.delta_points",
		"coverage.pct",
		"coverage.total",
		"tests.count",
		"tests.delta",
		"tests.packages_without_tests",
	}
	if got := strings.Join(sortedNames(measurements), ","); got != strings.Join(want, ",") {
		t.Errorf("measurement paths: got %v, want %v. A number moved buckets, which is a decision worth making on purpose rather than to quiet a test.", sortedNames(measurements), want)
	}
}
