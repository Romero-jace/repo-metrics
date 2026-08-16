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
// It is empty on purpose. Every registered signal has a measured zero a real
// collector produces, including coverage, so nothing here should opt out of the
// measured-zero half of TestEverySignalDistinguishesZeroFromUnmeasured. The
// field's doc comment above says why coverage is not the exception it looks
// like; that sentence used to claim the opposite and was wrong.
//
// The reason it exists at all: zeroIsReachable wraps that entire assertion and
// has no else branch, so flipping one fixture's bool deleted the check for that
// signal while the subtest kept running and kept reporting ok. An audit did
// exactly that to SigLintFindings, whose own fixture comment calls it the single
// most important measured zero in the table, and build, vet, the full test run
// and golangci-lint all stayed green. Opting a signal out now has to be a diff
// against this map with a written reason, which a reviewer sees.
var zeroUnreachable = map[delta.SignalID]string{}

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
			// Nothing suppressed, which is the state a rising count is measured
			// against.
			measuredZero:    lintRunMarker("lint", 4, 1, 0),
			absent:          cov("m/a", 5, 10),
			zeroIsReachable: true,
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

// lintRunMarker is what the SARIF parser stores for one lint step: all three
// keys together, scoped by the step's name, written even when every count is
// zero. Scoped rather than repo-level because a polyglot repo runs two linters
// as two steps and they have to sum rather than collide.
func lintRunMarker(step string, findings, errs, suppressed int) []store.Metric {
	return []store.Metric{
		{Key: collect.KeyLintFindings, Scope: step, Value: float64(findings)},
		{Key: collect.KeyLintErrors, Scope: step, Value: float64(errs)},
		{Key: collect.KeyLintSuppressed, Scope: step, Value: float64(suppressed)},
	}
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
