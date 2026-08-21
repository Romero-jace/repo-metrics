package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// reposDoc is the repos payload. Coverage is a pointer for the reason every
// measurement group in this suite is: a plain struct decodes a repo that
// measured nothing straight back into 0.0 percent.
type reposDoc struct {
	Repos *[]repoStateDoc `json:"repos"`
}

type repoStateDoc struct {
	Name          string        `json:"name"`
	Status        string        `json:"status"`
	CollectedAt   *string       `json:"collected_at"`
	HasSnapshot   bool          `json:"has_snapshot"`
	Coverage      *coverageOnly `json:"coverage"`
	CoverageLines *coverageOnly `json:"coverage_lines"`
}

// coverageOnly is a repos coverage group: the rate and the counts it came from,
// and nothing derived from a second snapshot.
//
// The rate is spelled value, the same as the report's and history's, so one
// walker reads a measurement out of any of the three payloads without knowing
// which it is holding. It was pct until v0.2.0, and the divergence cost real
// parse mistakes.
type coverageOnly struct {
	Value   float64 `json:"value"`
	Covered int     `json:"covered"`
	Total   int     `json:"total"`
}

func decodeRepos(t *testing.T, out string) []repoStateDoc {
	t.Helper()
	var doc reposDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unparseable repos json: %v\n%s", err, out)
	}
	if doc.Repos == nil {
		t.Fatal("repos is null rather than a list, so a consumer reading its length crashes on an empty config")
	}
	return *doc.Repos
}

func repoStateNamed(t *testing.T, rows []repoStateDoc, name string) repoStateDoc {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no repo named %q in %+v", name, rows)
	return repoStateDoc{}
}

// headerOnlyProfile parses clean and instruments nothing.
//
// This is the fourth state, and the one that is easy to miss: the run succeeds,
// the snapshot is stored ok, and the percentage is zero for want of a
// denominator rather than because anything measured zero.
const headerOnlyProfile = "mode: set\n"

// The repos listing has four states, not three, and the JSON has to tell all
// four apart. The table has always distinguished them in prose; before this
// there was no JSON at all, so an agent asking the one operational question got
// an aligned table to parse by column.
func TestReposJSONTellsAllFourStatesApart(t *testing.T) {
	dir := t.TempDir()
	measured := repoDir(t, dir, "measured", sampleProfile)
	failing := repoDir(t, dir, "failing", "")
	empty := repoDir(t, dir, "empty", headerOnlyProfile)
	never := repoDir(t, dir, "never", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s%s%s",
		dbPath,
		ingestRepoEntry("measured", measured, "coverage.out"),
		ingestRepoEntry("failing", failing, "missing.out"),
		ingestRepoEntry("empty", empty, "coverage.out"),
		ingestRepoEntry("never", never, "coverage.out")))

	// Everything but "never" gets collected, so that repo stays the one the
	// database has never heard of.
	for _, name := range []string{"measured", "failing", "empty"} {
		// failing exits non-zero by design, so its error is the fixture working.
		_, _, _ = runCLI(t, "collect", "--config", cfgPath, "--repo", name)
	}

	stdout, stderr, err := runCLI(t, "repos", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("repos --format json: %v (stderr: %s)", err, stderr)
	}
	rows := decodeRepos(t, stdout)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want all 4 configured repos: a repo that quietly drops out is how a broken cron goes unnoticed", len(rows))
	}

	// State three, the control. Without it the assertions below would pass on a
	// renderer that published nothing at all.
	if got := repoStateNamed(t, rows, "measured"); got.Coverage == nil {
		t.Errorf("measured: coverage is null for a repo that measured 60 percent")
	} else if got.Coverage.Value != 60 {
		t.Errorf("measured: coverage.value = %v, want 60", got.Coverage.Value)
	}

	// State one: configured, never collected.
	nvr := repoStateNamed(t, rows, "never")
	if nvr.HasSnapshot {
		t.Error("never: has_snapshot is true for a repo the database has never heard of")
	}
	if nvr.CollectedAt != nil {
		t.Errorf("never: collected_at = %q, want null rather than an empty string a consumer has to know means never", *nvr.CollectedAt)
	}
	if nvr.Coverage != nil {
		t.Errorf("never: coverage = %+v, want null", nvr.Coverage)
	}

	// State two: collected, every run failed.
	fail := repoStateNamed(t, rows, "failing")
	if !fail.HasSnapshot {
		t.Error("failing: has_snapshot is false, so a repo whose build is broken reads as one nobody has run")
	}
	if fail.Status != string(store.StatusFailed) {
		t.Errorf("failing: status = %q, want failed", fail.Status)
	}
	if fail.Coverage != nil {
		t.Errorf("failing: coverage = %+v, want null", fail.Coverage)
	}

	// State four: collected fine, measured nothing. This is the one that has to
	// be a null group rather than a pct of zero.
	blank := repoStateNamed(t, rows, "empty")
	if !blank.HasSnapshot || blank.Status == string(store.StatusFailed) {
		t.Fatalf("empty: want a successful snapshot, got status %q has_snapshot %v; the fixture is not exercising the header-only case",
			blank.Status, blank.HasSnapshot)
	}
	if blank.Coverage != nil {
		t.Errorf("empty: coverage = %+v for a profile that instrumented nothing. Zero percent is a number nobody measured, and it is indistinguishable from a repo that genuinely covers none of its code.", blank.Coverage)
	}

	// States two and four share has_snapshot and differ only in status, so the
	// pair has to be distinguishable or the JSON has collapsed two findings.
	if fail.Status == blank.Status {
		t.Errorf("a failed run and a run that measured nothing both report status %q, so the two cannot be told apart", fail.Status)
	}
}

