package delta_test

import (
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// lines builds the row pair an LCOV tracefile stores for one source file.
func lines(file string, covered, total int) []store.Metric {
	return []store.Metric{
		{Key: collect.KeyCoveredLines, Scope: file, Value: float64(covered)},
		{Key: collect.KeyTotalLines, Scope: file, Value: float64(total)},
	}
}

// Both coverage signals answer to one config knob, which is what min_repo_delta
// has always been documented to be.
//
// It was not true. The comparison site tested `sig.ID == SigCoverage`, so when
// line coverage arrived it fell through to the generic floor of 1.0 and needed
// ten times the movement of statement coverage to nominate a repo at the
// documented min_repo_delta of 0.1. The registry entry carried a comment saying
// one field governed both, which is why it survived review: the comment asserted
// the behavior the code did not have.
//
// A 0.4pp move is the discriminating case. It clears a 0.1 config floor and
// loses to the 1.0 fall-through, so this test fails against the old code for the
// line signal and passes for the statement signal — which is the asymmetry
// itself, not merely its consequence.
func TestOneConfigFloorGovernsBothCoverageSignals(t *testing.T) {
	o := delta.Options{
		Window:        7 * 24 * time.Hour,
		MinStatements: 1,
		MinRepoDelta:  0.1,
		MaxCulprits:   5,
	}

	// 996/1000 = 99.6%, against 1000/1000 = 100%. A 0.4pp drop.
	cases := []struct {
		name       string
		head, base []store.Metric
		wantSignal delta.SignalID
	}{
		{"statements", cov("m/pkg", 996, 1000), cov("m/pkg", 1000, 1000), delta.SigCoverage},
		{"lines", lines("src/a.ts", 996, 1000), lines("src/a.ts", 1000, 1000), delta.SigCoverageLines},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := delta.Compute([]delta.Input{{
				Repo: store.Repo{Name: "svc"},
				Head: snap("go1.26"), HeadMetrics: c.head,
				Base: snap("go1.26"), BaseMetrics: c.base,
			}}, o, time.Now())

			if len(rep.Movers()) != 1 {
				t.Fatalf("a 0.4pp drop did not make the repo a mover at min_repo_delta 0.1, so this signal is being held to a floor the config never set. Movers: %d", len(rep.Movers()))
			}

			change, ok := rep.Repos[0].Signal(c.wantSignal).Change.Delta()
			if !ok {
				t.Fatalf("%s reported no comparable change, so the fixture is not exercising the case this test names", c.wantSignal)
			}
			if change > -0.3 || change < -0.5 {
				t.Errorf("%s moved %.2fpp, want about -0.40pp", c.wantSignal, change)
			}
		})
	}
}

// A nominating signal must have decided its own floor.
//
// Nomination reads MinMove, and a zero there falls through to a generic floor of
// 1. For a count that is a sensible default: one more failing test is a move. For
// a percentage it silently imposes a full point, which is a large move to apply
// without anyone choosing it, and it is exactly how the line-coverage bug above
// went unnoticed — the entry said MinMove: 0 and read as "no floor" rather than
// as "the default floor".
//
// So the requirement is that the decision be explicit, not that it be any
// particular value: either MinMove is set, or the signal declares that the config
// supplies its floor. Both zero means nobody chose.
func TestEveryNominatingSignalDeclaresItsFloor(t *testing.T) {
	for _, sig := range delta.Signals() {
		if !sig.Nominates {
			// The floor is unreachable for these: the comparison short-circuits
			// on Nominates before it ever reads MinMove.
			continue
		}

		if sig.MinMove == 0 && !sig.FloorFromMinRepoDelta {
			t.Errorf("signal %q nominates with no floor of its own, so it silently inherits the generic floor of 1 in whatever unit it happens to be measured in. Set MinMove, or set FloorFromMinRepoDelta if min_repo_delta is meant to govern it", sig.ID)
		}
		if sig.MinMove != 0 && sig.FloorFromMinRepoDelta {
			t.Errorf("signal %q sets both MinMove and FloorFromMinRepoDelta, and the config wins, so the MinMove value is dead and reads as though it were in force", sig.ID)
		}
	}
}

// Every percent-valued nominating signal takes the config's knob.
//
// Narrower than the test above and worth asserting separately: min_repo_delta is
// documented as the coverage floor, singular, and a reader setting it to 0.1
// expects it to reach every coverage number in the report. A future percent
// signal registered with its own hardcoded MinMove would satisfy the
// declares-its-floor rule while quietly reintroducing exactly the split this
// change closed.
func TestEveryPercentSignalTakesItsFloorFromTheConfig(t *testing.T) {
	for _, sig := range delta.Signals() {
		if !sig.Nominates || sig.Unit != delta.UnitPercent {
			continue
		}
		if !sig.FloorFromMinRepoDelta {
			t.Errorf("signal %q is a nominating percentage with its own floor of %v, so min_repo_delta does not govern it and two coverage columns in one report answer to two different thresholds. That split is the bug this test exists to prevent", sig.ID, sig.MinMove)
		}
	}
}
