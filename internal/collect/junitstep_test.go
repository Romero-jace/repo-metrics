package collect_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// junitStep builds an ingest-mode repo reading one JUnit report off disk, which
// is how a repo consumes something CI already produced.
func junitStep(t *testing.T, dir, name, body string) config.Repo {
	t.Helper()
	writeFile(t, dir, "junit.xml", body)
	return config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{{
			Name:           name,
			Artifact:       "junit.xml",
			ArtifactFormat: config.FormatJUnitXML,
			MaxAge:         config.Duration(24 * 60 * 60 * 1e9),
		}},
	}
}

const twoFileReport = `<testsuites>
	<testsuite name="src/a.test.ts">
		<testcase classname="src/a.test.ts" name="one" time="0.4"/>
		<testcase classname="src/a.test.ts" name="two" time="0.6"><failure message="x"/></testcase>
	</testsuite>
	<testsuite name="src/b.test.ts">
		<testcase classname="src/b.test.ts" name="three" time="0.5"><skipped/></testcase>
	</testsuite>
</testsuites>`

// Every row a JUnit step writes is scoped, and every scope starts with the
// step's own name.
//
// That prefix is what earns the format Repeatable and what lets a repo run it
// beside `go test -json`. Without it two test steps would write the same scope
// strings and collide on the metrics primary key, and the collector's answer to
// a collision is to drop a whole step's numbers.
func TestJUnitScopesEveryRowUnderItsStep(t *testing.T) {
	dir := t.TempDir()
	r := junitStep(t, dir, "web-tests", twoFileReport)

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	if got := metric(t, res, collect.KeyTestCount, "web-tests/src/a.test.ts"); got != 2 {
		t.Errorf("a.test.ts count: got %v, want 2", got)
	}
	if got := metric(t, res, collect.KeyTestFailed, "web-tests/src/a.test.ts"); got != 1 {
		t.Errorf("a.test.ts failed: got %v, want 1", got)
	}
	if got := metric(t, res, collect.KeyTestDurationMS, "web-tests/src/a.test.ts"); got != 1000 {
		t.Errorf("a.test.ts duration: got %v, want 1000", got)
	}
	if got := metric(t, res, collect.KeyTestSkipped, "web-tests/src/b.test.ts"); got != 1 {
		t.Errorf("b.test.ts skipped: got %v, want 1", got)
	}

	// The marker, at the step's own scope, so the delta layer can tell a repo
	// that added a second suite from one whose tests grew.
	if got := metric(t, res, collect.KeyTestSuites, "web-tests"); got != 2 {
		t.Errorf("test suites: got %v, want 2", got)
	}

	// The three keys a JUnit document cannot answer. pkg.without_tests needs to
	// know about source files that produced no suite, and test.build_failed needs
	// a compiler's opinion; neither is in the file, so neither may be written.
	// Their absence is what makes untested_packages report as unmeasured rather
	// than as a confident zero for every Python and TypeScript repo.
	for _, key := range []string{collect.KeyPkgWithoutTest, collect.KeyTestBuildFailed} {
		if hasMetric(res, key) {
			t.Errorf("%s was written from a JUnit report, which cannot know it", key)
		}
	}
}

// A well formed report with no cases records NOTHING, not a zero.
//
// The fixture is the document pytest 9.1.1 actually writes when it collects
// nothing: it exits 5, and the file is complete and full of zeroes. A
// misconfigured testpaths, a renamed test directory or a typo'd -k pattern all
// land here, and every one of them would otherwise publish "this repo has no
// tests" as the largest improvement it ever recorded.
func TestAnEmptyJUnitReportRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	r := junitStep(t, dir, "py-tests",
		`<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="0" failures="0" skipped="0" tests="0" time="0.004" /></testsuites>`)

	res := collectOnce(t, r)

	for _, key := range []string{
		collect.KeyTestSuites, collect.KeyTestCount,
		collect.KeyTestFailed, collect.KeyTestSkipped, collect.KeyTestDurationMS,
	} {
		if hasMetric(res, key) {
			t.Errorf("%s was recorded from a report containing no test cases, so a collection failure reads as a measurement", key)
		}
	}

	// Recorded nothing, but said so. Silence here would leave someone reading a
	// blank row with no idea whether the repo has no tests or the runner never
	// found them.
	if !strings.Contains(diagText(res), "not one test case") {
		t.Errorf("nothing explained the empty report:\n%s", diagText(res))
	}

	// The control: the same step against a report with cases in it does write
	// them, or the assertions above would pass on a parser that records nothing
	// ever.
	full := collectOnce(t, junitStep(t, t.TempDir(), "py-tests", twoFileReport))
	if !hasMetric(full, collect.KeyTestSuites) {
		t.Fatal("a populated report recorded nothing either, so this test proves nothing")
	}
}

// Two JUnit steps in one repo is the case the format exists to allow: a repo
// running pytest beside vitest. Their counts must sum rather than collide.
func TestTwoJUnitStepsCoexist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "py.xml", `<testsuite name="pytest"><testcase classname="mod" name="a" time="0.1"/></testsuite>`)
	writeFile(t, dir, "web.xml", twoFileReport)

	day := config.Duration(24 * 60 * 60 * 1e9)
	r := config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{
			{Name: "py", Artifact: "py.xml", ArtifactFormat: config.FormatJUnitXML, MaxAge: day},
			{Name: "web", Artifact: "web.xml", ArtifactFormat: config.FormatJUnitXML, MaxAge: day},
		},
	}

	// Config validation's half of this is pinned separately, in the config
	// package, by TestTwoJUnitStepsAreAllowedInOneRepo. Here the question is
	// whether the collector actually writes distinguishable rows.
	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if len(res.Failed) != 0 {
		t.Fatalf("steps failed, which is what a metric-key collision looks like: %v\n%s", res.Failed, diagText(res))
	}
	if got := metric(t, res, collect.KeyTestSuites, "py"); got != 1 {
		t.Errorf("py suites: got %v, want 1", got)
	}
	if got := metric(t, res, collect.KeyTestSuites, "web"); got != 2 {
		t.Errorf("web suites: got %v, want 2", got)
	}
}
