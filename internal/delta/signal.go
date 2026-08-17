package delta

import (
	"fmt"
	"strings"

	"github.com/Romero-jace/repo-metrics/internal/collect"
)

// Measurement is a value and whether anything measured it.
//
// The fields are unexported and the only read goes through Value, which hands
// back the question along with the number. Nothing can take the figure while
// forgetting to ask. The zero value is unmeasured, so a missing map entry, a
// hand-built RepoDelta and a half-filled fixture all fail closed rather than
// reporting a confident zero.
//
// This exists because one bug has been found and fixed in this project eight
// times: something unmeasured published as a measurement of zero. Every one of
// those was a place where a float64 sat next to a boolean and somebody read the
// float64.
type Measurement struct {
	value    float64
	measured bool
	// scopeKey fingerprints WHICH rows this value was summed over, for the
	// signals where that can change underneath the number.
	//
	// It is empty for every signal whose value does not depend on a set of
	// contributing scopes, and empty on a hand-built Measurement, so the
	// comparability rule below is permissive by default and only bites where a
	// signal asked for it.
	scopeKey string
	// partial marks a value that is real but is a floor rather than a total,
	// because something the collector could not measure belongs inside it.
	//
	// It is a different question from scopeKey. That one asks whether the two
	// sides covered the same things; this one asks whether either side covered
	// everything it claims to. A package whose test binary would not compile
	// contributes no test count, so the repo's total is short by however many
	// tests that package has, which is a number nobody knows.
	partial bool
}

// Measured is a real measurement, including a real zero. A repo whose tests all
// passed measured zero failures, and that is a finding rather than an absence.
func Measured(v float64) Measurement { return Measurement{value: v, measured: true} }

// measuredOver is a real measurement summed across a known set of scopes. Two of
// these are comparable only when the sets match.
func measuredOver(v float64, scopeKey string) Measurement {
	return Measurement{value: v, measured: true, scopeKey: scopeKey}
}

// Unmeasured is the absence of a measurement. It is the zero value, spelled out
// for the places that mean it deliberately.
func Unmeasured() Measurement { return Measurement{} }

// partially marks a measurement as a floor rather than a total. The value still
// stands and is still published: what it cannot do is be subtracted from
// another week's.
func (m Measurement) partially(incomplete bool) Measurement {
	m.partial = incomplete
	return m
}

// Value returns the measurement and whether there is one. The comma-ok is the
// whole point: a caller has to write the second variable to get the first.
func (m Measurement) Value() (float64, bool) { return m.value, m.measured }

// IsMeasured reports whether anything measured this.
func (m Measurement) IsMeasured() bool { return m.measured }

// Change is a comparison between two Measurements.
//
// It has no exported fields and exactly one constructor, so no code in this
// module can produce a Change that skipped the both-sides-measured question.
// The rule that has been re-fixed eight times is written in Compare, once, and
// cannot be routed around, not even by a test building a RepoDelta literal.
type Change struct {
	delta      float64
	meaningful bool
}

// Compare is the one place the rule lives.
//
// A baseline existing is not enough. A head that stored nothing subtracts its
// zero from a real baseline and posts the entire baseline as this week's drop.
// A baseline that stored nothing posts the entire head as this week's growth,
// which is what turning on a stdout format used to do to a repo's test count.
// Both halves get the same guard here, once, for every signal there will ever
// be.
//
// A third condition joins the two: both sides have to have summed over the same
// set of scopes. For a signal built by summing per-step rows, a step that died
// this week does not make the repo better, it makes the sum cover less. Left
// unguarded, a crashed linter posts its own findings as this week's improvement
// and leads the report, and turning a second linter ON posts its entire backlog
// as a regression. Both were real: found by review, reproduced, and now pinned.
//
// A fourth asks whether either side is a floor rather than a total. A package
// whose test binary would not compile contributes no test count, so the repo's
// total is short by an unknown amount, and subtracting last week's real total
// from this week's short one reports a drop that is really a build error. That
// one shipped too, and it was the worse kind: the same broken build also made
// the package look newly untested, which is a number nobody measured being
// published as a finding.
func Compare(head, base Measurement, hasBaseline bool) Change {
	if !hasBaseline || !head.measured || !base.measured {
		return Change{}
	}
	if head.scopeKey != base.scopeKey {
		return Change{}
	}
	if head.partial || base.partial {
		return Change{}
	}
	return Change{delta: head.value - base.value, meaningful: true}
}

