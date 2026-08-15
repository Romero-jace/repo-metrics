package report

import (
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

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
	Name                 string        `json:"name"`
	Status               string        `json:"status"`
	CollectedAt          string        `json:"collected_at"`
	CoveragePct          float64       `json:"coverage_pct"`
	CoveredStatements    int           `json:"covered_statements"`
	TotalStatements      int           `json:"total_statements"`
	CoverageDeltaPoints  *float64      `json:"coverage_delta_points"`
	Tests                int           `json:"tests"`
	TestsDelta           *int          `json:"tests_delta"`
	PackagesWithoutTests int           `json:"packages_without_tests"`
	HasBaseline          bool          `json:"has_baseline"`
	EnvChanged           bool          `json:"env_changed"`
	Culprits             []CulpritView `json:"culprits"`
	AddedPackages        []string      `json:"added_packages"`
	RemovedPackages      []string      `json:"removed_packages"`
	Error                string        `json:"error,omitempty"`
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
	v := View{
		GeneratedAt: rep.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"),
		WindowDays:  rep.Window.Hours() / 24,
		Repos:       make([]RepoView, 0, len(rep.Repos)),
	}
	for _, r := range rep.Repos {
		v.Repos = append(v.Repos, buildRepo(r))
	}
	for _, r := range rep.Movers() {
		// delta.Compute has no opinion about status: it marks a repo a mover
		// from the size of its move, and a failed run's "move" is its baseline
		// measured against zero. Left in, the report leads with the biggest
		// drop of the week, which is really a crashed test command.
		if failed(r.Head) {
			continue
		}
		v.Movers = append(v.Movers, buildRepo(r))
	}
	for _, r := range rep.Problems() {
		v.Problems = append(v.Problems, buildRepo(r))
	}
	return v
}

// failed reports whether a snapshot came back with nothing usable. A partial
// one is not failed: it carries real numbers and they stay in the report.
func failed(s *store.Snapshot) bool { return s != nil && s.Status == store.StatusFailed }

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
		Status:               string(store.StatusOK),
	}
	if r.Head != nil {
		out.Status = string(r.Head.Status)
		out.CollectedAt = r.Head.CollectedAt.UTC().Format("2006-01-02 15:04 UTC")
		out.Error = r.Head.Error
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
	out.AddedPackages = r.Added
	out.RemovedPackages = r.Removed

	// Only publish deltas when there is something to compare against.
	if r.HasBaseline {
		coverageDelta := r.CoverageChange()
		testsDelta := r.TestChange()
		out.CoverageDeltaPoints = &coverageDelta
		out.TestsDelta = &testsDelta
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
