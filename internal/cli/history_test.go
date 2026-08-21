package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// historyDoc is the history payload, decoded the safe way.
//
// Measurement is a pointer for the same reason every measurement group in this
// suite is: a struct of plain floats would decode a point that measured nothing
// straight back into 0.0, and a test suite that reads the wire unsafely cannot
// catch the bug the wire shape exists to prevent.
type historyDoc struct {
	Since         string      `json:"since"`
	SinceDays     float64     `json:"since_days"`
	Scope         *scopeDoc   `json:"scope"`
	Signal        signalDoc   `json:"signal"`
	LastCollected *string     `json:"last_collected"`
	Points        *[]pointDoc `json:"points"`
}

type signalDoc struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Unit      string `json:"unit"`
	Direction string `json:"direction"`
}

type pointDoc struct {
	CollectedAt string            `json:"collected_at"`
	Status      string            `json:"status"`
	Error       string            `json:"error"`
	Measurement *pointMeasurement `json:"measurement"`
}

type pointMeasurement struct {
	Value float64 `json:"value"`
	// Nullable for the twelve signals that are not a rate over counts. Plain
	// ints would decode a test count back into a denominator of zero, which is
	// the shape this suite exists to refuse.
	Covered *int `json:"covered"`
	Total   *int `json:"total"`
}

func decodeHistory(t *testing.T, out string) historyDoc {
	t.Helper()
	var doc historyDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unparseable history json: %v\n%s", err, out)
	}
	if doc.Points == nil {
		t.Fatal("points is null rather than a list, so a consumer reading its length crashes on the case that is most common")
	}
	return doc
}

