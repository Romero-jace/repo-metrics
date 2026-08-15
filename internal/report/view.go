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
	GeneratedAt string  `json:"generated_at"`
	WindowDays  float64 `json:"window_days"`
	// Section says which slice of the report was asked for. It is on the wire
	// because the three lists below go nil when they were not requested, and a
	// consumer looking at a null needs to be able to tell "you did not ask for
	// this" from "the tool has nothing to say".
	Section Section `json:"section"`
	// Scope says which repos the answer covers. Same reason as Section, one axis
	// over: --repo narrows what was asked about, and without this the answer to a
	// narrow question is indistinguishable from an answer to a broad one.
	Scope ScopeView `json:"scope"`
	// The three lists are allocated and empty when the section was rendered and
	// found nothing, and nil when the section was not rendered at all. Those are
	// different answers: an empty movers list says nothing moved this week, and
	// a null one says the caller asked for repos. Collapsing them would let a
	// narrowed report be read as a quiet week, which is the same absent-read-as-
	// measured mistake the repo rows below are shaped to refuse.
	Movers   []RepoView `json:"movers"`
	Repos    []RepoView `json:"repos"`
	Problems []RepoView `json:"problems"`
}

// Scope is what the caller narrowed the report to, in the terms the caller knows
// it: the name it passed to --repo, and how many repos the config names in total.
//
// It is the input side and deliberately does not carry the selected count. That
// number is a fact about the report rather than about the request, so BuildSection
// derives it and no caller is able to supply a wrong one.
type Scope struct {
	// Repo is the name --repo narrowed to, empty when the report covers
	// everything the config names.
	Repo string
	// Configured is how many repos the config names, narrowing or not.
	Configured int
}

// ScopeView is the same fact on the wire.
//
// It is always an object and never null, which is the opposite of how the
// measurement groups behave, and on purpose. A null group means nothing measured
// this; scope is not a measurement, it is a statement about which question was
// asked, and every report has an answer to that. Section is the precedent: also
// always present, also carrying an explicit value for the unnarrowed case.
//
// The failure it exists to refuse: `report --repo alpha --section problems` on a
// clean week answers with an empty problems list, and an empty problems list from
// an unnarrowed run means no repo anywhere failed to collect. Those are wildly
// different findings and without this field they are the same bytes.
type ScopeView struct {
	// Repo is nil when the report was not narrowed, which is a genuinely
	// different answer from any repo name and so cannot be an empty string: a
	// consumer testing `scope.repo == ""` would have to know that empty means all,
	// while nil reads as absent-of-filter on its own.
	Repo *string `json:"repo"`
	// Selected and Configured are counts of repos the caller and the config
	// supplied, not measurements of anything a collection did or did not find, so
	// they sit here as plain numbers rather than inside a nullable group. There is
	// no state in which they were not measured. Anything derived from what a
	// collection produced does not get to follow them here.
	Selected   int `json:"selected"`
	Configured int `json:"configured"`
}

// Narrowed reports whether --repo restricted this report to one repo. The
// template branches on it so the header line can never announce a narrowing that
// did not happen.
func (s ScopeView) Narrowed() bool { return s.Repo != nil }

// RepoName reads through the nil so the template cannot dereference it. See the
// comment on RepoView.Culprits for why these accessors are methods.
func (s ScopeView) RepoName() string {
	if s.Repo == nil {
		return ""
	}
	return *s.Repo
}

