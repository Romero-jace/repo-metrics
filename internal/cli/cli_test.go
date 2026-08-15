package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/cli"
	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// sampleProfile is two packages: demo/a covers 2 of 4 statements, demo/b covers
// 1 of 1. The repo total is therefore 3 of 5, which is 60.0 percent, and not the
// 75 percent you would get by averaging the two package rates.
const sampleProfile = `mode: set
example.com/demo/a/a.go:1.1,3.2 2 1
example.com/demo/a/b.go:4.1,6.2 2 0
example.com/demo/b/b.go:1.1,2.2 1 1
`

func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = cli.Run(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// repoDir makes a directory that passes config validation, optionally with a
// coverage profile already in it.
func repoDir(t *testing.T, parent, name, profile string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if profile != "" {
		writeFile(t, filepath.Join(dir, "coverage.out"), profile)
	}
	return dir
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "repo-metrics.yaml")
	writeFile(t, path, body)
	return path
}

// ingestRepoEntry is a repo with no command, so collection just reads whatever
// is on disk. That keeps these tests hermetic: no test suite gets run.
func ingestRepoEntry(name, path, coverprofile string) string {
	return fmt.Sprintf("  - name: %s\n    path: %q\n    coverprofile: %s\n", name, path, coverprofile)
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func repoByName(t *testing.T, st *store.Store, name string) store.Repo {
	t.Helper()
	repos, err := st.Repos(context.Background())
	if err != nil {
		t.Fatalf("listing repos: %v", err)
	}
	for _, r := range repos {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no repo named %q in the database (have %v)", name, repos)
	return store.Repo{}
}

// coverageTotals mirrors what the report does: sum first, divide once.
func coverageTotals(metrics []store.Metric) (covered, total int) {
	for _, m := range metrics {
		if m.Scope == "" {
			continue
		}
		switch m.Key {
		case collect.KeyCoveredStmts:
			covered += int(m.Value)
		case collect.KeyTotalStmts:
			total += int(m.Value)
		}
	}
	return covered, total
}

func TestRunNoArguments(t *testing.T) {
	stdout, stderr, err := runCLI(t)
	if err == nil {
		t.Error("want an error so the process exits non-zero")
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("want usage on stderr, got %q", stderr)
	}
	if stdout != "" {
		t.Errorf("want nothing on stdout, got %q", stdout)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	_, stderr, err := runCLI(t, "collct")
	if err == nil {
		t.Error("want an error for an unknown subcommand")
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("want usage on stderr, got %q", stderr)
	}
}

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			_, stderr, err := runCLI(t, arg)
			if err != nil {
				t.Errorf("asking for help is not a failure, got %v", err)
			}
			if !strings.Contains(stderr, "repo-metrics collect") {
				t.Errorf("want the subcommand list, got %q", stderr)
			}
		})
	}
}

// Generated text is meant to read like a person wrote it, and the house style
// says no em dashes anywhere in output.
func TestUsageHasNoEmDash(t *testing.T) {
	_, stderr, _ := runCLI(t, "help")
	if strings.Contains(stderr, "—") {
		t.Error("usage text contains an em dash")
	}
}

func TestInitWritesAConfigThatLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo-metrics.yaml")

	stdout, stderr, err := runCLI(t, "init", "--config", path)
	if err != nil {
		t.Fatalf("init: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stdout, path) {
		t.Errorf("want the written path on stdout, got %q", stdout)
	}

	// The round trip is the point. A starter config the tool cannot load is
	// worse than no starter config, because the first thing a new user sees is
	// a validation error from a file this program just wrote.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config init wrote does not load: %v", err)
	}
	if len(cfg.Repos) == 0 {
		t.Error("want at least one live example repo")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	// Both collection modes should be visible in the file someone is about to
	// edit, even though only one of them can be live.
	if !strings.Contains(string(contents), "command:") {
		t.Error("want a command-mode example")
	}
	if !strings.Contains(string(contents), "max_age") {
		t.Error("want an ingest-mode example with a max_age")
	}
}

func TestInitRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo-metrics.yaml")
	writeFile(t, path, "database: ./mine.db\n")

	if _, stderr, err := runCLI(t, "init", "--config", path); err == nil {
		t.Errorf("want a refusal, stderr was %q", stderr)
	}

	kept, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(kept) != "database: ./mine.db\n" {
		t.Fatalf("the existing config was overwritten: %q", kept)
	}

	if _, stderr, err := runCLI(t, "init", "--config", path, "--force"); err != nil {
		t.Fatalf("init --force: %v (stderr: %s)", err, stderr)
	}
	replaced, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(replaced) == "database: ./mine.db\n" {
		t.Error("--force did not overwrite")
	}
}

