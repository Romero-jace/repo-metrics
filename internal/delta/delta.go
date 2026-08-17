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

	// HeadCoverage and BaseCoverage are coverage's raw statement counts. They
	// carry no measured flag of their own: Pct() on a head that stored nothing
	// answers 0 for want of a denominator, and that zero is exactly the figure
	// this package exists to withhold.
	//
	// The gate that protects them is the coverage signal's own,
	// Signal(SigCoverage).Head.IsMeasured(), and nothing may read these counts
	// until it has answered yes. report.buildRepo is the only real reader and
	// does exactly that: it publishes Covered and Total only after buildSignal
	// returned a non-nil group for coverage, which is that same question.
	// TestUnmeasuredCoverageCountsReadAsAFabricatedZero pins the trap itself, so
	// the warning survives a rewording of this comment.
	//
	// They stay as counts because repo coverage is the sum of covered over the
	// sum of total, which is not the mean of the per-package rates, and because
	// the culprit ranking needs the per-package map anyway.
	HeadCoverage Coverage
	BaseCoverage Coverage

	// HasBaseline is false when there is no earlier snapshot to compare
	// against. There is no synthetic delta in that case: the report says so.
	HasBaseline bool
	// BaselineAge is how far apart the two snapshots actually are.
	//
	// It is not the window that was asked for and is never shorter than it. The
	// baseline is the newest snapshot at or before the cutoff, with no floor on
	// how far before, so a repo nobody collected for two months compares against
	// a two-month-old snapshot and every number is a two-month change. The
	// report used to call all of them "on the week" regardless.
	BaselineAge time.Duration
	// BaselineStale means that gap is far enough past the window asked for that
	// this repo should not lead the report on the strength of it. The delta is
	// still published and still labeled with its real span: what it loses is the
	// right to be the headline.
	BaselineStale bool
	// EnvChanged means the two snapshots were measured under different
	// toolchains, so the difference between them is not purely code.
	EnvChanged bool
	// EnvUnknown means at least one of the two snapshots never established what
	// toolchain it was measured under, so nothing here can say whether it changed.
	//
	// It is deliberately separate from EnvChanged rather than folded into it. The
	// two call for different words and, more to the point, comparing the
	// fingerprints would answer the wrong question: two snapshots that both failed
	// to identify a toolchain carry the same placeholder and so read as unchanged,
	// which is an unmeasured fact published as a measurement. EnvChanged is
	// therefore left false here, because "we know they match" is not what happened.
	EnvUnknown bool
	// IsMover means this repo cleared the reporting threshold on at least one
	// signal that is allowed to nominate.
	IsMover bool
	// MovedBy names the signals that qualified it, in registry order. With one
	// signal "this repo moved" was a complete answer; with several it is not,
	// and a report that says a repo moved without saying which measurement moved
	// makes the reader open the table to find out.
	MovedBy []SignalID
	// Severity ranks movers against each other: the largest qualifying move
	// measured in multiples of its own floor. That is what makes a four point
	// coverage drop and three new test failures comparable at all, since they
	// are not in the same unit and never will be.
	Severity float64

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

	if in.Base == nil {
		return d
	}
	d.HasBaseline = true

	if in.Head != nil {
		d.BaselineAge = in.Head.CollectedAt.Sub(in.Base.CollectedAt)
		// Three times the window asked for. There is no natural boundary to read
		// off the code here, because baseline selection guarantees the gap is at
		// least the window and says nothing about the upper end, so this is a
		// judgment call written down rather than derived. Retune it here.
		//
		// Twice was tried first and is too tight. A weekly report whose cron
		// missed one run has a fortnight-old baseline, which is both common and
		// harmless, and it landed exactly on the boundary and tipped over it by
		// milliseconds. Three times means at least two collections in a row went
		// missing, which is a genuinely lapsed repo rather than a hiccup.
		//
		// It can afford to be generous because the label is honest now: the
		// delta says the span it actually covers either way. All this decides is
		// whether a repo gets to be the headline, and the case it exists for is
		// a quarter of accumulated drift outranking the repos that really moved
		// this week.
		//
		// What it must not become is a rule that refuses the comparison. A
		// two-month-old baseline is still the best answer available, and saying
		// so plainly beats saying nothing.
		if opts.Window > 0 && d.BaselineAge > 3*opts.Window {
			d.BaselineStale = true
		}
	}

	base := newSide(in.Base, in.BaseMetrics)
	d.BaseSignals = measureAll(base)

	basePkgs := base.Packages
	d.BaseCoverage = base.Coverage

	// Three states, not two. Either side being unidentified means the comparison
	// cannot be made at all, and it is checked first: a repo that names no
	// toolchain records the same placeholder every run, so the equality test below
	// would quietly answer "unchanged" for exactly the repos nothing is watching.
	if in.Head != nil {
		switch {
		case collect.EnvIsUnidentified(in.Head.Env) || collect.EnvIsUnidentified(in.Base.Env):
			d.EnvUnknown = true
		case in.Head.Env != in.Base.Env:
			d.EnvChanged = true
		}
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
	d.MovedBy, d.Severity = nominate(d, base, opts)
	d.IsMover = len(d.MovedBy) > 0

	return d
}