// RepoView is one repo's line in the report.
//
// Every number this tool publishes lives inside Coverage or Tests, and both are
// nil when nothing measured them. That is not a formatting choice, it is the
// only defense that holds against this project's recurring bug: something
// unmeasured presented as a measurement of zero. A flat coverage_pct sitting
// beside a coverage_measured boolean is read as `row.coverage_pct ?? 0` by the
// next consumer along, and absence turns straight back into a measured zero.
// Reaching into a null object cannot be defaulted so quietly: it throws.
//
// The boolean gates that used to sit beside the numbers are gone rather than
// kept alongside the nulls. A gate and a structural null saying the same thing
// are two sources of truth with nothing keeping them in step.
type RepoView struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	CollectedAt string `json:"collected_at"`
	// Coverage is nil when this run stored no coverage metrics at all: a failed
	// run, a repo nobody has ever collected, or a coverage profile carrying only
	// its "mode: set" header. That last one is the case a boolean gate was
	// originally added for: it parses clean, nothing downgrades the status, so
	// the snapshot is stored ok and its percentage is zero for want of a
	// denominator. Over a healthy baseline it would otherwise lead the report as
	// the week's biggest drop.
	Coverage *CoverageView `json:"coverage"`
	// Tests is nil when no test stream was parsed, which is the normal case in
	// ingest mode and whenever stdout_format is unset. The count and the
	// untested-package tally are then absent because nothing looked, not because
	// nothing is there. Publishing "tests 0" for a repo with seventy test files
	// is the same silent wrong answer one column over.
	Tests *TestsView `json:"tests"`
	// HasSnapshot is false when this repo has never been collected. Coverage and
	// Tests are both nil in that case, so it is not gating a number; it tells a
	// consumer which kind of nothing it is looking at, alongside Status.
	HasSnapshot bool   `json:"has_snapshot"`
	HasBaseline bool   `json:"has_baseline"`
	EnvChanged  bool   `json:"env_changed"`
	Error       string `json:"error,omitempty"`
}

// CoverageView is what a run actually measured about coverage, plus everything
// derived from comparing it against a baseline.
//
// The culprits and the package churn live here rather than beside it because
// they are coverage findings: buildRepo only fills them in when coverage was
// measured, and a package named as the cause of a move is as meaningless as the
// move itself when nothing measured either side. Keeping them here is also what
// lets the rule be stated without an exception: every number in the report is
// inside a nullable group.
type CoverageView struct {
	Pct     float64 `json:"pct"`
	Covered int     `json:"covered"`
	Total   int     `json:"total"`
	// DeltaPoints is nil when there is no baseline to compare against, or when
	// the baseline itself measured no coverage. This is the second, inner level
	// of null and it means something different from the outer one: measured, but
	// nothing to compare it to. A zero here is a real finding, so the two must
	// not be collapsed.
	DeltaPoints     *float64      `json:"delta_points"`
	Culprits        []CulpritView `json:"culprits"`
	AddedPackages   []string      `json:"added_packages"`
	RemovedPackages []string      `json:"removed_packages"`
}

// TestsView is what a run actually counted about tests.
type TestsView struct {
	Count int `json:"count"`
	// Delta is nil when there is no baseline, or when the baseline never parsed
	// a test stream. A repo that gained a stdout_format between runs would
	// otherwise post its entire existing suite as this week's growth.
	Delta                *int `json:"delta"`
	PackagesWithoutTests int  `json:"packages_without_tests"`
}

// Culprits, AddedPackages and RemovedPackages are read by the markdown template,
// which has no way to guard a nil group: text/template resolves a field on a nil
// pointer into a run-time error, so `{{ if .Coverage.Culprits }}` on a repo that
// measured nothing would fail while rendering rather than at build time. These
// three read through the nil instead, so a template that forgets the guard
// prints nothing rather than dying. They are methods, so they never reach the
// wire and cannot become a second flat copy of the nested data.
func (r RepoView) Culprits() []CulpritView {
	if r.Coverage == nil {
		return nil
	}
	return r.Coverage.Culprits
}

// AddedPackages reads through a nil coverage group. See Culprits.
func (r RepoView) AddedPackages() []string {
	if r.Coverage == nil {
		return nil
	}
	return r.Coverage.AddedPackages
}

