package delta_test

import (
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// testFamily is the five signals that come out of one parsed test stream. They
// share a marker, and a build failure makes all five a floor rather than a
// total, so they are asserted together.
var testFamily = []delta.SignalID{
	delta.SigTests,
	delta.SigTestFailures,
	delta.SigTestSkipped,
	delta.SigUntestedPackages,
	delta.SigTestTime,
}

// testRun is what a collector stores for a repo whose stream it parsed: per
// package counts, the marker, and the count of packages that would not build.
func testRun(pkg string, count, failed, skipped, ms, withoutTests, brokenBuilds int) []store.Metric {
	return []store.Metric{
		{Key: collect.KeyTestCount, Scope: pkg, Value: float64(count)},
		{Key: collect.KeyTestFailed, Scope: pkg, Value: float64(failed)},
		{Key: collect.KeyTestSkipped, Scope: pkg, Value: float64(skipped)},
		{Key: collect.KeyTestDurationMS, Scope: pkg, Value: float64(ms)},
		{Key: collect.KeyPkgWithoutTest, Value: float64(withoutTests)},
		{Key: collect.KeyTestBuildFailed, Value: float64(brokenBuilds)},
	}
}

// A package whose test binary would not build takes its test count with it, so
// the repo's total is short by an unknown amount and cannot be compared against
// a week when everything built.
//
// This shipped. Rename a function, forget one test file, and the report
// announced "Tests 268, -8 on the week, worse" with no failing test anywhere,
// which reads as somebody having deleted them.
func TestABrokenBuildIsNotTestsDisappearing(t *testing.T) {
	d := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"),
		// One package builds and reports 9 tests. A second would not compile, so
		// it contributes nothing at all and is counted as a broken build.
		HeadMetrics: testRun("m/alpha", 9, 0, 0, 40, 0, 1),
		Base:        snap("go1.26"),
		// Last week both built, and between them they had 17.
		BaseMetrics: testRun("m/alpha", 17, 0, 0, 90, 0, 0),
	}, opts())

	for _, id := range testFamily {
		sd := d.Signal(id)
		if _, measured := sd.Head.Value(); !measured {
			t.Errorf("%s: the packages that did build are a real measurement and must still be published", id)
		}
		if change, meaningful := sd.Change.Delta(); meaningful {
			t.Errorf("%s: published a change of %v against a week when every package built. A build error is not tests being deleted.",
				id, change)
		}
	}
	if d.IsMover {
		t.Errorf("a repo whose build broke leads the report, MovedBy=%v", d.MovedBy)
	}
}

// The mirror, and the one that matters for anybody fixing a build: the week the
// package compiles again, its tests all arrive at once and are not this week's
// improvement.
func TestAFixedBuildIsNotTestsAppearing(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snap("go1.26"),
		HeadMetrics: testRun("m/alpha", 17, 0, 0, 90, 0, 0),
		Base:        snap("go1.26"),
		BaseMetrics: testRun("m/alpha", 9, 0, 0, 40, 0, 1),
	}, opts())

	if change, meaningful := d.Signal(delta.SigTests).Change.Delta(); meaningful {
		t.Errorf("fixing a build published +%v as new tests", change)
	}
}

// The anti-vacuity control. Without it, refusing every comparison would satisfy
// both tests above.
func TestAWorkingBuildStillCompares(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snap("go1.26"),
		HeadMetrics: testRun("m/alpha", 20, 1, 2, 90, 3, 0),
		Base:        snap("go1.26"),
		BaseMetrics: testRun("m/alpha", 17, 0, 2, 80, 3, 0),
	}, opts())

	change, meaningful := d.Signal(delta.SigTests).Change.Delta()
	if !meaningful {
		t.Fatal("two weeks where everything built must be comparable")
	}
	if change != 3 {
		t.Errorf("tests change: got %v, want 3", change)
	}
	if !d.IsMover {
		t.Error("a repo that really did gain three tests should lead the report")
	}
}

// Every snapshot taken before this key existed has no row for it, which reads as
// zero and compares normally.
//
// That is the right answer rather than a convenient one. Nothing was checking
// for broken builds then, so refusing every comparison against older history
// would replace a rare wrong number with a permanent absence of numbers, and the
// database this was written against is full of such snapshots.
func TestHistoryFromBeforeThisKeyExistedStillCompares(t *testing.T) {
	old := []store.Metric{
		{Key: collect.KeyTestCount, Scope: "m/alpha", Value: 17},
		{Key: collect.KeyPkgWithoutTest, Value: 0},
	}

	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snap("go1.26"),
		HeadMetrics: testRun("m/alpha", 20, 0, 0, 90, 0, 0),
		Base:        snap("go1.26"),
		BaseMetrics: old,
	}, opts())

	change, meaningful := d.Signal(delta.SigTests).Change.Delta()
	if !meaningful {
		t.Fatal("a baseline predating the build-failure key stopped comparing, which retires the whole history")
	}
	if change != 3 {
		t.Errorf("tests change: got %v, want 3", change)
	}
}

// Coverage comes out of a different artifact and is unaffected: the packages
// that did build were still instrumented, and their profile is complete for
// them. Refusing it too would throw away a measurement nothing compromised.
func TestCoverageStillComparesThroughABrokenBuild(t *testing.T) {
	head := append(cov("m/alpha", 50, 100), testRun("m/alpha", 9, 0, 0, 40, 0, 1)...)
	base := append(cov("m/alpha", 40, 100), testRun("m/alpha", 17, 0, 0, 90, 0, 0)...)

	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snap("go1.26"),
		HeadMetrics: head,
		Base:        snap("go1.26"),
		BaseMetrics: base,
	}, opts())

	if _, meaningful := d.Signal(delta.SigCoverage).Change.Delta(); !meaningful {
		t.Error("coverage stopped comparing because a test binary would not link, which is a different artifact entirely")
	}
}
