package golang_test

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
)

// TestAgainstGoToolCover is the real correctness gate for this parser.
//
// Hand-built fixtures prove the dedup logic does what the code intends. Only a
// genuine profile proves the intent matches what the Go toolchain actually
// emits, and a profile from a repo built with -coverpkg=./... is where naive
// summing goes visibly wrong: every test binary re-emits every block it linked,
// so an 80,000-line profile can describe a few thousand distinct blocks.
//
// Opt-in, so the unit suite stays hermetic and offline:
//
//	REPO_METRICS_TEST_PROFILE=/path/to/coverage.out \
//	REPO_METRICS_TEST_PROFILE_DIR=/path/to/the/repo \
//	go test ./internal/collect/golang/ -run TestAgainstGoToolCover -v
func TestAgainstGoToolCover(t *testing.T) {
	profile := os.Getenv("REPO_METRICS_TEST_PROFILE")
	if profile == "" {
		t.Skip("set REPO_METRICS_TEST_PROFILE to cross-check against a real coverage profile")
	}

	// go tool cover -func reads the source files the profile references, so it
	// has to run somewhere the module resolves.
	dir := os.Getenv("REPO_METRICS_TEST_PROFILE_DIR")
	if dir == "" {
		dir = filepath.Dir(profile)
	}

	f, err := os.Open(profile)
	if err != nil {
		t.Fatalf("opening profile: %v", err)
	}
	defer func() { _ = f.Close() }()

	got, err := golang.ParseCoverProfile(f)
	if err != nil {
		t.Fatalf("ParseCoverProfile: %v", err)
	}
	covered, total := got.Totals()
	if total == 0 {
		t.Fatal("parsed profile has no statements")
	}
	ourPct := float64(covered) / float64(total) * 100

	wantPct := goToolCoverTotal(t, dir, profile)

	t.Logf("packages=%d statements=%d covered=%d ours=%.1f%% go-tool-cover=%.1f%%",
		len(got.Packages), total, covered, ourPct, wantPct)

	// go tool cover prints one decimal place, so agreement to within half of
	// that last digit is exact agreement.
	if math.Abs(ourPct-wantPct) > 0.05 {
		t.Errorf("coverage disagrees with go tool cover: ours %.4f%%, official %.1f%%.\n"+
			"A large overshoot in the denominator is the signature of duplicate blocks "+
			"being summed instead of merged.", ourPct, wantPct)
	}

	// The counts, which the check above cannot reach.
	//
	// This parser stores a numerator and a denominator and never a percentage,
	// for the reason CONTRIBUTING's "Adding a metric" section gives: a stored
	// rate bakes in an averaging error and makes rollups impossible. So the one
	// quantity the check above compares is the one quantity the parser does not
	// produce, and every bug that scales covered and total together survives it.
	// Double-counting is exactly that shape: two test binaries emitting the same
	// block double both halves and leave the ratio alone.
	//
	// go tool cover cannot supply the missing numbers. -func prints a percentage
	// per function and a percentage on the total line, and nothing else, so
	// everything recoverable from it is a ratio and every ratio is invariant
	// under the bug. The independent source for absolute counts is therefore the
	// artifact the toolchain wrote, recounted here by a different route: keyed on
	// the span as raw text rather than on five parsed integers, so a parser that
	// merged blocks it should have kept apart disagrees with it.
	raw, err := os.ReadFile(profile)
	if err != nil {
		t.Fatalf("reading profile for an independent recount: %v", err)
	}
	wantCovered, wantTotal, dataLines, distinct := recountProfile(t, string(raw))
	if covered != wantCovered || total != wantTotal {
		t.Errorf("statement counts disagree with an independent recount of the same file: ours covered=%d total=%d, recount covered=%d total=%d.\n"+
			"The percentage check above passes through this, because inflating both halves by the same factor leaves the ratio alone.",
			covered, total, wantCovered, wantTotal)
	}

	t.Logf("profile has %d data lines over %d distinct blocks", dataLines, distinct)
	if dataLines == distinct {
		// Not a failure: the documented recipe produces such a profile, because
		// it names one test-bearing package and cmd/repo-metrics, which has no
		// tests, so exactly one binary emits counters. It is worth saying out
		// loud, since a reader would otherwise take a pass here as evidence that
		// dedup was exercised against real toolchain output. The check below is
		// what actually exercises it.
		t.Logf("no block appears twice in this profile, so nothing above exercises the dedup logic")
	}

	assertDuplicateBlocksMerge(t, dir, string(raw), covered, total, wantPct)
}