// nominate answers which signals, if any, make this repo worth leading with.
//
// Selection and publication ask the same question, because both go through
// Compare. That property is the one this function exists to preserve across
// seven signals: for a long time only the test half of the old IsMover had a
// guard, so a head that measured coverage over a baseline that did not would
// clear any threshold on headPct minus zero, lead the report as the week's
// biggest mover, and render its delta as null because the gate one layer down
// was working correctly.
func nominate(d RepoDelta, base Side, opts Options) (movedBy []SignalID, severity float64) {
	// A repo whose baseline is far older than the window asked for does not lead
	// the report. Its numbers are real and its deltas are published with the span
	// they actually cover; what it must not do is present a quarter's drift as
	// this week's news and push aside the repos that really did move this week.
	//
	// This is the same outcome the failed-head guard already refuses further
	// down, arriving through a different door: there the biggest drop of the week
	// is really a crashed command, here it is really two months of ordinary
	// change nobody was watching.
	if d.BaselineStale {
		return nil, 0
	}

	for _, sig := range signals {
		// Nomination is opt-in per signal rather than a property of having a
		// delta. Without that, a signal that moves on every run, like a test
		// suite's wall time on a shared machine, makes every repo a mover every
		// week and buries the ones that matter.
		if !sig.Nominates {
			continue
		}

		change, meaningful := d.Signal(sig.ID).Change.Delta()
		if !meaningful || change == 0 {
			continue
		}

		floor := sig.MinMove
		if sig.FloorFromMinRepoDelta {
			// The registry says which signals these are, rather than this line
			// naming them. Naming them here is what made line coverage ten times
			// harder to nominate than statement coverage: the test read
			// `sig.ID == SigCoverage`, and a second percent signal added later
			// could not be seen from the config field it was documented to use.
			floor = opts.MinRepoDelta
		}
		if floor <= 0 {
			floor = 1
		}
		if math.Abs(change) < floor {
			continue
		}

		// A relative floor on top, for quantities that are noisy in proportion
		// to their size. A suite has to get meaningfully slower, not slower by
		// some fixed number of milliseconds that means one thing for a
		// three-second suite and another for a ten-minute one.
		if sig.MinMoveFraction > 0 {
			if baseValue, ok := base.measure(sig).Value(); ok {
				if math.Abs(change) < sig.MinMoveFraction*math.Abs(baseValue) {
					continue
				}
			}
		}

		movedBy = append(movedBy, sig.ID)
		if s := math.Abs(change) / floor; s > severity {
			severity = s
		}
	}
	return movedBy, severity
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
