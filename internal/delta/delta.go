// Package delta turns stored snapshots into the thing anyone actually reads:
// what moved, and which package accounts for it.
//
// Everything here is a pure function over values loaded elsewhere, so the whole
// package tests without a database.
package delta

import (
	"math"
	"sort"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// DefaultMaxCulprits bounds how many packages are named per repo. A report that
// lists forty packages is a dashboard, and nobody reads dashboards.
const DefaultMaxCulprits = 5

// Coverage is a pair of statement counts. Percentages are derived, never
// stored, because a repo's rate is sum(covered)/sum(total) and not the mean of
// its packages' rates.
type Coverage struct {
	Covered int
	Total   int
}

// Pct is the coverage percentage, or zero when nothing is instrumented.
func (c Coverage) Pct() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Covered) / float64(c.Total) * 100
}

// State says how a package relates to the baseline.
type State string

const (
	// StateChanged means the package exists in both snapshots.
	StateChanged State = "changed"
	// StateAdded means it appears only in the newer snapshot.
	StateAdded State = "added"
	// StateRemoved means it appears only in the baseline.
	StateRemoved State = "removed"
)

// PackageDelta is one package's movement.
type PackageDelta struct {
	Package string
	Head    Coverage
	Base    Coverage
	State   State
	// Contribution is how many percentage points of the repo's overall move
	// this package accounts for. See culprits for why it is not simply the
	// package's own change in percentage.
	Contribution float64
}

// PointChange is the package's own coverage movement, in percentage points.
// Useful to display, useless for ranking.
func (p PackageDelta) PointChange() float64 { return p.Head.Pct() - p.Base.Pct() }

// RepoDelta is one repo's movement between two snapshots.
type RepoDelta struct {
	Repo store.Repo
	Head *store.Snapshot
	Base *store.Snapshot

	// HeadSignals and BaseSignals are what each side measured, keyed by signal.
	//
	// They are exported so a test can build a RepoDelta that Compute does not
	// currently produce, which report/backstop_test.go exists to do. That is safe
	// because a Measurement can only be built by Measured or Unmeasured, and a
	// nil map answers Unmeasured for every key, so a half-filled literal fails
	// closed instead of reporting confident zeroes.
	HeadSignals map[SignalID]Measurement
	BaseSignals map[SignalID]Measurement

	HeadCoverage Coverage
	BaseCoverage Coverage
	HeadTests    int
	BaseTests    int
	// PkgWithoutTests is from the newer snapshot.
	PkgWithoutTests int
	// HasTestData is false when the collection never parsed a test stream at
	// all, which happens in ingest mode and whenever stdout_format is unset.
	//
	// Unmeasured is not the same as zero, and conflating them is the same
	// silent wrong answer as reporting a never-collected repo at 0% coverage.
	// A repo with seventy test files rendering as "tests 0" tells the reader
	// something false with no hint that anything is missing.
	HasTestData     bool
	BaseHasTestData bool
	// HasCoverageData is false when the snapshot stored no coverage metrics at
	// all. That is what a coverage profile carrying only its "mode: set" header
	// produces: it parses clean, no package is instrumented, and nothing
	// downgrades the status, so the snapshot is stored ok. HeadCoverage is then
	// {0, 0} and Pct() returns 0, which reads as a repo measured at zero.
	//
	// Presence is the signal, not value. A repo whose packages genuinely cover
	// nothing still stored a metric per package, and calling that unmeasured
	// would be the same mistake in the other direction.
	//
	// This is deliberately separate from whether a snapshot exists at all. The
	// two answer different questions, and reusing that gate here would hide this
	// gap behind a name that does not describe it.
	HasCoverageData bool
	// BaseHasCoverageData is the same question asked of the baseline, and both
	// halves are needed before any coverage comparison means anything.
	BaseHasCoverageData bool

	// HasBaseline is false when there is no earlier snapshot to compare
	// against. There is no synthetic delta in that case: the report says so.
	HasBaseline bool
	// EnvChanged means the two snapshots were measured under different
	// toolchains, so the difference between them is not purely code.
	EnvChanged bool
	// IsMover means this repo cleared the reporting threshold.
	IsMover bool

	// Culprits are the packages that account for the repo's move, ranked by
	// absolute contribution and capped.
	Culprits []PackageDelta
	// Added and Removed are package names. They are report lines, not deltas:
	// a deleted package is not a coverage regression.
	Added   []string
	Removed []string
}

