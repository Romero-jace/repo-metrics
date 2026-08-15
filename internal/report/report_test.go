package report_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// Package names are import-path shaped so the report is exercised with the
// strings it will really carry, not with "a" and "b".
const (
	pkgAlpha = "example.com/x/alpha"
	pkgBravo = "example.com/x/bravo"
	// These two are named so that neither contains the word the template uses
	// to label its state. Otherwise "is this package labeled gone?" would pass
	// just because the import path happens to say so.
	pkgGone = "example.com/x/departed"
	pkgNew  = "example.com/x/arrived"
)

// Repo names deliberately do not occur anywhere in the template's own prose, so
// a line-scoped assertion can find one repo's row by substring without also
// matching boilerplate.
const (
	repoMover  = "moverepo"
	repoSteady = "steadyrepo"
	repoQuiet  = "quietrepo"
	repoFresh  = "freshrepo"
	repoBroken = "brokenrepo"
)

// fixedNow pins GeneratedAt. Using the wall clock would make the determinism
// test pass for the wrong reason on a fast machine and flake on a slow one.
func fixedNow() time.Time { return time.Date(2026, 3, 2, 15, 4, 5, 0, time.UTC) }

func cov(pkg string, covered, total int) []store.Metric {
	return []store.Metric{
		{Key: collect.KeyCoveredStmts, Scope: pkg, Value: float64(covered)},
		{Key: collect.KeyTotalStmts, Scope: pkg, Value: float64(total)},
	}
}

func testCount(pkg string, n int) store.Metric {
	return store.Metric{Key: collect.KeyTestCount, Scope: pkg, Value: float64(n)}
}

func snap(id, repoID int64, env string, status store.Status, errText string) *store.Snapshot {
	return &store.Snapshot{
		ID:          id,
		RepoID:      repoID,
		CollectedAt: fixedNow().Add(-time.Hour),
		GitSHA:      "0123456789abcdef",
		GitBranch:   "main",
		Env:         env,
		Status:      status,
		Error:       errText,
	}
}

func metrics(groups ...[]store.Metric) []store.Metric {
	var out []store.Metric
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func options() delta.Options {
	return delta.Options{
		Window:        7 * 24 * time.Hour,
		MinStatements: 20,
		MinRepoDelta:  0.5,
		MaxCulprits:   5,
	}
}

// fullReport exercises every branch the template has: a mover with culprits in
// all three package states plus a toolchain change, a mover whose test count did
// not move, a quiet repo, a repo on its very first run, and a failed collection.
func fullReport() delta.Report {
	moverHead := metrics(
		cov(pkgAlpha, 80, 200),
		cov(pkgBravo, 100, 200),
		cov(pkgNew, 30, 100),
		[]store.Metric{testCount(pkgAlpha, 10), testCount(pkgBravo, 5)},
		[]store.Metric{{Key: collect.KeyPkgWithoutTest, Value: 2}},
	)
	moverBase := metrics(
		cov(pkgAlpha, 180, 200),
		cov(pkgBravo, 100, 200),
		cov(pkgGone, 95, 100),
		[]store.Metric{testCount(pkgAlpha, 12), testCount(pkgBravo, 5), testCount(pkgGone, 3)},
	)

	inputs := []delta.Input{
		{
			Repo:        store.Repo{ID: 1, Name: repoMover, Path: "/repos/moverepo"},
			Head:        snap(11, 1, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: moverHead,
			Base:        snap(10, 1, "go1.25.1", store.StatusOK, ""),
			BaseMetrics: moverBase,
		},
		{
			// Coverage moved but the test count did not. This is the only input
			// that reaches the "tests unchanged" branch of the movers paragraph.
			Repo:        store.Repo{ID: 2, Name: repoSteady, Path: "/repos/steadyrepo"},
			Head:        snap(21, 2, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 53, 100), []store.Metric{testCount(pkgAlpha, 4)}),
			Base:        snap(20, 2, "go1.26.5", store.StatusOK, ""),
			BaseMetrics: metrics(cov(pkgAlpha, 50, 100), []store.Metric{testCount(pkgAlpha, 4)}),
		},
		{
			Repo:        store.Repo{ID: 3, Name: repoQuiet, Path: "/repos/quietrepo"},
			Head:        snap(31, 3, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 50, 100), []store.Metric{testCount(pkgAlpha, 4)}),
			Base:        snap(30, 3, "go1.26.5", store.StatusOK, ""),
			BaseMetrics: metrics(cov(pkgAlpha, 50, 100), []store.Metric{testCount(pkgAlpha, 4)}),
		},
		{
			// First ever collection. Nothing about this repo may be printed as
			// though there were something to compare against.
			Repo:        store.Repo{ID: 4, Name: repoFresh, Path: "/repos/freshrepo"},
			Head:        snap(41, 4, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 10, 100), []store.Metric{testCount(pkgAlpha, 1)}),
		},
		{
			Repo: store.Repo{ID: 5, Name: repoBroken, Path: "/repos/brokenrepo"},
			Head: snap(51, 5, "go1.26.5", store.StatusFailed, "command exited 0 but wrote no fresh profile"),
		},
	}

	return delta.Compute(inputs, options(), fixedNow())
}

