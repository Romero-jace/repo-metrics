package report

import (
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// StatusNotCollected is the status of a configured repo that has no snapshot at
// all: nobody has ever run a collection against it, or every run so far was
// discarded before it was stored.
//
// It is deliberately not one of store's statuses. Calling that repo ok publishes
// a healthy zero (0.0% coverage, 0 packages without tests) for something that was
// never measured, which is indistinguishable from a repo that really is at zero.
const StatusNotCollected = "not collected"

// View is the single computed shape both renderers consume.
//
// Markdown and JSON are two renderings of this one value rather than two
// independent walks of the report, which is what makes it structurally
// impossible for them to disagree about a number.
type View struct {
	GeneratedAt string     `json:"generated_at"`
	WindowDays  float64    `json:"window_days"`
	Movers      []RepoView `json:"movers"`
	Repos       []RepoView `json:"repos"`
	Problems    []RepoView `json:"problems"`
}

// RepoView is one repo's line in the report.
//
// The delta fields are pointers so that "no baseline yet" is representable.
// A zero delta and an absent one mean very different things, and collapsing
// them is how a first run ends up claiming nothing changed.
type RepoView struct {
	Name                 string   `json:"name"`
	Status               string   `json:"status"`
	CollectedAt          string   `json:"collected_at"`
	CoveragePct          float64  `json:"coverage_pct"`
	CoveredStatements    int      `json:"covered_statements"`
	TotalStatements      int      `json:"total_statements"`
	CoverageDeltaPoints  *float64 `json:"coverage_delta_points"`
	Tests                int      `json:"tests"`
	TestsDelta           *int     `json:"tests_delta"`
	PackagesWithoutTests int      `json:"packages_without_tests"`
	// TestsMeasured is false when no test stream was parsed, which is the
	// normal case in ingest mode and whenever stdout_format is unset. Tests and
	// PackagesWithoutTests are then zero because nothing looked, not because
	// nothing is there. Rendering that as "tests 0" tells a reader with seventy
	// test files something flatly false, so the template says so instead.
	TestsMeasured bool `json:"tests_measured"`
	// CoverageMeasured is false when the snapshot stored no coverage metrics at
	// all, which is what a coverage profile carrying only its "mode: set" header
	// produces. Nothing downgrades the status in that case, so the run is stored
	// ok and CoveragePct is 0 because the total is 0, not because the code is
	// uncovered. Over a healthy baseline that repo would otherwise lead the
	// report as the week's biggest drop.
	//
	// It is deliberately not folded into HasSnapshot. A snapshot existing and a
	// snapshot having measured coverage are different questions, and answering
	// the second with the first would bury this gap instead of naming it.
	CoverageMeasured bool `json:"coverage_measured"`
	// HasSnapshot is false when this repo has never been collected. Every count
	// on such a repo is a Go zero value rather than a measurement, so a consumer
	// has to be able to tell before it charts any of them.
	HasSnapshot     bool          `json:"has_snapshot"`
	HasBaseline     bool          `json:"has_baseline"`
	EnvChanged      bool          `json:"env_changed"`
	Culprits        []CulpritView `json:"culprits"`
	AddedPackages   []string      `json:"added_packages"`
	RemovedPackages []string      `json:"removed_packages"`
	Error           string        `json:"error,omitempty"`
}

// EnvWarned reports whether this repo's delta has to carry the toolchain
// warning. The template calls it everywhere it prints a delta, both in the
// movers write-up and in the every-repo table, so a repo whose coverage barely
// moved still gets told on: its delta spans a toolchain change and is exactly
// as untrustworthy as a big mover's, and warning only about big moves means the
// small ones get read as real code changes.
//
// It also requires a delta to actually be on display. A failed or never
// collected run publishes no delta, and a footnote about a marker that is not
// in the table is just noise.
func (r RepoView) EnvWarned() bool {
	return r.EnvChanged && r.HasBaseline && r.HasSnapshot && r.Status != string(store.StatusFailed)
}

// CulpritView is one package named as accounting for a repo's move.
type CulpritView struct {
	Package string `json:"package"`
	// State is changed, added, or removed. An added or removed package is
	// listed for context, never described as a regression.
	State   string  `json:"state"`
	FromPct float64 `json:"from_pct"`
	ToPct   float64 `json:"to_pct"`
	// ContributionPoints is how many percentage points of the repo's move
	// this package accounts for.
	ContributionPoints float64 `json:"contribution_points"`
	Statements         int     `json:"statements"`
}

// Build converts a computed report into the render-ready view.
func Build(rep delta.Report) View {
	movers, problems := rep.Movers(), rep.Problems()

	// All three slices are allocated, never left nil. A nil slice marshals to
	// JSON null, so on a quiet week a consumer doing data.movers.length would
	// crash on exactly the week that is most common.
	v := View{
		GeneratedAt: rep.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"),
		WindowDays:  rep.Window.Hours() / 24,
		Repos:       make([]RepoView, 0, len(rep.Repos)),
		Movers:      make([]RepoView, 0, len(movers)),
		Problems:    make([]RepoView, 0, len(problems)),
	}
	for _, r := range rep.Repos {
		v.Repos = append(v.Repos, buildRepo(r))
	}
	for _, r := range movers {
		// delta.Compute has no opinion about status: it marks a repo a mover
		// from the size of its move, and a failed run's "move" is its baseline
		// measured against zero. Left in, the report leads with the biggest
		// drop of the week, which is really a crashed test command. A repo with
		// no snapshot at all is the same artifact one step further along. A run
		// that stored no coverage metrics is the third door into the same room:
		// it comes back status ok, so neither test above catches it, and its
		// "move" is still a healthy baseline measured against zero.
		if failed(r.Head) || neverCollected(r.Head) || !r.HasCoverageData {
			continue
		}
		v.Movers = append(v.Movers, buildRepo(r))
	}
	for _, r := range problems {
		v.Problems = append(v.Problems, buildRepo(r))
	}
	return v
}

// failed reports whether a snapshot came back with nothing usable. A partial
// one is not failed: it carries real numbers and they stay in the report.
func failed(s *store.Snapshot) bool { return s != nil && s.Status == store.StatusFailed }

// neverCollected reports whether a repo is configured but has no snapshot at
// all. It is kept separate from failed so that neither name has to mean two
// things: one is a run that came back with nothing, the other is no run.
func neverCollected(s *store.Snapshot) bool { return s == nil }

func buildRepo(r delta.RepoDelta) RepoView {
	out := RepoView{
		Name:                 r.Repo.Name,
		CoveragePct:          r.HeadCoverage.Pct(),
		CoveredStatements:    r.HeadCoverage.Covered,
		TotalStatements:      r.HeadCoverage.Total,
		Tests:                r.HeadTests,
		PackagesWithoutTests: r.PkgWithoutTests,
		HasBaseline:          r.HasBaseline,
		EnvChanged:           r.EnvChanged,
		Status:               StatusNotCollected,
	}
	if r.Head != nil {
		out.HasSnapshot = true
		out.Status = string(r.Head.Status)
		out.CollectedAt = r.Head.CollectedAt.UTC().Format("2006-01-02 15:04 UTC")
		out.Error = r.Head.Error
	}

	// A repo nobody has ever collected is not a measurement at zero, so it gets
	// the same treatment as a failed run: status says what happened and nothing
	// derived from the missing numbers is published. Seeding the status ok here
	// instead would put a configured-but-never-run repo in the table at 0.0%
	// coverage with zero packages untested, which reads as a real, healthy
	// measurement of an empty repo.
	if neverCollected(r.Head) {
		return out
	}

	// A failed run produced no metrics, so every comparison derived from it is
	// the baseline measured against nothing: a total coverage cliff, every test
	// gone, every package listed as deleted. Those are artifacts of the zero
	// values, not findings, so none of them are published. Status and Error say
	// what actually happened, and the raw counts stay zero because that is what
	// came back.
	if failed(r.Head) {
		return out
	}
	out.TestsMeasured = r.HasTestData
	// Set after the two early returns above, so a failed or never collected run
	// reports false without either branch having to remember to say so.
	out.CoverageMeasured = r.HasCoverageData
	// Package churn is a comparison too. A head that measured no coverage makes
	// every baseline package look deleted, so the report would list a repo's
	// whole tree as gone on the strength of an empty profile.
	if r.CoverageChangeMeaningful() || !r.HasBaseline {
		out.AddedPackages = r.Added
		out.RemovedPackages = r.Removed
	}

	// A baseline existing is not enough for a coverage delta: both sides have
	// to have measured. Otherwise an empty profile subtracts its zero from a
	// real baseline and posts the entire baseline as this week's drop.
	if r.CoverageChangeMeaningful() {
		coverageDelta := r.CoverageChange()
		out.CoverageDeltaPoints = &coverageDelta
	}
	// The test delta needs both sides measured, not just a baseline to exist.
	// A repo that gained a stdout_format between runs would otherwise post its
	// entire suite as this week's growth.
	if r.TestChangeMeaningful() {
		testsDelta := r.TestChange()
		out.TestsDelta = &testsDelta
	}

	// Culprits explain a coverage move. With nothing measured on one side there
	// is no move to explain, only an artifact, and naming a package as its
	// cause is worse than saying nothing.
	if !r.CoverageChangeMeaningful() {
		return out
	}

	for _, c := range r.Culprits {
		out.Culprits = append(out.Culprits, CulpritView{
			Package:            c.Package,
			State:              string(c.State),
			FromPct:            c.Base.Pct(),
			ToPct:              c.Head.Pct(),
			ContributionPoints: c.Contribution,
			Statements:         maxInt(c.Head.Total, c.Base.Total),
		})
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
