package golang_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
)

func parse(t *testing.T, body string) *golang.Profile {
	t.Helper()
	p, err := golang.ParseCoverProfile(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseCoverProfile: %v", err)
	}
	return p
}

func find(t *testing.T, p *golang.Profile, pkg string) golang.PackageCoverage {
	t.Helper()
	for _, got := range p.Packages {
		if got.Package == pkg {
			return got
		}
	}
	t.Fatalf("package %q not in profile; have %+v", pkg, p.Packages)
	return golang.PackageCoverage{}
}

func TestParseAggregatesPerPackage(t *testing.T) {
	p := parse(t, `mode: set
example.com/m/alpha/a.go:1.1,3.2 3 1
example.com/m/alpha/a.go:5.1,6.2 2 0
example.com/m/beta/b.go:1.1,2.2 4 1
`)

	if p.Mode != "set" {
		t.Errorf("Mode: got %q, want set", p.Mode)
	}
	if len(p.Packages) != 2 {
		t.Fatalf("want 2 packages, got %d: %+v", len(p.Packages), p.Packages)
	}
	// Packages are sorted by import path, which report output relies on.
	if p.Packages[0].Package != "example.com/m/alpha" {
		t.Errorf("packages not sorted: %+v", p.Packages)
	}

	alpha := find(t, p, "example.com/m/alpha")
	if alpha.Covered != 3 || alpha.Total != 5 {
		t.Errorf("alpha: got covered=%d total=%d, want 3/5", alpha.Covered, alpha.Total)
	}
	beta := find(t, p, "example.com/m/beta")
	if beta.Covered != 4 || beta.Total != 4 {
		t.Errorf("beta: got covered=%d total=%d, want 4/4", beta.Covered, beta.Total)
	}

	covered, total := p.Totals()
	if covered != 7 || total != 9 {
		t.Errorf("Totals: got %d/%d, want 7/9", covered, total)
	}
}

// The one that matters. Under -coverpkg=./... every test binary that links a
// package emits that package's blocks again, so a repo with 40 test binaries
// produces 40 copies of every block. Summing NumStmt naively inflates the
// denominator by that factor and yields a coverage percentage that looks
// entirely plausible and is badly wrong.
//
// Here the same block appears three times. Total must be 3, not 9.
func TestDuplicateBlocksAreMergedNotSummed(t *testing.T) {
	p := parse(t, `mode: set
example.com/m/alpha/a.go:1.1,3.2 3 0
example.com/m/alpha/a.go:1.1,3.2 3 0
example.com/m/alpha/a.go:1.1,3.2 3 0
`)

	alpha := find(t, p, "example.com/m/alpha")
	if alpha.Total != 3 {
		t.Errorf("Total: got %d, want 3 (the block was counted once per copy)", alpha.Total)
	}
	if alpha.Covered != 0 {
		t.Errorf("Covered: got %d, want 0", alpha.Covered)
	}
}

// A block is covered if ANY test binary exercised it, which is what summing the
// duplicate counts and testing for positive gives us. Taking only the first
// copy would report this block as uncovered.
func TestBlockIsCoveredIfAnyCopyWasExercised(t *testing.T) {
	p := parse(t, `mode: set
example.com/m/alpha/a.go:1.1,3.2 3 0
example.com/m/alpha/a.go:1.1,3.2 3 0
example.com/m/alpha/a.go:1.1,3.2 3 1
`)

	alpha := find(t, p, "example.com/m/alpha")
	if alpha.Covered != 3 || alpha.Total != 3 {
		t.Errorf("got covered=%d total=%d, want 3/3", alpha.Covered, alpha.Total)
	}
}

// Blocks that share a file but differ in extent are distinct, and must not be
// collapsed by an over-eager key.
func TestDistinctBlocksInOneFileStaySeparate(t *testing.T) {
	p := parse(t, `mode: set
example.com/m/alpha/a.go:1.1,3.2 3 1
example.com/m/alpha/a.go:1.1,3.9 3 0
example.com/m/alpha/a.go:1.2,3.2 3 0
example.com/m/alpha/a.go:9.1,3.2 3 0
`)

	alpha := find(t, p, "example.com/m/alpha")
	if alpha.Total != 12 {
		t.Errorf("Total: got %d, want 12 (four distinct blocks of 3)", alpha.Total)
	}
	if alpha.Covered != 3 {
		t.Errorf("Covered: got %d, want 3", alpha.Covered)
	}
}

