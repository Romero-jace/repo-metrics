package report_test

import (
	"encoding/json"
	"fmt"
	"slices"
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
	"line coverage",
	"line coverage change",
	"tests",
	"tests change",
	"packages without tests",
	"packages without tests change",
	"lint findings",
	"lint findings change",
	"outdated dependencies",
	"outdated dependencies change",
}

// pairedGroups ties each JSON group to the markdown column showing the same
// signal, so the two renderings can be checked against each other by name rather
// than by position.
//
// Hand-written, and pinned to the registry by TestEveryTableSignalIsPaired
// rather than derived from it. Deriving would make it agree automatically, and
// automatic agreement proves nothing: the point is that a signal earning a table
// column is a decision someone made, and the check that its two renderings match
// has to be a second decision rather than a consequence of the first.
var pairedGroups = []struct{ key, column string }{
	{"coverage", "coverage"},
	{"coverage_lines", "line coverage"},
	{"tests", "tests"},
	{"untested_packages", "packages without tests"},
	{"lint_findings", "lint findings"},
	{"outdated_dependencies", "outdated dependencies"},
}

// A signal in the every-repo table has to be compared across both renderings.
//
// Without this the pairing list is a hand-written subset that silently stops
// growing: a sixth table signal would get a markdown column, a JSON group, and
// nothing checking that the two agree about whether it was measured. The
// degraded rows would still pass, because they only assert the markdown.
func TestEveryTableSignalIsPaired(t *testing.T) {
	paired := make(map[string]bool, len(pairedGroups))
	for _, g := range pairedGroups {
		paired[g.key] = true
	}
	for _, sig := range delta.Signals() {
		if sig.Table && !paired[string(sig.ID)] {
			t.Errorf("%q has a column in the every-repo table but no entry in pairedGroups, so nothing checks that its markdown cell and its json group agree about whether anything measured it",
				sig.ID)
		}
	}
	// And the reverse, so a signal dropped from the table does not leave a
	// pairing that quietly matches nothing.
	table := make(map[string]bool, len(pairedGroups))
	for _, sig := range delta.Signals() {
		if sig.Table {
			table[string(sig.ID)] = true
		}
	}
	for _, g := range pairedGroups {
		if !table[g.key] {
			t.Errorf("pairedGroups pairs %q, which no longer has a table column, so the pairing is rot", g.key)
		}
	}
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
	// cells is keyed by column name. A column not named here is REQUIRED to
	// render as unmeasured, which is the safe default: adding a signal cannot
	// quietly grant a degraded row permission to print a number, and a row that
	// should print one has to say so.
	cells map[string]cellSpec
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
			name:  repoNever,
			why:   "configured but never collected: every count on it is a Go zero value, not a measurement",
			in:    delta.Input{Repo: repoAt(1, repoNever)},
			cells: nil,
		},
		{
			name: repoFailedOnly,
			why:  "the run came back with nothing and there is no history either",
			in: delta.Input{
				Repo: repoAt(2, repoFailedOnly),
				Head: snap(21, 2, "go1.26.5", store.StatusFailed, "test command exited 2"),
			},
			cells: nil,
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
			cells: nil,
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
			cells: map[string]cellSpec{
				"coverage":        measured("60.0%"),
				"coverage change": measured("+10.0 pts"),
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
			cells: map[string]cellSpec{
				"coverage":               measured("10.0%"),
				"tests":                  measured("1"),
				"packages without tests": measured("0"),
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
			cells: map[string]cellSpec{
				"tests":                         measured("5"),
				"tests change":                  measured("+0"),
				"packages without tests":        measured("0"),
				"packages without tests change": measured("+0"),
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
			// This row also carries the offline dependency case, which is the one
			// state where two signals out of one parsed stream disagree about
			// having been measured. The module list was read, so the count and
			// the ages are real; nothing consulted the proxy, so the outdated
			// count is absent rather than zero. It is unlisted below, which the
			// rule reads as REQUIRED to be unmeasured.
			name: repoPartial,
			why:  "a partial run collected real numbers and must keep showing them",
			in: delta.Input{
				Repo: repoAt(7, repoPartial),
				Head: snap(71, 7, "go1.26.5", store.StatusPartial, "test command exited 1"),
				HeadMetrics: metrics(
					cov(pkgAlpha, 33, 100),
					testStream(2, testCount(pkgAlpha, 7)),
					lintRun("lint", 4, 1, 2),
					depsOffline(27, 410),
				),
			},
			cells: map[string]cellSpec{
				"coverage":               measured("33.0%"),
				"tests":                  measured("7"),
				"packages without tests": measured("2"),
				"lint findings":          measured("4"),
			},
		},
		{
			name: repoHealthy,
			why:  "the healthy control: if this row blanks, the invariant is over-broad",
			in: delta.Input{
				Repo: repoAt(8, repoHealthy),
				Head: snap(81, 8, "go1.26.5", store.StatusOK, ""),
				// Both coverage units, because the healthy control has to fill every
				// group it can. A polyglot repo running a Go profile beside a
				// tracefile-producing suite is the real shape this models, and
				// leaving line coverage null here would make the null in every other
				// row prove nothing.
				HeadMetrics: metrics(cov(pkgAlpha, 80, 100), covLines("src/a.ts", 30, 60),
					testStream(1, testCount(pkgAlpha, 9)),
					lintRun("lint", 5, 0, 1), depsRun(27, 400, 3),
					[]store.Metric{stepTiming("coverage", 42000)}),
				Base: snap(80, 8, "go1.26.5", store.StatusOK, ""),
				BaseMetrics: metrics(cov(pkgAlpha, 75, 100), covLines("src/a.ts", 24, 60),
					testStream(1, testCount(pkgAlpha, 8)),
					lintRun("lint", 5, 0, 1), depsRun(27, 400, 3),
					[]store.Metric{stepTiming("coverage", 42000)}),
			},
			cells: map[string]cellSpec{
				"coverage":                      measured("80.0%"),
				"coverage change":               measured("+5.0 pts"),
				"line coverage":                 measured("50.0%"),
				"line coverage change":          measured("+10.0 pts"),
				"tests":                         measured("9"),
				"tests change":                  measured("+1"),
				"packages without tests":        measured("1"),
				"packages without tests change": measured("+0"),
				// Held level across the two sides on purpose. A moving lint or
				// dependency count would nominate this repo again and change what
				// the mover assertions below are actually testing.
				"lint findings":                measured("5"),
				"lint findings change":         measured("+0"),
				"outdated dependencies":        measured("3"),
				"outdated dependencies change": measured("+0"),
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
	known := make(map[string]bool, len(degradedColumns))
	for _, col := range degradedColumns {
		known[col] = true
	}
	for _, row := range rows {
		for col, c := range row.cells {
			if !known[col] {
				t.Fatalf("fixture is wrong: %s names a column %q that the table does not have, so the expectation is silently never checked",
					row.name, col)
			}
			if c.measured && !c.hasDigit() {
				t.Fatalf("fixture is wrong: %s declares the %s column measured but expects %q, which carries no number",
					row.name, col, c.want)
			}
			if !c.measured && c.want != "" {
				t.Fatalf("fixture is wrong: %s declares the %s column unmeasured but also expects the text %q",
					row.name, col, c.want)
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
			// Every column is checked, not only the ones the row named. A column
			// the fixture leaves out is REQUIRED to be unmeasured, so a new
			// signal cannot quietly grant a degraded row permission to print a
			// number: the safe expectation is the one you get for free.
			for i, col := range degradedColumns {
				want := row.cells[col]
				got := cells[i]
				switch {
				case want.measured && got != want.want:
					t.Errorf("%s (%s): %s column reads %q, want exactly %q\nfull row: %s",
						row.name, row.why, col, got, want.want, repoRow(t, md, row.name))
				case !want.measured && hasDigit(got):
					t.Errorf("%s (%s): %s column reads %q, which is a number nobody measured\nfull row: %s",
						row.name, row.why, col, got, repoRow(t, md, row.name))
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
			// Each wire group is paired with the markdown column that shows the
			// same signal, by name rather than by index, so reordering the table
			// cannot silently compare coverage against the tests column.
			//
			// TestEveryTableSignalIsPaired keeps this list in step with the
			// registry. Without it, a sixth signal joining the table would get a
			// column that nothing compares against the wire, and the two
			// renderers could disagree about it forever.
			for _, group := range pairedGroups {
				v, present := wireRow[group.key]
				if !present {
					t.Errorf("%s: the %s key is missing from the json rather than null. A consumer that never sees the key defaults it, and the zeros come straight back.",
						row.name, group.key)
					continue
				}
				column := slices.Index(degradedColumns[:], group.column)
				if got := v != nil; got != row.cells[group.column].measured {
					t.Errorf("%s %s: json group present=%v, but the markdown %s column reads %q. The two renderers disagree about whether anything was measured.",
						row.name, group.key, got, group.column, cells[column])
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
	// kindInput is a number the caller or the config supplied rather than one a
	// collection produced: the reporting window, how many repos are configured.
	// There is no state in which one of these was not measured, so it is allowed
	// outside a group, which is the whole reason this kind is dangerous.
	//
	// It exists only for the envelope. Nothing in a repo row may use it, and
	// TestNoRepoFieldEscapesThroughKindInput enforces that, because the moment a
	// figure derived from what a collection found is declared an input, this is
	// no longer a guard, it is a hole with a name.
	kindInput fieldKind = "input"
	// kindRepoRows is one of the envelope's three lists of repo rows. The
	// envelope census stops here rather than descending: the rows are walked in
	// full by TestEveryNumberIsInsideANullableGroup against repoWireFields, and
	// all three lists are []RepoView, so guarding one shape guards all of them.
	// Descending again would mean a second copy of every repo classification,
	// keyed three ways, which is three places for one rule to rot.
	kindRepoRows fieldKind = "repo rows"
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
	// A timestamp, so context rather than a measurement, and null for a repo
	// with no baseline. It cannot be kindInput even though it is the reality
	// half of a request-and-reality pair with window_days: inputs are envelope
	// only, which TestNoRepoFieldEscapesThroughKindInput enforces.
	"baseline_collected_at": kindContext,
	"error":                 kindContext,
	// These four are booleans, and none of them gates a number any more. They
	// say which kind of nothing a null group is, alongside status, or what the
	// numbers in the groups have to be read against.
	"has_snapshot": kindContext,
	"has_baseline": kindContext,
	"env_changed":  kindContext,
	// env_unknown is env_changed's other half: it says the comparison could not
	// be made rather than that it came back equal. Context for the same reason,
	// and it has to be here rather than folded into env_changed, since a consumer
	// reading only that one would take false for an assurance nobody gave.
	"env_unknown": kindContext,
	// git_dirty is not a number, so it cannot be a measurement, and kindInput is
	// banned on a repo row. It is the same kind of thing env_changed is: a fact
	// about how the measurement was taken that changes what it means.
	"git_dirty": kindContext,

	// MovedBy names which signals made this repo lead the report. It carries no
	// numbers, only signal names, which is why it is context rather than a
	// group: there is nothing here for a consumer to default to zero.
	"moved_by": kindContext,

	"coverage":         kindGroup,
	"coverage.value":   kindMeasurement,
	"coverage.delta":   kindMeasurement,
	"coverage.covered": kindMeasurement,
	"coverage.total":   kindMeasurement,
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

	// Every signal but coverage renders the same two keys, because they share
	// one SignalView. Listing them per signal rather than deriving them is the
	// point of this table: a new signal reaching the wire has to be classified
	// by a person before it can ship, and generating these entries from the
	// registry would be exactly the automatic agreement this guard refuses.
	// Line coverage is a plain group, unlike statement coverage above, because it
	// carries no counts and no culprits. That is deliberate: the per-file ranking
	// belongs to one unit, and two culprit lists in different units competing to
	// explain one repo would be worse than one.
	"coverage_lines":       kindGroup,
	"coverage_lines.value": kindMeasurement,
	"coverage_lines.delta": kindMeasurement,

	"tests":       kindGroup,
	"tests.value": kindMeasurement,
	"tests.delta": kindMeasurement,

	"test_failures":       kindGroup,
	"test_failures.value": kindMeasurement,
	"test_failures.delta": kindMeasurement,

	"test_skipped":       kindGroup,
	"test_skipped.value": kindMeasurement,
	"test_skipped.delta": kindMeasurement,

	"untested_packages":       kindGroup,
	"untested_packages.value": kindMeasurement,
	"untested_packages.delta": kindMeasurement,

	"test_time":       kindGroup,
	"test_time.value": kindMeasurement,
	"test_time.delta": kindMeasurement,

	// The lint signals. They share a marker and null together, the way the test
	// signals do, because all three come out of one parsed analysis log.
	//
	// Suppressed findings are their own group rather than being folded into the
	// total, and that is a measurement decision rather than a layout one: adding
	// them to the total would make a repo look worse for having triaged its
	// findings.
	"lint_findings":       kindGroup,
	"lint_findings.value": kindMeasurement,
	"lint_findings.delta": kindMeasurement,

	"lint_errors":       kindGroup,
	"lint_errors.value": kindMeasurement,
	"lint_errors.delta": kindMeasurement,

	"lint_suppressed":       kindGroup,
	"lint_suppressed.value": kindMeasurement,
	"lint_suppressed.delta": kindMeasurement,

	// The dependency signals come out of one parsed stream but null
	// INDEPENDENTLY, unlike every family above them, because they are measurable
	// under different conditions. The count needs nothing, the age needs a
	// publish timestamp, and outdated needs the module proxy to have been
	// consulted at all. The partial-run fixture renders the first two filled and
	// the third null in the same row, which is what proves they are separable.
	"dependencies":       kindGroup,
	"dependencies.value": kindMeasurement,
	"dependencies.delta": kindMeasurement,

	"outdated_dependencies":       kindGroup,
	"outdated_dependencies.value": kindMeasurement,
	"outdated_dependencies.delta": kindMeasurement,

	"dependency_age":       kindGroup,
	"dependency_age.value": kindMeasurement,
	"dependency_age.delta": kindMeasurement,

	// The only signal that measures what collection COST rather than what it
	// found. It is null in ingest mode, where nothing ran, which is a different
	// answer from a collection that took no time.
	"collect_time":       kindGroup,
	"collect_time.value": kindMeasurement,
	"collect_time.delta": kindMeasurement,
}

// envelopeWireFields is the same census one level up, over the object the repo
// rows sit inside.
//
// It exists because the row census could not see this. It is seeded from
// jsonRepoRows, which keeps doc["repos"] and throws the envelope away, so
// window_days has sat on the wire as a bare ungrouped number for as long as it
// has existed and nothing noticed. That was harmless, because the window is an
// input rather than a measurement, but the absence of any check was not: it made
// the envelope the one place a derived number could be added with no test
// pressure at all, which is precisely where the next instance of this project's
// recurring bug would have landed.
//
// Two tables rather than one merged walk, on purpose. Merging would mean keying
// every repo classification three times over, once per list, since movers, repos
// and problems all render []RepoView. Three copies of one rule is three places
// for it to rot.
var envelopeWireFields = map[string]fieldKind{
	"generated_at": kindContext,
	"section":      kindContext,
	"window_days":  kindInput,

	// scope descends: it is an object, but not a nullable one, so it is context
	// rather than a group. A group has to be demonstrated null somewhere, and this
	// one never is, which is the point of it.
	"scope": kindContext,
	// The narrowed-to name is nullable and is not a number, so it is context. Its
	// null means no filter was applied, which is a real answer rather than an
	// absent measurement.
	"scope.repo": kindContext,
	// Counts of repos the caller and the config supplied. Nothing measured them,
	// nothing can fail to measure them.
	"scope.selected":   kindInput,
	"scope.configured": kindInput,

	// The signal catalog: what each measurement below is called, what unit it is
	// in, and which direction is good news. A list is safe here for the reason a
	// list of repo rows would not be: it provably carries no numbers, so the
	// walk collapsing its element paths costs nothing.
	"signals":             kindContext,
	"signals[].id":        kindContext,
	"signals[].label":     kindContext,
	"signals[].unit":      kindContext,
	"signals[].direction": kindContext,

	"movers":   kindRepoRows,
	"repos":    kindRepoRows,
	"problems": kindRepoRows,
}

// wireCensus is what walking the rendered JSON learned.
type wireCensus struct {
	// fields is the classification table this walk is judged against. It is a
	// field rather than the package-level map because there are two regions to
	// guard with one walk: the repo rows, and the envelope around them.
	fields map[string]fieldKind
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

func newWireCensus(fields map[string]fieldKind) *wireCensus {
	return &wireCensus{
		fields: fields,
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
			kind, ok := c.fields[childPath]
			if !ok {
				c.problem("%q reaches the wire and nothing classifies it. Add it: is it a number, and if so which nullable group is it inside?", childPath)
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
			case kindInput:
				// The one kind allowed outside a group, so it gets the mirror of
				// the measurement check: inside a group it would be indistinguishable
				// from a measurement to anyone reading the JSON, and the null that
				// group renders would then look like it governed this too.
				if inGroup {
					c.problem("%q is classified an input but sits inside a nullable group. An input has no unmeasured state, so a null group around it says something untrue about it. Move it out, or it is a measurement and should say so.", childPath)
				}
				switch child.(type) {
				case float64, string:
				default:
					c.problem("%q is classified an input but rendered as %T (%v). An input is a number or a string the caller supplied, and never absent.", childPath, child, child)
				}
			case kindRepoRows:
				// Deliberately not walked. See kindRepoRows.
				switch child.(type) {
				case []any, nil:
				default:
					c.problem("%q is classified a list of repo rows but rendered as %T. It is a list, or null when the section was not asked for, and nothing else.", childPath, child)
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
	census := newWireCensus(repoWireFields)
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
		"collect_time.delta",
		"collect_time.value",
		"coverage.covered",
		"coverage.culprits[].contribution_points",
		"coverage.culprits[].from_pct",
		"coverage.culprits[].statements",
		"coverage.culprits[].to_pct",
		"coverage.delta",
		"coverage.total",
		"coverage.value",
		"coverage_lines.delta",
		"coverage_lines.value",
		"dependencies.delta",
		"dependencies.value",
		"dependency_age.delta",
		"dependency_age.value",
		"lint_errors.delta",
		"lint_errors.value",
		"lint_findings.delta",
		"lint_findings.value",
		"lint_suppressed.delta",
		"lint_suppressed.value",
		"outdated_dependencies.delta",
		"outdated_dependencies.value",
		"test_failures.delta",
		"test_failures.value",
		"test_skipped.delta",
		"test_skipped.value",
		"test_time.delta",
		"test_time.value",
		"tests.delta",
		"tests.value",
		"untested_packages.delta",
		"untested_packages.value",
	}
	if got := strings.Join(sortedNames(measurements), ","); got != strings.Join(want, ",") {
		t.Errorf("measurement paths: got %v, want %v. A number moved buckets, which is a decision worth making on purpose rather than to quiet a test.", sortedNames(measurements), want)
	}
}

// TestEveryEnvelopeFieldIsDeclared is the test above's blind spot, closed.
//
// That one is seeded from jsonRepoRows, which keeps doc["repos"] and discards
// everything around it, so nothing has ever checked the object the rows sit in. A
// number added there would have shipped with no test pressure whatever.
//
// It runs under every section and under both narrowings, because scope is the one
// envelope field whose value changes with either. A census that only ever walked
// an unnarrowed --section all render would pass while a narrowed --section movers
// call published something else entirely.
func TestEveryEnvelopeFieldIsDeclared(t *testing.T) {
	rows := degradedRows()
	inputs := make([]delta.Input, 0, len(rows))
	for _, row := range rows {
		inputs = append(inputs, row.in)
	}
	rep := delta.Compute(inputs, options(), fixedNow())

	scopes := map[string]report.Scope{
		"unnarrowed": {Configured: len(rep.Repos)},
		"narrowed":   {Repo: repoHealthy, Configured: len(rep.Repos) + 2},
	}

	census := newWireCensus(envelopeWireFields)
	for _, sec := range report.Sections() {
		for name, scope := range scopes {
			var doc map[string]any
			raw := mustJSONScoped(t, rep, report.Section(sec), scope)
			if err := json.Unmarshal([]byte(raw), &doc); err != nil {
				t.Fatalf("--section %s, %s: decoding the rendered json: %v", sec, name, err)
			}
			census.walk("", doc, false)
		}
	}

	for _, problem := range census.problems {
		t.Error(problem)
	}

	for path := range envelopeWireFields {
		if !census.paths[path] {
			t.Errorf("envelopeWireFields classifies %q but no render carries it, so the classification is rot.", path)
		}
	}

	// Pin the input set the same way the measurement set is pinned. Reclassifying
	// a derived number as an input is how this guard would be quietly relaxed, and
	// it is the only reclassification that lets a number out of a nullable group.
	var inputs2 []string
	for path, kind := range envelopeWireFields {
		if kind == kindInput {
			inputs2 = append(inputs2, path)
		}
	}
	want := []string{"scope.configured", "scope.selected", "window_days"}
	if got := strings.Join(sortedNames(inputs2), ","); got != strings.Join(want, ",") {
		t.Errorf("input paths: got %v, want %v. A number became an input, which means it left the only rule that keeps an unmeasured figure off the wire. That is a decision to make on purpose.", sortedNames(inputs2), want)
	}
}

// TestNoRepoFieldEscapesThroughKindInput keeps the envelope's escape hatch out of
// the region it would actually hurt.
//
// kindInput exists so window_days and the scope counts can sit outside a nullable
// group, which is honest for a figure the caller supplied. Applied to a repo row
// it would be a license to publish a measurement ungrouped, which is the entire
// bug this package is built against. One line, so that stays impossible rather
// than merely unlikely.
func TestNoRepoFieldEscapesThroughKindInput(t *testing.T) {
	for path, kind := range repoWireFields {
		if kind == kindInput {
			t.Errorf("%q is a repo field classified as an input. A repo row carries what a collection found, and everything a collection finds can fail to be found, so it belongs inside a nullable group.", path)
		}
	}
}
