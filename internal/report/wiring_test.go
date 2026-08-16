package report_test

import (
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// repoWired is a repo whose collection looked at every signal in the registry.
// Named so it shares no substring with the template's own prose, for the same
// reason the other fixture repo names do.
const repoWired = "wiredrepo"

// signalSentinel is the value every filled group carries below.
//
// Any non-zero number would do. The assertion is that a digit reaches the
// rendered cell at all, not which digit: formatValue renders four different
// units, so 42 arrives as "42", "42.0%", "42ms" or "42 days" depending on the
// signal, and pinning the text per signal would be pinning the formatter rather
// than the wiring.
const signalSentinel = 42

// signalGroup is a filled measurement group, for every signal but coverage.
// Coverage carries counts and package findings the others do not, so it has its
// own type and fills itself below.
func signalGroup() *report.SignalView { return &report.SignalView{Value: signalSentinel} }

// signalFills says how to fill each signal's field on a RepoView.
//
// Hand-written, and that is the point rather than an inconvenience: a signal
// added to the registry has no row here until a person adds one, which is the
// step-4 half of CONTRIBUTING's checklist (a field on RepoView and a case in
// group()) being asked for out loud.
//
// It cannot be derived from the struct, because the join it guards is exactly
// the thing that would be doing the deriving.
var signalFills = []struct {
	id delta.SignalID
	// fill sets this signal's field to a non-nil sentinel. Filling EVERY field
	// is what stops this test being vacuous: group() ends in default: return
	// nil, so on a zero-value RepoView every signal answers nil whether or not
	// its case exists, and an assertion over that would pass forever while
	// guarding nothing.
	fill func(*report.RepoView)
}{
	{delta.SigCoverage, func(r *report.RepoView) {
		r.Coverage = &report.CoverageView{SignalView: report.SignalView{Value: signalSentinel}}
	}},
	{delta.SigTests, func(r *report.RepoView) { r.Tests = signalGroup() }},
	{delta.SigTestFailures, func(r *report.RepoView) { r.TestFailures = signalGroup() }},
	{delta.SigTestSkipped, func(r *report.RepoView) { r.TestSkipped = signalGroup() }},
	{delta.SigUntestedPackages, func(r *report.RepoView) { r.UntestedPackages = signalGroup() }},
	{delta.SigTestTime, func(r *report.RepoView) { r.TestTime = signalGroup() }},
	{delta.SigLintFindings, func(r *report.RepoView) { r.LintFindings = signalGroup() }},
	{delta.SigLintErrors, func(r *report.RepoView) { r.LintErrors = signalGroup() }},
	{delta.SigLintSuppressed, func(r *report.RepoView) { r.LintSuppressed = signalGroup() }},
	{delta.SigDependencies, func(r *report.RepoView) { r.Dependencies = signalGroup() }},
	{delta.SigOutdatedDeps, func(r *report.RepoView) { r.OutdatedDependencies = signalGroup() }},
	{delta.SigDependencyAge, func(r *report.RepoView) { r.DependencyAge = signalGroup() }},
	{delta.SigCollectTime, func(r *report.RepoView) { r.CollectTime = signalGroup() }},
}

// TestEverySignalReachesTheWire pins the registry against RepoView's fields.
//
// group() is a switch from a signal id to a struct field, and it ends in
// default: return nil. So a signal registered without a field and a case there
// does not fail anything: it renders as "not measured" on every repo forever,
// in the movers write-up and in the every-repo table, and the report says a
// collection that measured it never looked. That is this project's recurring bug
// pointed the other way, and it is the one step of CONTRIBUTING's seven that had
// no guard behind it.
//
// The measured-versus-unmeasured predicate is "does the cell carry a digit",
// which is the same rule the degraded-state table uses. It covers "not
// measured", "not collected" and any wording nobody has invented yet, and it
// does not have to be updated when one of those is reworded.
func TestEverySignalReachesTheWire(t *testing.T) {
	// A snapshot that succeeded, so a signal with no case renders "not measured"
	// rather than "not collected". Both fail the assertion; the first one says
	// what actually went wrong.
	filled := report.RepoView{HasSnapshot: true, Status: string(store.StatusOK)}

	filledIDs := make(map[delta.SignalID]bool, len(signalFills))
	for _, f := range signalFills {
		if filledIDs[f.id] {
			t.Fatalf("fixture is wrong: signalFills has two rows for %q, so one of them is filling a field nothing checks", f.id)
		}
		filledIDs[f.id] = true
		f.fill(&filled)
	}

	registered := make(map[delta.SignalID]bool, len(delta.Signals()))
	for _, sig := range delta.Signals() {
		registered[sig.ID] = true
		if !filledIDs[sig.ID] {
			t.Errorf("%q is in the registry but signalFills has no row for it, so nothing here fills its group and the assertion below would pass for it whether or not RepoView has a field. Add a field to RepoView, a case to group(), a line to buildRepo, and a row here.",
				sig.ID)
		}
	}
	// And the reverse, so a signal dropped from the registry does not leave a
	// row that quietly fills a field nothing reads. Same shape as
	// TestEveryTableSignalIsPaired, one join over.
	for _, f := range signalFills {
		if !registered[f.id] {
			t.Errorf("signalFills fills %q, which is no longer a registered signal, so the row is rot", f.id)
		}
	}

	cells := make(map[string]report.SignalCell, len(delta.Signals()))
	for _, cell := range filled.Signals() {
		cells[cell.ID] = cell
	}
	for _, sig := range delta.Signals() {
		cell, ok := cells[string(sig.ID)]
		if !ok {
			t.Errorf("%q is in the registry but RepoView.Signals() renders no cell for it at all", sig.ID)
			continue
		}
		if !hasDigit(cell.Value) {
			t.Errorf("%q has its group filled on this RepoView, but its cell reads %q. group() has no case for it, so every repo that ever measures it will be reported as never having looked.",
				sig.ID, cell.Value)
		}
	}

	// The anti-vacuity control. Without it, a cell renderer that printed a digit
	// unconditionally would satisfy every assertion above, and the digit rule
	// would be measuring nothing.
	blank := report.RepoView{HasSnapshot: true, Status: string(store.StatusOK)}
	for _, cell := range blank.Signals() {
		if hasDigit(cell.Value) {
			t.Errorf("%q renders %q on a RepoView that measured nothing, so a digit in a cell no longer proves anything was measured, and the assertions above prove nothing either",
				cell.ID, cell.Value)
		}
	}
}

// everySignalMeasured is one repo whose collection looked at every signal there
// is: a coverage profile, a test stream, a lint log, a module listing with the
// proxy consulted, and a step that timed itself.
//
// Assembled from the same helpers the other fixtures use, so it describes output
// the collector can really produce rather than a shape only this test sees.
func everySignalMeasured() delta.Input {
	return delta.Input{
		Repo: repoAt(9, repoWired),
		Head: snap(91, 9, "go1.26.5", store.StatusOK, ""),
		HeadMetrics: metrics(
			cov(pkgAlpha, 80, 100),
			testStream(1, testCount(pkgAlpha, 9)),
			lintRun("lint", 5, 0, 1),
			depsRun(27, 400, 3),
			[]store.Metric{stepTiming("coverage", 42000)},
		),
	}
}

// TestEverySignalIsPublishedUnderItsRegistryID is the JSON half of the same
// join, and it is what makes the envelope's signal catalog usable.
//
// The catalog publishes each signal's id and nothing else ties that id to the
// key its numbers arrive under, so a consumer looking up row[entry.id] is
// relying on a correspondence that pairedGroups checks for the signals with a
// table column and nothing checks for the rest.
//
// It renders through delta.Compute and the real JSON path rather than marshaling
// a hand-built RepoView, so it also covers the third part of CONTRIBUTING's step
// 4, the line in buildRepo. A field and a case with no buildRepo line publishes
// null for a repo that measured the signal.
func TestEverySignalIsPublishedUnderItsRegistryID(t *testing.T) {
	rep := delta.Compute([]delta.Input{everySignalMeasured()}, options(), fixedNow())
	row := jsonRepoRows(t, rep)[repoWired]
	if row == nil {
		t.Fatalf("%s is missing from the json entirely", repoWired)
	}

	for _, sig := range delta.Signals() {
		v, present := row[string(sig.ID)]
		if !present {
			t.Errorf("%q is in the registry, but no key of that name is on the wire. The envelope's signal catalog publishes this id as the legend for a row's groups, so a consumer reading row[%q] finds nothing.",
				sig.ID, sig.ID)
			continue
		}
		if v == nil {
			t.Errorf("%q renders as null for a repo whose collection measured every signal there is. Either buildRepo has no line for it, or this fixture no longer emits its marker.",
				sig.ID)
			continue
		}
		if _, isObject := v.(map[string]any); !isObject {
			t.Errorf("%q renders as %T, want the nullable object every measurement group is", sig.ID, v)
		}
	}
}
