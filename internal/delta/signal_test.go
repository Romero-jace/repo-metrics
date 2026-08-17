package delta_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// testStreamMarker is the row a collector emits whenever it parsed a test
// stream at all, including when everything it counted came back zero. Five
// signals share it, because they come out of one parse: there is no state where
// the collector counted the passes but not the failures.
func testStreamMarker(withoutTests int) store.Metric {
	return store.Metric{Key: collect.KeyPkgWithoutTest, Scope: "", Value: float64(withoutTests)}
}

// signalFixture is one signal's two states, written by hand because the whole
// point is that a person decided what each one looks like.
//
// measuredZero is what the collector stores when it looked and the answer was
// zero. absent is what it stores when it never looked. The two must not be
// confusable, and every registered signal needs a row here or the test fails.
type signalFixture struct {
	measuredZero []store.Metric
	absent       []store.Metric
	// zeroIsReachable is false for a signal whose measured-zero state no real
	// collector can produce. Nothing sets it false today, and zeroUnreachable
	// below is what pins that. It is a claim about the collector, so it is
	// spelled out here rather than inferred from anything the compiler sees.
	//
	// Coverage is the one worth spelling out, because it reads like an exception
	// and is not. Two different things arrive as zero and only one of them is a
	// measurement. A profile with no instrumented packages stores no rows at
	// all, so the marker is absent and the signal is unmeasured: collect's
	// parser warns and emits nothing. A profile whose blocks are all present and
	// none of them covered stores both keys for every package, so the marker is
	// there and the value is 0.0 percent. The second is the ordinary state of an
	// untested package under -coverpkg, and `go tool cover -func` prints
	// "total: (statements) 0.0%" for it, so the toolchain calls it a
	// measurement too.
	zeroIsReachable bool
}

// zeroUnreachable pins which signals are allowed to set zeroIsReachable false,
// with the reason each one is exempt, in the same idiom as the wire census's
// pinned measurement list in report/degraded_test.go.
//
// It held nothing for a long time, and the single entry it holds now is the kind
// of case it was built for: an exemption that is a property of the FORMAT, not of
// the quantity. Every other registered signal has a measured zero a real
// collector produces, including coverage, whose fixture comment above says why it
// is not the exception it looks like.
//
// The reason it exists at all: zeroIsReachable wraps that entire assertion and
// has no else branch, so flipping one fixture's bool deleted the check for that
// signal while the subtest kept running and kept reporting ok. An audit did
// exactly that to SigLintFindings, whose own fixture comment calls it the single
// most important measured zero in the table, and build, vet, the full test run
// and golangci-lint all stayed green. Opting a signal out now has to be a diff
// against this map with a written reason, which a reviewer sees.
var zeroUnreachable = map[delta.SignalID]string{
	delta.SigLintSuppressed: "A SARIF suppressions array is the only way a document can report a finding that " +
		"was raised and then silenced, and almost no linter writes one: golangci-lint drops a //nolint finding " +
		"before it reaches the log, and so do ruff for # noqa, rubocop for # rubocop:disable, detekt for " +
		"@Suppress and clippy for #[allow]. A document with no suppressions is therefore the same bytes whether " +
		"the repo suppresses nothing or the linter cannot say, so parseSARIF stores no row and the zero is never " +
		"a measurement. This is the one exemption here that comes from what the format can express rather than " +
		"from what the number counts.",
}