// historyFixture seeds one repo with backdated snapshots: two healthy runs, a
// failed one between them, and one older than any window this test asks for.
//
// Seeded through the store rather than by running collect, because two
// collections a second apart would land in the same range and prove nothing
// about ordering or about the window boundary.
func historyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := repoDir(t, dir, "charted", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("charted", repo, "coverage.out")))

	ctx := context.Background()
	seed := openStore(t, dbPath)
	repoID, err := seed.UpsertRepo(ctx, "charted", repo)
	if err != nil {
		t.Fatalf("seeding the repo: %v", err)
	}

	points := []struct {
		daysAgo int
		status  store.Status
		covered int
		errText string
	}{
		{120, store.StatusOK, 20, ""}, // outside a 90 day window
		{60, store.StatusOK, 40, ""},  // inside
		{30, store.StatusFailed, 0, "test command exited 2"},
		{10, store.StatusOK, 80, ""},
	}
	for _, p := range points {
		snap := store.Snapshot{
			RepoID:      repoID,
			CollectedAt: time.Now().Add(-time.Duration(p.daysAgo) * 24 * time.Hour),
			Status:      p.status,
			Error:       p.errText,
		}
		var metrics []store.Metric
		// A failed run stores nothing, which is what makes it a gap rather than
		// a zero. Seeding it with metrics would defeat the whole test.
		if p.status != store.StatusFailed {
			metrics = []store.Metric{
				{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/a", Value: float64(p.covered)},
				{Key: collect.KeyTotalStmts, Scope: "example.com/demo/a", Value: 100},
			}
		}
		if _, err := seed.InsertSnapshot(ctx, snap, metrics); err != nil {
			t.Fatalf("seeding a snapshot: %v", err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed store: %v", err)
	}
	return cfgPath
}

// The point of the whole feature: every snapshot ever collected is in the
// database, and until now nothing could ask for more than two of them.
func TestHistoryReturnsASeriesOldestFirst(t *testing.T) {
	cfgPath := historyFixture(t)

	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted",
		"--since", "90d", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v (stderr: %s)", err, stderr)
	}
	doc := decodeHistory(t, stdout)
	points := *doc.Points

	// Three of the four seeded points are inside a 90 day window. The fourth
	// proves the window is a filter rather than decoration.
	if len(points) != 3 {
		t.Fatalf("got %d points, want the 3 inside the window and not the one at 120 days: %+v", len(points), points)
	}
	for i := 1; i < len(points); i++ {
		if points[i-1].CollectedAt > points[i].CollectedAt {
			t.Errorf("points are not oldest first: %q comes before %q", points[i-1].CollectedAt, points[i].CollectedAt)
		}
	}
	// Anti-vacuity: the series carries real, moving numbers rather than three
	// copies of one value.
	if points[0].Measurement == nil || points[0].Measurement.Value != 40 {
		t.Errorf("first point = %+v, want 40 percent", points[0].Measurement)
	}
	if points[2].Measurement == nil || points[2].Measurement.Value != 80 {
		t.Errorf("last point = %+v, want 80 percent", points[2].Measurement)
	}
}

// The case the store's two-query design exists for, asserted end to end.
//
// A run that failed measured nothing, and that is the finding. Dropping the
// point renders a gap in collection as though the question was never asked, and
// drawing it at zero turns a crashed test command into a coverage cliff. Both
// are worse than showing the gap.
func TestHistoryKeepsFailedRunsAsVisibleGaps(t *testing.T) {
	cfgPath := historyFixture(t)

	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted",
		"--since", "90d", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v (stderr: %s)", err, stderr)
	}

	var failed []pointDoc
	for _, p := range *decodeHistory(t, stdout).Points {
		if p.Status == string(store.StatusFailed) {
			failed = append(failed, p)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("got %d failed points, want the one that was seeded: the series dropped it, which renders a broken collection as though nobody asked", len(failed))
	}
	if failed[0].Measurement != nil {
		t.Errorf("the failed point carries a measurement of %v, which nothing measured", failed[0].Measurement.Value)
	}
	if failed[0].Error == "" {
		t.Error("the failed point carries no error text, so the series says a run failed without saying why")
	}

	// And the markdown says so in words rather than leaving a blank cell.
	md, _, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted", "--since", "90d")
	if err != nil {
		t.Fatalf("history markdown: %v", err)
	}
	// A TABLE row, which means a line starting with a pipe. Selecting on the
	// word alone picked the last line mentioning it, and that is the prose under
	// the table explaining what "not collected" means, not a row at all. With no
	// pipes in it, the cell check below split it into a single cell, skipped the
	// two indexes it always skips, and could never return true: the assertion
	// passed on every input it was ever given.
	//
	// Keeping the last match is still right. There is one failed row in this
	// fixture, and taking the last means a future fixture with several is
	// checked on its last rather than silently on its first.
	var failedRow string
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "failed") {
			failedRow = line
		}
	}
	if failedRow == "" {
		t.Fatalf("no failed row in the markdown table:\n%s", md)
	}
	if hasDigitOutsideTimestamp(failedRow) {
		t.Errorf("the failed row carries a measurement: %q", failedRow)
	}
}

// hasDigitOutsideTimestamp looks for a number in the cells that are not the
// timestamp, since a timestamp is nothing but digits and cannot be judged.
func hasDigitOutsideTimestamp(row string) bool {
	cells := strings.Split(row, "|")
	for i, cell := range cells {
		// cells[0] is empty, cells[1] is the timestamp.
		if i < 2 {
			continue
		}
		if strings.ContainsAny(cell, "0123456789") {
			return true
		}
	}
	return false
}

