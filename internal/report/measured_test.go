package report_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// "no baseline yet" is a claim about the repo, not about one signal, so a row
// must not make it while another column in the same row disproves it.
//
// This shipped and was found by review. A repo that gained a lint step this week
// rendered its coverage column as +10.0 pts against a baseline, and its lint
// column as "no baseline yet", in the same line. Both came from one null delta
// meaning two different things.
func TestNoBaselineYetIsOnlySaidWhenThereIsNoBaseline(t *testing.T) {
	rep := delta.Compute([]delta.Input{
		{
			// A real baseline, with coverage. Nothing linted it back then.
			Repo:        store.Repo{ID: 1, Name: repoQuiet},
			Head:        snap(11, 1, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 60, 100), lintRun("lint", 7, 0, 0)),
			Base:        snap(10, 1, "go1.26.5", store.StatusOK, ""),
			BaseMetrics: cov(pkgAlpha, 50, 100),
		},
		{
			// No baseline at all. This one may say so.
			Repo:        store.Repo{ID: 2, Name: repoFresh},
			Head:        snap(21, 2, "go1.26.5", store.StatusOK, ""),
			HeadMetrics: metrics(cov(pkgAlpha, 60, 100), lintRun("lint", 7, 0, 0)),
		},
	}, options(), fixedNow())

	md := mustMarkdown(t, rep)

	withBaseline := repoRow(t, md, repoQuiet)
	if !strings.Contains(withBaseline, "+10.0 pts") {
		t.Fatalf("fixture wrong: this repo should show a coverage delta:\n%s", withBaseline)
	}
	if strings.Contains(withBaseline, "no baseline yet") {
		t.Errorf("a row showing a delta against its baseline also claims there is no baseline:\n%s", withBaseline)
	}
	if !strings.Contains(withBaseline, "not comparable") {
		t.Errorf("the signal that could not be compared does not say so:\n%s", withBaseline)
	}

	// The anti-vacuity control. Without it, deleting the phrase entirely would
	// pass the assertion above.
	if noBaseline := repoRow(t, md, repoFresh); !strings.Contains(noBaseline, "no baseline yet") {
		t.Errorf("a repo with no baseline at all stopped saying so:\n%s", noBaseline)
	}
}

// A repo collected in ingest mode never has its tests counted, because there is
// no stdout stream to parse. Rendering that as "tests 0" tells a reader whose
// repo has seventy test files something flatly false, and gives no hint that
// anything is missing. It is the same silent wrong answer as showing a
// never-collected repo at 0.0% coverage.
//
// Found by running the tool against real repos, not by a unit test: one of
// them, a Go CLI, reported "Tests 0" while carrying 71 test files.
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

	// Checked cell by cell rather than by scanning the whole row. This repo
	// legitimately says "not measured" in the lint columns, since nothing linted
	// it, so a row-wide scan asserts something this test never meant: that a repo
	// measuring ANY signal measured EVERY signal. It held only while every column
	// came from the same two parsers.
	measuredCells := tableCells(t, md, repoFresh)
	for _, col := range []string{"tests", "packages without tests"} {
		got := measuredCells[slices.Index(degradedColumns[:], col)]
		if strings.Contains(got, "not measured") {
			t.Errorf("the %s column reads %q for a repo whose test stream really was parsed:\n%s",
				col, got, repoRow(t, md, repoFresh))
		}
	}
	if got := measuredCells[slices.Index(degradedColumns[:], "packages without tests")]; got != "3" {
		t.Errorf("packages without tests reads %q, want the measured count 3", got)
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
	if got := measuredTests["value"]; got == nil {
		t.Error("tests.value is null for a repo whose stream was parsed")
	}

	// The untested-package tally moved out of the tests group and became a
	// signal of its own, so it now carries a delta: a repo gaining an untested
	// package is news, and inside the tests group it could never say so.
	untested, ok := rows[repoFresh]["untested_packages"].(map[string]any)
	if !ok {
		t.Fatalf("untested_packages: got %v for the measured repo, want an object", rows[repoFresh]["untested_packages"])
	}
	if got := untested["value"]; got != float64(3) {
		t.Errorf("untested_packages.value: got %v, want 3; this zero-free count was really measured", got)
	}
	if rows[repoQuiet]["untested_packages"] != nil {
		t.Errorf("untested_packages: got %v for the ingest-mode repo, want null", rows[repoQuiet]["untested_packages"])
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
