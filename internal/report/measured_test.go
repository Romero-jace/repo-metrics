package report_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// A repo collected in ingest mode never has its tests counted, because there is
// no stdout stream to parse. Rendering that as "tests 0" tells a reader whose
// repo has seventy test files something flatly false, and gives no hint that
// anything is missing. It is the same silent wrong answer as showing a
// never-collected repo at 0.0% coverage.
//
// Found by running the tool against real repos, not by a unit test: go-tool
// reported "Tests 0" while carrying 71 test files.
func TestUnmeasuredTestsAreNotRenderedAsZero(t *testing.T) {
	rep := delta.Compute([]delta.Input{
		{
			// Ingest mode: coverage only, no test stream.
			Repo:        store.Repo{ID: 1, Name: repoQuiet},
			Head:        snap(11, 1, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: cov(pkgAlpha, 500, 1000),
			Base:        snap(10, 1, "go1.26.5", store.StatusOK, ""),
			BaseMetrics: cov(pkgAlpha, 500, 1000),
		},
		{
			// Measured, and genuinely has no tests. This zero is real and must
			// still print as a number.
			Repo:        store.Repo{ID: 2, Name: repoFresh},
			Head:        snap(21, 2, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 0, 1000), testStream(3)),
		},
	}, options(), fixedNow())

	md := mustMarkdown(t, rep)

	unmeasured := repoRow(t, md, repoQuiet)
	if !strings.Contains(unmeasured, "not measured") {
		t.Errorf("ingest-mode repo does not say its tests were not measured:\n%s", unmeasured)
	}
	// The bug this pins: a bare "| 0 |" in the tests column reads as a
	// measurement of zero.
	if strings.Contains(unmeasured, "| 0 |") {
		t.Errorf("unmeasured tests rendered as a zero count:\n%s", unmeasured)
	}

	measured := repoRow(t, md, repoFresh)
	if strings.Contains(measured, "not measured") {
		t.Errorf("a repo that really was measured is claimed unmeasured:\n%s", measured)
	}
	if !strings.Contains(measured, "| 3 |") {
		t.Errorf("measured packages-without-tests count of 3 is missing:\n%s", measured)
	}

	rows := jsonRepoRows(t, rep)
	// The gate a consumer used to read is gone, and the group it governed is
	// gone with it. That is the point: there is no longer a tests count on the
	// wire for an ingest-mode repo to be defaulted to zero.
	v, present := rows[repoQuiet]["tests"]
	if !present {
		t.Error("the tests key is missing rather than null for the ingest-mode repo, so a consumer defaulting it sees a count of zero")
	}
	if v != nil {
		t.Errorf("tests: got %v for the ingest-mode repo, want null", v)
	}
	measuredTests, ok := rows[repoFresh]["tests"].(map[string]any)
	if !ok {
		t.Fatalf("tests: got %v for the measured repo, want an object", rows[repoFresh]["tests"])
	}
	if got := measuredTests["packages_without_tests"]; got != float64(3) {
		t.Errorf("tests.packages_without_tests: got %v, want 3; this zero-free count was really measured", got)
	}
}

// Turning on stdout_format between two runs must not post the repo's whole
// existing suite as this week's growth, and must not make it a mover on that
// basis alone.
func TestGainingTestDataIsNotTestGrowth(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo:        store.Repo{ID: 1, Name: repoSteady},
		Head:        snap(11, 1, "go1.26.5", store.StatusOK, ""),
		HeadMetrics: metrics(cov(pkgAlpha, 500, 1000), testStream(0, testCount(pkgAlpha, 400))),
		Base:        snap(10, 1, "go1.26.5", store.StatusOK, ""),
		BaseMetrics: cov(pkgAlpha, 500, 1000),
	}}, options(), fixedNow())

	md := mustMarkdown(t, rep)
	if strings.Contains(md, "+400") {
		t.Errorf("the whole suite was reported as this week's test growth:\n%s", md)
	}

	rows := jsonRepoRows(t, rep)
	tests, ok := rows[repoSteady]["tests"].(map[string]any)
	if !ok {
		t.Fatalf("tests: got %v, want an object; the head really did count tests", rows[repoSteady]["tests"])
	}
	// The inner null. The head measured, so the group is there; the baseline did
	// not, so there is nothing to subtract from.
	if got := tests["delta"]; got != nil {
		t.Errorf("tests.delta: got %v, want null when the baseline never measured tests", got)
	}
}
