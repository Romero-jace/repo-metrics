package report

import (
	"fmt"
	"math"
	"strconv"
	"time"

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
	// Signals is the legend for every measurement below: what each one is
	// called, what unit it is in, and which direction is good news. It is on the
	// envelope rather than on each row because it is the same for every repo,
	// and repeating it per row would be payload carrying no information.
	Signals []SignalCatalogEntry `json:"signals"`
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
	// The rest of the signals. Each is nil when nothing measured it, for the
	// same reason and by the same mechanism.
	//
	// These are separate named fields rather than a map keyed by signal name,
	// and that is a decision the field census forces. The census classifies
	// every rendered path against a hand-written table, and its walk collapses
	// list elements to a single path, so a list of signal objects would give
	// every signal one shared classification forever and a seventh signal would
	// arrive without the census ever noticing. A map would keep the paths
	// distinct, but it forces one value type on every entry, and coverage
	// carries counts and culprits the others do not. Named fields cost a line
	// each and keep both properties.
	Tests            *SignalView `json:"tests"`
	TestFailures     *SignalView `json:"test_failures"`
	TestSkipped      *SignalView `json:"test_skipped"`
	UntestedPackages *SignalView `json:"untested_packages"`
	TestTime         *SignalView `json:"test_time"`
	LintFindings     *SignalView `json:"lint_findings"`
	LintErrors       *SignalView `json:"lint_errors"`
	LintSuppressed   *SignalView `json:"lint_suppressed"`
	// The three dependency groups null INDEPENDENTLY of each other, unlike the
	// test and lint families. They come from one parsed stream but are measurable
	// under different conditions: the count needs nothing, the age needs a publish
	// timestamp, and the outdated count needs the module proxy to have been
	// consulted at all.
	Dependencies         *SignalView `json:"dependencies"`
	OutdatedDependencies *SignalView `json:"outdated_dependencies"`
	DependencyAge        *SignalView `json:"dependency_age"`
	// HasSnapshot is false when this repo has never been collected. Every
	// measurement group is nil in that case, so it is not gating a number; it
	// tells a consumer which kind of nothing it is looking at, alongside Status.
	HasSnapshot bool `json:"has_snapshot"`
	HasBaseline bool `json:"has_baseline"`
	EnvChanged  bool `json:"env_changed"`
	// MovedBy names the signals that made this repo lead the report. With one
	// signal it was obvious; with several, "this repo moved" is not an answer
	// unless it also says which measurement moved. It carries no numbers, so it
	// is a list of names rather than a group.
	MovedBy []string `json:"moved_by"`
	Error   string   `json:"error,omitempty"`
}

// SignalView is what one signal measured, plus its comparison against the
// baseline. One type for every signal, so the two levels of null reach the wire
// through exactly one function.
//
// The keys are value and delta rather than something signal-specific like pct
// or count, because a consumer walking signals should not need per-signal key
// knowledge. What the number means is answered once by the envelope's signal
// catalog rather than repeated on every row of every repo.
type SignalView struct {
	Value float64 `json:"value"`
	// Delta is nil when there is no baseline, or when either side did not
	// measure. This is the second, inner level of null and it means something
	// different from the outer one: measured, but nothing to compare it to. A
	// zero here is a real finding, so the two must not be collapsed.
	Delta *float64 `json:"delta"`
}

// CoverageView is a SignalView plus the findings only coverage has.
//
// The culprits and the package churn live here rather than beside it because
// they are coverage findings: buildRepo only fills them in when coverage was
// measured, and a package named as the cause of a move is as meaningless as the
// move itself when nothing measured either side. Keeping them here is also what
// lets the rule be stated without an exception: every number in the report is
// inside a nullable group.
//
// The embedding is flattened by encoding/json, so coverage renders as one
// object rather than a group inside a group. That matters: the census rejects a
// nested group outright, because a null on the outer one would leave a consumer
// unable to say which level was not measured.
type CoverageView struct {
	SignalView
	Covered         int           `json:"covered"`
	Total           int           `json:"total"`
	Culprits        []CulpritView `json:"culprits"`
	AddedPackages   []string      `json:"added_packages"`
	RemovedPackages []string      `json:"removed_packages"`
}