// RemovedPackages reads through a nil coverage group. See Culprits.
func (r RepoView) RemovedPackages() []string {
	if r.Coverage == nil {
		return nil
	}
	return r.Coverage.RemovedPackages
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
//
// It carries no gate of its own. A culprit only exists inside a non-nil
// CoverageView, so the parent already says these figures were measured, and
// restating it here would be the redundant second source of truth the boolean
// gates were removed for.
type CulpritView struct {
	Package string `json:"package"`
	// State is changed, added, or removed. An added or removed package is
	// listed for context, never described as a regression.
	State string `json:"state"`
	// FromPct is nil for an added package and ToPct is nil for a removed one,
	// because in each case the figure was never measured: the package did not
	// exist on that side.
	//
	// They were plain float64 until the nesting work, and rendered the Go zero
	// on the absent side: a removed package went over the wire at "to_pct": 0
	// and an added one at "from_pct": 0. That is the sixth instance of this
	// project's recurring bug, and a nastier one than most because the markdown
	// already refuses to print those figures (TestPackageChurnIsNotARegression
	// pins it) while the JSON beside it published them. A consumer charting
	// from_pct against to_pct read a deleted package as a collapse to zero and
	// a brand-new one as a climb from it, which is exactly what the state field
	// exists to prevent.
	FromPct *float64 `json:"from_pct"`
	ToPct   *float64 `json:"to_pct"`
	// ContributionPoints is how many percentage points of the repo's move
	// this package accounts for. It is real in all three states: a package
	// shifts the repo total by appearing or disappearing just as much as by
	// being tested more.
	ContributionPoints float64 `json:"contribution_points"`
	Statements         int     `json:"statements"`
}

// Build converts a whole computed report into the render-ready view.
//
// Its contract is that nothing was narrowed, so it states that scope rather than
// leaving it to a zero value: every repo in the report is every repo there is.
// That is true by construction here, which is why this wrapper is safe when a
// general-purpose one taking a default would not be. A caller that did narrow has
// to say so through BuildSection.
func Build(rep delta.Report) View {
	return BuildSection(rep, SectionAll, Scope{Configured: len(rep.Repos)})
}

// BuildSection converts a computed report into the render-ready view, narrowed
// to one section and describing whatever repo narrowing produced it.
//
// Narrowing happens here rather than in either renderer so that markdown and
// JSON cannot disagree about what a section contains, which is the same reason
// they share a View at all.
func BuildSection(rep delta.Report, sec Section, scope Scope) View {
	movers, problems := rep.Movers(), rep.Problems()

	// Counted from the computed report rather than from v.Repos below, which is
	// nil under every section but repos and all. Reading the rendered slice would
	// publish "selected: 0" on every --section movers call, which is this
	// project's recurring bug wearing the field that exists to prevent it.
	sv := ScopeView{Selected: len(rep.Repos), Configured: scope.Configured}
	if scope.Repo != "" {
		sv.Repo = &scope.Repo
	}

	// The requested slices are allocated, never left nil. A nil slice marshals
	// to JSON null, so on a quiet week a consumer doing data.movers.length would
	// crash on exactly the week that is most common. The slices that were not
	// requested stay nil on purpose: see the comment on View.
	v := View{
		GeneratedAt: rep.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"),
		WindowDays:  rep.Window.Hours() / 24,
		Section:     sec,
		Scope:       sv,
	}
	if sec.shows(SectionRepos) {
		v.Repos = make([]RepoView, 0, len(rep.Repos))
		for _, r := range rep.Repos {
			v.Repos = append(v.Repos, buildRepo(r))
		}
	}
	if sec.shows(SectionMovers) {
		v.Movers = make([]RepoView, 0, len(movers))
		for _, r := range movers {
			// delta.Compute has no opinion about status: it marks a repo a mover
			// from the size of its move, and a failed run's "move" is its
			// baseline measured against zero. Left in, the report leads with the
			// biggest drop of the week, which is really a crashed test command. A
			// repo with no snapshot at all is the same artifact one step further
			// along. A run that stored no coverage metrics is the third door into
			// the same room: it comes back status ok, so neither test above
			// catches it, and its "move" is still a healthy baseline measured
			// against zero.
			if failed(r.Head) || neverCollected(r.Head) || !r.HasCoverageData {
				continue
			}
			v.Movers = append(v.Movers, buildRepo(r))
		}
	}
	if sec.shows(SectionProblems) {
		v.Problems = make([]RepoView, 0, len(problems))
		for _, r := range problems {
			v.Problems = append(v.Problems, buildRepo(r))
		}
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
		Name:        r.Repo.Name,
		HasBaseline: r.HasBaseline,
		EnvChanged:  r.EnvChanged,
		Status:      StatusNotCollected,
	}
	if r.Head != nil {
		out.HasSnapshot = true
		out.Status = string(r.Head.Status)
		out.CollectedAt = r.Head.CollectedAt.UTC().Format("2006-01-02 15:04 UTC")
		out.Error = r.Head.Error
	}

	// A repo nobody has ever collected is not a measurement at zero, so it gets
	// the same treatment as a failed run: status says what happened and no
	// measurement group is published at all. Filling one in here instead would
	// put a configured-but-never-run repo in the table at 0.0% coverage with
	// zero packages untested, which reads as a real, healthy measurement of an
	// empty repo.
	if neverCollected(r.Head) {
		return out
	}

	// A failed run produced no metrics, so every comparison derived from it is
	// the baseline measured against nothing: a total coverage cliff, every test
	// gone, every package listed as deleted. Those are artifacts of the zero
	// values, not findings, so neither group is published. Status and Error say
	// what actually happened.
	if failed(r.Head) {
		return out
	}

	if r.HasTestData {
		tests := TestsView{
			Count:                r.HeadTests,
			PackagesWithoutTests: r.PkgWithoutTests,
		}
		// The test delta needs both sides measured, not just a baseline to
		// exist. A repo that gained a stdout_format between runs would otherwise
		// post its entire suite as this week's growth.
		if r.TestChangeMeaningful() {
			testsDelta := r.TestChange()
			tests.Delta = &testsDelta
		}
		out.Tests = &tests
	}

	if !r.HasCoverageData {
		return out
	}
	coverage := CoverageView{
		Pct:     r.HeadCoverage.Pct(),
		Covered: r.HeadCoverage.Covered,
		Total:   r.HeadCoverage.Total,
	}

	// Package churn is a comparison too. A head that measured no coverage makes
	// every baseline package look deleted, so the report would list a repo's
	// whole tree as gone on the strength of an empty profile.
	if r.CoverageChangeMeaningful() || !r.HasBaseline {
		coverage.AddedPackages = r.Added
		coverage.RemovedPackages = r.Removed
	}

	// A baseline existing is not enough for a coverage delta: both sides have
	// to have measured. Otherwise an empty profile subtracts its zero from a
	// real baseline and posts the entire baseline as this week's drop.
	//
	// Culprits explain a coverage move under the same condition. With nothing
	// measured on one side there is no move to explain, only an artifact, and
	// naming a package as its cause is worse than saying nothing.
	if r.CoverageChangeMeaningful() {
		coverageDelta := r.CoverageChange()
		coverage.DeltaPoints = &coverageDelta
		for _, c := range r.Culprits {
			view := CulpritView{
				Package:            c.Package,
				State:              string(c.State),
				ContributionPoints: c.Contribution,
				Statements:         maxInt(c.Head.Total, c.Base.Total),
			}
			// Each side is published only in the states where something
			// measured it. Coverage.Pct() answers 0 on a package that is not
			// there, because its total is zero, and that zero is the fabricated
			// figure this switch exists to withhold.
			if c.State != delta.StateAdded {
				from := c.Base.Pct()
				view.FromPct = &from
			}
			if c.State != delta.StateRemoved {
				to := c.Head.Pct()
				view.ToPct = &to
			}
			coverage.Culprits = append(coverage.Culprits, view)
		}
	}

	out.Coverage = &coverage
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