func TestCollectIngestMode(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath)
	if err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}
	for _, want := range []string{"healthy", "ok", "60.0%"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in the progress output, got %q", want, stdout)
		}
	}

	st := openStore(t, dbPath)
	repo := repoByName(t, st, "healthy")
	snap, err := st.LatestSnapshot(context.Background(), repo.ID)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("collect wrote no snapshot")
	}
	if snap.Status != store.StatusOK {
		t.Errorf("status = %q, want ok (error: %s)", snap.Status, snap.Error)
	}

	metrics, err := st.MetricsFor(context.Background(), snap.ID)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	covered, total := coverageTotals(metrics)
	if covered != 3 || total != 5 {
		t.Errorf("coverage = %d/%d, want 3/5", covered, total)
	}
}

func TestCollectContinuesPastAFailingRepo(t *testing.T) {
	dir := t.TempDir()
	// Ingest mode with nothing at the configured path: the profile is missing,
	// so this repo fails. The config still validates, which is what makes it a
	// usable fixture for "keep going".
	broken := repoDir(t, dir, "broken", "")
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("broken", broken, "missing.out"),
		ingestRepoEntry("healthy", healthy, "coverage.out")))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath)
	if err == nil {
		t.Error("want an error so the exit status is 1 when a repo failed")
	}
	if !strings.Contains(stdout, "broken") || !strings.Contains(stdout, "failed") {
		t.Errorf("want the failing repo reported on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "broken") {
		t.Errorf("want a diagnostic for the failing repo on stderr, got %q", stderr)
	}

	// The whole point: the healthy repo was still collected.
	st := openStore(t, dbPath)
	snap, err := st.LatestSnapshot(context.Background(), repoByName(t, st, "healthy").ID)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if snap == nil || snap.Status != store.StatusOK {
		t.Fatalf("the healthy repo was not collected: %+v", snap)
	}

	// The failed run is recorded too, so that repos can tell "it broke" apart
	// from "it never ran". LatestSnapshot skips it because it carries no
	// numbers, which is why this asserts on the repo row instead.
	broke := repoByName(t, st, "broken")
	failed, err := st.LatestSnapshot(context.Background(), broke.ID)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if failed != nil {
		t.Errorf("a failed snapshot should not be usable as a head, got %+v", failed)
	}
}

func TestCollectSingleRepo(t *testing.T) {
	dir := t.TempDir()
	first := repoDir(t, dir, "first", sampleProfile)
	second := repoDir(t, dir, "second", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("first", first, "coverage.out"),
		ingestRepoEntry("second", second, "coverage.out")))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--repo", "second")
	if err != nil {
		t.Fatalf("collect --repo: %v (stderr: %s)", err, stderr)
	}
	if strings.Contains(stdout, "first") {
		t.Errorf("--repo should have collected only second, got %q", stdout)
	}

	if _, _, err := runCLI(t, "collect", "--config", cfgPath, "--repo", "nope"); err == nil {
		t.Error("want an error for a repo name that is not in the config")
	}
}

// A stale artifact is reported as stale, not withheld and not presented as
// current, so the run still exits 0. This is the other half of the exit-code
// contract: only an outright failure earns a non-zero status.
func TestCollectReportsAStaleArtifactAsPartial(t *testing.T) {
	dir := t.TempDir()
	stale := repoDir(t, dir, "stale", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	profile := filepath.Join(stale, "coverage.out")
	twoDaysAgo := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(profile, twoDaysAgo, twoDaysAgo); err != nil {
		t.Fatalf("backdating the profile: %v", err)
	}

	cfgPath := writeConfig(t, dir, fmt.Sprintf(
		"database: %q\nrepos:\n  - name: stale\n    path: %q\n    coverprofile: coverage.out\n    max_age: 24h\n",
		dbPath, stale))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath)
	if err != nil {
		t.Fatalf("a stale artifact is a downgrade, not a failure, got %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stdout, "partial") {
		t.Errorf("want partial in the progress output, got %q", stdout)
	}
	if !strings.Contains(stderr, "stale") {
		t.Errorf("want a staleness diagnostic on stderr, got %q", stderr)
	}

	st := openStore(t, dbPath)
	snap, err := st.LatestSnapshot(context.Background(), repoByName(t, st, "stale").ID)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if snap == nil || snap.Status != store.StatusPartial {
		t.Fatalf("snapshot = %+v, want a partial one", snap)
	}
	if snap.Error != "" {
		t.Errorf("a warning is not an error, got %q", snap.Error)
	}

	// The numbers are still there. Partial means "believe this less", not
	// "there is nothing here".
	metrics, err := st.MetricsFor(context.Background(), snap.ID)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if covered, total := coverageTotals(metrics); covered != 3 || total != 5 {
		t.Errorf("coverage = %d/%d, want 3/5", covered, total)
	}
}

func TestSubcommandsRejectPositionalArguments(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	// The dangerous shape: flag parsing stops at the positional, so --config is
	// never seen and the default path is used instead of the one written here.
	_, stderr, err := runCLI(t, "collect", "healthy", "--config", cfgPath)
	if err == nil {
		t.Fatal("want an error rather than a run against a different config")
	}
	if !strings.Contains(stderr, "positional") {
		t.Errorf("want an explanation of the leftover argument, got %q", stderr)
	}

	if _, err := os.Stat(dbPath); err == nil {
		t.Error("the rejected run should not have opened the database")
	}
}

func TestReportSaysThereIsNoBaseline(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--window", "7d")
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}
	// One snapshot is not a trend. Inventing a delta from it would be the
	// tool's own version of the silent wrong answer it exists to catch.
	if !strings.Contains(stdout, "no baseline yet") {
		t.Errorf("want the report to admit there is no baseline, got %q", stdout)
	}
	if !strings.Contains(stdout, "60.0%") {
		t.Errorf("want the current coverage in the report, got %q", stdout)
	}
}

