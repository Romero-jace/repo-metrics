package golang_test

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
)

// Property tests for ParseCoverProfile.
//
// The hand-built fixtures in coverprofile_test.go pin the cases someone thought
// of. This file generates the ones nobody did, because the failure worth
// guarding here is not a crash, it is a plausible number: a real profile from
// go-library was 84,336 lines describing 4,024 distinct statements, and
// getting the merge wrong reported 1.4% where the truth was 28.7%.
//
// A fixed seed table rather than testing.F on purpose. A fuzz corpus in CI is a
// moving part that reproduces differently on every machine, and this repo has no
// fuzz precedent. A seed table fails identically everywhere, which matters more
// here than corpus discovery. Every generator uses its own rand.New source, so
// no test can perturb another through the global one.
var propSeeds = []int64{
	1, 2, 3, 5, 8, 13, 21, 34, 55, 89,
	1009, 4242, 90210, 525600, 1000003,
	-1, -7, -99, 314159, 271828,
}

// sentinelCount is the execution count on the "this copy ran" line of every
// mixed sentinel block, deliberately far larger than any numStmt the generator
// emits. Adding execution counts into Covered instead of statement counts is a
// tempting wrong implementation, and this makes it overshoot Total loudly
// instead of landing on a believable percentage.
const sentinelCount = 1009

// maxStmt bounds a block's statement count. Kept well under sentinelCount.
const maxStmt = 12

// genBlock is one distinct coverage block plus the execution count of every
// copy of it the generated profile emits. Under -coverpkg=./... the same block
// is re-emitted once per test binary that linked it, which is the duplication
// this parser exists to merge.
type genBlock struct {
	pkg, file                            string
	startLine, startCol, endLine, endCol int
	numStmt                              int
	counts                               []int
}

// line renders this block as one profile line with the given execution count.
func (b genBlock) line(count int) string {
	return fmt.Sprintf("%s:%d.%d,%d.%d %d %d",
		b.file, b.startLine, b.startCol, b.endLine, b.endCol, b.numStmt, count)
}

// covered is the ground truth: a block is covered when at least one of its
// copies ran, not when its first copy did.
func (b genBlock) covered() bool {
	for _, c := range b.counts {
		if c > 0 {
			return true
		}
	}
	return false
}

// corpus is a generated profile together with the ground truth, built up as the
// generator goes so assertions compare against something computed independently
// of the parser rather than against the parser's own output.
type corpus struct {
	mode   string
	blocks []genBlock
	lines  []string

	pkgs  []string
	files int

	// Indices into blocks of the three shapes the generator guarantees on every
	// seed, so that "the mutation was caught" is a fact about the generator and
	// not about which seeds happened to get picked.
	twinIdx  []int // shares start and end LINES with another block, differs only in columns
	mixedIdx []int // emitted as zero, positive, zero in that order
	zeroIdx  []int // every copy zero
}

func (c *corpus) shape() string {
	return fmt.Sprintf("pkgs=%d files=%d blocks=%d lines=%d mode=%s",
		len(c.pkgs), c.files, len(c.blocks), len(c.lines), c.mode)
}

// want computes per-package coverage from the generator's own record. It never
// calls path.Dir or anything else the parser uses: the package name is carried
// on each block from the moment it is created.
func (c *corpus) want() map[string]golang.PackageCoverage {
	out := make(map[string]golang.PackageCoverage, len(c.pkgs))
	for _, b := range c.blocks {
		agg := out[b.pkg]
		agg.Package = b.pkg
		agg.Total += b.numStmt
		if b.covered() {
			agg.Covered += b.numStmt
		}
		out[b.pkg] = agg
	}
	return out
}

// text renders a full profile body from the given line order.
func (c *corpus) text(lines []string) string {
	// Only the block lines are ever shuffled. The mode header is the first
	// non-blank line by definition and the parser rejects anything else there.
	return "mode: " + c.mode + "\n" + strings.Join(lines, "\n") + "\n"
}