// An empty series has two meanings that call for opposite actions, so it has to
// say which one it is.
func TestHistoryTellsNeverCollectedFromCollectionStopped(t *testing.T) {
	cfgPath := historyFixture(t)

	// A window so short that every seeded point falls outside it. Collection
	// happened, it just stopped, and last_collected is what says so.
	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted",
		"--since", "60m", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v (stderr: %s)", err, stderr)
	}
	stopped := decodeHistory(t, stdout)
	if len(*stopped.Points) != 0 {
		t.Fatalf("want an empty series for a one hour window, got %d points", len(*stopped.Points))
	}
	if stopped.LastCollected == nil {
		t.Error("last_collected is null for a repo that has been collected, so an empty series reads as never collected and points at the wrong fix")
	}

	md, _, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted", "--since", "60m")
	if err != nil {
		t.Fatalf("history markdown: %v", err)
	}
	if !strings.Contains(md, "collection stopped") && !strings.Contains(md, "stopped or the window") {
		t.Errorf("the markdown does not say collection stopped:\n%s", md)
	}

	// The other half: a configured repo the database has never heard of.
	dir := t.TempDir()
	fresh := repoDir(t, dir, "unseen", sampleProfile)
	freshCfg := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		filepath.Join(dir, "metrics.db"), ingestRepoEntry("unseen", fresh, "coverage.out")))

	stdout, _, err = runCLI(t, "history", "--config", freshCfg, "--repo", "unseen", "--format", "json")
	if err != nil {
		t.Fatalf("history on a never-collected repo: %v", err)
	}
	never := decodeHistory(t, stdout)
	if never.LastCollected != nil {
		t.Errorf("last_collected = %q for a repo that has never been collected", *never.LastCollected)
	}
}

// The usage text advertises `--since 26w` and the parser rejected it, because
// there was no week suffix anywhere. 26 weeks is 182 days, which reaches the
// seeded point that a 90 day window leaves out.
func TestHistoryAcceptsAWindowInWeeks(t *testing.T) {
	cfgPath := historyFixture(t)

	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted",
		"--since", "26w", "--format", "json")
	if err != nil {
		t.Fatalf("history --since 26w: %v (stderr: %s)", err, stderr)
	}
	if got := len(*decodeHistory(t, stdout).Points); got != 4 {
		t.Errorf("got %d points for a 26 week window, want all 4 seeded", got)
	}

	// Anti-vacuity: the same fixture through a 90 day window leaves the oldest
	// point out, so the w is being read as weeks rather than shrugged off.
	narrow, _, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted",
		"--since", "90d", "--format", "json")
	if err != nil {
		t.Fatalf("history --since 90d: %v", err)
	}
	if got := len(*decodeHistory(t, narrow).Points); got != 3 {
		t.Errorf("got %d points for a 90 day window, want 3", got)
	}
}

// History is narrowed by construction, so it says what it covers for the same
// reason the report does.
func TestHistorySaysWhatItCoversAndWhichSignal(t *testing.T) {
	cfgPath := historyFixture(t)

	stdout, _, err := runCLI(t, "history", "--config", cfgPath, "--repo", "charted",
		"--signal", "untested_packages", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	doc := decodeHistory(t, stdout)

	if doc.Scope == nil {
		t.Fatal("history carries no scope, so nothing says which repo it is about")
	}
	if doc.Scope.Repo == nil || *doc.Scope.Repo != "charted" {
		t.Errorf("scope.repo = %v, want charted", doc.Scope.Repo)
	}
	// The envelope names the signal, which is what makes a uniform measurement
	// key readable at all: without it, "value" is a number with no unit.
	if doc.Signal.ID != "untested_packages" {
		t.Errorf("signal.id = %q, want untested_packages", doc.Signal.ID)
	}
	if doc.Signal.Unit == "" || doc.Signal.Direction == "" {
		t.Errorf("signal = %+v, want a unit and a direction so a consumer can read the numbers", doc.Signal)
	}
	if doc.Signal.Direction != "lower_is_better" {
		t.Errorf("signal.direction = %q, want lower_is_better for untested packages", doc.Signal.Direction)
	}
}

// Every narrowing flag rejects a name it does not know, rather than falling back
// to a default and answering a question nobody asked.
func TestHistoryRejectsWhatItCannotChart(t *testing.T) {
	cfgPath := historyFixture(t)

	cases := []struct {
		name     string
		args     []string
		wantIn   []string
		wantExit bool
	}{
		{
			name:   "no repo at all",
			args:   []string{"history", "--config", cfgPath},
			wantIn: []string{"--repo", "charted"},
		},
		{
			name:   "a repo that is not configured",
			args:   []string{"history", "--config", cfgPath, "--repo", "chartd"},
			wantIn: []string{"chartd", "charted"},
		},
		{
			name:   "a signal that does not exist",
			args:   []string{"history", "--config", cfgPath, "--repo", "charted", "--signal", "covrage"},
			wantIn: []string{"covrage", "coverage"},
		},
		{
			name:   "a format that does not exist",
			args:   []string{"history", "--config", cfgPath, "--repo", "charted", "--format", "yaml"},
			wantIn: []string{"yaml", "markdown", "json"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, tc.args...)
			if err == nil {
				t.Fatalf("want an error, got none. stdout: %s", stdout)
			}
			if stdout != "" {
				t.Errorf("want nothing on stdout for a rejected run, got %q", stdout)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not mention %q: %s", want, stderr)
				}
			}
		})
	}
}