// Signal returns one signal's head measurement and its comparison against the
// baseline.
//
// The Change is derived on every call rather than stored, so it cannot go stale
// and cannot be hand-forged into a RepoDelta literal: the only way to get one is
// through Compare, which is where the both-sides-measured rule lives.
func (r RepoDelta) Signal(id SignalID) SignalDelta {
	return SignalDelta{
		Signal: SignalByID(id),
		Head:   r.HeadSignals[id],
		Change: Compare(r.HeadSignals[id], r.BaseSignals[id], r.HasBaseline),
	}
}

// SignalDeltas returns every registered signal in registry order, including the
// ones this repo did not measure. Ranging this rather than the maps is what
// keeps rendering deterministic without sorting at each call site.
func (r RepoDelta) SignalDeltas() []SignalDelta {
	out := make([]SignalDelta, 0, len(signals))
	for _, sig := range signals {
		out = append(out, r.Signal(sig.ID))
	}
	return out
}

// SignalDelta is one signal as everything downstream reads it.
//
// It deliberately does not carry the baseline measurement. Handing a caller both
// sides invites subtracting them directly, which is the same bug with two
// discarded booleans instead of one. The arithmetic that genuinely needs the
// baseline value stays inside this package.
type SignalDelta struct {
	Signal Signal
	Head   Measurement
	Change Change
}

// CoverageDetail is everything coverage carries beyond a bare value: the counts
// that are its authority, and the package findings derived from them.
type CoverageDetail struct {
	Counts   Coverage
	Change   Change
	Culprits []PackageDelta
	Added    []string
	Removed  []string
}

// CoverageDetail returns coverage's detail and whether the head measured it.
//
// This is the sanctioned read of HeadCoverage. The counts are raw and carry no
// measured flag of their own, so Pct() on a head that stored nothing answers 0
// for want of a denominator, and that zero is precisely the figure this package
// exists to withhold. Going through here puts the gate on the same expression as
// the number.
func (r RepoDelta) CoverageDetail() (CoverageDetail, bool) {
	sd := r.Signal(SigCoverage)
	if !sd.Head.IsMeasured() {
		return CoverageDetail{}, false
	}
	return CoverageDetail{
		Counts:   r.HeadCoverage,
		Change:   sd.Change,
		Culprits: r.Culprits,
		Added:    r.Added,
		Removed:  r.Removed,
	}, true
}

// CoverageChange is the repo's movement in percentage points.
//
// It reads through the generic layer rather than subtracting the counts, so the
// headline percentage has one implementation and the comparison rule has one
// implementation, and neither is coverage-specific any more.
func (r RepoDelta) CoverageChange() float64 {
	d, _ := r.Signal(SigCoverage).Change.Delta()
	return d
}

// TestChange is the repo's movement in test count.
//
// Only meaningful when TestChangeMeaningful reports true: subtracting an
// unmeasured side from a measured one manufactures the whole count as a delta.
func (r RepoDelta) TestChange() int {
	d, _ := r.Signal(SigTests).Change.Delta()
	return int(d)
}

// CoverageChangeMeaningful reports whether both snapshots actually measured
// coverage.
//
// A baseline existing is not enough. A head that stored no coverage subtracts
// its zero from a real baseline and posts the whole baseline as this week's
// drop, then blames whichever package was largest. That is the fabricated cliff
// a failed run was already fixed for, reached instead through a profile that
// parsed cleanly and simply had nothing in it.
func (r RepoDelta) CoverageChangeMeaningful() bool {
	return r.Signal(SigCoverage).Change.Meaningful()
}

// TestChangeMeaningful reports whether both snapshots actually measured tests.
// Without it, a repo that gained a stdout_format between runs would post its
// entire test suite as this week's growth.
func (r RepoDelta) TestChangeMeaningful() bool {
	return r.Signal(SigTests).Change.Meaningful()
}

// Input is one repo's two snapshots and their metrics.
type Input struct {
	Repo        store.Repo
	Head        *store.Snapshot
	HeadMetrics []store.Metric
	Base        *store.Snapshot
	BaseMetrics []store.Metric
}