// Delta returns the change and whether it means anything. Read it nowhere else.
func (c Change) Delta() (float64, bool) { return c.delta, c.meaningful }

// Meaningful reports whether both sides measured, so a caller that only needs
// the question does not have to discard the number to ask it.
func (c Change) Meaningful() bool { return c.meaningful }

// SignalID names one measurable thing. The set is closed and compile-time,
// which is what lets the report's field census pin a literal path per signal.
type SignalID string

// The signals this tool publishes.
const (
	SigCoverage         SignalID = "coverage"
	SigCoverageLines    SignalID = "coverage_lines"
	SigTests            SignalID = "tests"
	SigTestFailures     SignalID = "test_failures"
	SigTestSkipped      SignalID = "test_skipped"
	SigUntestedPackages SignalID = "untested_packages"
	SigTestTime         SignalID = "test_time"
	SigLintFindings     SignalID = "lint_findings"
	SigLintErrors       SignalID = "lint_errors"
	SigLintSuppressed   SignalID = "lint_suppressed"
	SigDependencies     SignalID = "dependencies"
	SigOutdatedDeps     SignalID = "outdated_dependencies"
	SigDependencyAge    SignalID = "dependency_age"
	SigCollectTime      SignalID = "collect_time"
)

// Unit is how a signal's numbers should be read and rendered.
type Unit int8

// The units in play.
const (
	// UnitPercent is a rate out of a hundred, rendered with a percent sign and
	// compared in points.
	UnitPercent Unit = iota
	// UnitCount is a whole number of things.
	UnitCount
	// UnitMilliseconds is a duration, stored in milliseconds because the
	// metrics table holds one float per row and a duration has to be a number.
	UnitMilliseconds
	// UnitDays is a longer duration, kept separate from milliseconds because the
	// two want opposite renderings: a test suite reads as 3m34s, and a dependency
	// pinned at a version from 2023 reads as 412 days rather than as 35 billion
	// milliseconds.
	UnitDays
)

// units is every unit, in declaration order. It exists so a test can range them:
// each of the rendering switches has a default branch, so a new unit that nobody
// added a case for renders silently as a bare count, with its meaning stripped
// and nothing failing.
var units = []Unit{UnitPercent, UnitCount, UnitMilliseconds, UnitDays}

// Units returns every declared unit, so the renderers can be checked for
// covering all of them rather than trusted to.
func Units() []Unit {
	out := make([]Unit, len(units))
	copy(out, units)
	return out
}

// Direction says which way is good news.
//
// There is deliberately no neutral option. A signal whose direction nobody will
// commit to renders as "up" rather than "worse", and a reader then has to know
// on their own that rising test failures are bad. An escape hatch here is how
// every future signal ends up neutral.
type Direction int8

// The two directions.
const (
	// HigherIsBetter: coverage, test count.
	HigherIsBetter Direction = iota
	// LowerIsBetter: failures, untested packages, durations, lint findings.
	LowerIsBetter
)

// MarkerScope says whether a signal's presence marker is a repo-level row or one
// of possibly several rows carrying a scope.
type MarkerScope int8

// The two scopes a metric row can carry.
const (
	// ScopeRepo is the empty scope column: one row for the whole repo.
	ScopeRepo MarkerScope = iota
	// ScopeDetail is a row carrying a scope value, so presence means at least
	// one exists.
	//
	// What the scope names depends on the signal, which is why this is no longer
	// spelled ScopePackage: coverage breaks down by package, lint by the
	// collection step that produced it, since a polyglot repo can run two linters
	// and their findings have to sum rather than collide.
	ScopeDetail
)

// Marker is a metric key paired with the scope it is written at. Its presence in
// a snapshot is what proves something looked.
type Marker struct {
	Key   string
	Scope MarkerScope
}