func mustMarkdown(t *testing.T, rep delta.Report) string {
	t.Helper()
	md, err := report.MarkdownString(rep)
	if err != nil {
		t.Fatalf("MarkdownString: %v", err)
	}
	return md
}

func mustJSON(t *testing.T, rep delta.Report) string {
	t.Helper()
	var b strings.Builder
	if err := report.JSON(&b, rep); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	return b.String()
}

// linesMentioning scopes an assertion to one repo's lines instead of the whole
// document, which keeps a failure message readable.
func linesMentioning(md, needle string) []string {
	var out []string
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

// sectionAfter returns the text from a heading up to the next level-two heading,
// so "does X appear under heading Y" is answerable.
func sectionAfter(md, heading string) string {
	_, rest, found := strings.Cut(md, heading)
	if !found {
		return ""
	}
	if next := strings.Index(rest, "\n## "); next >= 0 {
		return rest[:next]
	}
	return rest
}

func repoRow(t *testing.T, md, name string) string {
	t.Helper()
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| ") && strings.Contains(line, "| "+name+" |") {
			return line
		}
	}
	t.Fatalf("no table row for repo %q in:\n%s", name, md)
	return ""
}

func findRepoView(t *testing.T, repos []report.RepoView, name string) report.RepoView {
	t.Helper()
	for _, r := range repos {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no repo named %q in the view (it has %d repos)", name, len(repos))
	return report.RepoView{}
}

func TestMarkdownRenders(t *testing.T) {
	md := mustMarkdown(t, fullReport())

	if strings.TrimSpace(md) == "" {
		t.Fatal("rendered markdown is empty")
	}
	for _, name := range []string{repoMover, repoSteady, repoQuiet, repoFresh, repoBroken} {
		if !strings.Contains(md, name) {
			t.Errorf("rendered markdown never mentions repo %q:\n%s", name, md)
		}
	}
	// The culprit packages are the whole point of the report, so a render that
	// silently drops them is not a render.
	for _, want := range []string{"## What moved", pkgAlpha, pkgGone, pkgNew} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q:\n%s", want, md)
		}
	}
	t.Logf("rendered markdown:\n%s", md)
}

// TestRenderIsDeterministic stands in for a golden file. A committed fixture
// would rot on every wording change, but rendering twice still catches the thing
// a golden file is really there for: map iteration order reaching the output.
// Each iteration recomputes the report rather than re-rendering one value. That
// is the whole point: the only map iteration in the pipeline is over package
// names inside delta.culprits, so a loop that renders a single already-computed
// Report would prove nothing except that text/template walks a slice in order.
func TestRenderIsDeterministic(t *testing.T) {
	firstMD := mustMarkdown(t, fullReport())
	firstJSON := mustJSON(t, fullReport())
	for i := 2; i <= 20; i++ {
		if got := mustMarkdown(t, fullReport()); got != firstMD {
			t.Fatalf("markdown render %d differs from the first\nfirst:\n%s\ngot:\n%s", i, firstMD, got)
		}
		if got := mustJSON(t, fullReport()); got != firstJSON {
			t.Fatalf("json render %d differs from the first\nfirst:\n%s\ngot:\n%s", i, firstJSON, got)
		}
	}
}

// TestNoEmDashes enforces the owner's stated prose rule for generated output.
func TestNoEmDashes(t *testing.T) {
	md := mustMarkdown(t, fullReport())
	i := strings.IndexRune(md, '\u2014')
	if i < 0 {
		return
	}
	start := max(i-60, 0)
	end := min(i+60, len(md))
	t.Errorf("rendered markdown contains an em dash (U+2014) at byte %d. Generated prose here uses commas, colons, or separate sentences instead. Context:\n...%s...", i, md[start:end])
}