// sampleJUnit is two passing tests, which is what a JUnit report contributes
// that a tracefile cannot: a count signal, so this fixture carries a measurement
// that is not a rate over counts.
const sampleJUnit = `<?xml version="1.0" encoding="utf-8"?>
<testsuites>
  <testsuite name="pkg" tests="2" failures="0" skipped="0" time="0.5">
    <testcase classname="pkg" name="test_a" time="0.2"/>
    <testcase classname="pkg" name="test_b" time="0.3"/>
  </testsuite>
</testsuites>
`

// lcovHistoryConfig is a repo measured through an LCOV tracefile and a JUnit
// report and no Go profile at all, which is the shape a Python or TypeScript
// service has. One step naming both files, the way the pytest example does.
func lcovHistoryConfig(t *testing.T) (cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	repo := repoDir(t, dir, "webapp", "")
	writeFile(t, filepath.Join(repo, "coverage.lcov"), sampleLCOV)
	writeFile(t, filepath.Join(repo, "junit.xml"), sampleJUnit)
	entry := fmt.Sprintf(
		"  - name: webapp\n    path: %q\n    signals:\n      - name: tests\n"+
			"        artifacts:\n          - {path: junit.xml, format: %s}\n"+
			"          - {path: coverage.lcov, format: %s}\n",
		repo, config.FormatJUnitXML, config.FormatLCOV)
	cfgPath = writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		filepath.Join(dir, "metrics.db"), entry))
	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}
	return cfgPath
}

// history charts the coverage unit the repo actually recorded.
//
// The default was statement coverage, which is Go's, so a repo measured through
// LCOV answered with a run of null measurements on snapshots that had collected
// perfectly well. Reported from a mixed fleet, where it was read as a collection
// that had not worked: right and unreadable, which for an agent is the same as
// wrong.
func TestHistoryChartsTheCoverageUnitTheRepoRecorded(t *testing.T) {
	cfgPath := lcovHistoryConfig(t)

	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "webapp", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v (stderr: %s)", err, stderr)
	}
	doc := decodeHistory(t, stdout)

	if doc.Signal.ID != "coverage_lines" {
		t.Errorf("signal.id = %q, want coverage_lines for a repo that records only lines", doc.Signal.ID)
	}
	if len(*doc.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(*doc.Points))
	}
	point := (*doc.Points)[0]
	if point.Measurement == nil {
		t.Fatal("measurement is null on an ok snapshot that measured 75 percent, which is the failure this test exists for")
	}
	if point.Measurement.Value != 75 {
		t.Errorf("measurement.value = %v, want 75", point.Measurement.Value)
	}
	// The denominator, which setting a coverage floor needs and which used to
	// mean a second command to recover.
	if point.Measurement.Covered == nil || point.Measurement.Total == nil {
		t.Fatalf("measurement carries no counts: %+v", point.Measurement)
	}
	if *point.Measurement.Covered != 3 || *point.Measurement.Total != 4 {
		t.Errorf("measurement counts = %d/%d, want 3/4", *point.Measurement.Covered, *point.Measurement.Total)
	}

	// Said out loud, and on stderr rather than stdout, since --format json is
	// pure JSON on stdout and nothing else.
	if !strings.Contains(stderr, "coverage_lines") {
		t.Errorf("nothing on stderr says the charted signal was not the default: %q", stderr)
	}
}

