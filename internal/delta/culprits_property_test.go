package delta_test

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// propSeeds is a fixed table rather than a fuzz corpus.
//
// testing.F would discover more shapes, but a corpus is a moving part that
// reproduces differently on every machine, and this repo has no fuzz precedent
// to hang one off. A fixed seed table fails identically everywhere, which is
// worth more here than discovery.
var propSeeds = []uint64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89}

// casesPerSeed is how many package sets each seed generates.
const casesPerSeed = 40

// twinPackage exists in both snapshots with identical numbers in every single
// generated case. Its contribution is zero by construction, which makes it the
// cheapest available check on a sign or index slip in the counterfactual.
const twinPackage = "example.com/g/twin"

// propOpts turns off every filter culprits applies.
//
// MinStatements 0 keeps small packages in, and MaxCulprits has to be large and
// positive: Compute rewrites a zero to DefaultMaxCulprits, and truncating the
// ranking would silently break the summation invariant below in a way that
// looks like a maths bug.
func propOpts() delta.Options {
	return delta.Options{
		Window:        7 * 24 * time.Hour,
		MinStatements: 0,
		MinRepoDelta:  0.5,
		MaxCulprits:   1000,
	}
}

// pkgCoverage is one package's statement counts on one side of the comparison.
type pkgCoverage struct {
	covered int
	total   int
}

type genCase struct {
	head map[string]pkgCoverage
	base map[string]pkgCoverage
	// totalsPreserved is true when every package appears on both sides with the
	// same total. Only then do all the counterfactual denominators collapse to
	// the repo total, and only then do the contributions sum to the repo move.
	totalsPreserved bool
}

// shape is a coarse fingerprint of a generated case. Counting distinct shapes
// is what tells a reader the generator is exploring rather than emitting one
// arrangement forty times, which would be a table test in costume.
func (c genCase) shape() string {
	var added, removed, zeroTotal int
	for name, h := range c.head {
		if _, ok := c.base[name]; !ok {
			added++
		}
		if h.total == 0 {
			zeroTotal++
		}
	}
	for name := range c.base {
		if _, ok := c.head[name]; !ok {
			removed++
		}
	}
	return fmt.Sprintf("pkgs=%d preserved=%v added=%d removed=%d zero=%d",
		len(c.head), c.totalsPreserved, added, removed, zeroTotal)
}

func (c genCase) metrics(side map[string]pkgCoverage) []store.Metric {
	out := make([]store.Metric, 0, len(side)*2)
	for name, cv := range side {
		out = append(out,
			store.Metric{Key: collect.KeyCoveredStmts, Scope: name, Value: float64(cv.covered)},
			store.Metric{Key: collect.KeyTotalStmts, Scope: name, Value: float64(cv.total)},
		)
	}
	return out
}

// generate builds one package set. Covered is always within [0, total] because
// that is what a real coverage profile can express, and generating impossible
// input would test arithmetic nobody can reach.
func generate(rng *rand.Rand) genCase {
	c := genCase{
		head:            map[string]pkgCoverage{},
		base:            map[string]pkgCoverage{},
		totalsPreserved: rng.IntN(2) == 0,
	}

	twin := pkgCoverage{total: 1 + rng.IntN(400)}
	twin.covered = rng.IntN(twin.total + 1)
	c.head[twinPackage] = twin
	c.base[twinPackage] = twin

	for i := range 1 + rng.IntN(6) {
		name := fmt.Sprintf("example.com/g/pkg%02d", i)

		if c.totalsPreserved {
			// Same package set, same size, only the covered count moves. This
			// branch is constructed rather than hoped for: leaving it to chance
			// would mean the strongest invariant here almost never ran.
			total := rng.IntN(500)
			c.head[name] = pkgCoverage{covered: coveredWithin(rng, total), total: total}
			c.base[name] = pkgCoverage{covered: coveredWithin(rng, total), total: total}
			continue
		}

		headTotal := rng.IntN(500)
		baseTotal := rng.IntN(500)
		switch rng.IntN(6) {
		case 0:
			// Added: head only.
			c.head[name] = pkgCoverage{covered: coveredWithin(rng, headTotal), total: headTotal}
		case 1:
			// Removed: base only.
			c.base[name] = pkgCoverage{covered: coveredWithin(rng, baseTotal), total: baseTotal}
		default:
			c.head[name] = pkgCoverage{covered: coveredWithin(rng, headTotal), total: headTotal}
			c.base[name] = pkgCoverage{covered: coveredWithin(rng, baseTotal), total: baseTotal}
		}
	}
	return c
}

func coveredWithin(rng *rand.Rand, total int) int {
	if total == 0 {
		return 0
	}
	return rng.IntN(total + 1)
}

