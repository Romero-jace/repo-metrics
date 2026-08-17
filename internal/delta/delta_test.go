package delta_test

import (
	"math"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

func cov(pkg string, covered, total int) []store.Metric {
	return []store.Metric{
		{Key: collect.KeyCoveredStmts, Scope: pkg, Value: float64(covered)},
		{Key: collect.KeyTotalStmts, Scope: pkg, Value: float64(total)},
	}
}

func snap(env string) *store.Snapshot {
	return &store.Snapshot{Env: env, Status: store.StatusOK}
}

func opts() delta.Options {
	return delta.Options{
		Window:        7 * 24 * time.Hour,
		MinStatements: 20,
		MinRepoDelta:  0.5,
		MaxCulprits:   5,
	}
}

func one(t *testing.T, in delta.Input, o delta.Options) delta.RepoDelta {
	t.Helper()
	rep := delta.Compute([]delta.Input{in}, o, time.Now())
	if len(rep.Repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(rep.Repos))
	}
	return rep.Repos[0]
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// Repo coverage is sum(covered)/sum(total). It is emphatically not the mean of
// the per-package percentages, and the difference is large whenever package
// sizes differ, which is always.
func TestRepoCoverageIsWeightedNotAveraged(t *testing.T) {
	metrics := append(cov("m/big", 0, 990), cov("m/tiny", 10, 10)...)

	got := one(t, delta.Input{Repo: store.Repo{Name: "svc"}, Head: snap("go1.26"), HeadMetrics: metrics}, opts())

	if !approx(got.HeadCoverage.Pct(), 1.0) {
		t.Errorf("coverage: got %.2f%%, want 1.00%%", got.HeadCoverage.Pct())
	}
	// The naive average of 0%% and 100%% would be 50%%, which is off by a
	// factor of fifty.
	if approx(got.HeadCoverage.Pct(), 50) {
		t.Error("coverage was averaged across packages instead of weighted by size")
	}
}

// The core ranking claim. Two packages move: a large one that actually caused
// the repo's drop, and a tiny one that posts a bigger percentage swing while
// moving the repo by almost nothing. The large one must rank first.
func TestCulpritsRankByContributionNotPercentageSwing(t *testing.T) {
	head := append(cov("m/big", 500, 1000), cov("m/tiny", 3, 3)...)
	base := append(cov("m/big", 900, 1000), cov("m/tiny", 0, 3)...)

	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: head,
		Base: snap("go1.26"), BaseMetrics: base,
	}, delta.Options{MinStatements: 1, MinRepoDelta: 0.5, MaxCulprits: 5})

	if len(got.Culprits) != 2 {
		t.Fatalf("want 2 culprits, got %d: %+v", len(got.Culprits), got.Culprits)
	}

	// m/tiny swung a full 100 points on its own; m/big only swung 40.
	// Contribution has to disagree with that ordering.
	if got.Culprits[0].Scope != "m/big" {
		t.Errorf("top culprit: got %q, want m/big. Ranking fell back to per-package percentage swing.",
			got.Culprits[0].Scope)
	}
	if !approx(got.Culprits[0].Contribution, -39.88) {
		t.Errorf("m/big contribution: got %.2f, want about -39.88", got.Culprits[0].Contribution)
	}
	if !approx(got.Culprits[1].Contribution, 0.30) {
		t.Errorf("m/tiny contribution: got %.2f, want about 0.30", got.Culprits[1].Contribution)
	}
	if !approx(got.Culprits[1].PointChange(), 100) {
		t.Errorf("m/tiny point change: got %.2f, want 100 (the misleading number)", got.Culprits[1].PointChange())
	}

	// Contributions decompose the repo's move, which is what makes them
	// trustworthy to quote in a sentence.
	sum := got.Culprits[0].Contribution + got.Culprits[1].Contribution
	if !approx(sum, covChange(got)) {
		t.Errorf("contributions sum to %.2f but the repo moved %.2f", sum, covChange(got))
	}
}

