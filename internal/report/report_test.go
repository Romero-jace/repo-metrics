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
	// repoUnseen is configured but has never been collected. It is named so
	// that it shares no substring with the template's own "not collected" and
	// "never" wording, which a row assertion has to be able to tell apart.
	repoUnseen = "unseenrepo"
	// repoUpgraded changed toolchain without moving enough to be a mover.
	repoUpgraded = "upgradedrepo"
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

// testStream wraps per-package counts the way the collector actually emits
// them: whenever it parses a stdout stream it also writes a repo-level
// pkg.without_tests metric, including when the value is zero.
//
// That marker is what distinguishes "measured and found none" from "never
// looked". A fixture carrying bare test counts without it describes output the
// collector cannot produce, and would let an unmeasured-versus-zero bug pass.
func testStream(withoutTests int, counts ...store.Metric) []store.Metric {
	return append(counts,
		store.Metric{Key: collect.KeyPkgWithoutTest, Value: float64(withoutTests)})
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
		testStream(2, testCount(pkgAlpha, 10), testCount(pkgBravo, 5)),
	)
	moverBase := metrics(
		cov(pkgAlpha, 180, 200),
		cov(pkgBravo, 100, 200),
		cov(pkgGone, 95, 100),
		testStream(2, testCount(pkgAlpha, 12), testCount(pkgBravo, 5), testCount(pkgGone, 3)),
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
			HeadMetrics: metrics(cov(pkgAlpha, 53, 100), testStream(0, testCount(pkgAlpha, 4))),
			Base:        snap(20, 2, "go1.26.5", store.StatusOK, ""),
			BaseMetrics: metrics(cov(pkgAlpha, 50, 100), testStream(0, testCount(pkgAlpha, 4))),
		},
		{
			Repo:        store.Repo{ID: 3, Name: repoQuiet, Path: "/repos/quietrepo"},
			Head:        snap(31, 3, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 50, 100), testStream(0, testCount(pkgAlpha, 4))),
			Base:        snap(30, 3, "go1.26.5", store.StatusOK, ""),
			BaseMetrics: metrics(cov(pkgAlpha, 50, 100), testStream(0, testCount(pkgAlpha, 4))),
		},
		{
			// First ever collection. Nothing about this repo may be printed as
			// though there were something to compare against.
			Repo:        store.Repo{ID: 4, Name: repoFresh, Path: "/repos/freshrepo"},
			Head:        snap(41, 4, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 10, 100), testStream(0, testCount(pkgAlpha, 1))),
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

