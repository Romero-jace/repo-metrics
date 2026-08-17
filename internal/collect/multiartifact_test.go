package collect_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

const (
	junitBody = `<testsuites><testsuite name="s"><testcase classname="mod" name="a" time="0.1"/></testsuite></testsuites>`
	lcovBody  = "SF:src/a.ts\nLF:10\nLH:6\nend_of_record\n"
)

// twoArtifactStep is a step whose one command writes two files, which is what
// `pytest --junitxml=… --cov-report=lcov:…` and vitest both actually do.
func twoArtifactStep(dir string, command ...string) config.Repo {
	return config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{{
			Name:    "tests",
			Command: command,
			Artifacts: []config.Artifact{
				{Path: "j.xml", Format: config.FormatJUnitXML},
				{Path: "c.info", Format: config.FormatLCOV},
			},
			Timeout: config.Duration(time.Minute),
			MaxAge:  config.Duration(24 * time.Hour),
		}},
	}
}

// writeBoth is a command that produces both files, standing in for the test
// runner. Written as a shell command because that is what a real step is.
func writeBoth(t *testing.T, dir string) []string {
	t.Helper()
	return []string{"sh", "-c",
		"cat " + writeFile(t, dir, "j.src", junitBody) + " > j.xml && " +
			"cat " + writeFile(t, dir, "c.src", lcovBody) + " > c.info"}
}

// One command, two files, both read.
//
// Before this a step could name one artifact, so the second file could only be
// reached by a second step with no command — which swapped the "changed during
// this run" check for a 24 hour age window and let yesterday's profile through.
func TestOneCommandCanProduceTwoArtifacts(t *testing.T) {
	dir := t.TempDir()
	r := twoArtifactStep(dir, writeBoth(t, dir)...)

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	if got := metric(t, res, collect.KeyTestSuites, "tests"); got != 1 {
		t.Errorf("test suites from the JUnit artifact: got %v, want 1", got)
	}
	if got := metric(t, res, collect.KeyTotalLines, "src/a.ts"); got != 10 {
		t.Errorf("line total from the LCOV artifact: got %v, want 10", got)
	}
}

// One stale artifact must not cost the fresh ones.
//
// This is the regression the change is most likely to ship. The unusable check
// used to sit ahead of source assembly, so a single bad file removed EVERY
// artifact source in the step. Carried over unchanged it would mean a coverage
// profile the command failed to rewrite silently takes the test report written
// by the same command with it — the same failure this code already fought once
// between an artifact and stdout, in the direction it had not covered.
//
// Every other test in this package has at most one artifact per step and would
// stay green through that.
func TestOneStaleArtifactDoesNotCostTheFreshOne(t *testing.T) {
	dir := t.TempDir()

	// c.info exists and is old, and the command below does not rewrite it.
	stale := filepath.Join(dir, "c.info")
	if err := os.WriteFile(stale, []byte(lcovBody), 0o600); err != nil {
		t.Fatalf("seeding the stale artifact: %v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("aging the stale artifact: %v", err)
	}

	r := twoArtifactStep(dir, "sh", "-c", "cat "+writeFile(t, dir, "j.src", junitBody)+" > j.xml")

	res := collectOnce(t, r)

	// The fresh one survives. This is the assertion the test exists for.
	if got := metric(t, res, collect.KeyTestSuites, "tests"); got != 1 {
		t.Errorf("the JUnit artifact's metrics were lost along with the stale LCOV one: got %v, want 1", got)
	}
	// The stale one records nothing rather than yesterday's numbers.
	if hasMetric(res, collect.KeyTotalLines) {
		t.Error("the stale LCOV artifact was read, so a profile the command never rewrote is reported as current")
	}
	// And the snapshot says so: real numbers, with a caveat.
	if res.Snapshot.Status != store.StatusPartial {
		t.Errorf("Status: got %q, want partial. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if !strings.Contains(diagText(res), "c.info") {
		t.Errorf("the diagnostic does not name which file was stale:\n%s", diagText(res))
	}
	// Named by path, not only by format. With two sources in one step, "the lcov
	// artifact" no longer identifies one.
	if !strings.Contains(diagText(res), "wrote no fresh") {
		t.Errorf("the diagnostic does not say what went wrong:\n%s", diagText(res))
	}
}

// Ingest mode with several artifacts: a missing one costs its own measurements
// and nothing else.
//
// At one artifact this used to fail the whole step, and still does, because
// nothing else in the step parses. The generalization is what changes at two.
func TestAMissingIngestArtifactCostsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "j.xml", junitBody)

	r := config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{{
			Name: "ci-output",
			Artifacts: []config.Artifact{
				{Path: "j.xml", Format: config.FormatJUnitXML},
				{Path: "never-written.info", Format: config.FormatLCOV},
			},
			Timeout: config.Duration(time.Minute),
			MaxAge:  config.Duration(24 * time.Hour),
		}},
	}

	res := collectOnce(t, r)

	if got := metric(t, res, collect.KeyTestSuites, "ci-output"); got != 1 {
		t.Errorf("the present artifact's metrics were lost with the missing one: got %v, want 1", got)
	}
	if res.Snapshot.Status != store.StatusPartial {
		t.Errorf("Status: got %q, want partial", res.Snapshot.Status)
	}
	if !strings.Contains(diagText(res), "never-written.info") {
		t.Errorf("the diagnostic does not name the missing file:\n%s", diagText(res))
	}

	// The control, and the behavior that must NOT have changed: with the missing
	// file as the step's only artifact, the step fails outright.
	solo := config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{{
			Name:           "ci-output",
			Artifact:       "never-written.info",
			ArtifactFormat: config.FormatLCOV,
			Timeout:        config.Duration(time.Minute),
			MaxAge:         config.Duration(24 * time.Hour),
		}},
	}
	soloRes := collectOnce(t, solo)
	if soloRes.Snapshot.Status != store.StatusFailed {
		t.Errorf("a step whose only artifact is missing: got %q, want failed", soloRes.Snapshot.Status)
	}
	if !strings.Contains(diagText(soloRes), "no command configured") {
		t.Errorf("the single-artifact message changed; README and a sibling test pin it:\n%s", diagText(soloRes))
	}
}
