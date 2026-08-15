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