// The fixture table. A signal missing from here fails the test below, which is
// the mechanism that stops a new signal shipping without anyone deciding what
// its unmeasured state looks like.
func signalFixtures() map[delta.SignalID]signalFixture {
	// One package with statements, none of them covered. This is a real
	// measurement of zero percent, not an absence: the collector instrumented
	// the package and found nothing covered.
	coverageZero := cov("m/a", 0, 10)

	return map[delta.SignalID]signalFixture{
		delta.SigCoverage: {
			measuredZero:    coverageZero,
			absent:          []store.Metric{testStreamMarker(0)},
			zeroIsReachable: true,
		},
		delta.SigCoverageLines: {
			// One file with lines, none of them hit. A real measurement of zero
			// percent: the tracefile named the file and recorded no coverage on it.
			measuredZero: []store.Metric{
				{Key: collect.KeyCoveredLines, Scope: "src/a.ts", Value: 0},
				{Key: collect.KeyTotalLines, Scope: "src/a.ts", Value: 12},
			},
			// Statement coverage present and line coverage absent, which is what
			// every Go repo stores. The two must not stand in for each other: a
			// repo measured in statements has no line-coverage number, and reading
			// the statement rate here would publish one denominator's answer under
			// the other's name.
			absent:          coverageZero,
			zeroIsReachable: true,
		},
		delta.SigTests: {
			// The stream was parsed and the repo genuinely has no tests.
			measuredZero:    []store.Metric{testStreamMarker(3)},
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigTestFailures: {
			// Every test passed. Zero failures is the good news, and reporting
			// it as unmeasured would throw away the finding.
			measuredZero: []store.Metric{
				testStreamMarker(0),
				{Key: collect.KeyTestFailed, Scope: "m/a", Value: 0},
			},
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigTestSkipped: {
			measuredZero: []store.Metric{
				testStreamMarker(0),
				{Key: collect.KeyTestSkipped, Scope: "m/a", Value: 0},
			},
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigUntestedPackages: {
			// Zero packages without tests is the best possible answer and has to
			// survive as a number.
			measuredZero:    []store.Metric{testStreamMarker(0)},
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigTestTime: {
			measuredZero: []store.Metric{
				testStreamMarker(0),
				{Key: collect.KeyTestDurationMS, Scope: "m/a", Value: 0},
			},
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigLintFindings: {
			// A clean lint run. This is the single most important measured zero
			// in the whole table: a repo with nothing left to fix is the good
			// news the tool should be able to report, and it is byte-identical
			// to a repo nobody lints unless the marker is there.
			measuredZero:    lintRunMarker("lint", 0, 0, 0),
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigLintErrors: {
			// Findings, but none at error level. Reporting that as unmeasured
			// would throw away the distinction the signal exists for.
			measuredZero:    lintRunMarker("lint", 4, 0, 0),
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigLintSuppressed: {
			// A lint run that suppressed nothing, which is what the overwhelming
			// majority of real SARIF documents describe.
			measuredZero: lintRunMarker("lint", 4, 1, 0),
			absent:       cov("m/a", 5, 10),
			// Opted out, and the one signal here where the opt-out is a property of
			// the FORMAT rather than of the thing being counted. A SARIF
			// suppressions array is the only way a document can report a silenced
			// finding, and almost no linter writes one: //nolint, # noqa and
			// #[allow] all delete the finding instead. So a document carrying no
			// suppressions cannot distinguish "this repo suppresses nothing" from
			// "this linter has no way to tell you", and the parser stores no row.
			//
			// It was true, on a fixture that hand-wrote the zero row the parser
			// wrote unconditionally. That made this entry a witness FOR the bug: the
			// census asked whether a measured zero was reachable, the fixture
			// supplied one by construction, and the answer came back yes for a
			// signal whose zero was never a measurement.
			zeroIsReachable: false,
		},
		delta.SigDependencies: {
			// A repo that vendors nothing. Rare in Go and entirely real.
			measuredZero:    []store.Metric{{Key: collect.KeyDepsTotal, Value: 0}},
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
		delta.SigOutdatedDeps: {
			// Everything current, and demonstrably so: the proxy was consulted
			// and reported no newer versions.
			measuredZero: []store.Metric{
				{Key: collect.KeyDepsTotal, Value: 27},
				{Key: collect.KeyDepsOutdatedDirect, Value: 0},
			},
			// The module list was read but nothing checked for updates, which is
			// what GOPROXY=off produces. This is the reason these three signals do
			// not share a marker: the stream here is identical to the one above
			// minus the outdated row, and a shared marker would report zero
			// outdated dependencies for a repo nobody asked the proxy about.
			absent:          []store.Metric{{Key: collect.KeyDepsTotal, Value: 27}},
			zeroIsReachable: true,
		},
		delta.SigDependencyAge: {
			// A dependency published today. The median age really is zero days.
			measuredZero: []store.Metric{
				{Key: collect.KeyDepsTotal, Value: 1},
				{Key: collect.KeyDepsAgeMedianDays, Value: 0},
			},
			// Dependencies exist but none carried a publish timestamp, which a
			// directory replacement produces. Not an age of zero.
			absent:          []store.Metric{{Key: collect.KeyDepsTotal, Value: 27}},
			zeroIsReachable: true,
		},
		delta.SigCollectTime: {
			// A command that finished inside a millisecond. Rare and entirely
			// real, and the row is written because something ran.
			measuredZero: []store.Metric{
				{Key: collect.KeySignalDurationMS, Scope: "coverage", Value: 0},
			},
			// Ingest mode. Nothing ran, so there is no wall time, and a zero here
			// would be a duration nobody measured.
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
		},
	}
}

// lintRunMarker is what the SARIF parser stores for one lint step, scoped by the
// step's name. Scoped rather than repo-level because a polyglot repo runs two
// linters as two steps and they have to sum rather than collide.
//
// The two finding counts are written even at zero. The suppression count is not,
// and passing zero here omits the row exactly as the parser does — see parseSARIF
// for why a zero there would be a fabrication rather than a measurement. This
// helper used to write all three unconditionally, which made it a fixture for a
// row no collector produces, and the census below then asserted that a measured
// zero was reachable for a signal where it is not.
func lintRunMarker(step string, findings, errs, suppressed int) []store.Metric {
	out := []store.Metric{
		{Key: collect.KeyLintFindings, Scope: step, Value: float64(findings)},
		{Key: collect.KeyLintErrors, Scope: step, Value: float64(errs)},
	}
	if suppressed > 0 {
		out = append(out, store.Metric{Key: collect.KeyLintSuppressed, Scope: step, Value: float64(suppressed)})
	}
	return out
}

// TestEverySignalDistinguishesZeroFromUnmeasured is the reason the signal layer
// exists.
//
// This project has found and fixed the same bug eight times: something
// unmeasured published as a measurement of zero. Every one of those was a place
// where the guard was written by hand for one signal and forgotten for another.
// This asserts the property once and proves it per signal, and it fails when a
// signal is registered without anyone deciding what its two states look like.
func TestEverySignalDistinguishesZeroFromUnmeasured(t *testing.T) {
	fixtures := signalFixtures()

	for _, sig := range delta.Signals() {
		fixture, ok := fixtures[sig.ID]
		if !ok {
			t.Errorf("signal %q is registered but has no fixture here, so nothing has decided what its unmeasured state looks like. Add a row: what does the collector store when it looked and found zero, and what does it store when it never looked?", sig.ID)
			continue
		}

		t.Run(string(sig.ID), func(t *testing.T) {
			measured := one(t, delta.Input{
				Repo: store.Repo{Name: "measured"},
				Head: snap("go1.26"), HeadMetrics: fixture.measuredZero,
			}, opts()).Signal(sig.ID)
			value, got := measured.Head.Value()

			if fixture.zeroIsReachable {
				if !got {
					t.Errorf("a collector that looked and found zero reports unmeasured, so a real finding is being thrown away")
				}
				if value != 0 {
					t.Errorf("the measured-zero fixture measured %v, not 0, so this test is not exercising the case it names", value)
				}
			} else if got && value == 0 {
				// The opt-out is a claim about the collector: no input it can be
				// handed produces this signal's marker with a value of zero.
				// Nothing here can check a collector, but the fixture can be held
				// to the claim made on its behalf, so a signal opted out while its
				// own measuredZero rows still measure zero fails here instead of
				// silently dropping the assertion above.
				t.Errorf("this signal is opted out of the measured-zero check on the grounds that a real collector cannot produce a measured zero, and yet its own measuredZero fixture produces exactly that. One of the two is wrong: either the rows are not really unreachable, or they are not the rows a collector would store")
			}

			absent := one(t, delta.Input{
				Repo: store.Repo{Name: "absent"},
				Head: snap("go1.26"), HeadMetrics: fixture.absent,
			}, opts()).Signal(sig.ID)

			if _, got := absent.Head.Value(); got {
				t.Errorf("a collector that never looked reports a measurement, which is this project's recurring bug: an absence published as a number")
			}
		})
	}
}

// TestNoSignalOptsOutOfTheMeasuredZeroCheckUnannounced closes the hole in the
// test above.
//
// zeroIsReachable is a one-word switch that deletes the measured-zero assertion
// for whichever signal carries it, and the subtest that loses the assertion
// still runs and still passes on the strength of the absent-metrics check alone.
// Nothing else in the package reads the field, so a flip is invisible in a
// review of the test run: it looks like a bool changing in a fixture table.
//
// Pinning the opt-out set turns that into a failure here, naming the signal, so
// dropping the check for coverage or for a clean lint run costs a second diff
// with a reason in it. The set is empty today, which is the claim being pinned:
// every registered signal has a measured zero, so none of them opt out.
func TestNoSignalOptsOutOfTheMeasuredZeroCheckUnannounced(t *testing.T) {
	var optedOut []string
	for id, fixture := range signalFixtures() {
		if !fixture.zeroIsReachable {
			optedOut = append(optedOut, string(id))
		}
	}
	sort.Strings(optedOut)

	pinned := make([]string, 0, len(zeroUnreachable))
	for id := range zeroUnreachable {
		pinned = append(pinned, string(id))
	}
	sort.Strings(pinned)

	if strings.Join(optedOut, ",") != strings.Join(pinned, ",") {
		t.Errorf("signals skipping the measured-zero assertion: got %v, pinned %v. A signal whose fixture sets zeroIsReachable false is asserting that no real collector can store its marker with a value of zero, which is a claim about a collector and belongs in zeroUnreachable with the reason written next to it, not in a bool nobody reads.", optedOut, pinned)
	}

	for id, reason := range zeroUnreachable {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("zeroUnreachable[%q] carries no reason, so the opt-out it grants cannot be reviewed", id)
		}
		if delta.SignalByID(id).ID != id {
			t.Errorf("zeroUnreachable exempts %q, which is not a registered signal, so the exemption is rot from a rename or a removal", id)
		}
	}
}

// The comparison rule has one implementation, so proving it once proves it for
// every signal there will ever be. These are the four states Compare exists to
// tell apart, and three of them must produce no delta at all.
func TestAChangeNeedsBothSidesAndABaseline(t *testing.T) {
	measured, unmeasured := delta.Measured(10), delta.Unmeasured()

	cases := []struct {
		name        string
		head, base  delta.Measurement
		hasBaseline bool
		want        bool
	}{
		{"both sides measured", measured, delta.Measured(4), true, true},
		{"no baseline at all", measured, delta.Measured(4), false, false},
		{"head never measured", unmeasured, delta.Measured(4), true, false},
		{"baseline never measured", measured, unmeasured, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			change := delta.Compare(tc.head, tc.base, tc.hasBaseline)
			d, meaningful := change.Delta()

			if meaningful != tc.want {
				t.Errorf("meaningful = %v, want %v", meaningful, tc.want)
			}
			if !meaningful && d != 0 {
				t.Errorf("a change that means nothing still carries %v, which a caller discarding the boolean would publish", d)
			}
			if meaningful && d != 6 {
				t.Errorf("delta = %v, want 6", d)
			}
		})
	}
}

// The zero Measurement has to be the unmeasured one, because that is what a
// missing map key, an uninitialized field and a half-built test fixture all
// produce. If the zero value were a measured zero, every one of those would
// publish a number nobody measured.
func TestTheZeroMeasurementIsUnmeasured(t *testing.T) {
	var zero delta.Measurement
	if zero.IsMeasured() {
		t.Error("the zero Measurement claims to be measured, so every unset field in this package is a fabricated zero")
	}

	// A RepoDelta with nil signal maps is what a hand-built literal looks like.
	// It must answer unmeasured rather than panicking or reporting zeroes.
	var empty delta.RepoDelta
	if _, ok := empty.Signal(delta.SigCoverage).Head.Value(); ok {
		t.Error("a RepoDelta with no signal maps reports a measurement")
	}
	if empty.Signal(delta.SigCoverage).Change.Meaningful() {
		t.Error("a RepoDelta with no signal maps reports a meaningful change")
	}
}

// A snapshot carrying only the toolchain-neutral marker must read as measured.
//
// This is the whole point of the marker list, and it is the half no existing
// fixture exercises: every one of them writes pkg.without_tests, which a Go
// stream produces and nothing else can. A JUnit document lists the suites that
// ran and cannot say how many source files carry no tests, so a TypeScript repo
// writes test.suites and nothing else. Without this, the four shared test
// signals would report null for every non-Go repo and the whole extension would
// be inert while the suite stayed green.
//
// The negative half matters just as much. untested_packages keeps the Go-only
// marker on purpose: nobody counted untested files here, so the honest answer is
// unmeasured, and a zero would be a count of nothing published as a count of none.
func TestTheNeutralTestMarkerMeasuresWithoutTheGoOne(t *testing.T) {
	metrics := []store.Metric{
		{Key: collect.KeyTestSuites, Scope: "unit", Value: 12},
		{Key: collect.KeyTestCount, Scope: "src/a.test.ts", Value: 40},
		{Key: collect.KeyTestFailed, Scope: "src/a.test.ts", Value: 2},
		{Key: collect.KeyTestSkipped, Scope: "src/a.test.ts", Value: 1},
		{Key: collect.KeyTestDurationMS, Scope: "src/a.test.ts", Value: 900},
	}

	got := delta.Measure(metrics)

	for _, want := range []struct {
		id    delta.SignalID
		value float64
	}{
		{delta.SigTests, 40},
		{delta.SigTestFailures, 2},
		{delta.SigTestSkipped, 1},
		{delta.SigTestTime, 900},
	} {
		m := got[want.id]
		v, ok := m.Value()
		if !ok {
			t.Errorf("%s: unmeasured, but a test result was parsed and its counts are right there", want.id)
			continue
		}
		if v != want.value {
			t.Errorf("%s: got %v, want %v", want.id, v, want.value)
		}
	}

	if got[delta.SigUntestedPackages].IsMeasured() {
		t.Error("untested_packages measured from a source that cannot count untested files; nobody looked, so nothing may be published")
	}
}

// The legacy marker alone still measures, or every snapshot already in a
// database goes blank the day this ships.
func TestTheLegacyTestMarkerStillMeasures(t *testing.T) {
	metrics := []store.Metric{
		{Key: collect.KeyPkgWithoutTest, Value: 3},
		{Key: collect.KeyTestCount, Scope: "m/a", Value: 40},
	}

	got := delta.Measure(metrics)

	if v, ok := got[delta.SigTests].Value(); !ok || v != 40 {
		t.Errorf("tests from a pre-test.suites snapshot: got %v measured=%v, want 40", v, ok)
	}
	if v, ok := got[delta.SigUntestedPackages].Value(); !ok || v != 3 {
		t.Errorf("untested_packages: got %v measured=%v, want 3", v, ok)
	}
}

// Two sides that matched on DIFFERENT markers are not comparable.
//
// The counts either side of that boundary were summed over different things: one
// over whatever rows a Go stream wrote at repo scope, the other over the steps
// that emitted the neutral marker. Subtracting them would report the change in
// apparatus as a change in the repo, which is the same failure as a linter being
// switched on mid-series.
func TestASideWithoutTheNeutralMarkerIsNotComparable(t *testing.T) {
	head := delta.Measure([]store.Metric{
		{Key: collect.KeyTestSuites, Scope: "unit", Value: 1},
		{Key: collect.KeyTestCount, Scope: "m/a", Value: 42},
	})
	base := delta.Measure([]store.Metric{
		{Key: collect.KeyPkgWithoutTest, Value: 0},
		{Key: collect.KeyTestCount, Scope: "m/a", Value: 40},
	})

	if delta.Compare(head[delta.SigTests], base[delta.SigTests], true).Meaningful() {
		t.Error("a delta was published across a marker change, so a change in how tests were counted reads as tests appearing")
	}

	// The control: two sides on the same marker do compare, or the assertion
	// above would pass on an implementation that never compares anything.
	base2 := delta.Measure([]store.Metric{
		{Key: collect.KeyTestSuites, Scope: "unit", Value: 1},
		{Key: collect.KeyTestCount, Scope: "m/a", Value: 40},
	})
	change := delta.Compare(head[delta.SigTests], base2[delta.SigTests], true)
	if d, ok := change.Delta(); !ok || d != 2 {
		t.Errorf("two sides on the same marker: got %v meaningful=%v, want 2", d, ok)
	}
}