// jsonRepoRows renders the JSON and returns its repos array keyed by name,
// still decoded as map[string]any. Decoding into View instead would round-trip
// a Go nil back into a Go nil and prove nothing about what goes over the wire,
// which is where a downstream consumer reads it.
func jsonRepoRows(t *testing.T, rep delta.Report) map[string]map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, rep)), &doc); err != nil {
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
	return byName
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

// fromToPct matches the "90.0% to 40.0%" phrasing that describes a package
// moving between two measurements. Only a package present in both snapshots
// may be described that way: a deleted or brand-new one has one figure, and
// pairing it with the zero on the other side invents a move.
var fromToPct = regexp.MustCompile(`[0-9]+\.[0-9]% to [0-9]+\.[0-9]%`)

// zeroPct matches a coverage figure of exactly zero. The leading guard is what
// keeps it from also matching the tail of a real figure: "30.0% covered"
// contains the characters 0.0% and is not a zero.
var zeroPct = regexp.MustCompile(`(^|[^0-9])0\.0%`)

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
	byName := jsonRepoRows(t, fullReport())

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

	// The third pairing of those two fields, and the one a consumer is most
	// likely to get wrong: a repo with plenty of history whose latest run came
	// back with nothing. has_baseline is true, so a consumer that reads it as
	// "the deltas are populated" would chart a cliff. There is still nothing to
	// compare against, so both deltas stay null.
	crashed := jsonRepoRows(t, delta.Compute([]delta.Input{{
		Repo:        store.Repo{ID: 8, Name: "crashedrepo", Path: "/repos/crashedrepo"},
		Head:        snap(81, 8, "go1.26.5", store.StatusFailed, "coverage profile was stale"),
		Base:        snap(80, 8, "go1.26.5", store.StatusOK, ""),
		BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0, testCount(pkgAlpha, 40))),
	}}, options(), fixedNow()))["crashedrepo"]
	if crashed == nil {
		t.Fatalf("crashedrepo is missing from the json entirely")
	}
	if got := crashed["has_baseline"]; got != true {
		t.Fatalf("fixture is wrong: crashedrepo has history, so has_baseline should be true, got %v", got)
	}
	if got := crashed["status"]; got != string(store.StatusFailed) {
		t.Fatalf("fixture is wrong: crashedrepo should be failed, got %v", got)
	}
	if got, present := crashed["has_snapshot"]; !present || got != true {
		t.Errorf("crashedrepo did run, it just came back empty, so has_snapshot should be true: got %v (present=%v)", got, present)
	}
	for _, field := range []string{"coverage_delta_points", "tests_delta"} {
		v, present := crashed[field]
		if !present {
			t.Errorf("crashedrepo: %s is absent from the json; a consumer cannot tell an absent delta from a key it forgot to read", field)
		}
		if v != nil {
			t.Errorf("crashedrepo has a baseline but its latest run collected nothing, so %s must be null, got %v", field, v)
		}
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

	moved := sectionAfter(md, "## What moved")

	// This is the control for the two assertions after it. fromToPct has to
	// match the phrasing the template really uses for a package that moved,
	// or "the removed package's line does not look like that" would pass
	// because nothing in the report looks like that, which is how a guard ends
	// up asserting a substring the template can never emit under any input.
	changed := linesMentioning(moved, pkgAlpha)
	if len(changed) == 0 {
		t.Fatalf("%s exists in both snapshots and moved, so it should be named as a culprit:\n%s", pkgAlpha, moved)
	}
	for _, line := range changed {
		if !fromToPct.MatchString(line) {
			t.Fatalf("%s moved between two snapshots, so its line should read as a from-to coverage move, and the rest of this test is meaningless if it does not:\n%s", pkgAlpha, line)
		}
	}

	// A package that was deleted did not drop to zero coverage, it stopped
	// existing, and one that has just appeared did not climb from zero. Keying
	// on the from-to shape rather than on one literal ("to 0.0%") means a
	// reworded version of the same mistake still fails.
	gone := linesMentioning(moved, pkgGone)
	if len(gone) == 0 {
		t.Fatalf("removed package %s is not named as a culprit at all:\n%s", pkgGone, moved)
	}
	for _, line := range gone {
		if fromToPct.MatchString(line) {
			t.Errorf("removed package %s is described as a coverage move between two figures, but it only has one:\n%s", pkgGone, line)
		}
		if zeroPct.MatchString(line) {
			t.Errorf("removed package %s is quoted at 0.0%% coverage, which is a number nobody measured:\n%s", pkgGone, line)
		}
		if !strings.Contains(line, "gone") {
			t.Errorf("removed package %s is not identified as removed, so its contribution figure reads as a regression:\n%s", pkgGone, line)
		}
	}

	arrived := linesMentioning(moved, pkgNew)
	if len(arrived) == 0 {
		t.Fatalf("added package %s is not named as a culprit at all:\n%s", pkgNew, moved)
	}
	for _, line := range arrived {
		if fromToPct.MatchString(line) {
			t.Errorf("added package %s is described as a coverage move between two figures, but it had no earlier one:\n%s", pkgNew, line)
		}
		if zeroPct.MatchString(line) {
			t.Errorf("added package %s is quoted at 0.0%% before it existed, which reads as an improvement from nothing:\n%s", pkgNew, line)
		}
		if !strings.Contains(line, "new") {
			t.Errorf("added package %s is not identified as new, so its contribution figure reads as an improvement:\n%s", pkgNew, line)
		}
	}

	// The reader has to be able to tell which state each culprit is in, or the
	// contribution figure next to it is unreadable.
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
		HeadMetrics: metrics(cov(pkgAlpha, 33, 100), testStream(0, testCount(pkgAlpha, 7))),
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
		BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0, testCount(pkgAlpha, 40))),
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

