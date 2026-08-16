package delta_test

import (
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// lintStep is what the SARIF parser stores for one lint step: findings filed
// under the step's own name, because a polyglot repo runs two linters and their
// rows have to sum rather than collide.
func lintStep(step string, findings, errs, suppressed int) []store.Metric {
	return []store.Metric{
		{Key: collect.KeyLintFindings, Scope: step, Value: float64(findings)},
		{Key: collect.KeyLintErrors, Scope: step, Value: float64(errs)},
		{Key: collect.KeyLintSuppressed, Scope: step, Value: float64(suppressed)},
	}
}

func timing(step string, ms float64) store.Metric {
	return store.Metric{Key: collect.KeySignalDurationMS, Scope: step, Value: ms}
}

func join(groups ...[]store.Metric) []store.Metric {
	var out []store.Metric
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// A signal summed across collection steps is only comparable when both sides
// summed the same steps.
//
// This shipped broken and was found by review. A repo running golangci-lint and
// eslint has its findings summed across both. The week eslint's step dies, its
// rows are dropped whole, the sum falls by everything eslint used to find, and
// the report announced that as an improvement and led with it. The linter
// crashing did not fix anything.
func TestADroppedStepIsNotAnImprovement(t *testing.T) {
	d := one(t, delta.Input{
		Repo: store.Repo{Name: "polyglot"},
		Head: snap("go1.26"),
		// Only golangci survived this week.
		HeadMetrics: lintStep("golangci", 12, 3, 0),
		Base:        snap("go1.26"),
		// Both linters ran a week ago.
		BaseMetrics: join(lintStep("golangci", 12, 3, 0), lintStep("eslint", 30, 5, 0)),
	}, opts())

	for _, id := range []delta.SignalID{delta.SigLintFindings, delta.SigLintErrors, delta.SigLintSuppressed} {
		sd := d.Signal(id)
		if _, measured := sd.Head.Value(); !measured {
			t.Errorf("%s: the surviving step's findings are a real measurement and must still be published", id)
		}
		if change, meaningful := sd.Change.Delta(); meaningful {
			t.Errorf("%s: published a change of %v across two different sets of linters. A crashed linter is not an improvement.",
				id, change)
		}
	}
	if d.IsMover {
		t.Errorf("a repo whose linter crashed leads the report, MovedBy=%v", d.MovedBy)
	}
}

// The mirror, and the more common one: switching a second linter ON posts its
// entire existing backlog as this week's regression, when no code changed.
func TestAnAddedStepIsNotARegression(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "grew"},
		Head:        snap("go1.26"),
		HeadMetrics: join(lintStep("golangci", 12, 3, 0), lintStep("eslint", 400, 40, 0)),
		Base:        snap("go1.26"),
		BaseMetrics: lintStep("golangci", 12, 3, 0),
	}, opts())

	if change, meaningful := d.Signal(delta.SigLintFindings).Change.Delta(); meaningful {
		t.Errorf("turning on a second linter published +%v as a regression", change)
	}
	if d.IsMover {
		t.Errorf("and it leads the report, MovedBy=%v", d.MovedBy)
	}
}

// The same rule on collection time, where the trigger is far broader: any repo
// with two or more command steps, one of which failed. A crashed test suite made
// collection look faster.
func TestADeadStepIsNotFasterCollection(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snap("go1.26"),
		HeadMetrics: []store.Metric{timing("coverage", 35000)},
		Base:        snap("go1.26"),
		BaseMetrics: []store.Metric{timing("coverage", 30000), timing("tests", 120000)},
	}, opts())

	if change, meaningful := d.Signal(delta.SigCollectTime).Change.Delta(); meaningful {
		t.Errorf("a step that never ran published %v as a change in collection time", change)
	}
}

// The rule must not fire when the steps are stable, or every multi-step repo
// would lose its deltas permanently. This is the anti-vacuity control: without
// it, returning not-comparable unconditionally would pass every test above.
func TestAStableStepSetStillCompares(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "steady"},
		Head:        snap("go1.26"),
		HeadMetrics: join(lintStep("golangci", 10, 2, 0), lintStep("eslint", 20, 4, 0)),
		Base:        snap("go1.26"),
		BaseMetrics: join(lintStep("golangci", 12, 3, 0), lintStep("eslint", 30, 5, 0)),
	}, opts())

	change, meaningful := d.Signal(delta.SigLintFindings).Change.Delta()
	if !meaningful {
		t.Fatal("two runs over the same two linters must be comparable")
	}
	// 12 plus 30 last week, 10 plus 20 this week.
	if change != -12 {
		t.Errorf("lint findings change: got %v, want -12, from 42 down to 30", change)
	}
	if !d.IsMover {
		t.Error("a repo whose findings really did fall by 12 should lead the report")
	}
}

// The fingerprint is over a SET, so the order rows arrive in cannot change it.
//
// Go randomizes map iteration, so without the sort two sides holding the same
// two linters would fingerprint differently on some runs and compare fine on
// others: a report that intermittently refuses a delta for no reason anyone
// could reproduce. The rows below are deliberately in opposite orders.
func TestTheScopeFingerprintDoesNotDependOnRowOrder(t *testing.T) {
	for i := range 20 {
		d := one(t, delta.Input{
			Repo:        store.Repo{Name: "svc"},
			Head:        snap("go1.26"),
			HeadMetrics: join(lintStep("aaa", 10, 1, 0), lintStep("zzz", 20, 2, 0)),
			Base:        snap("go1.26"),
			// Same two steps, written in the other order.
			BaseMetrics: join(lintStep("zzz", 20, 2, 0), lintStep("aaa", 12, 3, 0)),
		}, opts())

		if _, meaningful := d.Signal(delta.SigLintFindings).Change.Delta(); !meaningful {
			t.Fatalf("run %d: the same two steps in a different row order stopped comparing", i)
		}
	}
}

// Coverage is deliberately NOT under this rule, and that is the distinction the
// whole design turns on. Its scope names a package, which is part of the thing
// being measured, so a package appearing or vanishing is a real change with
// designed handling. Applying the rule there would refuse a delta every week
// anybody added a file.
func TestCoverageStillComparesAcrossAChangedPackageSet(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "grew"},
		Head:        snap("go1.26"),
		HeadMetrics: join(cov("m/a", 50, 100), cov("m/b", 50, 100)),
		Base:        snap("go1.26"),
		BaseMetrics: cov("m/a", 40, 100),
	}, opts())

	if _, meaningful := d.Signal(delta.SigCoverage).Change.Delta(); !meaningful {
		t.Error("coverage stopped comparing across a new package, which would refuse a delta any week a file was added")
	}
}