// Signal declares one metric the report publishes.
//
// Marker is a contract on collectors rather than an observation about one: a
// collector that looked at this signal must emit this key even when the answer
// is zero. That contract is the entire basis for telling a measured zero from an
// absence, and it is the one thing in this struct that a registry entry cannot
// check for itself. It is written down in CONTRIBUTING.md for that reason.
type Signal struct {
	ID    SignalID
	Label string
	Unit  Unit
	// Direction is which way is good news, used to say better or worse rather
	// than up or down.
	Direction Direction

	// Markers are the metric keys whose presence proves something looked. They
	// are read by presence and never by value, so a package covering none of its
	// statements still counts as measured.
	//
	// A list, with any-of semantics, because one measurable thing can have more
	// than one proof. A test count is the case that forced it: `go test -json`
	// can say how many source packages carry no tests at all, and JUnit XML
	// cannot, since a JUnit file lists the suites that ran and knows nothing
	// about files that contain none. Requiring the Go-only key would have left
	// every Python and TypeScript repo reporting null tests; swapping in a
	// neutral key instead would have blanked the stored history of every Go repo,
	// which no migration can recover because the neutral key is step scoped and
	// the metrics table has no step column. Accepting either is the only option
	// that fabricates nothing and loses nothing.
	//
	// Order matters where ScopeSetMustMatch is set. The first marker a side
	// carries is the one whose rows fingerprint the scope set, so the newer and
	// more precise key belongs first. Two sides that match on DIFFERENT markers
	// then produce different fingerprints and Compare refuses the delta, which is
	// the right answer: a total measured by one toolchain and a total measured by
	// another did not cover the same ground.
	Markers []Marker

	// Extract reads the value. Presence is not its job: the marker answered
	// that before Extract is called, so an extractor summing to zero is a
	// measured zero and nothing else.
	Extract func(Side) float64

	// Nominates says a move in this signal can make a repo lead the report.
	// It is opt-in rather than automatic, because a signal that changes on
	// every run would otherwise make every repo a mover every week and drown
	// the ones that matter.
	Nominates bool
	// MinMove is the absolute floor a move has to clear, in this signal's own
	// unit.
	MinMove float64
	// MinMoveFraction is an additional relative floor, for quantities that are
	// noisy in proportion to their size. A test suite has to get meaningfully
	// slower, not slower by a fixed number of milliseconds.
	MinMoveFraction float64

	// Table says this signal earns a column in the every-repo table. It is a
	// layout decision and nothing else: a signal left out of the table is still
	// in the JSON, still in the movers write-up, and still guarded.
	Table bool

	// ScopeSetMustMatch says this signal's value is a sum whose meaning depends
	// on WHICH rows it summed, so two of them are comparable only when both sides
	// summed the same set.
	//
	// The distinction it draws is what the scope column NAMES. For coverage it
	// names a package, part of the thing being measured, so a package appearing
	// or vanishing is a real change in the repo and the report has designed
	// answers for it: culprit ranking, added and removed lists. Requiring
	// identical package sets there would refuse a delta every week anybody added
	// a file.
	//
	// For lint findings and collection time it names the STEP that produced the
	// row, which is the measuring apparatus rather than the subject. A step that
	// crashed did not make the repo better, and a step newly switched on did not
	// make it worse. Both of those shipped and both are reproduced in
	// TestADroppedStepIsNotAnImprovement.
	//
	// The rule of thumb: set this when the scope names how the measurement was
	// taken, and leave it off when the scope names what was measured.
	ScopeSetMustMatch bool

	// PartialWhen names a repo-scoped metric key whose non-zero value means this
	// signal's number is a floor rather than a total, so it may be published but
	// not compared.
	//
	// The neighboring rule asks whether the two sides covered the same things.
	// This one asks whether either side covered everything it claims to, which a
	// scope fingerprint cannot see: the missing contribution leaves no row to be
	// missing from the set. A package whose test binary would not compile is
	// exactly that shape. It is still a package, it still has however many tests
	// it has, and the stream carries no count for any of them.
	//
	// Absent rows read as zero, so a snapshot written before the key existed
	// compares normally. That is the right answer: nothing was checking then, and
	// refusing every comparison against older history would be a worse lie than
	// the one this prevents.
	PartialWhen string
}