// Options tunes what counts as worth reporting.
type Options struct {
	Window time.Duration
	// MinStatements keeps tiny packages out of the culprit ranking. A
	// three-statement package swinging from 0 to 100 percent is not news.
	MinStatements int
	// MinRepoDelta is how far a repo's coverage must move, in percentage
	// points, to be a mover.
	MinRepoDelta float64
	MaxCulprits  int
}

// Report is the whole computed result, shared by every output format so the
// markdown and the JSON can never disagree.
type Report struct {
	GeneratedAt time.Time
	Window      time.Duration
	Repos       []RepoDelta
}

// Movers returns only the repos worth leading with.
func (r Report) Movers() []RepoDelta {
	var out []RepoDelta
	for _, repo := range r.Repos {
		if repo.IsMover {
			out = append(out, repo)
		}
	}
	return out
}

// Problems returns repos whose most recent collection was not clean.
func (r Report) Problems() []RepoDelta {
	var out []RepoDelta
	for _, repo := range r.Repos {
		if repo.Head != nil && repo.Head.Status != store.StatusOK {
			out = append(out, repo)
		}
	}
	return out
}

// Compute builds the report. Repos come back sorted by name.
func Compute(inputs []Input, opts Options, now time.Time) Report {
	if opts.MaxCulprits <= 0 {
		opts.MaxCulprits = DefaultMaxCulprits
	}

	rep := Report{GeneratedAt: now, Window: opts.Window, Repos: make([]RepoDelta, 0, len(inputs))}
	for _, in := range inputs {
		rep.Repos = append(rep.Repos, computeRepo(in, opts))
	}
	sort.Slice(rep.Repos, func(i, j int) bool {
		return rep.Repos[i].Repo.Name < rep.Repos[j].Repo.Name
	})
	return rep
}

func computeRepo(in Input, opts Options) RepoDelta {
	d := RepoDelta{Repo: in.Repo, Head: in.Head, Base: in.Base}

	// One pass per snapshot, indexed once. Every signal's extractor is then a
	// map lookup, and the per-package coverage map the culprit ranking needs is
	// not built a second time.
	head := newSide(in.Head, in.HeadMetrics)
	d.HeadSignals = measureAll(head)
	d.BaseSignals = map[SignalID]Measurement{}

	headPkgs := head.Packages
	d.HeadCoverage = head.Coverage
	d.HeadTests = sumMetric(in.HeadMetrics, collect.KeyTestCount)
	d.PkgWithoutTests = repoLevel(in.HeadMetrics, collect.KeyPkgWithoutTest)
	d.HasTestData = hasTestData(in.HeadMetrics)
	d.HasCoverageData = hasCoverageData(in.HeadMetrics)

	if in.Base == nil {
		return d
	}
	d.HasBaseline = true

	base := newSide(in.Base, in.BaseMetrics)
	d.BaseSignals = measureAll(base)

	basePkgs := base.Packages
	d.BaseCoverage = base.Coverage
	d.BaseTests = sumMetric(in.BaseMetrics, collect.KeyTestCount)
	d.BaseHasTestData = hasTestData(in.BaseMetrics)
	d.BaseHasCoverageData = hasCoverageData(in.BaseMetrics)

	if in.Head != nil && in.Head.Env != in.Base.Env {
		d.EnvChanged = true
	}

	d.Culprits, d.Added, d.Removed = culprits(headPkgs, basePkgs, d.HeadCoverage, opts)

	// A test-count change only counts when both sides measured tests. Otherwise
	// turning on stdout_format would make every repo a mover on the strength of
	// its whole suite appearing out of nowhere.
	// Both halves need their own guard, and for a long time only the test half
	// had one. A head that measured coverage over a baseline that did not gets
	// CoverageChange = headPct - 0, clears any sane threshold on a number that
	// is entirely an artifact, and leads the report as the week's biggest mover
	// while its delta renders as null, because the gate one layer down is
	// working correctly. Selecting a mover and publishing its delta have to ask
	// the same question.
	d.IsMover = (d.CoverageChangeMeaningful() && math.Abs(d.CoverageChange()) >= opts.MinRepoDelta) ||
		(d.TestChangeMeaningful() && d.TestChange() != 0)

	return d
}