// generate builds a random profile and its ground truth from one seeded source.
func generate(rng *rand.Rand) *corpus {
	// Merged -coverpkg profiles in the wild are count or atomic; set mode is
	// already covered by the hand-built table, and its 0/1 counts would defeat
	// the sentinel that catches Covered summing execution counts.
	c := &corpus{mode: []string{"count", "atomic"}[rng.Intn(2)]}

	nPkgs := 1 + rng.Intn(4)
	for p := range nPkgs {
		// Always at least one slash: path.Dir("main.go") is ".", and a stray "."
		// package would look like a parser bug that isn't one.
		pkg := fmt.Sprintf("example.com/m/pkg%d", p)
		c.pkgs = append(c.pkgs, pkg)
		for f := range 1 + rng.Intn(3) {
			c.addFile(rng, pkg, fmt.Sprintf("%s/f%d.go", pkg, f))
		}
	}
	c.emit(rng)
	return c
}

// addFile appends one file's worth of blocks, including the three guaranteed
// shapes described on corpus.
func (c *corpus) addFile(rng *rand.Rand, pkg, file string) {
	c.files++

	// nextLine hands out disjoint line ranges so every extent is distinct.
	line := 1
	newBlock := func() genBlock {
		start := line
		end := start + 1 + rng.Intn(4)
		line = end + 1
		return genBlock{
			pkg:       pkg,
			file:      file,
			startLine: start,
			startCol:  1 + rng.Intn(9),
			endLine:   end,
			endCol:    1 + rng.Intn(9),
			numStmt:   1 + rng.Intn(maxStmt),
		}
	}

	for range 1 + rng.Intn(5) {
		b := newBlock()
		for range 1 + rng.Intn(5) {
			if rng.Intn(10) < 6 {
				b.counts = append(b.counts, 0)
			} else {
				b.counts = append(b.counts, 1+rng.Intn(60))
			}
		}
		c.blocks = append(c.blocks, b)
	}

	// A column twin: same start and end lines as an existing block, different
	// columns, its own statement count. A dedup key that drops the column
	// numbers collapses these two into one and loses a denominator.
	src := c.blocks[len(c.blocks)-1]
	twin := genBlock{
		pkg:       pkg,
		file:      file,
		startLine: src.startLine,
		startCol:  src.startCol + 10,
		endLine:   src.endLine,
		endCol:    src.endCol + 10,
		numStmt:   1 + rng.Intn(maxStmt),
		counts:    []int{0, 1 + rng.Intn(60)},
	}
	c.twinIdx = append(c.twinIdx, len(c.blocks))
	c.blocks = append(c.blocks, twin)

	// A mixed sentinel, emitted below as zero, positive, zero in exactly that
	// order. Keeping only the first copy's count and keeping only the last one's
	// both report this block uncovered, and both are implementations someone
	// reaches for when they stop thinking of the profile as a merge.
	mixed := newBlock()
	mixed.counts = []int{0, sentinelCount, 0}
	c.mixedIdx = append(c.mixedIdx, len(c.blocks))
	c.blocks = append(c.blocks, mixed)

	// An all-zero block, so every package has room to gain coverage. The
	// "appending a positive duplicate" property needs one to aim at.
	zero := newBlock()
	zero.counts = []int{0, 0}
	c.zeroIdx = append(c.zeroIdx, len(c.blocks))
	c.blocks = append(c.blocks, zero)
}