// testMarkers is the proof-of-measurement shared by every signal that comes out
// of a parsed test result, in the order the presence check tries them.
//
// One slice rather than four copies, because the ordering is load bearing and
// four literals would let one of them drift. The neutral key first: a snapshot
// carrying both matches on it, which is what keeps a Go repo's fingerprint
// scoped by step rather than by the repo-level legacy row.
//
// One transitional cost worth knowing about. The first collection after this
// shipped carries test.suites and the one before it does not, so for those two
// snapshots the two sides match on different markers, fingerprint differently,
// and that week's test deltas are refused. It resolves itself on the next
// collection, and refusing one comparison is the cheaper error: the alternative
// was to compare a repo-scoped sum against a step-scoped one and call the
// difference news.
var testMarkers = []Marker{
	{collect.KeyTestSuites, ScopeDetail},
	{collect.KeyPkgWithoutTest, ScopeRepo},
}

// signals is the one ordered list. Everything downstream ranges this rather
// than a map, which is what keeps rendering deterministic without sorting at
// every call site.
//
// Order is the order a reader should meet them in: the headline first, then the
// test signals from most to least consequential.
var signals = []Signal{
	{
		ID: SigCoverage,
		// Named for its unit rather than just "Coverage", now that a second
		// coverage signal exists in a different one. Go's profile counts
		// statements and LCOV counts lines; several statements on one source line
		// collapse to one line, so the two rates are not comparable and a column
		// header that did not say which is which would invite exactly that
		// comparison.
		Label:     "Statement coverage",
		Unit:      UnitPercent,
		Direction: HigherIsBetter,
		// The total-statements key rather than the covered one. A collector
		// emits the pair together, so either would do today, and requiring the
		// denominator is the tighter contract: a profile with no instrumented
		// packages stores neither, which is the header-only case this marker
		// exists to catch.
		Markers:   []Marker{{collect.KeyTotalStmts, ScopeDetail}},
		Extract:   func(s Side) float64 { return s.Coverage.Pct() },
		Nominates: true,
		// Filled from Options.MinRepoDelta at compute time, so the existing
		// config field keeps working and no config file changes.
		MinMove: 0,
		Table:   true,
	},
	{
		ID: SigCoverageLines,
		// Named for its unit, and the statement signal above was relabeled to
		// match. A reader seeing two coverage columns has to be able to tell what
		// each denominator counts, because they are not the same number and one
		// repo can legitimately report only one of them.
		Label:     "Line coverage",
		Unit:      UnitPercent,
		Direction: HigherIsBetter,
		// The denominator key, on the same reasoning as the statement marker: a
		// tracefile that instruments no files stores neither key, which is the
		// zero-byte lcov.info case a real vitest run produced.
		Markers:   []Marker{{collect.KeyTotalLines, ScopeDetail}},
		Extract:   ratioOver(collect.KeyCoveredLines, collect.KeyTotalLines),
		Nominates: true,
		// Filled from Options.MinRepoDelta at compute time, like the statement
		// signal, so one config field still governs both.
		MinMove: 0,
		Table:   true,
	},
	{
		ID:        SigTests,
		Label:     "Tests",
		Unit:      UnitCount,
		Direction: HigherIsBetter,
		// Four signals share this pair because they come from one parsed stream:
		// either the collector read the test output or it did not, and there is no
		// state where it counted the passes but not the failures. They null and
		// fill together, and the census's null-and-filled ledger showing them flip
		// at once is that fact, not a broken check.
		//
		// Two markers, newest first. test.suites is what any test parser can
		// write; pkg.without_tests is what only a Go stream can, and it is still
		// listed so that snapshots taken before test.suites existed keep reading
		// as measured. Dropping it would blank the whole stored history of every
		// repo already being collected.
		Markers: testMarkers,
		Extract: sumOver(collect.KeyTestCount),
		// A package that would not build contributes no count, so this total is
		// short by however many tests it has, which nothing knows.
		PartialWhen: collect.KeyTestBuildFailed,
		// The marker's scope names the step that produced the count, so a repo
		// that switched a second suite on did not gain those tests this week: its
		// sum simply covers more. Same reasoning as the lint signals below, and
		// the same refusal.
		ScopeSetMustMatch: true,
		Nominates:         true,
		MinMove:           1,
		Table:             true,
	},
	{
		ID:                SigTestFailures,
		Label:             "Failing tests",
		Unit:              UnitCount,
		Direction:         LowerIsBetter,
		Markers:           testMarkers,
		Extract:           sumOver(collect.KeyTestFailed),
		PartialWhen:       collect.KeyTestBuildFailed,
		ScopeSetMustMatch: true,
		Nominates:         true,
		MinMove:           1,
		Table:             false,
	},
	{
		ID:        SigUntestedPackages,
		Label:     "Packages without tests",
		Unit:      UnitCount,
		Direction: LowerIsBetter,
		// The one test signal that keeps the Go-only marker and only that one,
		// because it is the one measurement no other toolchain's output can
		// supply. `go test -json` reports a package with no test files; a JUnit
		// document lists the suites that ran and cannot know what did not, and
		// neither pytest nor vitest enumerates source files carrying no tests.
		//
		// So a Python or TypeScript repo reports this as unmeasured rather than as
		// zero, which is the true answer: nobody counted. Adding test.suites here
		// to make the row fill would be publishing a count of nothing as a count
		// of none.
		Markers: []Marker{{collect.KeyPkgWithoutTest, ScopeRepo}},
		Extract: repoValue(collect.KeyPkgWithoutTest),
		// A package that would not build is deliberately not counted here, and
		// nobody can say whether it belongs: it may be full of tests or have
		// none. So this is a floor too, for the opposite reason to the others.
		PartialWhen: collect.KeyTestBuildFailed,
		Nominates:   true,
		MinMove:     1,
		Table:       true,
	},
	{
		ID:                SigTestSkipped,
		Label:             "Skipped tests",
		Unit:              UnitCount,
		Direction:         LowerIsBetter,
		Markers:           testMarkers,
		Extract:           sumOver(collect.KeyTestSkipped),
		PartialWhen:       collect.KeyTestBuildFailed,
		ScopeSetMustMatch: true,
		// Skips move for reasons that are rarely news on their own, so this is
		// reported and never leads.
		Nominates: false,
		Table:     false,
	},
	{
		ID:                SigTestTime,
		Label:             "Total test time",
		Unit:              UnitMilliseconds,
		Direction:         LowerIsBetter,
		Markers:           testMarkers,
		Extract:           sumOver(collect.KeyTestDurationMS),
		PartialWhen:       collect.KeyTestBuildFailed,
		ScopeSetMustMatch: true,
		// The sum of per-package durations, which is not wall clock: go test
		// runs packages in parallel, so this is machine work rather than time
		// anyone waited. It also swings with load on the machine that ran it,
		// which is why it never nominates and why its floor is relative.
		Nominates:       false,
		MinMoveFraction: 0.25,
		Table:           false,
	},
	{
		ID:        SigLintFindings,
		Label:     "Lint findings",
		Unit:      UnitCount,
		Direction: LowerIsBetter,
		// The three lint signals share this marker and this scope, for the same
		// reason the test signals share theirs: they come out of one parsed log
		// together. The scope holds the collection step's name rather than a
		// package, so a repo running two linters sums them.
		//
		// Suppressed findings are deliberately NOT in this total. Counting them
		// would make a repo look worse for having triaged its findings, which is
		// the opposite of the incentive this tool should create.
		Markers:           []Marker{{collect.KeyLintFindings, ScopeDetail}},
		Extract:           sumOver(collect.KeyLintFindings),
		ScopeSetMustMatch: true,
		Nominates:         true,
		MinMove:           1,
		Table:             true,
	},
	{
		ID:                SigLintErrors,
		Label:             "Lint errors",
		Unit:              UnitCount,
		Direction:         LowerIsBetter,
		Markers:           []Marker{{collect.KeyLintFindings, ScopeDetail}},
		Extract:           sumOver(collect.KeyLintErrors),
		ScopeSetMustMatch: true,
		// Nominates on its own rather than leaving it to the total, because a
		// week that trades ten nits for one error-level finding moves the total
		// down and is not good news.
		Nominates: true,
		MinMove:   1,
		Table:     false,
	},
	{
		ID:                SigLintSuppressed,
		Label:             "Suppressed findings",
		Unit:              UnitCount,
		Direction:         LowerIsBetter,
		Markers:           []Marker{{collect.KeyLintFindings, ScopeDetail}},
		Extract:           sumOver(collect.KeyLintSuppressed),
		ScopeSetMustMatch: true,
		// Reported and never leading. A rising suppression count is worth
		// knowing about, but it moves for legitimate reasons often enough that
		// leading the report on it would cost the reader's attention.
		Nominates: false,
		Table:     false,
	},
	{
		ID:        SigOutdatedDeps,
		Label:     "Outdated dependencies",
		Unit:      UnitCount,
		Direction: LowerIsBetter,
		// Its own marker rather than one shared with the other two dependency
		// signals, and that is the whole point: the module count is measurable
		// offline while this one needs the proxy to have been consulted. A shared
		// marker would claim this was measured whenever the count was.
		Markers:   []Marker{{collect.KeyDepsOutdatedDirect, ScopeRepo}},
		Extract:   repoValue(collect.KeyDepsOutdatedDirect),
		Nominates: true,
		MinMove:   1,
		Table:     true,
	},
	{
		ID:        SigDependencies,
		Label:     "Dependencies",
		Unit:      UnitCount,
		Direction: LowerIsBetter,
		Markers:   []Marker{{collect.KeyDepsTotal, ScopeRepo}},
		Extract:   repoValue(collect.KeyDepsTotal),
		// Dependency count moves for ordinary reasons and a repo that added one
		// library has not had a bad week. It is context for the two signals
		// around it rather than news of its own.
		Nominates: false,
		Table:     false,
	},
	{
		ID:        SigDependencyAge,
		Label:     "Median dependency age",
		Unit:      UnitDays,
		Direction: LowerIsBetter,
		Markers:   []Marker{{collect.KeyDepsAgeMedianDays, ScopeRepo}},
		Extract:   repoValue(collect.KeyDepsAgeMedianDays),
		// This one drifts upward by a day every day just by nobody touching the
		// repo, so an absolute floor would make every repo a mover every week.
		// The relative floor asks whether the drift outran the calendar.
		Nominates:       false,
		MinMoveFraction: 0.25,
		Table:           false,
	},
	{
		ID:        SigCollectTime,
		Label:     "Collection time",
		Unit:      UnitMilliseconds,
		Direction: LowerIsBetter,
		// Wall clock across every step that ran a command, which is the build and
		// test timing the brief asks for: whatever a repo is configured to run,
		// this is how long running it took.
		//
		// Distinct from test_time, which sums the per-package durations `go test`
		// reports. That is machine work and counts parallel packages more than
		// once; this is time somebody waited. Both are worth having and neither
		// is derivable from the other.
		Markers:           []Marker{{collect.KeySignalDurationMS, ScopeDetail}},
		Extract:           sumOver(collect.KeySignalDurationMS),
		ScopeSetMustMatch: true,
		// Never leads. This is the signal most sensitive to what else the machine
		// was doing, and a report that led with it every time a laptop was busy
		// would train its reader to skip the movers section.
		Nominates:       false,
		MinMoveFraction: 0.25,
		Table:           false,
	},
}