// TestNeverCollectedRepoIsNotPublishedAsHealthy is the third repo state, after
// measured and failed: configured, but never collected at all. Nothing has ever
// run against it, so every count on it is a Go zero value. Publishing those as
// status ok says the repo sits at 0.0% coverage with no untested packages,
// which is a measurement nobody took and is indistinguishable from a repo that
// really is at zero.
func TestNeverCollectedRepoIsNotPublishedAsHealthy(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo: store.Repo{ID: 7, Name: repoUnseen, Path: "/repos/unseenrepo"},
	}}, options(), fixedNow())

	built := report.Build(rep)
	view := findRepoView(t, built.Repos, repoUnseen)

	if view.HasSnapshot {
		t.Errorf("%s has never been collected, but the view says it has a snapshot", repoUnseen)
	}
	if view.Status == string(store.StatusOK) {
		t.Errorf("%s has never been collected, so calling it %q publishes a health verdict nobody measured", repoUnseen, store.StatusOK)
	}
	if view.Status != report.StatusNotCollected {
		t.Errorf("%s status: got %q, want %q", repoUnseen, view.Status, report.StatusNotCollected)
	}
	if view.CoverageDeltaPoints != nil || view.TestsDelta != nil {
		t.Errorf("%s has nothing to compare, but the view publishes deltas: coverage=%v tests=%v",
			repoUnseen, view.CoverageDeltaPoints, view.TestsDelta)
	}
	if len(view.Culprits) != 0 || len(view.AddedPackages) != 0 || len(view.RemovedPackages) != 0 {
		t.Errorf("%s has no snapshot, but the view publishes package findings: culprits=%d added=%v removed=%v",
			repoUnseen, len(view.Culprits), view.AddedPackages, view.RemovedPackages)
	}
	if len(built.Movers) != 0 {
		t.Errorf("%s has no snapshot, so it cannot have moved, but it leads the report: %v", repoUnseen, built.Movers)
	}

	// The markdown is where a human forms the impression, so the row has to
	// read the same way a failed collection does.
	md := mustMarkdown(t, rep)
	row := repoRow(t, md, repoUnseen)
	if strings.Contains(row, "0.0%") {
		t.Errorf("%s was never collected, so its row must not quote a coverage figure:\n%s", repoUnseen, row)
	}
	if strings.Contains(row, " pts") {
		t.Errorf("%s was never collected, so its row must not quote a change in points:\n%s", repoUnseen, row)
	}
	if !strings.Contains(row, "not collected") {
		t.Errorf("%s was never collected, but its row does not say so:\n%s", repoUnseen, row)
	}
	if !strings.Contains(row, "never") {
		t.Errorf("%s has no collection date, so its row should say never:\n%s", repoUnseen, row)
	}

	// And the JSON, since a consumer charting coverage_pct needs a field to key
	// the blank off before it plots a zero.
	wire := jsonRepoRows(t, rep)[repoUnseen]
	if wire == nil {
		t.Fatalf("%s is missing from the json entirely", repoUnseen)
	}
	if got, present := wire["has_snapshot"]; !present || got != false {
		t.Errorf("%s has_snapshot: got %v (present=%v), want false", repoUnseen, got, present)
	}
	if got := wire["status"]; got == string(store.StatusOK) {
		t.Errorf("%s status went over the wire as %q, so a consumer reads it as a healthy repo at zero", repoUnseen, got)
	}
	for _, field := range []string{"coverage_delta_points", "tests_delta"} {
		if v := wire[field]; v != nil {
			t.Errorf("%s has nothing to compare, so %s must be null, got %v", repoUnseen, field, v)
		}
	}
	t.Logf("never collected repo report:\n%s", md)
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