// signedNumber matches anything a reader would take for a measured change, so a
// no-baseline line fails this even if the wording changes.
var signedNumber = regexp.MustCompile(`[+\-][0-9]`)

// timestamp exists only so signedNumber can be applied to a line that also
// carries a collection date. An ISO date is full of hyphens and none of them
// mean "went down".
var timestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC`)

// TestNoBaselineIsNeverRenderedAsADelta is the one that matters most here. A
// first run that prints "+0.0 pts" claims it measured no change when it measured
// nothing at all, and a tool that does that on day one never gets trusted again.
func TestNoBaselineIsNeverRenderedAsADelta(t *testing.T) {
	rep := fullReport()
	md := mustMarkdown(t, rep)

	view := findRepoView(t, report.Build(rep).Repos, repoFresh)
	if view.HasBaseline {
		t.Fatalf("fixture is wrong: %s is supposed to have no baseline", repoFresh)
	}
	if view.CoverageDeltaPoints != nil || view.TestsDelta != nil {
		t.Fatalf("%s has no baseline but the view carries deltas: coverage=%v tests=%v",
			repoFresh, view.CoverageDeltaPoints, view.TestsDelta)
	}

	lines := linesMentioning(md, repoFresh)
	if len(lines) == 0 {
		t.Fatalf("no rendered line mentions %s at all:\n%s", repoFresh, md)
	}
	for _, line := range lines {
		if stripped := timestamp.ReplaceAllString(line, ""); signedNumber.MatchString(stripped) {
			t.Errorf("%s has no baseline, but its line carries a signed number, which reads as a measured change:\n%s", repoFresh, line)
		}
		if strings.Contains(line, " pts") {
			t.Errorf("%s has no baseline, but its line quotes a change in points:\n%s", repoFresh, line)
		}
		if !strings.Contains(line, "no baseline yet") {
			t.Errorf("%s has no baseline, but its line does not say so:\n%s", repoFresh, line)
		}
	}

	// The other half of the same requirement: a repo that does have a baseline
	// and genuinely did not move must print a real zero, not "no baseline yet".
	// If both cases rendered the same string the distinction would be words only.
	quiet := findRepoView(t, report.Build(rep).Repos, repoQuiet)
	if !quiet.HasBaseline {
		t.Fatalf("fixture is wrong: %s is supposed to have a baseline", repoQuiet)
	}
	if quiet.CoverageDeltaPoints == nil || *quiet.CoverageDeltaPoints != 0 {
		t.Fatalf("fixture is wrong: %s should have a zero coverage delta, got %v", repoQuiet, quiet.CoverageDeltaPoints)
	}
	quietRow := repoRow(t, md, repoQuiet)
	if !strings.Contains(quietRow, "+0.0 pts") {
		t.Errorf("%s has a baseline and did not move, so its row should show a measured zero:\n%s", repoQuiet, quietRow)
	}
	if strings.Contains(quietRow, "no baseline yet") {
		t.Errorf("%s has a baseline, so its row must not claim otherwise:\n%s", repoQuiet, quietRow)
	}
}

// TestMarkdownAndJSONAgree pins the reason both formats are built from one View.
func TestMarkdownAndJSONAgree(t *testing.T) {
	rep := fullReport()
	md := mustMarkdown(t, rep)

	var decoded report.View
	if err := json.Unmarshal([]byte(mustJSON(t, rep)), &decoded); err != nil {
		t.Fatalf("decoding the rendered json: %v", err)
	}
	built := report.Build(rep)

	if decoded.GeneratedAt != built.GeneratedAt {
		t.Errorf("generated_at: json %q, Build %q", decoded.GeneratedAt, built.GeneratedAt)
	}
	if decoded.WindowDays != built.WindowDays {
		t.Errorf("window_days: json %v, Build %v", decoded.WindowDays, built.WindowDays)
	}
	if len(decoded.Repos) != len(built.Repos) {
		t.Fatalf("repo count: json %d, Build %d", len(decoded.Repos), len(built.Repos))
	}

	for i, want := range built.Repos {
		got := decoded.Repos[i]
		if got.Name != want.Name {
			t.Fatalf("repo %d: json name %q, Build name %q", i, got.Name, want.Name)
		}
		if got.CoveragePct != want.CoveragePct {
			t.Errorf("%s coverage_pct: json %v, Build %v", want.Name, got.CoveragePct, want.CoveragePct)
		}
		if got.Tests != want.Tests {
			t.Errorf("%s tests: json %d, Build %d", want.Name, got.Tests, want.Tests)
		}
		checkFloatPtr(t, want.Name+" coverage_delta_points", got.CoverageDeltaPoints, want.CoverageDeltaPoints)
		checkIntPtr(t, want.Name+" tests_delta", got.TestsDelta, want.TestsDelta)

		// A failed collection has no figures to agree about. The JSON still
		// carries the Go zero values, but it also carries status, so a machine
		// can tell; the markdown withholds them because a human scanning rows
		// cannot. That divergence is deliberate and covered by
		// TestFailedCollectionQuotesNoNumbers.
		if got.Status == string(store.StatusFailed) {
			continue
		}

		// The markdown prints these same figures through the template's own
		// formatters, so the two renderings have to show the same numbers.
		row := repoRow(t, md, want.Name)
		wantPct := fmt.Sprintf("%.1f%%", want.CoveragePct)
		if !strings.Contains(row, "| "+wantPct+" |") {
			t.Errorf("%s: markdown row does not show coverage %s:\n%s", want.Name, wantPct, row)
		}
		wantTests := "| " + strconv.Itoa(want.Tests) + " |"
		if !strings.Contains(row, wantTests) {
			t.Errorf("%s: markdown row does not show test count %d:\n%s", want.Name, want.Tests, row)
		}
	}
}

func checkFloatPtr(t *testing.T, label string, got, want *float64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s: json %v, Build %v (one is absent and the other is not)", label, got, want)
	case *got != *want:
		t.Errorf("%s: json %v, Build %v", label, *got, *want)
	}
}

func checkIntPtr(t *testing.T, label string, got, want *int) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s: json %v, Build %v (one is absent and the other is not)", label, got, want)
	case *got != *want:
		t.Errorf("%s: json %v, Build %v", label, *got, *want)
	}
}

// TestJSONWireShape decodes into map[string]any on purpose. Decoding into View
// would round-trip a Go nil back into a Go nil and prove nothing about what
// actually goes over the wire, which is where a downstream consumer reads it.
func TestJSONWireShape(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, fullReport())), &doc); err != nil {
		t.Fatalf("decoding the rendered json: %v", err)
	}
	repos, ok := doc["repos"].([]any)
	if !ok {
		t.Fatalf("json has no repos array, got %T", doc["repos"])
	}

	byName := make(map[string]map[string]any, len(repos))
	for _, r := range repos {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("repos entry is %T, want an object", r)
		}
		name, ok := row["name"].(string)
		if !ok {
			t.Fatalf("repos entry has no name: %v", row)
		}
		byName[name] = row
	}

	for _, field := range []string{"coverage_delta_points", "tests_delta"} {
		fresh, ok := byName[repoFresh]
		if !ok {
			t.Fatalf("%s missing from the json", repoFresh)
		}
		v, present := fresh[field]
		if !present {
			t.Errorf("%s: %s is absent from the json; a consumer cannot tell an absent delta from a key it forgot to read", repoFresh, field)
		}
		if v != nil {
			t.Errorf("%s has no baseline, so %s must be null, got %v", repoFresh, field, v)
		}

		mover, ok := byName[repoMover]
		if !ok {
			t.Fatalf("%s missing from the json", repoMover)
		}
		if mover[field] == nil {
			t.Errorf("%s has a baseline, so %s must not be null", repoMover, field)
		}
		if _, ok := mover[field].(float64); !ok {
			t.Errorf("%s: %s decoded as %T, want a json number", repoMover, field, mover[field])
		}
	}

	if got := byName[repoFresh]["has_baseline"]; got != false {
		t.Errorf("%s has_baseline: got %v, want false", repoFresh, got)
	}
	if got := byName[repoMover]["has_baseline"]; got != true {
		t.Errorf("%s has_baseline: got %v, want true", repoMover, got)
	}
}

// TestPackageChurnIsNotARegression guards the first embarrassment this tool
// could produce: a deleted package rendered as if its coverage collapsed to
// zero, or a brand-new one rendered as if it had been let go.
func TestPackageChurnIsNotARegression(t *testing.T) {
	md := mustMarkdown(t, fullReport())

	churn := sectionAfter(md, "## Packages that came and went")
	if churn == "" {
		t.Fatalf("no packages-that-came-and-went section in:\n%s", md)
	}
	if !strings.Contains(churn, pkgGone) {
		t.Errorf("removed package %s is not listed under the churn heading:\n%s", pkgGone, churn)
	}
	if !strings.Contains(churn, pkgNew) {
		t.Errorf("added package %s is not listed under the churn heading:\n%s", pkgNew, churn)
	}

	for _, line := range linesMentioning(md, pkgGone) {
		// "95.0% to 0.0%" is the misleading phrasing: the package did not drop
		// to zero coverage, it stopped existing.
		if strings.Contains(line, "to 0.0%") {
			t.Errorf("removed package %s is described as a coverage collapse:\n%s", pkgGone, line)
		}
	}
	for _, line := range linesMentioning(md, pkgNew) {
		if strings.Contains(line, "0.0% to ") {
			t.Errorf("added package %s is described as if it improved from zero:\n%s", pkgNew, line)
		}
	}

	// The reader has to be able to tell which state each culprit is in, or the
	// contribution figure next to it is unreadable.
	moved := sectionAfter(md, "## What moved")
	for pkg, marker := range map[string]string{pkgGone: "gone", pkgNew: "new"} {
		var found bool
		for _, line := range strings.Split(moved, "\n") {
			if strings.Contains(line, pkg) && strings.Contains(line, marker) {
				found = true
			}
		}
		if !found {
			t.Errorf("culprit %s is not labeled %q in the movers section:\n%s", pkg, marker, moved)
		}
	}
}

// TestProblemsSection checks that a failed collection is called out with its
// error text rather than quietly contributing zeros to the table.
func TestProblemsSection(t *testing.T) {
	md := mustMarkdown(t, fullReport())

	problems := sectionAfter(md, "## Collection problems")
	if problems == "" {
		t.Fatalf("no collection-problems section in:\n%s", md)
	}
	if !strings.Contains(problems, repoBroken) {
		t.Errorf("%s failed collection but is not in the problems section:\n%s", repoBroken, problems)
	}
	if !strings.Contains(problems, string(store.StatusFailed)) {
		t.Errorf("problems section does not give the status:\n%s", problems)
	}
	if !strings.Contains(problems, "command exited 0 but wrote no fresh profile") {
		t.Errorf("problems section drops the error text, which is the only part that says what to fix:\n%s", problems)
	}
	// A healthy repo must not be dragged in with it.
	if strings.Contains(problems, repoQuiet) {
		t.Errorf("%s collected fine but appears in the problems section:\n%s", repoQuiet, problems)
	}
}

// TestFailedCollectionQuotesNoNumbers is the same rule as the no-baseline test,
// one column to the left. A failed run produces no metrics, so every count for
// it defaults to zero. Printing those defaults in the table would tell a reader
// scanning the rows that the repo was measured at zero percent coverage with
// every package tested, which is the most flattering possible reading of a total
// failure and the one thing this tool must never say.
func TestFailedCollectionQuotesNoNumbers(t *testing.T) {
	rep := fullReport()
	md := mustMarkdown(t, rep)

	view := findRepoView(t, report.Build(rep).Repos, repoBroken)
	if view.Status != string(store.StatusFailed) {
		t.Fatalf("fixture is wrong: %s should be failed, got %q", repoBroken, view.Status)
	}

	row := repoRow(t, md, repoBroken)
	if strings.Contains(row, "0.0%") {
		t.Errorf("%s collected nothing, so its row must not quote a coverage figure:\n%s", repoBroken, row)
	}
	if !strings.Contains(row, "not collected") {
		t.Errorf("%s collected nothing, but its row does not say so:\n%s", repoBroken, row)
	}

	// A partial repo is the case this must not catch, so the guard has to key on
	// failed alone. Prove that by rendering one.
	partial := delta.Compute([]delta.Input{{
		Repo:        store.Repo{ID: 9, Name: "partialrepo", Path: "/repos/partialrepo"},
		Head:        snap(91, 9, "go1.26.5", store.StatusPartial, "test command exited 1"),
		HeadMetrics: metrics(cov(pkgAlpha, 33, 100), []store.Metric{testCount(pkgAlpha, 7)}),
	}}, options(), fixedNow())

	partialRow := repoRow(t, mustMarkdown(t, partial), "partialrepo")
	if !strings.Contains(partialRow, "33.0%") {
		t.Errorf("a partial snapshot still carries real numbers and must keep showing them:\n%s", partialRow)
	}
	if strings.Contains(partialRow, "not collected") {
		t.Errorf("a partial snapshot did collect something, so its row must not say otherwise:\n%s", partialRow)
	}
}

// TestFailedCollectionAfterAGoodOneInventsNoCliff is the nastier half of the
// same problem, and it is the exact failure store/types.go:8 warns about: a
// failed run has no metrics, so subtracting a healthy baseline from it produces
// a cliff, followed next week by a recovery, neither of which happened.
func TestFailedCollectionAfterAGoodOneInventsNoCliff(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo:        store.Repo{ID: 8, Name: "crashedrepo", Path: "/repos/crashedrepo"},
		Head:        snap(81, 8, "go1.26.5", store.StatusFailed, "coverage profile was stale"),
		Base:        snap(80, 8, "go1.26.5", store.StatusOK, ""),
		BaseMetrics: metrics(cov(pkgAlpha, 72, 100), []store.Metric{testCount(pkgAlpha, 40)}),
	}}, options(), fixedNow())

	view := findRepoView(t, report.Build(rep).Repos, "crashedrepo")
	if !view.HasBaseline {
		t.Fatalf("fixture is wrong: crashedrepo is supposed to have a baseline")
	}

	row := repoRow(t, mustMarkdown(t, rep), "crashedrepo")
	for _, invented := range []string{"-72.0 pts", "-40"} {
		if strings.Contains(row, invented) {
			t.Errorf("crashedrepo collected nothing, so %q is a cliff that did not happen:\n%s", invented, row)
		}
	}
	if strings.Contains(row, " pts") {
		t.Errorf("crashedrepo collected nothing, so its row must not quote any change in points:\n%s", row)
	}
	if !strings.Contains(row, "not collected") {
		t.Errorf("crashedrepo collected nothing, but its row does not say so:\n%s", row)
	}

	// The table is not the only place that cliff can surface. delta.Compute sets
	// IsMover from the same subtraction, so a failed run against a healthy
	// baseline also leads the report as the week's biggest drop.
	md := mustMarkdown(t, rep)
	moved := sectionAfter(md, "## What moved")
	if strings.Contains(moved, "crashedrepo") {
		t.Errorf("crashedrepo collected nothing, so it must not lead the report as a mover:\n%s", moved)
	}

	// Same artifact, third surface: with no head metrics every baseline package
	// looks deleted, so the churn section would report the repo as wiped.
	if strings.Contains(md, pkgAlpha) {
		t.Errorf("crashedrepo collected nothing, so no package of its may be described as coming or going:\n%s", md)
	}

	// The JSON has to be honest about the same thing, since a consumer reading
	// coverage_delta_points would otherwise chart the cliff.
	if view.CoverageDeltaPoints != nil || view.TestsDelta != nil {
		t.Errorf("crashedrepo collected nothing but the view publishes deltas: coverage=%v tests=%v",
			view.CoverageDeltaPoints, view.TestsDelta)
	}
	if len(view.Culprits) != 0 || len(view.RemovedPackages) != 0 || len(view.AddedPackages) != 0 {
		t.Errorf("crashedrepo collected nothing but the view publishes package findings: culprits=%d added=%v removed=%v",
			len(view.Culprits), view.AddedPackages, view.RemovedPackages)
	}
	if view.Status != string(store.StatusFailed) || view.Error == "" {
		t.Errorf("crashedrepo must still carry the status and error that explain the blanks: status=%q error=%q", view.Status, view.Error)
	}
	t.Logf("crashed repo full report:\n%s", md)
}

// TestEnvChangeIsCalledOut covers the toolchain-change branch, which otherwise
// only ever runs in production.
func TestEnvChangeIsCalledOut(t *testing.T) {
	md := mustMarkdown(t, fullReport())
	moved := sectionAfter(md, "## What moved")
	if !strings.Contains(moved, "different toolchains") {
		t.Errorf("%s changed toolchain between snapshots but the report does not say so:\n%s", repoMover, moved)
	}
}

// TestEmptyReport is the day-zero case: a config with no repos, or a run before
// anything has been collected.
func TestEmptyReport(t *testing.T) {
	rep := delta.Compute(nil, options(), fixedNow())

	md := mustMarkdown(t, rep)
	if strings.TrimSpace(md) == "" {
		t.Fatal("empty report rendered nothing at all, not even a heading")
	}
	if !strings.Contains(md, "Nothing cleared the reporting threshold") {
		t.Errorf("empty report does not explain that nothing moved:\n%s", md)
	}
	if strings.ContainsRune(md, '\u2014') {
		t.Errorf("empty report contains an em dash:\n%s", md)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, rep)), &doc); err != nil {
		t.Fatalf("decoding the rendered json for an empty report: %v", err)
	}
	if got := doc["repos"]; got == nil {
		t.Error("empty report json has a null repos field; an empty list is the honest answer")
	}
	t.Logf("empty report:\n%s", md)
}