// A three-statement package swinging from nothing to everything is not news,
// and letting it into the ranking pushes out the package that matters.
func TestNoiseFloorExcludesTinyPackages(t *testing.T) {
	head := append(cov("m/big", 500, 1000), cov("m/tiny", 3, 3)...)
	base := append(cov("m/big", 900, 1000), cov("m/tiny", 0, 3)...)

	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: head,
		Base: snap("go1.26"), BaseMetrics: base,
	}, opts()) // MinStatements 20

	if len(got.Culprits) != 1 {
		t.Fatalf("want only the big package, got %+v", got.Culprits)
	}
	if got.Culprits[0].Scope != "m/big" {
		t.Errorf("got %q", got.Culprits[0].Scope)
	}
}

// Without an earlier snapshot there is no delta. Reporting one anyway is how a
// tool loses trust on its very first run.
func TestNoBaselineProducesNoDelta(t *testing.T) {
	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: cov("m/a", 5, 10),
	}, opts())

	if got.HasBaseline {
		t.Error("HasBaseline set with no baseline snapshot")
	}
	if len(got.Culprits) != 0 {
		t.Errorf("culprits invented without a baseline: %+v", got.Culprits)
	}
	if covChange(got) != got.HeadCoverage.Pct() {
		// Base is a zero Coverage, so this is arithmetic, not a claim. The
		// report is responsible for not printing it.
		t.Logf("CoverageChange with no baseline is %.2f and must not be rendered", covChange(got))
	}
}

// A deleted package is not a coverage regression, and a new one is not an
// improvement. Both are report lines.
func TestAddedAndRemovedPackagesAreClassified(t *testing.T) {
	head := append(cov("m/kept", 50, 100), cov("m/new", 40, 100)...)
	base := append(cov("m/kept", 50, 100), cov("m/gone", 90, 100)...)

	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: head,
		Base: snap("go1.26"), BaseMetrics: base,
	}, opts())

	if len(got.Added) != 1 || got.Added[0] != "m/new" {
		t.Errorf("Added: got %v, want [m/new]", got.Added)
	}
	if len(got.Removed) != 1 || got.Removed[0] != "m/gone" {
		t.Errorf("Removed: got %v, want [m/gone]", got.Removed)
	}

	for _, c := range got.Culprits {
		switch c.Scope {
		case "m/new":
			if c.State != delta.StateAdded {
				t.Errorf("m/new state: got %q, want added", c.State)
			}
		case "m/gone":
			if c.State != delta.StateRemoved {
				t.Errorf("m/gone state: got %q, want removed", c.State)
			}
		}
	}
}

// The specific embarrassment this guards: a deleted package rendering as a
// hundred-point coverage collapse.
func TestRemovedPackageIsNotAHundredPointDrop(t *testing.T) {
	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: cov("m/kept", 50, 100),
		Base: snap("go1.26"), BaseMetrics: append(cov("m/kept", 50, 100), cov("m/gone", 100, 100)...),
	}, opts())

	for _, c := range got.Culprits {
		if c.Scope == "m/gone" && c.PointChange() <= -99 {
			// The point change is arithmetically -100; what matters is that
			// it is labeled removed so the report never calls it a drop.
			if c.State != delta.StateRemoved {
				t.Error("a deleted package was reported as a coverage regression")
			}
		}
	}
	if len(got.Removed) != 1 {
		t.Errorf("Removed: got %v", got.Removed)
	}
}

