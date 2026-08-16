package delta_test

import (
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// withTests marks a snapshot's metrics as having come from a parsed test
// stream, the way the collector does: the repo-level pkg.without_tests metric
// is emitted unconditionally whenever stdout was parsed, including at zero.
func withTests(metrics []store.Metric, count, withoutTests int) []store.Metric {
	out := append([]store.Metric{}, metrics...)
	out = append(out,
		store.Metric{Key: collect.KeyTestCount, Scope: "m/a", Value: float64(count)},
		store.Metric{Key: collect.KeyPkgWithoutTest, Scope: "", Value: float64(withoutTests)},
	)
	return out
}

// A repo collected in ingest mode never has its tests counted. Reporting that
// as "tests 0" tells a reader with seventy test files something flatly false,
// which is the same silent wrong answer as showing an uncollected repo at 0%
// coverage.
func TestUnmeasuredTestsAreDistinctFromZeroTests(t *testing.T) {
	unmeasured := one(t, delta.Input{
		Repo: store.Repo{Name: "ingest-only"},
		Head: snap("go1.26"), HeadMetrics: cov("m/a", 500, 1000),
	}, opts())

	if measuredSignal(unmeasured, delta.SigTests) {
		t.Error("HasTestData is true for a snapshot whose stdout was never parsed")
	}

	// A repo that really does have zero tests still parsed a stream, so its
	// zero is a measurement and must not be confused with the case above.
	measuredZero := one(t, delta.Input{
		Repo: store.Repo{Name: "genuinely-empty"},
		Head: snap("go1.26"), HeadMetrics: withTests(cov("m/a", 0, 1000), 0, 1),
	}, opts())

	if !measuredSignal(measuredZero, delta.SigTests) {
		t.Error("HasTestData is false for a repo that parsed a stream and found no tests")
	}
	if value, _ := signalValue(measuredZero, delta.SigTests); value != 0 {
		t.Errorf("test count: got %v, want 0", value)
	}
}

// Turning on stdout_format between two runs must not post the repo's entire
// existing suite as this week's growth.
func TestTestDeltaNeedsBothSidesMeasured(t *testing.T) {
	gained := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: withTests(cov("m/a", 500, 1000), 400, 0),
		Base: snap("go1.26"), BaseMetrics: cov("m/a", 500, 1000), // no test data
	}, opts())

	if testMeaningful(gained) {
		t.Error("test delta treated as meaningful when the baseline never measured tests")
	}
	if gained.IsMover {
		t.Errorf("repo became a mover purely from test data appearing: coverage moved %.2f, tests %v",
			covChange(gained), testChange(gained))
	}

	both := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: withTests(cov("m/a", 500, 1000), 400, 0),
		Base: snap("go1.26"), BaseMetrics: withTests(cov("m/a", 500, 1000), 390, 0),
	}, opts())

	if !testMeaningful(both) {
		t.Error("test delta not meaningful when both sides measured tests")
	}
	if testChange(both) != 10 {
		t.Errorf("test change: got %v, want 10", testChange(both))
	}
	if !both.IsMover {
		t.Error("a real test-count change should make a repo a mover")
	}
}

func TestHasTestDataIgnoresTheValueAndReadsPresence(t *testing.T) {
	// Zero packages without tests is a perfectly normal measurement, so the
	// marker cannot be keyed on the value being non-zero.
	got := delta.Compute([]delta.Input{{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"),
		HeadMetrics: append(cov("m/a", 500, 1000),
			store.Metric{Key: collect.KeyPkgWithoutTest, Scope: "", Value: 0}),
	}}, opts(), time.Now())

	if !measuredSignal(got.Repos[0], delta.SigTests) {
		t.Error("a pkg.without_tests metric of zero was read as no test data at all")
	}
}