// An explicitly named signal is never overridden.
//
// This is the control for the test above, and it is the more important half.
// Answering a different question than the one asked is the failure this whole
// tool argues against, so --signal coverage on a repo that records lines has to
// chart the empty series and say what the repo does record.
func TestHistoryNeverOverridesAnExplicitSignal(t *testing.T) {
	cfgPath := lcovHistoryConfig(t)

	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "webapp",
		"--signal", "coverage", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v (stderr: %s)", err, stderr)
	}
	doc := decodeHistory(t, stdout)

	if doc.Signal.ID != "coverage" {
		t.Errorf("signal.id = %q, want the coverage that was asked for. An explicit flag that gets swapped answers a question nobody asked", doc.Signal.ID)
	}
	if len(*doc.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(*doc.Points))
	}
	if m := (*doc.Points)[0].Measurement; m != nil {
		t.Errorf("measurement = %+v, want null: nothing measured statements in this repo", m)
	}
	if !strings.Contains(stderr, "coverage_lines") {
		t.Errorf("stderr does not say what this repo does record, so the empty series is a dead end: %q", stderr)
	}
}

// A count signal carries no denominator, and must not borrow one.
//
// The snapshot this charts stores line counts too, from the tracefile the same
// step read, so a build that attached whatever counts it found to whatever
// signal was asked for would publish 3 of 4 as the denominator of a test count.
// That is the fabricated number this payload shape exists to refuse, one signal
// over from where it usually appears.
func TestHistoryPublishesNoDenominatorForACountSignal(t *testing.T) {
	cfgPath := lcovHistoryConfig(t)

	stdout, stderr, err := runCLI(t, "history", "--config", cfgPath, "--repo", "webapp",
		"--signal", "tests", "--format", "json")
	if err != nil {
		t.Fatalf("history: %v (stderr: %s)", err, stderr)
	}
	doc := decodeHistory(t, stdout)
	if len(*doc.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(*doc.Points))
	}
	m := (*doc.Points)[0].Measurement
	if m == nil {
		t.Fatal("the JUnit report in this fixture counts two tests, so this point should carry them")
	}
	if m.Value != 2 {
		t.Errorf("measurement.value = %v, want the 2 tests the report lists", m.Value)
	}
	if m.Covered != nil || m.Total != nil {
		t.Errorf("tests published counts %v/%v. A test count has no numerator, and lending it the line counts stored beside it is a number nobody measured",
			m.Covered, m.Total)
	}
}

// The collect progress line names the unit it measured, and never sums the two.
func TestCollectProgressLineNamesTheCoverageUnit(t *testing.T) {
	dir := t.TempDir()
	lines := repoDir(t, dir, "webapp", "")
	writeFile(t, filepath.Join(lines, "coverage.lcov"), sampleLCOV)
	stmts := repoDir(t, dir, "service", sampleProfile)
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		filepath.Join(dir, "metrics.db"),
		ingestLCOVEntry("webapp", lines, "coverage.lcov"),
		ingestRepoEntry("service", stmts, "coverage.out")))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath)
	if err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	// Bound to each repo's own row. A substring search over the whole output
	// cannot fail when one repo's figure lands on another repo's line.
	if row := rowFor(t, stdout, "webapp"); !strings.Contains(row, "75.0% of 4 lines") {
		t.Errorf("webapp progress line does not report its line coverage: %q", row)
	}
	if row := rowFor(t, stdout, "service"); !strings.Contains(row, "60.0% of 5 statements") {
		t.Errorf("service progress line does not report its statement coverage: %q", row)
	}
	// The word matters as much as the figure: two units on one line with no
	// nouns would read as one measurement taken twice.
	if row := rowFor(t, stdout, "webapp"); strings.Contains(row, "statements") {
		t.Errorf("webapp is described in statements, which nothing measured there: %q", row)
	}
}