// culprits ranks packages by how much of the repo's move each one accounts for.
//
// The ranking is a counterfactual: recompute the repo's coverage with this one
// package held at its baseline values, and the gap from the real number is that
// package's contribution. It is exact and it costs one pass.
//
// Ranking by a package's own percentage change instead would be actively
// misleading. A three-statement helper going from 0 to 100 percent posts a
// hundred-point swing while moving the repo by nothing, and would outrank the
// thousand-statement package that actually caused the drop.
func culprits(head, base map[string]Coverage, headTotal Coverage, opts Options) (ranked []PackageDelta, added, removed []string) {
	names := make(map[string]bool, len(head)+len(base))
	for name := range head {
		names[name] = true
	}
	for name := range base {
		names[name] = true
	}

	headPct := headTotal.Pct()

	for name := range names {
		h, inHead := head[name]
		b, inBase := base[name]

		d := PackageDelta{Package: name, Head: h, Base: b, State: StateChanged}
		switch {
		case !inBase:
			d.State = StateAdded
			added = append(added, name)
		case !inHead:
			d.State = StateRemoved
			removed = append(removed, name)
		}

		// Swap this package back to its baseline and see what the repo would
		// have read.
		counterfactual := Coverage{
			Covered: headTotal.Covered - h.Covered + b.Covered,
			Total:   headTotal.Total - h.Total + b.Total,
		}
		d.Contribution = headPct - counterfactual.Pct()

		// The noise floor uses the larger of the two sizes so that a package
		// that was deleted, or has only just appeared, is still judged on the
		// size it actually had.
		if maxInt(h.Total, b.Total) >= opts.MinStatements && d.Contribution != 0 {
			ranked = append(ranked, d)
		}
	}

	sort.Slice(ranked, func(i, j int) bool {
		a, b := math.Abs(ranked[i].Contribution), math.Abs(ranked[j].Contribution)
		if a != b {
			return a > b
		}
		// Stable tiebreak so report output is deterministic.
		return ranked[i].Package < ranked[j].Package
	})
	if len(ranked) > opts.MaxCulprits {
		ranked = ranked[:opts.MaxCulprits]
	}

	sort.Strings(added)
	sort.Strings(removed)
	return ranked, added, removed
}

func coverageByPackage(metrics []store.Metric) map[string]Coverage {
	out := make(map[string]Coverage)
	for _, m := range metrics {
		if m.Scope == "" {
			continue
		}
		c := out[m.Scope]
		switch m.Key {
		case collect.KeyCoveredStmts:
			c.Covered = int(m.Value)
		case collect.KeyTotalStmts:
			c.Total = int(m.Value)
		default:
			continue
		}
		out[m.Scope] = c
	}
	return out
}

func sumCoverage(pkgs map[string]Coverage) Coverage {
	var total Coverage
	for _, c := range pkgs {
		total.Covered += c.Covered
		total.Total += c.Total
	}
	return total
}

func sumMetric(metrics []store.Metric, key string) int {
	var sum float64
	for _, m := range metrics {
		if m.Key == key && m.Scope != "" {
			sum += m.Value
		}
	}
	return int(sum)
}

// hasTestData reports whether a test stream was parsed for this snapshot.
//
// The marker is the repo-level pkg.without_tests metric, which the collector
// emits unconditionally whenever it parses stdout, including when the value is
// zero. Presence is the signal, so this deliberately ignores the value; summing
// test.count instead would read a genuinely empty repo as unmeasured.
func hasTestData(metrics []store.Metric) bool {
	for _, m := range metrics {
		if m.Key == collect.KeyPkgWithoutTest && m.Scope == "" {
			return true
		}
	}
	return false
}

// hasCoverageData reports whether any coverage was stored for this snapshot.
//
// It mirrors hasTestData: the marker is presence of a package-scoped coverage
// metric, and the value is deliberately ignored so that a package covering none
// of its statements still counts as measured. A header-only coverage profile
// stores no such metric, and that is the case this exists to tell apart from a
// repo genuinely sitting at zero.
func hasCoverageData(metrics []store.Metric) bool {
	for _, m := range metrics {
		if m.Scope == "" {
			continue
		}
		if m.Key == collect.KeyCoveredStmts || m.Key == collect.KeyTotalStmts {
			return true
		}
	}
	return false
}

func repoLevel(metrics []store.Metric, key string) int {
	for _, m := range metrics {
		if m.Key == key && m.Scope == "" {
			return int(m.Value)
		}
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