// emit flattens every block copy into a shuffled line list.
func (c *corpus) emit(rng *rand.Rand) {
	type entry struct{ block, count int }

	entries := make([]entry, 0, len(c.blocks)*3)
	for i, b := range c.blocks {
		for _, n := range b.counts {
			entries = append(entries, entry{i, n})
		}
	}
	rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })

	// Pin each mixed sentinel's own copies to zero, positive, zero. The slots
	// stay wherever the shuffle put them and the multiset of lines is unchanged;
	// only which of that block's three counts lands in which of its own slots is
	// fixed. Without this, whether a first-copy-wins bug is exposed on a given
	// seed is a coin flip.
	for _, bi := range c.mixedIdx {
		want := c.blocks[bi].counts
		k := 0
		for i, e := range entries {
			if e.block != bi {
				continue
			}
			entries[i].count = want[k]
			k++
		}
	}

	c.lines = make([]string, len(entries))
	for i, e := range entries {
		c.lines[i] = c.blocks[e.block].line(e.count)
	}
}

func TestParseCoverProfileProperties(t *testing.T) {
	shapes := make(map[string]bool)

	for _, seed := range propSeeds {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			c := generate(rng)
			shapes[c.shape()] = true
			t.Logf("%s duplicate-ratio=%.2fx twins=%d mixed=%d all-zero=%d",
				c.shape(), float64(len(c.lines))/float64(len(c.blocks)),
				len(c.twinIdx), len(c.mixedIdx), len(c.zeroIdx))

			// Generator self-check. If these ever stop holding, the properties
			// below are still green and no longer testing anything.
			if len(c.lines) <= len(c.blocks) {
				t.Fatalf("generator emitted %d lines for %d blocks: no duplicates to merge",
					len(c.lines), len(c.blocks))
			}
			if len(c.twinIdx) == 0 || len(c.mixedIdx) == 0 || len(c.zeroIdx) == 0 {
				t.Fatalf("generator dropped a guaranteed shape: twins=%d mixed=%d all-zero=%d",
					len(c.twinIdx), len(c.mixedIdx), len(c.zeroIdx))
			}

			base := parse(t, c.text(c.lines))
			checkGroundTruth(t, c, base)
			checkShuffleInvariance(t, c, base, rng)
			checkZeroDuplicateIsInert(t, c, base)
			checkPositiveDuplicateOnlyAddsCovered(t, c, base)
		})
	}

	// A generator that emits one shape repeatedly is a table test wearing a
	// costume, and the only way to see that is to count.
	t.Logf("distinct generated shapes: %d over %d seeds", len(shapes), len(propSeeds))
	if len(shapes) < len(propSeeds)/2 {
		t.Errorf("only %d distinct shapes over %d seeds; the generator is not exploring",
			len(shapes), len(propSeeds))
	}
}

// checkGroundTruth is the invariant that catches the 21x inflation: Total is the
// sum of DISTINCT block statement counts, and Covered counts a block when any
// copy of it ran.
func checkGroundTruth(t *testing.T, c *corpus, got *golang.Profile) {
	t.Helper()

	if got.Mode != c.mode {
		t.Errorf("Mode: got %q, want %q", got.Mode, c.mode)
	}

	want := c.want()
	if len(got.Packages) != len(want) {
		t.Fatalf("got %d packages, want %d: %+v", len(got.Packages), len(want), got.Packages)
	}
	if !slices.IsSortedFunc(got.Packages, func(a, b golang.PackageCoverage) int {
		return strings.Compare(a.Package, b.Package)
	}) {
		t.Errorf("packages are not sorted by import path, which report output relies on: %+v", got.Packages)
	}

	var sumCovered, sumTotal int
	for _, pkg := range got.Packages {
		w, ok := want[pkg.Package]
		if !ok {
			t.Fatalf("parser reported package %q, which the generator never created", pkg.Package)
		}
		if pkg.Total != w.Total {
			t.Errorf("%s Total: got %d, want %d. The profile emits %d lines for %d distinct blocks, "+
				"so summing duplicates instead of merging inflates the denominator by that ratio "+
				"and yields a plausible, wrong percentage.",
				pkg.Package, pkg.Total, w.Total, len(c.lines), len(c.blocks))
		}
		if pkg.Covered != w.Covered {
			t.Errorf("%s Covered: got %d, want %d. A block is covered when ANY copy of it ran, "+
				"not when its first or last copy did.", pkg.Package, pkg.Covered, w.Covered)
		}
		if pkg.Covered > pkg.Total {
			t.Errorf("%s: Covered %d exceeds Total %d, which is not a coverage number at all",
				pkg.Package, pkg.Covered, pkg.Total)
		}
		sumCovered += pkg.Covered
		sumTotal += pkg.Total
	}

	covered, total := got.Totals()
	if covered != sumCovered || total != sumTotal {
		t.Errorf("Totals: got %d/%d, want the per-package sum %d/%d",
			covered, total, sumCovered, sumTotal)
	}
	if covered > total {
		t.Errorf("rollup: Covered %d exceeds Total %d", covered, total)
	}
}