func TestMoverThreshold(t *testing.T) {
	cases := []struct {
		name         string
		head, base   []store.Metric
		headT, baseT int
		wantIsMover  bool
	}{
		{"big coverage move", cov("m/a", 500, 1000), cov("m/a", 900, 1000), 0, 0, true},
		{"move below the floor", cov("m/a", 999, 1000), cov("m/a", 1000, 1000), 0, 0, false},
		{"flat coverage, test count changed", cov("m/a", 500, 1000), cov("m/a", 500, 1000), 10, 9, true},
		{"nothing moved", cov("m/a", 500, 1000), cov("m/a", 500, 1000), 10, 10, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := append([]store.Metric{}, tc.head...)
			base := append([]store.Metric{}, tc.base...)
			// Both sides get the repo-level pkg.without_tests marker the
			// collector always emits alongside real counts. Without it these
			// fixtures describe output the collector cannot produce, and a
			// test-count delta is correctly ignored as unmeasured.
			if tc.headT > 0 {
				head = append(head,
					store.Metric{Key: collect.KeyTestCount, Scope: "m/a", Value: float64(tc.headT)},
					store.Metric{Key: collect.KeyPkgWithoutTest, Value: 0})
			}
			if tc.baseT > 0 {
				base = append(base,
					store.Metric{Key: collect.KeyTestCount, Scope: "m/a", Value: float64(tc.baseT)},
					store.Metric{Key: collect.KeyPkgWithoutTest, Value: 0})
			}

			got := one(t, delta.Input{
				Repo: store.Repo{Name: "svc"},
				Head: snap("go1.26"), HeadMetrics: head,
				Base: snap("go1.26"), BaseMetrics: base,
			}, opts())

			if got.IsMover != tc.wantIsMover {
				t.Errorf("IsMover: got %v, want %v (coverage moved %.2f, tests moved %v)",
					got.IsMover, tc.wantIsMover, covChange(got), testChange(got))
			}
		})
	}
}

// Snapshots taken under different toolchains differ for reasons that have
// nothing to do with the code, so the difference must be labeled.
func TestEnvironmentChangeIsFlagged(t *testing.T) {
	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go=go1.26.5;gowork=off"), HeadMetrics: cov("m/a", 500, 1000),
		Base: snap("go=go1.26.5;gowork=on"), BaseMetrics: cov("m/a", 900, 1000),
	}, opts())

	if !got.EnvChanged {
		t.Error("a toolchain change between snapshots was not flagged")
	}
	if got.EnvUnknown {
		t.Error("two identified toolchains were reported as unidentified, which would hide a real change behind a vaguer warning")
	}
}

// Two snapshots that both failed to identify a toolchain must not read as having
// been taken under the same one.
//
// This is the bug the unidentified sentinel exists for, and it is the shape this
// whole tool is built against: the placeholder is a string like any other, so
// comparing it answered "unchanged" for exactly the repos where nothing was
// watching. Every non-Go repo recorded that placeholder on every run, so the
// toolchain warning could never fire for the repos most likely to have their
// runtime upgraded under them.
func TestTwoUnidentifiedToolchainsAreNotReportedAsUnchanged(t *testing.T) {
	for _, env := range []string{"unidentified", "go=unknown", ""} {
		got := one(t, delta.Input{
			Repo: store.Repo{Name: "svc"},
			Head: snap(env), HeadMetrics: cov("m/a", 500, 1000),
			Base: snap(env), BaseMetrics: cov("m/a", 900, 1000),
		}, opts())

		if !got.EnvUnknown {
			t.Errorf("env %q: two snapshots that established no toolchain were not flagged as unidentified", env)
		}
		// Not merely "also true". EnvChanged asserts the two were demonstrably
		// different, which nothing here established either, and a report saying so
		// would be as wrong as one saying nothing.
		if got.EnvChanged {
			t.Errorf("env %q: reported as a toolchain CHANGE, which nothing measured", env)
		}
	}

	// The control: one identified side and one not is still unidentified, because
	// the comparison needs both. Without it, a check that only looked at the head
	// would pass everything above.
	half := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go=go1.26.5;gowork=off"), HeadMetrics: cov("m/a", 500, 1000),
		Base: snap("unidentified"), BaseMetrics: cov("m/a", 900, 1000),
	}, opts())
	if !half.EnvUnknown {
		t.Error("a baseline that established no toolchain was compared against an identified head as though the pair were comparable")
	}
	if half.EnvChanged {
		t.Error("an unidentified baseline was reported as a toolchain change; the two strings differ but nothing about the toolchains does")
	}
}

