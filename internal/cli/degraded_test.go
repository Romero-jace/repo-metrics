package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// staleAndBrokenFleet is the pair the problems section could not tell apart: a
// repo whose numbers are real but were taken under protest, and one whose step
// produced nothing at all.
//
// Staleness is the hermetic way to reach the first. An artifact past its max_age
// sets the same degraded flag a non-zero exit does, without this test having to
// run a test suite to get a red one.
func staleAndBrokenFleet(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	stale := repoDir(t, dir, "stale", sampleProfile)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(stale, "coverage.out"), old, old); err != nil {
		t.Fatalf("backdating the artifact: %v", err)
	}
	broken := repoDir(t, dir, "broken", "")

	return writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		filepath.Join(dir, "metrics.db"),
		fmt.Sprintf("  - name: stale\n    path: %q\n    signals:\n      - name: coverage\n"+
			"        artifact: coverage.out\n        artifact_format: go-coverprofile\n        max_age: 1h\n", stale),
		fmt.Sprintf("  - name: broken\n    path: %q\n    signals:\n      - name: coverage\n"+
			"        artifact: missing.out\n        artifact_format: go-coverprofile\n", broken)))
}

// A red run and a run that collected nothing stop reading the same.
//
// Both make the snapshot partial or failed, so both have always landed in the
// problems section, side by side and spelled identically. On the reported fleet
// that meant a repo with known-failing tests sat there forever next to a package
// that would not build, and the section stopped meaning "do not trust this row".
func TestProblemsTellsAProtestedRunFromOneThatCollectedNothing(t *testing.T) {
	cfgPath := staleAndBrokenFleet(t)

	// broken fails outright, so collect exits 1 by design. The failure is the
	// fixture working.
	_, _, _ = runCLI(t, "collect", "--config", cfgPath)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--section", "problems", "--format", "json")
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}
	doc := decodeReport(t, stdout)
	if doc.Problems == nil {
		t.Fatal("problems is null for a section that was asked for")
	}

	stale := jsonRepo(t, *doc.Problems, "stale")
	if stale.Degraded == nil {
		t.Fatal("degraded is null on a run this binary collected, so nothing recorded what it knew")
	}
	if !*stale.Degraded {
		t.Error("degraded is false for a run over an artifact past its freshness limit")
	}

	broken := jsonRepo(t, *doc.Problems, "broken")
	if broken.Degraded != nil && *broken.Degraded {
		t.Errorf("degraded is true for a repo that collected nothing: there were no numbers to take under protest")
	}

	// And the markdown says which is which, since the section is read by people
	// more often than parsed.
	md, _, err := runCLI(t, "report", "--config", cfgPath, "--section", "problems")
	if err != nil {
		t.Fatalf("report markdown: %v", err)
	}
	staleRow := bulletFor(t, md, "stale")
	if !strings.Contains(staleRow, "measured under protest") {
		t.Errorf("the stale repo's row does not say its numbers are real: %q", staleRow)
	}
	brokenRow := bulletFor(t, md, "broken")
	if !strings.Contains(brokenRow, "did not collect") {
		t.Errorf("the broken repo's row does not say it collected nothing: %q", brokenRow)
	}
	if staleRow == brokenRow {
		t.Error("the two rows are identical, which is the failure this whole change is about")
	}
}

// bulletFor returns the problems-section line for one repo.
//
// Bound to the named repo's own bullet rather than searched for across the whole
// document, for the reason rowFor exists one rendering over: a substring check
// over everything cannot fail when one repo's verdict lands on another repo's
// line, which is precisely the mix-up worth catching in a section that has one
// line per repo.
func bulletFor(t *testing.T, md, name string) string {
	t.Helper()
	prefix := "- **" + name + "**"
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no problems line for %q in:\n%s", name, md)
	return ""
}