// buildSignal is the only place a SignalView is constructed.
//
// A nil return means the group renders as null, which is the outer level of the
// rule. A nil Delta inside a non-nil group is the inner level. Both, for every
// signal that exists or ever will, in eight lines rather than eight lines per
// signal.
func buildSignal(sd delta.SignalDelta) *SignalView {
	value, measured := sd.Head.Value()
	if !measured {
		return nil
	}
	out := &SignalView{Value: value}
	if d, meaningful := sd.Change.Delta(); meaningful {
		out.Delta = &d
	}
	return out
}

// SignalCell is one signal rendered for a human.
//
// Formatting happens here rather than in the template so the template has no
// pointer to dereference and no unit to know about, and so the words for an
// unmeasured signal are written once instead of once per column. It is produced
// by a method, so it never reaches the wire and cannot become a second flat copy
// of the nested data.
type SignalCell struct {
	ID    string
	Label string
	// Value is the formatted measurement, or the words for whichever state
	// withheld it. It never carries a digit unless something measured it.
	Value string
	// Delta is the formatted change, or the words for why there is not one.
	Delta string
	// Verdict is "better" or "worse", or empty when the signal did not move or
	// there is nothing to compare. It is the signal's direction applied to the
	// sign of its change, so a reader never has to know on their own whether a
	// rising number is good news.
	Verdict string
	// Moved is whether this signal is one of the ones that made the repo lead
	// the report, so the write-up can say why rather than only that.
	Moved bool
}

// SignalColumn is one column heading in the every-repo table, derived from the
// same registry that produces the cells under it so the two cannot drift.
type SignalColumn struct {
	ID    string
	Label string
}

// SignalColumns is the table's headings, taken from the registry rather than
// typed into the template, so a heading cannot end up over the wrong column or
// outlive the signal it names.
func (v View) SignalColumns() []SignalColumn {
	out := make([]SignalColumn, 0, len(delta.Signals()))
	for _, sig := range delta.Signals() {
		if sig.Table {
			out = append(out, SignalColumn{ID: string(sig.ID), Label: sig.Label})
		}
	}
	return out
}

// SignalCatalog is what every signal on the wire means, published once on the
// envelope instead of on every row of every repo.
//
// Without it the JSON's uniform "value" key is opaque: a consumer cannot tell
// whether 214000 is a count or a duration, or whether a rise is good news. With
// it, that costs a few dozen tokens once rather than four fields times every
// signal times every repo.
type SignalCatalogEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
	// Direction is "higher_is_better" or "lower_is_better". There is no neutral
	// value on purpose: a signal nobody will commit to a direction for renders
	// as "up" rather than "worse", and then a reader has to know on their own
	// that rising test failures are bad news.
	Direction string `json:"direction"`
}

// Signals returns every signal for this repo in registry order, formatted.
//
// It ranges the registry rather than the struct fields, so a signal that exists
// but was never given a field here renders as unmeasured rather than silently
// vanishing, and the test that pins registry against fields catches it.
func (r RepoView) Signals() []SignalCell {
	out := make([]SignalCell, 0, len(delta.Signals()))
	for _, sig := range delta.Signals() {
		out = append(out, r.cell(sig))
	}
	return out
}

// TableSignals is the subset that earns a column in the every-repo table.
//
// Every signal in the JSON would make a fourteen-column markdown table that
// nobody can read across. Which ones appear is a layout decision and nothing
// more: a signal left out is still on the wire, still in the movers write-up,
// and still guarded exactly the same.
func (r RepoView) TableSignals() []SignalCell {
	out := make([]SignalCell, 0, len(delta.Signals()))
	for _, sig := range delta.Signals() {
		if sig.Table {
			out = append(out, r.cell(sig))
		}
	}
	return out
}

// MovedSignals is the signals that made this repo a mover, for the write-up.
func (r RepoView) MovedSignals() []SignalCell {
	out := make([]SignalCell, 0, len(delta.Signals()))
	for _, cell := range r.Signals() {
		if cell.Moved {
			out = append(out, cell)
		}
	}
	return out
}