// checkShuffleInvariance requires identical output, package order included,
// from several random orderings of the same block set. Report determinism
// depends on that ordering, and map iteration order is the obvious way to lose
// it.
func checkShuffleInvariance(t *testing.T, c *corpus, base *golang.Profile, rng *rand.Rand) {
	t.Helper()

	for i := range 4 {
		lines := slices.Clone(c.lines)
		rng.Shuffle(len(lines), func(a, b int) { lines[a], lines[b] = lines[b], lines[a] })

		got := parse(t, c.text(lines))
		if got.Mode != base.Mode || !slices.Equal(got.Packages, base.Packages) {
			t.Errorf("permutation %d changed the result:\ngot  %+v\nwant %+v",
				i, got.Packages, base.Packages)
		}
	}
}

// checkZeroDuplicateIsInert: another test binary that linked the package but
// never exercised the block adds a count-0 line, which must move nothing.
func checkZeroDuplicateIsInert(t *testing.T, c *corpus, base *golang.Profile) {
	t.Helper()

	picks := []int{c.zeroIdx[0], c.mixedIdx[0], c.twinIdx[0], len(c.blocks) - 1}
	for _, bi := range picks {
		lines := append(slices.Clone(c.lines), c.blocks[bi].line(0))
		got := parse(t, c.text(lines))
		if !slices.Equal(got.Packages, base.Packages) {
			t.Errorf("a duplicate line with count 0 for block %d changed the result:\ngot  %+v\nwant %+v",
				bi, got.Packages, base.Packages)
		}
	}
}

// checkPositiveDuplicateOnlyAddsCovered: another binary that DID exercise the
// block adds a positive line. Total must not move at all, and Covered must grow
// by exactly that block's statement count in exactly its package.
func checkPositiveDuplicateOnlyAddsCovered(t *testing.T, c *corpus, base *golang.Profile) {
	t.Helper()

	for _, bi := range c.zeroIdx {
		b := c.blocks[bi]
		lines := append(slices.Clone(c.lines), b.line(1))
		got := parse(t, c.text(lines))

		if len(got.Packages) != len(base.Packages) {
			t.Fatalf("adding a duplicate line changed the package set: got %+v, want %+v",
				got.Packages, base.Packages)
		}
		for i, pkg := range got.Packages {
			was := base.Packages[i]
			if pkg.Package != was.Package {
				t.Fatalf("adding a duplicate line reordered packages: got %+v, want %+v",
					got.Packages, base.Packages)
			}
			if pkg.Total != was.Total {
				t.Errorf("%s Total moved from %d to %d on a duplicate of an existing block",
					pkg.Package, was.Total, pkg.Total)
			}
			if pkg.Covered < was.Covered {
				t.Errorf("%s Covered fell from %d to %d when a block was newly exercised",
					pkg.Package, was.Covered, pkg.Covered)
			}
			wantCovered := was.Covered
			if pkg.Package == b.pkg {
				wantCovered += b.numStmt
			}
			if pkg.Covered != wantCovered {
				t.Errorf("%s Covered: got %d, want %d after newly exercising a %d-statement block",
					pkg.Package, pkg.Covered, wantCovered, b.numStmt)
			}
		}
	}
}