// assertDuplicateBlocksMerge is the dedup check, run against real toolchain
// output regardless of whether the profile handed to this test happens to
// contain duplicates.
//
// Under -coverpkg every test binary emits counters for every package it linked,
// so the merged profile carries each block once per binary. That is the case the
// blockKey map exists for, and it is absent from the profile CONTRIBUTING tells
// you to generate, which came out at 141 data lines over 141 distinct blocks the
// day this was written. Repeating the real profile's data lines reproduces the
// case exactly, since that is the literal shape a second binary contributes.
//
// The premise is checked rather than assumed: go tool cover is handed the
// repeated profile too, and has to report the same percentage it reported for the
// original. If it did not, repeating blocks would not be the no-op this assumes
// and the assertion would be measuring nothing.
func assertDuplicateBlocksMerge(t *testing.T, dir, raw string, covered, total int, wantPct float64) {
	t.Helper()

	doubled := repeatDataLines(t, raw)

	path := filepath.Join(t.TempDir(), "doubled.out")
	if err := os.WriteFile(path, []byte(doubled), 0o600); err != nil {
		t.Fatalf("writing the repeated profile: %v", err)
	}
	if pct := goToolCoverTotal(t, dir, path); math.Abs(pct-wantPct) > 0.05 {
		t.Fatalf("go tool cover reports %.1f%% for the profile with every block repeated and %.1f%% for the original, "+
			"so repeating a block is not the merge this check assumes and the assertion below proves nothing", pct, wantPct)
	}

	got, err := golang.ParseCoverProfile(strings.NewReader(doubled))
	if err != nil {
		t.Fatalf("ParseCoverProfile on the repeated profile: %v", err)
	}
	dupCovered, dupTotal := got.Totals()
	if dupCovered != covered || dupTotal != total {
		t.Errorf("repeating every block changed the counts: covered %d became %d, total %d became %d.\n"+
			"The same block emitted by two test binaries is one block seen twice. Summing the copies is the double count "+
			"the dedup exists to prevent, and it is invisible to a percentage: go tool cover reads the same %.1f%% either way.",
			covered, dupCovered, total, dupTotal, wantPct)
	}
}

// recountProfile totals a coverage profile without going through the parser
// under test, so the two can disagree.
//
// The dedup rule it applies is the toolchain's own: identical spans are one
// block, and a block is covered when any copy ran. go tool cover merges them the
// same way, which is why it reads the same percentage off a profile with every
// block repeated.
func recountProfile(t *testing.T, raw string) (covered, total, dataLines, distinct int) {
	t.Helper()

	stmts := make(map[string]int)
	ran := make(map[string]bool)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("profile line %q does not split into three fields, so this recount cannot read it. A path containing a space would do that, and would need the same right-anchored split the parser uses.", line)
		}
		numStmt, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("statement count in %q: %v", line, err)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("execution count in %q: %v", line, err)
		}

		dataLines++
		span := fields[0]
		if prev, seen := stmts[span]; seen && prev != numStmt {
			t.Fatalf("block %s is listed with %d statements and with %d, which go tool cover rejects outright, so this file is not toolchain output", span, prev, numStmt)
		}
		stmts[span] = numStmt
		if count > 0 {
			ran[span] = true
		}
	}

	for span, numStmt := range stmts {
		total += numStmt
		if ran[span] {
			covered += numStmt
		}
	}
	return covered, total, dataLines, len(stmts)
}

// repeatDataLines returns the profile with its data lines listed twice under a
// single header, which is what a second test binary covering the same packages
// contributes to a merged profile. The second "mode:" line is dropped because a
// merged profile carries one.
func repeatDataLines(t *testing.T, raw string) string {
	t.Helper()

	var header string
	var data []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "mode:"):
			header = line
		default:
			data = append(data, line)
		}
	}
	if header == "" {
		t.Fatalf("profile has no mode: header, so it is not a coverage profile")
	}
	if len(data) == 0 {
		t.Fatalf("profile has no data lines, so repeating them proves nothing")
	}

	lines := make([]string, 0, len(data)*2+1)
	lines = append(lines, header)
	lines = append(lines, data...)
	lines = append(lines, data...)
	return strings.Join(lines, "\n") + "\n"
}

// TestTestJSONAgainstRealStream checks the no-test-files detection against a
// genuine test2json stream rather than a hand-built fixture.
//
// The rule it guards is empirical: a package with no test files reports Action
// "skip" with an empty Test, which is also what a package whose every test
// called t.Skip reports. The parser additionally requires the "[no test files]"
// output marker, so this test independently counts that marker in the raw
// stream and compares.
//
// Opt-in:
//
//	REPO_METRICS_TEST_JSON=/path/to/stream.json \
//	go test ./internal/collect/golang/ -run TestTestJSONAgainstRealStream -v
func TestTestJSONAgainstRealStream(t *testing.T) {
	path := os.Getenv("REPO_METRICS_TEST_JSON")
	if path == "" {
		t.Skip("set REPO_METRICS_TEST_JSON to cross-check against a real go test -json stream")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}

	summary, err := golang.ParseTestJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ParseTestJSON: %v", err)
	}

	// Count the marker independently of the parser's own logic.
	var wantEmpty int
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "[no test files]") {
			wantEmpty++
		}
	}

	got := summary.PackagesWithoutTests()
	tests, failed, skipped, elapsed := summary.Totals()
	t.Logf("packages=%d without-tests=%d tests=%d failed=%d skipped=%d elapsed=%s malformed=%d",
		len(summary.Packages), got, tests, failed, skipped, elapsed, summary.Malformed)

	if got != wantEmpty {
		t.Errorf("PackagesWithoutTests: got %d, want %d (independent marker count)", got, wantEmpty)
	}
	if summary.Malformed != 0 {
		t.Errorf("a clean run produced %d malformed lines", summary.Malformed)
	}
}

// goToolCoverTotal shells out for the official number and returns the percentage
// from its trailing "total:" line.
func goToolCoverTotal(t *testing.T, dir, profile string) float64 {
	t.Helper()

	out, err := exec.Command("go", "-C", dir, "tool", "cover", "-func="+profile).Output()
	if err != nil {
		t.Fatalf("go tool cover: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "total:") {
		t.Fatalf("unexpected final line from go tool cover: %q", last)
	}

	fields := strings.Fields(last)
	pct, err := strconv.ParseFloat(strings.TrimSuffix(fields[len(fields)-1], "%"), 64)
	if err != nil {
		t.Fatalf("parsing %q from go tool cover: %v", last, err)
	}
	return pct
}