// The default rendering must not change just because the flag now exists.
func TestReposMarkdownIsUnchangedByTheNewFlag(t *testing.T) {
	dir := t.TempDir()
	repo := repoDir(t, dir, "only", sampleProfile)
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		filepath.Join(dir, "metrics.db"), ingestRepoEntry("only", repo, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	bare, _, err := runCLI(t, "repos", "--config", cfgPath)
	if err != nil {
		t.Fatalf("repos: %v", err)
	}
	explicit, _, err := runCLI(t, "repos", "--config", cfgPath, "--format", "markdown")
	if err != nil {
		t.Fatalf("repos --format markdown: %v", err)
	}
	if bare != explicit {
		t.Errorf("the default rendering changed when the flag was named explicitly:\nbare:\n%s\nexplicit:\n%s", bare, explicit)
	}
}

// A never-collected repo and one whose every run failed must not read the same,
// which is the assertion the markdown version of this test has always made. The
// JSON has to hold the same line.
func TestReposJSONSurvivesAStoreWithNothingInIt(t *testing.T) {
	dir := t.TempDir()
	repo := repoDir(t, dir, "fresh", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("fresh", repo, "coverage.out")))

	// Open the database so it exists but hold no snapshots at all.
	ctx := context.Background()
	seed := openStore(t, dbPath)
	if _, err := seed.UpsertRepo(ctx, "fresh", repo); err != nil {
		t.Fatalf("seeding the repo: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed store: %v", err)
	}

	stdout, _, err := runCLI(t, "repos", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("repos --format json: %v", err)
	}
	row := repoStateNamed(t, decodeRepos(t, stdout), "fresh")
	if row.HasSnapshot || row.CollectedAt != nil || row.Coverage != nil {
		t.Errorf("a repo upserted but never collected reports %+v, want no snapshot and no measurements", row)
	}
}

// sampleLCOV is one source file with 3 of its 4 executable lines covered, which
// is 75.0 percent. Lines rather than statements, and never summed with a Go
// profile's counts.
const sampleLCOV = "SF:src/app.ts\nDA:1,1\nDA:2,1\nDA:3,0\nDA:4,2\nend_of_record\n"

// ingestLCOVEntry is ingestRepoEntry for a repo measured through an LCOV
// tracefile rather than a Go profile, which is what pytest-cov, istanbul and nyc
// leave behind.
func ingestLCOVEntry(name, path, artifact string) string {
	return fmt.Sprintf(
		"  - name: %s\n    path: %q\n    signals:\n      - name: coverage\n        artifact: %s\n        artifact_format: %s\n",
		name, path, artifact, config.FormatLCOV)
}

// A repo measured only through LCOV reports a number, in both renderings.
//
// This is the failure that prompted the second column. The listing knew about
// statement coverage alone, so a Python or TypeScript repo that had measured
// perfectly well answered "no coverage" in the table and a null group in the
// JSON, and on a mixed fleet that reads as a collection which did not work
// rather than as a different unit. The repo has no Go profile at all, so its
// statement group must stay null while the line group fills: the two are
// separate units and neither ever stands in for the other.
func TestReposReportsLineCoverageWhenThatIsWhatLanded(t *testing.T) {
	dir := t.TempDir()
	lines := repoDir(t, dir, "webapp", "")
	writeFile(t, filepath.Join(lines, "coverage.lcov"), sampleLCOV)
	stmts := repoDir(t, dir, "service", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestLCOVEntry("webapp", lines, "coverage.lcov"),
		ingestRepoEntry("service", stmts, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "repos", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("repos --format json: %v (stderr: %s)", err, stderr)
	}
	rows := decodeRepos(t, stdout)

	web := repoStateNamed(t, rows, "webapp")
	if web.CoverageLines == nil {
		t.Fatalf("webapp: coverage_lines is null for a repo whose tracefile measured 75 percent of 4 lines")
	}
	if web.CoverageLines.Value != 75 {
		t.Errorf("webapp: coverage_lines.value = %v, want 75", web.CoverageLines.Value)
	}
	if web.CoverageLines.Covered != 3 || web.CoverageLines.Total != 4 {
		t.Errorf("webapp: coverage_lines counts = %d/%d, want 3/4. The counts are the authority and the rate is derived from them",
			web.CoverageLines.Covered, web.CoverageLines.Total)
	}
	if web.Coverage != nil {
		t.Errorf("webapp: coverage = %+v, want null. Nothing measured statements here, and a line count standing in for one would be the two units summed",
			web.Coverage)
	}

	// The mirror, and the control. A build that filled both groups from whichever
	// counts it found would pass every assertion above.
	svc := repoStateNamed(t, rows, "service")
	if svc.Coverage == nil || svc.Coverage.Value != 60 {
		t.Errorf("service: coverage = %+v, want 60 percent", svc.Coverage)
	}
	if svc.CoverageLines != nil {
		t.Errorf("service: coverage_lines = %+v, want null for a repo measured by a Go profile", svc.CoverageLines)
	}

	// And the table says it too, in its own column rather than only in the JSON.
	table, stderr, err := runCLI(t, "repos", "--config", cfgPath)
	if err != nil {
		t.Fatalf("repos: %v (stderr: %s)", err, stderr)
	}
	if row := rowFor(t, table, "webapp"); !strings.Contains(row, "75.0%") {
		t.Errorf("webapp row does not carry its line coverage: %q", row)
	}
}
