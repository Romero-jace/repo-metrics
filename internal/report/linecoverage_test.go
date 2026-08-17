package report_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// lcovOnly is the repo this whole change exists for: a Python or TypeScript
// service whose coverage arrives as an LCOV tracefile, so it stores line counts
// per source file and no statement counts at all.
//
// src/big.ts drops 40 lines and src/small.ts gains 1, over a 20-line floor that
// puts src/tiny.ts out of the ranking entirely.
func lcovOnly() delta.Report {
	head := metrics(
		covLines("src/big.ts", 160, 200),
		covLines("src/small.ts", 96, 100),
		covLines("src/tiny.ts", 1, 4),
	)
	base := metrics(
		covLines("src/big.ts", 200, 200),
		covLines("src/small.ts", 95, 100),
		covLines("src/tiny.ts", 4, 4),
	)
	return delta.Compute([]delta.Input{{
		Repo:        store.Repo{Name: "webapp"},
		Head:        snap(2, 1, "node=v24.14.1", store.StatusOK, ""),
		Base:        snap(1, 1, "node=v24.14.1", store.StatusOK, ""),
		HeadMetrics: head, BaseMetrics: base,
	}}, options(), fixedNow())
}

// The pair of assertions that proves the fix without proving the unit merge.
//
// A repo measured only through LCOV must get a named per-file culprit ranking —
// which it did not before, because the per-scope map was built from statement
// keys alone, so min_statements and the whole ranking governed nothing for it.
//
// And `coverage` must still be null for that same repo, in the same run. That
// half is not a formality: the cheapest way to make culprits appear would have
// been to feed line counts into the statement group, which is exactly the unit
// merge the split metric keys exist to prevent. One assertion without the other
// would pass for the wrong implementation.
func TestLineCoverageIsRankedWithoutBorrowingStatementCoverage(t *testing.T) {
	view := report.Build(lcovOnly()).Repos[0]

	if view.CoverageLines == nil {
		t.Fatal("a repo whose only coverage is an LCOV tracefile reports no line coverage group at all")
	}
	if got := len(view.CoverageLines.Culprits); got != 2 {
		t.Fatalf("line culprits: got %d, want 2 (src/tiny.ts is below the 20-line floor)", got)
	}

	top := view.CoverageLines.Culprits[0]
	if top.Scope != "src/big.ts" {
		t.Errorf("top line culprit is %q, want src/big.ts — the ranking is by contribution to the repo total, and the 200-line file that lost 40 lines outweighs the 100-line file that gained 1", top.Scope)
	}
	if top.Units != 200 {
		t.Errorf("top culprit units = %d, want 200. This field counts LINES here and statements under the coverage group, which is why it is not called statements", top.Units)
	}
	if top.ContributionPoints >= 0 {
		t.Errorf("top culprit contributed %v points, want a negative number: the file lost coverage", top.ContributionPoints)
	}

	// The other half. Nothing measured statements, so the statement group is
	// absent rather than filled from the line counts sitting right beside it.
	if view.Coverage != nil {
		t.Errorf("statement coverage was published for a repo that measured none of it, at %v%% — the line counts have been merged into the statement group, which is the arithmetic the separate metric keys exist to prevent", view.Coverage.Value)
	}
}

// The same claim at the wire, because the group being nil in Go and the key
// being null in JSON are two different things and only the second is what a
// consumer sees.
func TestAnLCOVRepoPublishesNullStatementCoverageAndRealLineCulprits(t *testing.T) {
	out, err := json.Marshal(report.Build(lcovOnly()))
	if err != nil {
		t.Fatalf("marshaling the payload: %v", err)
	}
	var payload struct {
		Repos []struct {
			Coverage      *json.RawMessage `json:"coverage"`
			CoverageLines *struct {
				Value    float64 `json:"value"`
				Total    int     `json:"total"`
				Culprits []struct {
					Scope string `json:"scope"`
					Units int    `json:"units"`
				} `json:"culprits"`
			} `json:"coverage_lines"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("reading the payload back: %v", err)
	}
	if len(payload.Repos) != 1 {
		t.Fatalf("want 1 repo in the payload, got %d", len(payload.Repos))
	}
	repo := payload.Repos[0]

	if repo.Coverage != nil && string(*repo.Coverage) != "null" {
		t.Errorf(`"coverage" is %s, want null: this repo measured no statements`, string(*repo.Coverage))
	}
	if repo.CoverageLines == nil {
		t.Fatal(`"coverage_lines" is null for a repo that measured line coverage`)
	}
	if repo.CoverageLines.Total != 304 {
		t.Errorf("line total = %d, want 304 (200 + 100 + 4, including the file below the ranking floor)", repo.CoverageLines.Total)
	}
	if len(repo.CoverageLines.Culprits) == 0 {
		t.Fatal(`"coverage_lines.culprits" is empty, so the payload names a coverage move with nothing accounting for it`)
	}
	// The old field names must be gone rather than sitting alongside the new
	// ones: publishing a line count under "statements" is the thing this rename
	// was for, and an additive change would have left it there.
	for _, gone := range []string{`"statements"`, `"package"`, `"added_packages"`, `"removed_packages"`} {
		if strings.Contains(string(out), gone) {
			t.Errorf("the payload still carries %s, which names a unit this group does not count", gone)
		}
	}
}

// A Go repo must be unaffected, and its line-coverage group must be null rather
// than an empty object. Both accessors the template calls on it read through a
// nil group, which is the path that panics if the nil check is dropped — and the
// path most of a Go-heavy fleet takes.
func TestAGoRepoReportsNullLineCoverageAndSurvivesTheTemplateAccessors(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo:        store.Repo{Name: "api"},
		Head:        snap(2, 1, "go1.26", store.StatusOK, ""),
		Base:        snap(1, 1, "go1.26", store.StatusOK, ""),
		HeadMetrics: metrics(cov("m/pkg", 160, 200)),
		BaseMetrics: metrics(cov("m/pkg", 200, 200)),
	}}, options(), fixedNow())

	view := report.Build(rep).Repos[0]

	if view.CoverageLines != nil {
		t.Errorf("a Go repo reports a line-coverage group at %v%%, which nothing measured", view.CoverageLines.Value)
	}
	if view.Coverage == nil {
		t.Fatal("a Go repo reports no statement coverage, so this test is not exercising what it names")
	}
	if len(view.Coverage.Culprits) == 0 {
		t.Error("the statement ranking came back empty, so the line work broke the path it was meant to leave alone")
	}

	// These three are what the markdown template calls. Before the line group
	// carried culprits they could not be reached on a nil group at all.
	if got := view.LineCulprits(); got != nil {
		t.Errorf("LineCulprits on a repo with no line coverage returned %v, want nil", got)
	}
	if got := view.AddedFiles(); got != nil {
		t.Errorf("AddedFiles on a repo with no line coverage returned %v, want nil", got)
	}
	if got := view.RemovedFiles(); got != nil {
		t.Errorf("RemovedFiles on a repo with no line coverage returned %v, want nil", got)
	}
}