// group returns the wire group for one signal, or nil when nothing measured it.
// This switch is the join between the registry and the struct fields above, and
// TestEverySignalReachesTheWire is what stops the two drifting.
func (r RepoView) group(id delta.SignalID) *SignalView {
	switch id {
	case delta.SigCoverage:
		if r.Coverage == nil {
			return nil
		}
		return &r.Coverage.SignalView
	case delta.SigTests:
		return r.Tests
	case delta.SigTestFailures:
		return r.TestFailures
	case delta.SigTestSkipped:
		return r.TestSkipped
	case delta.SigUntestedPackages:
		return r.UntestedPackages
	case delta.SigTestTime:
		return r.TestTime
	case delta.SigLintFindings:
		return r.LintFindings
	case delta.SigLintErrors:
		return r.LintErrors
	case delta.SigLintSuppressed:
		return r.LintSuppressed
	case delta.SigDependencies:
		return r.Dependencies
	case delta.SigOutdatedDeps:
		return r.OutdatedDependencies
	case delta.SigDependencyAge:
		return r.DependencyAge
	default:
		return nil
	}
}

func (r RepoView) cell(sig delta.Signal) SignalCell {
	out := SignalCell{ID: string(sig.ID), Label: sig.Label}

	group := r.group(sig.ID)
	if group == nil {
		// The two states that withhold everything are told apart here, because
		// "the collection failed" and "this signal was never configured" call
		// for opposite actions.
		out.Value, out.Delta = "not measured", "not measured"
		if !r.HasSnapshot || r.Status == string(store.StatusFailed) {
			out.Value, out.Delta = "not collected", "not collected"
		}
		return out
	}

	out.Value = formatValue(group.Value, sig.Unit)
	out.Moved = r.movedBy(sig.ID)
	if group.Delta == nil {
		out.Delta = "no baseline yet"
		return out
	}
	out.Delta = formatDelta(*group.Delta, sig.Unit)
	out.Verdict = verdict(*group.Delta, sig.Direction)
	return out
}

func (r RepoView) movedBy(id delta.SignalID) bool {
	for _, moved := range r.MovedBy {
		if moved == string(id) {
			return true
		}
	}
	return false
}

// verdict turns a change and a direction into a word a reader can act on.
// Without it the report says a number went up and leaves the reader to work out
// whether rising test failures are good news.
func verdict(change float64, dir delta.Direction) string {
	if change == 0 {
		return ""
	}
	improved := change > 0
	if dir == delta.LowerIsBetter {
		improved = change < 0
	}
	if improved {
		return "better"
	}
	return "worse"
}

