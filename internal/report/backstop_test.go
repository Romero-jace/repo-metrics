package report_test

import (
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// Build drops a mover that measured nothing, independently of whether
// delta.Compute would ever have marked it one.
//
// Two layers guard this now. delta.Compute asks CoverageChangeMeaningful before
// setting IsMover, and Build asks HasCoverageData before publishing a mover.
// Since the first guard landed, no fixture driven through Compute reaches the
// second, which means the second would sit unexercised and quietly rot until
// someone deleted it as dead code.
//
// So this constructs the RepoDelta directly rather than going through Compute.
// That is the point: it pins the backstop against a hand-built input Compute
// does not currently produce, which is exactly the input a future change to
// IsMover could start producing again.
func TestBuildDropsAMoverThatMeasuredNothing(t *testing.T) {
	head := &store.Snapshot{ID: 1, RepoID: 1, CollectedAt: fixedNow(), Status: store.StatusOK}

	rep := delta.Report{
		GeneratedAt: fixedNow(),
		Window:      7 * 24 * time.Hour,
		Repos: []delta.RepoDelta{
			{
				Repo: store.Repo{ID: 1, Name: "ungrounded"},
				Head: head,
				// The shape that matters: something upstream called it a mover
				// while nothing was measured to move. The head signal map is
				// deliberately empty, which is what an unmeasured signal looks
				// like now, and is also what a hand-built literal produces by
				// default. Failing closed on that default is the point.
				IsMover:      true,
				MovedBy:      []delta.SignalID{delta.SigCoverage},
				HasBaseline:  true,
				BaseSignals:  map[delta.SignalID]delta.Measurement{delta.SigCoverage: delta.Measured(72)},
				BaseCoverage: delta.Coverage{Covered: 72, Total: 100},
			},
			{
				Repo:         store.Repo{ID: 2, Name: "grounded"},
				Head:         &store.Snapshot{ID: 2, RepoID: 2, CollectedAt: fixedNow(), Status: store.StatusOK},
				IsMover:      true,
				MovedBy:      []delta.SignalID{delta.SigCoverage},
				HasBaseline:  true,
				HeadSignals:  map[delta.SignalID]delta.Measurement{delta.SigCoverage: delta.Measured(40)},
				BaseSignals:  map[delta.SignalID]delta.Measurement{delta.SigCoverage: delta.Measured(72)},
				HeadCoverage: delta.Coverage{Covered: 40, Total: 100},
				BaseCoverage: delta.Coverage{Covered: 72, Total: 100},
			},
		},
	}

	var got []string
	for _, m := range report.Build(rep).Movers {
		got = append(got, m.Name)
	}

	if len(got) != 1 || got[0] != "grounded" {
		t.Errorf("movers: got %v, want [grounded]. A repo that measured no coverage led "+
			"the report on a move computed from a baseline against nothing, which is the "+
			"fabricated cliff reached through the selection path instead of the rendering one.", got)
	}
}

// The mirror of the above, one layer down: delta.Compute must not mark such a
// repo a mover in the first place. Both halves of IsMover need their own guard,
// and for a long time only the test half had one.
func TestComputeDoesNotMarkAnUnmeasuredHeadAMover(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo: store.Repo{ID: 1, Name: "hollow"},
		// Head parsed a test stream but stored no coverage at all.
		Head:        snap(11, 1, "go1.26.5", store.StatusOK, ""),
		HeadMetrics: testStream(0),
		Base:        snap(10, 1, "go1.26.5", store.StatusOK, ""),
		BaseMetrics: metrics(cov(pkgAlpha, 72, 100), testStream(0)),
	}}, options(), fixedNow())

	if got := rep.Repos[0].IsMover; got {
		t.Errorf("IsMover = %v for a head that measured no coverage. CoverageChange is "+
			"headPct minus zero, so it clears any threshold on a number that is entirely "+
			"an artifact of the baseline being unmeasured.", got)
	}
	if len(rep.Movers()) != 0 {
		t.Errorf("Movers: got %d, want none", len(rep.Movers()))
	}
}