// Signals returns every registered signal in the order a report should show
// them. It hands back a copy so a caller cannot reorder the registry for
// everyone else.
func Signals() []Signal {
	out := make([]Signal, len(signals))
	copy(out, signals)
	return out
}

// ParseSignal turns a command-line value into a registered signal.
//
// An unknown name is an error naming the valid ones, never a silent fallback to
// coverage: a caller that asked to chart lint findings and got coverage instead
// has an answer to a question nobody asked, with nothing saying so.
func ParseSignal(s string) (Signal, error) {
	for _, sig := range signals {
		if SignalID(s) == sig.ID {
			return sig, nil
		}
	}
	return Signal{}, fmt.Errorf("delta: unknown signal %q, valid signals are %s", s, strings.Join(SignalNames(), ", "))
}

// SignalNames lists every registered signal, in registry order, so help text
// cannot drift from what ParseSignal accepts.
func SignalNames() []string {
	out := make([]string, 0, len(signals))
	for _, sig := range signals {
		out = append(out, string(sig.ID))
	}
	return out
}

// SignalByID returns one signal's declaration. An unknown id returns the zero
// Signal, whose Extract is nil, which panics loudly at the one call site that
// would use it rather than quietly measuring nothing.
func SignalByID(id SignalID) Signal {
	for _, s := range signals {
		if s.ID == id {
			return s
		}
	}
	return Signal{}
}