// The baseline is picked by reportInputs, so it needs a repo with history. A
// backdated snapshot is seeded through the store API rather than by collecting
// twice, because two collections a second apart would both fall inside the
// window and neither could be the other's baseline.
func TestReportComputesADeltaAgainstAnOlderSnapshot(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	ctx := context.Background()
	seed := openStore(t, dbPath)
	repoID, err := seed.UpsertRepo(ctx, "healthy", healthy)
	if err != nil {
		t.Fatalf("seeding the repo: %v", err)
	}
	// A fortnight back, at 1 of 5 statements covered, so the delta against the
	// fixture's 3 of 5 is exactly +40 points.
	if _, err := seed.InsertSnapshot(ctx, store.Snapshot{
		RepoID:      repoID,
		CollectedAt: time.Now().Add(-14 * 24 * time.Hour),
		Status:      store.StatusOK,
	}, []store.Metric{
		{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/a", Value: 0},
		{Key: collect.KeyTotalStmts, Scope: "example.com/demo/a", Value: 4},
		{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/b", Value: 1},
		{Key: collect.KeyTotalStmts, Scope: "example.com/demo/b", Value: 1},
	}); err != nil {
		t.Fatalf("seeding a baseline snapshot: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed store: %v", err)
	}

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--window", "7d")
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stdout, "+40.0 pts") {
		t.Errorf("want a +40.0 pts move against the backdated snapshot, got %q", stdout)
	}
	if strings.Contains(stdout, "no baseline yet") {
		t.Errorf("there is a baseline, the report should not say otherwise: %q", stdout)
	}
}

func TestReportJSONAndOutFile(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("report --format json: %v (stderr: %s)", err, stderr)
	}

	var parsed struct {
		Repos []struct {
			Name        string   `json:"name"`
			CoveragePct float64  `json:"coverage_pct"`
			HasBaseline bool     `json:"has_baseline"`
			Delta       *float64 `json:"coverage_delta_points"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("report --format json emitted unparseable output: %v\n%s", err, stdout)
	}
	if len(parsed.Repos) != 1 {
		t.Fatalf("want one repo in the json, got %d", len(parsed.Repos))
	}
	got := parsed.Repos[0]
	if got.Name != "healthy" || got.CoveragePct != 60 {
		t.Errorf("json = %+v, want healthy at 60", got)
	}
	if got.HasBaseline || got.Delta != nil {
		t.Errorf("want a null delta with only one snapshot, got %+v", got)
	}

	outPath := filepath.Join(dir, "report.md")
	if _, stderr, err := runCLI(t, "report", "--config", cfgPath, "--out", outPath); err != nil {
		t.Fatalf("report --out: %v (stderr: %s)", err, stderr)
	}
	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading the written report: %v", err)
	}
	if !strings.Contains(string(written), "healthy") {
		t.Errorf("the written report does not mention the repo: %q", written)
	}

	if _, _, err := runCLI(t, "report", "--config", cfgPath, "--format", "yaml"); err == nil {
		t.Error("want an error for an unsupported format rather than a silent default")
	}
	if _, _, err := runCLI(t, "report", "--config", cfgPath, "--window", "7"); err == nil {
		t.Error("want an error for a duration with no unit")
	}
}

func TestReposListsCollectedAndUncollected(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	never := repoDir(t, dir, "never", "")
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("healthy", healthy, "coverage.out"),
		ingestRepoEntry("never", never, "coverage.out")))

	// Collect only one of the two, which is exactly the state this subcommand
	// exists to make visible.
	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--repo", "healthy"); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "repos", "--config", cfgPath)
	if err != nil {
		t.Fatalf("repos: %v (stderr: %s)", err, stderr)
	}
	for _, want := range []string{"REPO", "healthy", "ok", "60.0%", "never", "no usable snapshot"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want %q in the repos table, got %q", want, stdout)
		}
	}
}

func TestSubcommandsReportAMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	for _, sub := range []string{"collect", "report", "repos"} {
		t.Run(sub, func(t *testing.T) {
			_, stderr, err := runCLI(t, sub, "--config", missing)
			if err == nil {
				t.Fatal("want an error for a config that is not there")
			}
			if !strings.Contains(stderr, "repo-metrics init") {
				t.Errorf("want a pointer at init, got %q", stderr)
			}
		})
	}
}