// formatValue renders a measurement in its own unit. It lives here rather than
// in the template because the template cannot branch on a unit without a func
// per unit, and because a number reaching the reader without its unit is how a
// duration gets read as a count.
func formatValue(v float64, unit delta.Unit) string {
	switch unit {
	case delta.UnitPercent:
		return fmt.Sprintf("%.1f%%", v)
	case delta.UnitMilliseconds:
		return formatDuration(v)
	case delta.UnitDays:
		// Whole days. A median dependency age is a number in the hundreds and the
		// fractional part is noise, but the word has to be there: 412 alone in a
		// column reads as a count of something.
		return fmt.Sprintf("%.0f days", v)
	case delta.UnitCount:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
}

// formatDelta renders a change, always signed, so a reader never has to work
// out which direction it went.
func formatDelta(d float64, unit delta.Unit) string {
	switch unit {
	case delta.UnitPercent:
		return fmt.Sprintf("%+.1f pts", d)
	case delta.UnitMilliseconds:
		sign := "+"
		if d < 0 {
			sign = "-"
		}
		return sign + formatDuration(math.Abs(d))
	case delta.UnitDays:
		return fmt.Sprintf("%+.0f days", d)
	case delta.UnitCount:
		return signed(d)
	default:
		return signed(d)
	}
}

// signed renders a number with an explicit plus, because FormatFloat only
// volunteers the minus and a bare "3" in a change column reads as a value.
func signed(d float64) string {
	out := strconv.FormatFloat(d, 'f', -1, 64)
	if d >= 0 {
		return "+" + out
	}
	return out
}

// formatDuration renders milliseconds at a scale a person reads. A test suite
// measured at 214000 tells a reader nothing that "3m34s" does not tell them
// faster.
func formatDuration(ms float64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
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
		Signals:     signalCatalog(),
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
			// A run that measured nothing at all is the third door into the
			// same room: it comes back status ok, so neither test above catches
			// it, and its "move" is a healthy baseline measured against zero.
			// delta.Compute now refuses to nominate such a repo, so this is a
			// backstop rather than the only guard, and it is kept for the reason
			// backstop_test.go gives: Compute is not the only thing that can
			// produce a RepoDelta, and the report layer has to be safe against a
			// hand-built one.
			if failed(r.Head) || neverCollected(r.Head) || !anyMeasured(r) {
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

// signalCatalog renders the registry as the envelope's legend. It ranges the
// registry, so a signal cannot reach the wire without appearing here.
func signalCatalog() []SignalCatalogEntry {
	out := make([]SignalCatalogEntry, 0, len(delta.Signals()))
	for _, sig := range delta.Signals() {
		out = append(out, SignalCatalogEntry{
			ID:        string(sig.ID),
			Label:     sig.Label,
			Unit:      unitName(sig.Unit),
			Direction: directionName(sig.Direction),
		})
	}
	return out
}

func unitName(u delta.Unit) string {
	switch u {
	case delta.UnitPercent:
		return "percent"
	case delta.UnitMilliseconds:
		return "milliseconds"
	case delta.UnitDays:
		return "days"
	case delta.UnitCount:
		return "count"
	default:
		return "count"
	}
}

func directionName(d delta.Direction) string {
	if d == delta.LowerIsBetter {
		return "lower_is_better"
	}
	return "higher_is_better"
}

// anyMeasured reports whether this repo measured anything a mover could be
// about. A repo that measured nothing has no move to lead with, whatever
// arithmetic says about the difference between its zeroes and a real baseline.
func anyMeasured(r delta.RepoDelta) bool {
	for _, sd := range r.SignalDeltas() {
		if sd.Head.IsMeasured() {
			return true
		}
	}
	return false
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

	// Every signal but coverage is built by the same eight lines, so a new one
	// needs a field above and a case here and nothing else. The measured versus
	// unmeasured rule and the has-a-baseline rule are both inside buildSignal,
	// which means neither can be written differently for a new signal by
	// accident.
	out.Tests = buildSignal(r.Signal(delta.SigTests))
	out.TestFailures = buildSignal(r.Signal(delta.SigTestFailures))
	out.TestSkipped = buildSignal(r.Signal(delta.SigTestSkipped))
	out.UntestedPackages = buildSignal(r.Signal(delta.SigUntestedPackages))
	out.TestTime = buildSignal(r.Signal(delta.SigTestTime))
	out.LintFindings = buildSignal(r.Signal(delta.SigLintFindings))
	out.LintErrors = buildSignal(r.Signal(delta.SigLintErrors))
	out.LintSuppressed = buildSignal(r.Signal(delta.SigLintSuppressed))
	out.Dependencies = buildSignal(r.Signal(delta.SigDependencies))
	out.OutdatedDependencies = buildSignal(r.Signal(delta.SigOutdatedDeps))
	out.DependencyAge = buildSignal(r.Signal(delta.SigDependencyAge))
	for _, id := range r.MovedBy {
		out.MovedBy = append(out.MovedBy, string(id))
	}

	// Coverage is the one signal built by hand, because it carries counts and
	// package findings the others do not. Its value and its delta still come
	// from buildSignal, so the part that can go wrong is shared and only the
	// extras are bespoke.
	base := buildSignal(r.Signal(delta.SigCoverage))
	if base == nil {
		return out
	}
	coverage := CoverageView{
		SignalView: *base,
		Covered:    r.HeadCoverage.Covered,
		Total:      r.HeadCoverage.Total,
	}

	// Package churn is a comparison too. A head that measured no coverage makes
	// every baseline package look deleted, so the report would list a repo's
	// whole tree as gone on the strength of an empty profile.
	if r.Signal(delta.SigCoverage).Change.Meaningful() || !r.HasBaseline {
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
	if r.Signal(delta.SigCoverage).Change.Meaningful() {
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
