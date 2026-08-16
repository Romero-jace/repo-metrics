package report_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// The gate that blanked the table cell did not reach everything derived from
// the same comparison.
//
// An empty coverage profile parses clean and stores status ok, so a head that
// measured nothing subtracted its zero from a real baseline and published
// coverage_delta_points of -72, a culprit naming the largest package as the
// cause, and every baseline package listed as deleted. The markdown table said
// "not measured" while the JSON beside it said the repo lost 72 points and
// named who did it.
//
// Contrast the tests path, which got this right: tests_delta is null unless
// BOTH sides measured. Coverage got a boolean gate where tests got a null, and
// the boolean only reached the cell.
func TestEmptyProfilePublishesNoComparison(t *testing.T) {
	const name = "hollowrepo"

	rep := delta.Compute([]delta.Input{{
		Repo: store.Repo{ID: 1, Name: name},
		// Head parsed a stream but stored no coverage: the header-only profile.
		Head:        snap(11, 1, "go1.26.5", store.StatusOK, ""),
		HeadMetrics: testStream(0),
		Base:        snap(10, 1, "go1.26.5", store.StatusOK, ""),
		BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0)),
	}}, options(), fixedNow())

	row := jsonRepoRows(t, rep)[name]

	// One assertion now covers what three used to. The delta, the culprits and
	// the churn lists all lived beside a boolean that said they meant nothing;
	// they live inside the coverage group instead, so an unmeasured head has
	// nowhere to put them. The key still has to be present and null, because a
	// missing key is what a consumer defaults.
	v, present := row["coverage"]
	if !present {
		t.Fatal("the coverage key is missing rather than null, so a consumer " +
			"defaulting it sees a measured zero")
	}
	if v != nil {
		t.Errorf("coverage: got %v, want null. Nothing was measured, so any "+
			"comparison here posts the whole baseline as this week's drop.", v)
	}
	// The tests group is the control: this run did parse a test stream, so the
	// coverage null above has to be about coverage rather than about the row
	// having been blanked wholesale.
	if _, ok := row["tests"].(map[string]any); !ok {
		t.Errorf("tests: got %v, want an object. This run measured tests, and a "+
			"gate that blanks a real measurement is the opposite failure.", row["tests"])
	}

	// And the prose must not describe the phantom move either.
	md := mustMarkdown(t, rep)
	if strings.Contains(md, "-72.0 pts") {
		t.Errorf("the report quotes a fabricated 72 point drop:\n%s", md)
	}
	if strings.Contains(md, pkgAlpha) {
		t.Errorf("the report names %s over a move that was never measured:\n%s", pkgAlpha, md)
	}
}

// The control: when both sides really did measure, all of that must still be
// published. A gate that blanks a real comparison is the opposite failure and
// just as bad.
func TestMeasuredBothSidesStillPublishesTheComparison(t *testing.T) {
	const name = "realrepo"

	rep := delta.Compute([]delta.Input{{
		Repo:        store.Repo{ID: 2, Name: name},
		Head:        snap(21, 2, "go1.26.5", store.StatusOK, ""),
		HeadMetrics: metrics(cov(pkgAlpha, 40, 100), testStream(0)),
		Base:        snap(20, 2, "go1.26.5", store.StatusOK, ""),
		BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0)),
	}}, options(), fixedNow())

	row := jsonRepoRows(t, rep)[name]

	coverage, ok := row["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("coverage: got %v, want an object for a repo that measured both sides", row["coverage"])
	}
	if coverage["delta"] == nil {
		t.Error("coverage.delta is null for a repo that measured both sides")
	}
	if got, _ := coverage["culprits"].([]any); len(got) == 0 {
		t.Error("no culprits for a real 32 point drop")
	}
}