// TestCulpritsHoldTheirInvariantsOverGeneratedInput is where a silent wrong
// number does the most damage: the culprit ranking is the part of the report
// that names a package as the cause of a move, and a sign or denominator slip
// there produces a plausible figure rather than a crash.
func TestCulpritsHoldTheirInvariantsOverGeneratedInput(t *testing.T) {
	shapes := map[string]int{}
	preserved := 0

	for _, seed := range propSeeds {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed*2+1))
			for i := range casesPerSeed {
				c := generate(rng)
				shapes[c.shape()]++
				if c.totalsPreserved {
					preserved++
				}
				// The shuffle draws from its own generator on purpose. Sharing
				// rng would let a change in how many numbers the production
				// code path consumes shift every later case, and the shape
				// census below would then be reporting on the code under test
				// rather than on the seed table.
				checkCase(t, fmt.Sprintf("seed %d case %d", seed, i), c,
					rand.New(rand.NewPCG(seed, uint64(i)+1)))
			}
		})
	}

	if preserved == 0 {
		t.Fatal("the generator produced no totals-preserved case, so the exact-summation invariant never ran")
	}
	t.Logf("generated %d cases across %d seeds in %d distinct shapes, %d of them totals-preserved",
		len(propSeeds)*casesPerSeed, len(propSeeds), len(shapes), preserved)
	if len(shapes) < 20 {
		t.Errorf("only %d distinct shapes generated; this is a table test wearing a costume", len(shapes))
	}
}

func checkCase(t *testing.T, label string, c genCase, rng *rand.Rand) {
	t.Helper()

	got := one(t, delta.Input{
		Repo:        store.Repo{Name: "generated"},
		Head:        snap("go1.26"),
		HeadMetrics: c.metrics(c.head),
		Base:        snap("go1.26"),
		BaseMetrics: c.metrics(c.base),
	}, propOpts())

	// Nothing here may be NaN or Inf. A zero total is a real possibility (a
	// package with no statements, a repo whose profile instrumented nothing)
	// and a division that slipped past its guard would surface downstream as a
	// percentage that formats as NaN%.
	for _, v := range []struct {
		name string
		f    float64
	}{
		{"head coverage", got.HeadCoverage.Pct()},
		{"base coverage", got.BaseCoverage.Pct()},
		{"coverage change", covChange(got)},
	} {
		if math.IsNaN(v.f) || math.IsInf(v.f, 0) {
			t.Fatalf("%s: %s is %v", label, v.name, v.f)
		}
	}
	for _, p := range got.Culprits {
		if math.IsNaN(p.Contribution) || math.IsInf(p.Contribution, 0) {
			t.Fatalf("%s: %s contributes %v", label, p.Scope, p.Contribution)
		}
		if math.IsNaN(p.PointChange()) || math.IsInf(p.PointChange(), 0) {
			t.Fatalf("%s: %s point change is %v", label, p.Scope, p.PointChange())
		}
	}

	// A package identical in both snapshots moved the repo by exactly nothing,
	// so it is filtered out of the ranking. Every filter above it is disabled,
	// which makes absence here equivalent to a contribution of exactly zero and
	// this an unconditional check on the counterfactual arithmetic.
	for _, p := range got.Culprits {
		if p.Scope == twinPackage {
			t.Fatalf("%s: a package unchanged between snapshots is ranked as a culprit contributing %v",
				label, p.Contribution)
		}
	}

	// Ties break by package name, so the report is the same document twice.
	for i := 1; i < len(got.Culprits); i++ {
		prev, cur := got.Culprits[i-1], got.Culprits[i]
		a, b := math.Abs(prev.Contribution), math.Abs(cur.Contribution)
		if a < b {
			t.Fatalf("%s: culprits are not ranked by absolute contribution: %v before %v", label, prev, cur)
		}
		if a == b && prev.Scope > cur.Scope {
			t.Fatalf("%s: tied culprits are not in name order: %q before %q", label, prev.Scope, cur.Scope)
		}
	}

	// Metrics come out of the database in whatever order the rows arrive, and
	// culprits walks a map internally, so the ranking has to survive a reshuffle
	// of its input unchanged.
	shuffledHead := c.metrics(c.head)
	shuffledBase := c.metrics(c.base)
	rng.Shuffle(len(shuffledHead), func(i, j int) {
		shuffledHead[i], shuffledHead[j] = shuffledHead[j], shuffledHead[i]
	})
	rng.Shuffle(len(shuffledBase), func(i, j int) {
		shuffledBase[i], shuffledBase[j] = shuffledBase[j], shuffledBase[i]
	})
	again := one(t, delta.Input{
		Repo:        store.Repo{Name: "generated"},
		Head:        snap("go1.26"),
		HeadMetrics: shuffledHead,
		Base:        snap("go1.26"),
		BaseMetrics: shuffledBase,
	}, propOpts())

	if len(again.Culprits) != len(got.Culprits) {
		t.Fatalf("%s: shuffling the input changed the culprit count from %d to %d",
			label, len(got.Culprits), len(again.Culprits))
	}
	for i := range got.Culprits {
		a, b := got.Culprits[i], again.Culprits[i]
		if a.Scope != b.Scope || a.State != b.State || a.Contribution != b.Contribution {
			t.Fatalf("%s: shuffling the input changed culprit %d from %+v to %+v", label, i, a, b)
		}
	}

	if !c.totalsPreserved {
		// With totals moving, each counterfactual recomputes its own
		// denominator and the contributions genuinely do not sum to the repo
		// move. Asserting it anyway would fail on realistic input and tempt
		// someone to loosen the tolerance until it meant nothing.
		return
	}

	// No package total changed, so every denominator collapses to the repo
	// total and the contributions account for the whole move exactly.
	var sum float64
	for _, p := range got.Culprits {
		sum += p.Contribution
	}
	if diff := math.Abs(sum - covChange(got)); diff > 1e-9 {
		t.Fatalf("%s: culprits sum to %.12f but the repo moved %.12f (off by %g); with no package total changing these must agree exactly",
			label, sum, covChange(got), diff)
	}
}