func TestCulpritsAreCappedAndSorted(t *testing.T) {
	var head, base []store.Metric
	for _, p := range []struct {
		name    string
		covered int
	}{
		{"m/a", 100}, {"m/b", 200}, {"m/c", 300},
		{"m/d", 400}, {"m/e", 500}, {"m/f", 600},
	} {
		head = append(head, cov(p.name, p.covered, 1000)...)
		base = append(base, cov(p.name, 1000, 1000)...)
	}

	o := opts()
	o.MaxCulprits = 3

	got := one(t, delta.Input{
		Repo: store.Repo{Name: "svc"},
		Head: snap("go1.26"), HeadMetrics: head,
		Base: snap("go1.26"), BaseMetrics: base,
	}, o)

	if len(got.Culprits) != 3 {
		t.Fatalf("want 3 culprits, got %d", len(got.Culprits))
	}
	for i := 1; i < len(got.Culprits); i++ {
		prev := math.Abs(got.Culprits[i-1].Contribution)
		cur := math.Abs(got.Culprits[i].Contribution)
		if cur > prev {
			t.Errorf("culprits not sorted by absolute contribution: %v", got.Culprits)
		}
	}
	// m/a fell furthest, so it must lead.
	if got.Culprits[0].Scope != "m/a" {
		t.Errorf("top culprit: got %q, want m/a", got.Culprits[0].Scope)
	}
}

func TestMoversAndProblemsSelection(t *testing.T) {
	failing := &store.Snapshot{Env: "go1.26", Status: store.StatusFailed}

	rep := delta.Compute([]delta.Input{
		{
			Repo: store.Repo{Name: "quiet"},
			Head: snap("go1.26"), HeadMetrics: cov("m/a", 500, 1000),
			Base: snap("go1.26"), BaseMetrics: cov("m/a", 500, 1000),
		},
		{
			Repo: store.Repo{Name: "moved"},
			Head: snap("go1.26"), HeadMetrics: cov("m/a", 500, 1000),
			Base: snap("go1.26"), BaseMetrics: cov("m/a", 900, 1000),
		},
		{Repo: store.Repo{Name: "broken"}, Head: failing},
	}, opts(), time.Now())

	movers := rep.Movers()
	if len(movers) != 1 || movers[0].Repo.Name != "moved" {
		t.Errorf("Movers: got %v", names(movers))
	}
	problems := rep.Problems()
	if len(problems) != 1 || problems[0].Repo.Name != "broken" {
		t.Errorf("Problems: got %v", names(problems))
	}
	// Repos come back sorted so report output is deterministic.
	if rep.Repos[0].Repo.Name != "broken" {
		t.Errorf("repos not sorted by name: %v", names(rep.Repos))
	}
}

func names(rs []delta.RepoDelta) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Repo.Name)
	}
	return out
}

func TestZeroTotalCoverageIsZeroNotNaN(t *testing.T) {
	c := delta.Coverage{}
	if got := c.Pct(); got != 0 || math.IsNaN(got) {
		t.Errorf("Pct on empty coverage: got %v, want 0", got)
	}
}

// The signal layer replaced a field and a method per signal, so these read one
// signal the way production reads it. They are helpers rather than restored
// methods because the point of the refactor is that there is no per-signal
// method left to restore.
func covChange(d delta.RepoDelta) float64 {
	c, _ := d.Signal(delta.SigCoverage).Change.Delta()
	return c
}

func testChange(d delta.RepoDelta) float64 {
	c, _ := d.Signal(delta.SigTests).Change.Delta()
	return c
}

func testMeaningful(d delta.RepoDelta) bool {
	return d.Signal(delta.SigTests).Change.Meaningful()
}

// signalValue returns a signal's measurement and whether there is one, so a
// test asserting on absence says so rather than comparing against zero.
func signalValue(d delta.RepoDelta, id delta.SignalID) (float64, bool) {
	return d.Signal(id).Head.Value()
}

func measuredSignal(d delta.RepoDelta, id delta.SignalID) bool {
	return d.Signal(id).Head.IsMeasured()
}