// count and atomic modes carry real hit counts rather than 0/1. Anything
// positive still means covered.
func TestCountModeTreatsAnyPositiveHitAsCovered(t *testing.T) {
	p := parse(t, `mode: count
example.com/m/alpha/a.go:1.1,3.2 3 47
example.com/m/alpha/a.go:5.1,6.2 2 0
`)

	if p.Mode != "count" {
		t.Errorf("Mode: got %q, want count", p.Mode)
	}
	alpha := find(t, p, "example.com/m/alpha")
	if alpha.Covered != 3 || alpha.Total != 5 {
		t.Errorf("got covered=%d total=%d, want 3/5", alpha.Covered, alpha.Total)
	}
}

// This is what an untested package looks like under -coverpkg=./...: present in
// the profile with a real denominator and nothing covered. Dropping it would
// silently inflate the repo percentage by excluding untested code.
func TestUntestedPackageKeepsItsDenominator(t *testing.T) {
	p := parse(t, `mode: set
example.com/m/tested/a.go:1.1,3.2 10 1
example.com/m/untested/b.go:1.1,9.2 90 0
`)

	untested := find(t, p, "example.com/m/untested")
	if untested.Covered != 0 || untested.Total != 90 {
		t.Errorf("untested: got covered=%d total=%d, want 0/90", untested.Covered, untested.Total)
	}

	covered, total := p.Totals()
	if covered != 10 || total != 100 {
		t.Errorf("Totals: got %d/%d, want 10/100", covered, total)
	}
	// The point of keeping the denominator: 10 percent, not 100 percent.
	if pct := float64(covered) / float64(total) * 100; pct != 10 {
		t.Errorf("repo coverage: got %.1f%%, want 10.0%%", pct)
	}
}

// A run where nothing was instrumented is a valid profile, not a parse error.
func TestModeHeaderAloneIsValid(t *testing.T) {
	p := parse(t, "mode: set\n")
	if len(p.Packages) != 0 {
		t.Errorf("want no packages, got %+v", p.Packages)
	}
	covered, total := p.Totals()
	if covered != 0 || total != 0 {
		t.Errorf("Totals: got %d/%d, want 0/0", covered, total)
	}
}

func TestBlankLinesAreSkipped(t *testing.T) {
	p := parse(t, "mode: set\n\nexample.com/m/a/a.go:1.1,3.2 3 1\n\n")
	if len(p.Packages) != 1 || p.Packages[0].Total != 3 {
		t.Errorf("got %+v", p.Packages)
	}
}

func TestRejectsMalformedProfiles(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"completely empty", "", "mode:"},
		{"no mode header", "example.com/m/a/a.go:1.1,3.2 3 1\n", "mode:"},
		{"empty mode", "mode:\n", "empty coverage mode"},
		{"too few fields", "mode: set\nexample.com/m/a/a.go:1.1,3.2 3\n", "three space-separated"},
		{"bad count", "mode: set\nexample.com/m/a/a.go:1.1,3.2 3 x\n", "execution count"},
		{"bad statement count", "mode: set\nexample.com/m/a/a.go:1.1,3.2 x 1\n", "statement count"},
		{"negative count", "mode: set\nexample.com/m/a/a.go:1.1,3.2 -3 1\n", "negative"},
		{"no colon", "mode: set\nexample.com/m/a/a.go 3 1\n", "<file>:<range>"},
		{"no comma in range", "mode: set\nexample.com/m/a/a.go:1.1 3 1\n", "comma-separated"},
		{"bad position", "mode: set\nexample.com/m/a/a.go:1x1,3.2 3 1\n", "line.column"},
		{"non-numeric line", "mode: set\nexample.com/m/a/a.go:x.1,3.2 3 1\n", "bad line"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := golang.ParseCoverProfile(strings.NewReader(tc.body))
			if err == nil {
				t.Fatal("ParseCoverProfile accepted a malformed profile")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Parse errors have to name the line, because a real profile is tens of
// thousands of lines long and "malformed block" alone is useless.
func TestParseErrorsNameTheLine(t *testing.T) {
	_, err := golang.ParseCoverProfile(strings.NewReader(
		"mode: set\nexample.com/m/a/a.go:1.1,3.2 3 1\nexample.com/m/a/a.go:1.1,3.2 3 nope\n"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error %q does not name line 3", err)
	}
}
