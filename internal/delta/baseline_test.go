package delta_test

import (
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

func snapAt(t time.Time) *store.Snapshot {
	return &store.Snapshot{Env: "go1.26", Status: store.StatusOK, CollectedAt: t}
}

// slid builds a repo whose coverage halved, with the two snapshots however far
// apart the caller asks.
func slid(gap time.Duration) delta.Input {
	head := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	return delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snapAt(head),
		HeadMetrics: cov("m/a", 40, 100),
		Base:        snapAt(head.Add(-gap)),
		BaseMetrics: cov("m/a", 80, 100),
	}
}

// The staleness line is a judgment call, so it is pinned here rather than left
// to be rediscovered from behavior.
//
// Baseline selection guarantees the gap is at least the window and says nothing
// about the upper end, so there is no boundary to derive: this is a decision.
// Twice the window was tried first and is too tight. A weekly cron that misses
// one run leaves a fortnight-old baseline, which is common and harmless, and it
// landed exactly on the boundary and tipped over by milliseconds.
func TestWhenABaselineCountsAsLapsed(t *testing.T) {
	const week = 7 * 24 * time.Hour

	for _, tc := range []struct {
		name      string
		gap       time.Duration
		wantStale bool
	}{
		{"the week it asked for", week, false},
		{"one collection missed", 2 * week, false},
		{"a fortnight and a bit", 2*week + time.Hour, false},
		{"exactly three windows", 3 * week, false},
		{"two collections missed", 3*week + time.Hour, true},
		{"a whole quarter", 13 * week, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := opts()
			o.Window = week
			d := one(t, slid(tc.gap), o)

			if d.BaselineStale != tc.wantStale {
				t.Errorf("BaselineStale = %v for a gap of %s against a %s window, want %v",
					d.BaselineStale, tc.gap, week, tc.wantStale)
			}
			if d.BaselineAge != tc.gap {
				t.Errorf("BaselineAge = %s, want %s", d.BaselineAge, tc.gap)
			}
			// Whatever the verdict, the comparison itself survives. Staleness
			// decides whether a repo leads the report, never whether its numbers
			// are published: a two-month-old baseline is still the best answer
			// available, and saying so plainly beats saying nothing.
			if change, meaningful := d.Signal(delta.SigCoverage).Change.Delta(); !meaningful || change != -40 {
				t.Errorf("coverage change = %v (meaningful=%v), want -40 either way", change, meaningful)
			}
			if d.IsMover == tc.wantStale {
				t.Errorf("IsMover = %v for a stale=%v baseline", d.IsMover, tc.wantStale)
			}
		})
	}
}

// A repo with no baseline has no age to report, and must not read as one that
// was collected at the zero time and is therefore two thousand years stale.
func TestNoBaselineIsNotAnInfinitelyOldOne(t *testing.T) {
	d := one(t, delta.Input{
		Repo:        store.Repo{Name: "svc"},
		Head:        snapAt(time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)),
		HeadMetrics: cov("m/a", 40, 100),
	}, opts())

	if d.HasBaseline {
		t.Fatal("there is no baseline here")
	}
	if d.BaselineAge != 0 {
		t.Errorf("BaselineAge = %s, want zero when there is nothing to measure from", d.BaselineAge)
	}
	if d.BaselineStale {
		t.Error("a repo with no baseline is reported as having a lapsed one")
	}
}