// TestEnvChangeIsCalledOutForARepoThatDidNotMove is the half the movers section
// cannot cover. A repo whose toolchain changed but whose coverage moved less
// than the reporting threshold never appears under "what moved", so a warning
// that only renders there leaves its delta looking like a plain code change.
// The delta is exactly as untrustworthy as a big mover's.
func TestEnvChangeIsCalledOutForARepoThatDidNotMove(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo:        store.Repo{ID: 6, Name: repoUpgraded, Path: "/repos/upgradedrepo"},
		Head:        snap(61, 6, "go1.26.5", store.StatusOK, ""),
		HeadMetrics: metrics(cov(pkgAlpha, 50, 100), testStream(0, testCount(pkgAlpha, 4))),
		Base:        snap(60, 6, "go1.25.1", store.StatusOK, ""),
		BaseMetrics: metrics(cov(pkgAlpha, 50, 100), testStream(0, testCount(pkgAlpha, 4))),
	}}, options(), fixedNow())

	built := report.Build(rep)
	view := findRepoView(t, built.Repos, repoUpgraded)
	if !view.EnvChanged {
		t.Fatalf("fixture is wrong: %s is supposed to have changed toolchain", repoUpgraded)
	}
	if !view.HasBaseline {
		t.Fatalf("fixture is wrong: %s is supposed to have a baseline", repoUpgraded)
	}
	if len(built.Movers) != 0 {
		t.Fatalf("fixture is wrong: %s is supposed to sit below the mover threshold, got movers %v", repoUpgraded, built.Movers)
	}

	md := mustMarkdown(t, rep)
	if strings.Contains(sectionAfter(md, "## What moved"), repoUpgraded) {
		t.Fatalf("fixture is wrong: %s should not be in the movers section:\n%s", repoUpgraded, md)
	}

	row := repoRow(t, md, repoUpgraded)
	if !strings.Contains(row, "toolchain") {
		t.Errorf("%s changed toolchain and its row shows a delta anyway, with no warning on it:\n%s\nfull report:\n%s", repoUpgraded, row, md)
	}
	// A marker nobody can read is not a warning, so the report has to say what
	// it means somewhere.
	if !strings.Contains(md, "not code") {
		t.Errorf("the report marks %s but never explains what the marker means:\n%s", repoUpgraded, md)
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
	// All three lists, not just repos. A quiet week is the most common week
	// there is, and a null here crashes any consumer that reads .length before
	// it looks at the contents.
	for _, field := range []string{"repos", "movers", "problems"} {
		if _, ok := doc[field].([]any); !ok {
			t.Errorf("empty report json has %s = %v (%T); an empty list is the honest answer and the only one a consumer can iterate", field, doc[field], doc[field])
		}
	}
	t.Logf("empty report:\n%s", md)
}

// TestWindowReadsCorrectlyBelowADay covers the reporting window's own prose. A
// sub-day window is a real setting, and rounding it to "0 days" tells the
// reader each repo was compared against right now.
func TestWindowReadsCorrectlyBelowADay(t *testing.T) {
	for _, tc := range []struct {
		window time.Duration
		want   string
	}{
		{7 * 24 * time.Hour, "7 days"},
		{24 * time.Hour, "1 day"},
		// Rounded to a whole day this is 1, and the noun has to follow the
		// rounded number rather than the raw one.
		{30 * time.Hour, "1 day"},
		{12 * time.Hour, "12 hours"},
		{time.Hour, "1 hour"},
		{90 * time.Minute, "2 hours"},
		{45 * time.Minute, "45 minutes"},
		{time.Minute, "1 minute"},
	} {
		opts := options()
		opts.Window = tc.window
		md := mustMarkdown(t, delta.Compute(nil, opts, fixedNow()))

		want := "about " + tc.want + " back"
		if !strings.Contains(md, want) {
			t.Errorf("window %s: report does not say %q. First line of the body:\n%s", tc.window, want, strings.SplitN(md, "\n", 4)[2])
		}
		if strings.Contains(md, "about 0 days back") {
			t.Errorf("window %s rendered as zero days, which reads as comparing against right now", tc.window)
		}
	}
}
