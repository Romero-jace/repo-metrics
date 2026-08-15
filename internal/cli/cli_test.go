package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

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

// rowFor returns the single output line whose first cell is name.
//
// Asserting with strings.Contains over the whole of stdout cannot fail when a
// row carries another row's numbers, which is exactly the mix-up worth catching
// in a table that has one line per repo. Binding each value to its own row is
// what makes the assertion mean anything.
func rowFor(t *testing.T, out, name string) string {
	t.Helper()
	var found string
	for _, line := range strings.Split(out, "\n") {
		if firstCell(line) != name {
			continue
		}
		if found != "" {
			t.Fatalf("more than one row for %q in:\n%s", name, out)
		}
		found = line
	}
	if found == "" {
		t.Fatalf("no row for %q in:\n%s", name, out)
	}
	return found
}

// firstCell returns the leading label of a table row, understanding both the
// space-aligned tables collect and repos print and the pipe-delimited ones the
// markdown report renders.
//
// Without the markdown case, an assertion about the report table would have to
// fall back to searching the whole document, which is the exact weakness rowFor
// exists to remove. Two notes on the fixtures this constrains: no repo may be
// named "repo", because that is the literal first cell of the report's header
// row, and none may be named "---", the separator.
func firstCell(line string) string {
	if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "|") {
		return strings.TrimSpace(strings.Split(strings.Trim(trimmed, "|"), "|")[0])
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// wantInRow asserts every want appears on the one row belonging to name.
func wantInRow(t *testing.T, out, name string, wants ...string) {
	t.Helper()
	row := rowFor(t, out, name)
	for _, want := range wants {
		if !strings.Contains(row, want) {
			t.Errorf("want %q on the %s row, got %q", want, name, row)
		}
	}
}

// reportRepo is the subset of a rendered JSON repo row these tests assert on.
//
// The report's JSON is the assertion target rather than its markdown prose
// wherever the question is what this command put into the report, because the
// prose is the renderer's wording and can be reworded without the numbers
// moving.
type reportRepo struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	CollectedAt string  `json:"collected_at"`
	CoveragePct float64 `json:"coverage_pct"`
	HasSnapshot bool    `json:"has_snapshot"`
	Error       string  `json:"error"`
}

type reportDoc struct {
	Repos    []reportRepo `json:"repos"`
	Movers   []reportRepo `json:"movers"`
	Problems []reportRepo `json:"problems"`
}

func decodeReport(t *testing.T, out string) reportDoc {
	t.Helper()
	var doc reportDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unparseable report json: %v\n%s", err, out)
	}
	return doc
}

// jsonRepo is the JSON counterpart of rowFor: it binds every assertion to one
// named repo instead of to the document as a whole.
func jsonRepo(t *testing.T, rows []reportRepo, name string) reportRepo {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no repo named %q in %+v", name, rows)
	return reportRepo{}
}

func listedIn(rows []reportRepo, name string) bool {
	for _, r := range rows {
		if r.Name == name {
			return true
		}
	}
	return false
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

// The starter config has to agree with the defaults the tool itself applies.
// When the two are written out separately, changing a default leaves every
// freshly generated config quietly saying something the tool no longer means.
//
// This is a drift guard rather than a reproduction: the literals it replaced
// happened to match the defaults on the day they were written, so it holds
// either way today and only earns its keep the next time a default moves.
func TestInitWritesTheRealDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo-metrics.yaml")

	if _, stderr, err := runCLI(t, "init", "--config", path); err != nil {
		t.Fatalf("init: %v (stderr: %s)", err, stderr)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config init wrote does not load: %v", err)
	}

	if cfg.Database != config.DefaultDatabase {
		t.Errorf("database = %q, want the default %q", cfg.Database, config.DefaultDatabase)
	}
	if time.Duration(cfg.Window) != config.DefaultWindow {
		t.Errorf("window = %s, want the default %s", cfg.Window, config.DefaultWindow)
	}
	if cfg.MinStatements != config.DefaultMinStatements {
		t.Errorf("min_statements = %d, want the default %d", cfg.MinStatements, config.DefaultMinStatements)
	}
	if cfg.MinRepoDelta != config.DefaultMinRepoDelta {
		t.Errorf("min_repo_delta = %v, want the default %v", cfg.MinRepoDelta, config.DefaultMinRepoDelta)
	}

	// The window has to be spelled in a unit the config loader understands.
	// time.ParseDuration tops out at hours, so a "7d" written here would be a
	// load error even though --window takes one.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if strings.Contains(string(contents), "window: 7d") {
		t.Error("the window is written in days, which config.Load cannot parse")
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
	// Bound to the row, not to stdout as a whole: with two repos in the run, a
	// loose Contains passes when "failed" is sitting on the healthy repo's line.
	wantInRow(t, stdout, "broken", "failed")
	wantInRow(t, stdout, "healthy", "ok", "60.0%")
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
	// from "it never ran".
	//
	// Asserting only that LatestSnapshot comes back nil would not show that:
	// nil is also what a repo collect never wrote a row for returns, so the
	// assertion would hold just as well if collect had silently stored nothing.
	// LatestSnapshotAny is what proves the row is really there.
	broke := repoByName(t, st, "broken")
	failed, err := st.LatestSnapshotAny(context.Background(), broke.ID)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if failed == nil {
		t.Fatal("collect recorded nothing for the broken repo, so nothing downstream can report it as failing")
	}
	if failed.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q", failed.Status, store.StatusFailed)
	}
	if failed.Error == "" {
		t.Error("a failed snapshot with no error text tells the reader nothing about what broke")
	}

	// And it still must not be usable as a head or a baseline, since it carries
	// no numbers.
	usable, err := st.LatestSnapshot(context.Background(), broke.ID)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if usable != nil {
		t.Errorf("a failed snapshot should not be usable as a head, got %+v", usable)
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

// A repo whose every collection failed is the one the reader most needs to see,
// and it used to be the one hardest to spot: LatestSnapshot skips failed rows,
// so the repo reached the renderer with no head at all and came out looking
// like a pristine repo sitting at 0.0 percent, collected never.
//
// The assertions are against the json rather than the markdown prose so that
// they pin the values this command puts into the report, not the wording the
// template happens to use around them.
func TestReportMarksARepoWhoseEveryRunFailed(t *testing.T) {
	dir := t.TempDir()
	// Ingest mode with no profile at the configured path, so collection fails
	// and the only row this repo ever gets is a failed one.
	broken := repoDir(t, dir, "broken", "")
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("broken", broken, "missing.out"),
		ingestRepoEntry("healthy", healthy, "coverage.out")))

	if _, _, err := runCLI(t, "collect", "--config", cfgPath); err == nil {
		t.Fatal("want a non-zero status from the run that failed a repo")
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("report --format json: %v (stderr: %s)", err, stderr)
	}

	type repoJSON struct {
		Name        string  `json:"name"`
		Status      string  `json:"status"`
		CollectedAt string  `json:"collected_at"`
		CoveragePct float64 `json:"coverage_pct"`
		Error       string  `json:"error"`
	}
	var parsed struct {
		Repos    []repoJSON `json:"repos"`
		Movers   []repoJSON `json:"movers"`
		Problems []repoJSON `json:"problems"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("unparseable json: %v\n%s", err, stdout)
	}

	var got *repoJSON
	for i := range parsed.Repos {
		if parsed.Repos[i].Name == "broken" {
			got = &parsed.Repos[i]
		}
	}
	if got == nil {
		t.Fatalf("the broken repo dropped out of the report entirely: %s", stdout)
	}
	if got.Status == string(store.StatusOK) {
		t.Errorf("a repo with no usable snapshot is not ok, got status %q", got.Status)
	}
	if got.Status != string(store.StatusFailed) {
		t.Errorf("status = %q, want %q", got.Status, store.StatusFailed)
	}
	// Empty here is what the renderer turns into "never", which is the lie:
	// this repo did run, and the timestamp of the last attempt is what tells
	// the reader whether the failure is from last night or from March.
	if got.CollectedAt == "" {
		t.Error("want the time of the last attempt, got nothing, which reads as never collected")
	}
	if got.Error == "" {
		t.Error("want the recorded failure text so the report says what broke")
	}

	// It must not lead the report on a fabricated cliff either.
	for _, m := range parsed.Movers {
		if m.Name == "broken" {
			t.Errorf("a crashed test command is not this week's biggest move: %+v", m)
		}
	}
	var listed bool
	for _, p := range parsed.Problems {
		if p.Name == "broken" {
			listed = true
		}
	}
	if !listed {
		t.Error("want the failing repo under collection problems")
	}

	// The healthy repo is untouched by any of this.
	for _, r := range parsed.Repos {
		if r.Name == "healthy" && r.CoveragePct != 60 {
			t.Errorf("healthy repo = %+v, want 60 percent", r)
		}
	}
}

// Deliberately not covered, so nobody reopens it: the four error returns in
// reportInputs (Repos, LatestSnapshot, LatestSnapshotAny, MetricsFor). Reaching
// any of them needs an injected failing database driver, and the resulting test
// would assert that fmt.Errorf wraps a string correctly rather than that the
// command does anything useful. Same call as the store's rows.Scan branches.

// A repo the database has never heard of is the most common degraded state
// there is: someone adds a repo to the config and runs report before collect.
// reportInputs has a branch for exactly that, and until now nothing ran it.
//
// Two ways to get it wrong, and the assertions here are aimed at both. Drop the
// repo and it vanishes from the report, which is how a cron job that never ran
// goes unnoticed for a month. Hand it a head instead and it arrives as a
// healthy repo sitting at 0.0 percent, which is this project's recurring bug:
// something unmeasured presented as a measurement of zero.
func TestReportIncludesARepoTheDatabaseHasNeverHeardOf(t *testing.T) {
	dir := t.TempDir()
	collected := repoDir(t, dir, "collected", sampleProfile)
	// A real profile sits in pending too. It is never read, and that is the
	// point: what puts a repo in the report is a row in the database, not a file
	// on disk, so the fixture must not let a disk read stand in for a collection.
	pending := repoDir(t, dir, "pending", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("collected", collected, "coverage.out"),
		ingestRepoEntry("pending", pending, "coverage.out")))

	// --repo is load-bearing. A bare collect would collect pending as well, give
	// it a repos row and a snapshot, and quietly move this test onto a different
	// branch of reportInputs than the one it is named after.
	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--repo", "collected"); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("report --format json: %v (stderr: %s)", err, stderr)
	}
	doc := decodeReport(t, stdout)

	// Present at all. This is the half that fails if the branch just skips.
	got := jsonRepo(t, doc.Repos, "pending")

	// And not fabricated. Every one of these is what a head the command invented
	// would flip.
	if got.HasSnapshot {
		t.Error("has_snapshot is true for a repo with no row in the database, so a consumer would chart its zeros")
	}
	if got.Status == string(store.StatusOK) {
		t.Errorf("status = %q, and a repo nobody has ever collected is not ok", got.Status)
	}
	if got.CollectedAt != "" {
		t.Errorf("collected_at = %q, want nothing: this repo has never been collected", got.CollectedAt)
	}
	if got.Error != "" {
		t.Errorf("error = %q, but nothing ran, so nothing broke", got.Error)
	}
	// Labeled rather than claimed: this one cannot currently fail. delta.Compute
	// only sets IsMover once a baseline exists, and reportInputs fetches no
	// baseline for a repo with no head, so pending has no delta to clear a
	// threshold with. Kept as a guard against that changing, not offered as a
	// proven assertion.
	if listedIn(doc.Movers, "pending") {
		t.Error("a repo that was never collected cannot be this week's biggest move")
	}

	// Anti-vacuity control: the collected repo still carries its own real
	// numbers, so a change that flattened every row would not pass this.
	if healthy := jsonRepo(t, doc.Repos, "collected"); !healthy.HasSnapshot || healthy.CoveragePct != 60 {
		t.Errorf("collected repo = %+v, want a snapshot at 60 percent", healthy)
	}

	// The markdown says the same thing, bound to its own row. A Contains over
	// the whole document would pass while pending carried collected's coverage.
	md, stderr, err := runCLI(t, "report", "--config", cfgPath)
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}
	// Shape, not wording: a row for a repo where nothing was measured must carry
	// no numeral anywhere. That one predicate covers "not collected", "not
	// measured", and whatever prose a future degraded state gets, it survives a
	// reword of the template, and it still fails on both of the ways to get this
	// wrong: a fabricated 0.0% and a fabricated zero timestamp are both digits.
	if row := rowFor(t, md, "pending"); strings.ContainsFunc(row, unicode.IsDigit) {
		t.Errorf("the row for a repo that was never collected carries a number: %q", row)
	}
	// The measured row is asserted exactly, which is the anti-vacuity control:
	// without it, a renderer that emitted prose for every repo would pass.
	wantInRow(t, md, "collected", "60.0%")
}

// "Registered but never collected" and "ran and broke every time" both reach
// reportInputs with LatestSnapshot returning nil, and they call for opposite
// actions: go run collect versus go fix your build. The LatestSnapshotAny
// fallback is what keeps them apart, by attaching the failed row as the head so
// it carries a status, a time, and what broke.
//
// TestReportMarksARepoWhoseEveryRunFailed already pins what the failed row
// itself carries. This test is only the contrast between the two, which is the
// thing neither test can check on its own.
func TestReportTellsNeverCollectedFromEveryRunFailed(t *testing.T) {
	dir := t.TempDir()
	// Ingest mode pointed at a profile that is not there, so every run fails.
	broken := repoDir(t, dir, "broken", "")
	registered := repoDir(t, dir, "registered", "")
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("broken", broken, "missing.out"),
		ingestRepoEntry("registered", registered, "missing.out")))

	// registered gets a repos row and nothing else, which is what a first run
	// interrupted between the upsert and the insert leaves behind. It is seeded
	// through the store rather than by collecting because any collect would
	// leave a snapshot too, failed or not, and that is the other case.
	ctx := context.Background()
	seed := openStore(t, dbPath)
	if _, err := seed.UpsertRepo(ctx, "registered", registered); err != nil {
		t.Fatalf("seeding the repo row: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed store: %v", err)
	}

	if _, _, err := runCLI(t, "collect", "--config", cfgPath, "--repo", "broken"); err == nil {
		t.Fatal("want a non-zero status from a repo that could not be collected")
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json")
	if err != nil {
		t.Fatalf("report --format json: %v (stderr: %s)", err, stderr)
	}
	doc := decodeReport(t, stdout)
	brokeRow := jsonRepo(t, doc.Repos, "broken")
	neverRow := jsonRepo(t, doc.Repos, "registered")

	// The whole point of the test: the two rows must not read the same.
	if brokeRow.Status == neverRow.Status {
		t.Errorf("both states render as status %q, so the report cannot tell go-fix-your-build from go-run-collect",
			brokeRow.Status)
	}
	if brokeRow.HasSnapshot == neverRow.HasSnapshot {
		t.Errorf("has_snapshot is %v for both, so a consumer cannot tell them apart either", brokeRow.HasSnapshot)
	}
	// The timestamp is the actionable half: it says whether the breakage is from
	// last night or from March.
	if brokeRow.CollectedAt == "" {
		t.Error("want the time of the last attempt on the repo that ran and broke")
	}
	if neverRow.CollectedAt != "" {
		t.Errorf("collected_at = %q on a repo that has never been collected", neverRow.CollectedAt)
	}
	// Neither is a measurement, so neither may lead the report on a cliff.
	//
	// Same caveat as the one in the test above, and worth stating twice because
	// the fabricated cliff is the failure this project keeps rediscovering: this
	// check cannot currently fail. Two independent things already prevent it, and
	// neither is reachable from here. reportInputs fetches no baseline for a
	// failed head, so there is no delta; and even with one, the renderer excludes
	// failed and never-collected repos from Movers. That second gate lives in
	// internal/report, which this unit does not own, so this assertion is not
	// proven by any revert available in this package.
	if listedIn(doc.Movers, "broken") || listedIn(doc.Movers, "registered") {
		t.Errorf("a repo with no numbers cannot be a mover: %+v", doc.Movers)
	}
}

// Deliberately not covered, so nobody reopens it: the render-to-file and Close
// failure branches of writeReport. Both need a file handle that fails partway
// through a write, which is not portably arrangeable, and the assertion would be
// about error wrapping rather than about behavior. The failure that is worth a
// test is the one a user actually hits, which is a path that cannot be created,
// and it is covered below.
func TestReportWriteDestinationsAndFormats(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	t.Run("markdown to stdout", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "report", "--config", cfgPath)
		if err != nil {
			t.Fatalf("report: %v (stderr: %s)", err, stderr)
		}
		wantInRow(t, stdout, "healthy", "60.0%")
		if stderr != "" {
			t.Errorf("a clean run should say nothing on stderr, got %q", stderr)
		}
	})

	t.Run("json to stdout", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json")
		if err != nil {
			t.Fatalf("report --format json: %v (stderr: %s)", err, stderr)
		}
		if got := jsonRepo(t, decodeReport(t, stdout).Repos, "healthy"); got.CoveragePct != 60 {
			t.Errorf("json = %+v, want healthy at 60", got)
		}
	})

	t.Run("markdown to a file", func(t *testing.T) {
		outPath := filepath.Join(dir, "report.md")
		stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--out", outPath)
		if err != nil {
			t.Fatalf("report --out: %v (stderr: %s)", err, stderr)
		}
		// stdout gets the confirmation and not the report. A --out run that also
		// printed the report would hand a pipeline the document twice.
		if !strings.Contains(stdout, outPath) {
			t.Errorf("want the written path confirmed on stdout, got %q", stdout)
		}
		if strings.Contains(stdout, "| repo |") {
			t.Errorf("the report went to stdout as well as to the file: %q", stdout)
		}
		written, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("reading the written report: %v", err)
		}
		wantInRow(t, string(written), "healthy", "60.0%")
	})

	t.Run("json to a file", func(t *testing.T) {
		outPath := filepath.Join(dir, "report.json")
		stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--out", outPath, "--format", "json")
		if err != nil {
			t.Fatalf("report --out --format json: %v (stderr: %s)", err, stderr)
		}
		if !strings.Contains(stdout, outPath) {
			t.Errorf("want the written path confirmed on stdout, got %q", stdout)
		}
		written, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("reading the written report: %v", err)
		}
		// The format flag has to reach the file, not just stdout. Writing
		// markdown into a .json a pipeline is about to parse is the failure.
		if got := jsonRepo(t, decodeReport(t, string(written)).Repos, "healthy"); got.CoveragePct != 60 {
			t.Errorf("the written json = %+v, want healthy at 60", got)
		}
	})

	// Both unwritable cases assert the same three things, because all three are
	// what "exit 1" is made of: an error back to Run, an explanation on stderr,
	// and no report on stdout pretending the run worked.
	for name, outPath := range map[string]string{
		"a parent directory that is not there": filepath.Join(dir, "no-such-dir", "report.md"),
		"a path that is a directory":           repoDir(t, dir, "already-a-directory", ""),
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--out", outPath)
			if err == nil {
				t.Fatal("want an error so the process exits 1 rather than claiming it wrote a report")
			}
			if !strings.Contains(stderr, outPath) {
				t.Errorf("want the path that could not be written named on stderr, got %q", stderr)
			}
			if stdout != "" {
				t.Errorf("want nothing on stdout when nothing was written, got %q", stdout)
			}
		})
	}
}

// closedWriter is stdout after the thing on the other end of the pipe has gone,
// which is what `repo-metrics report | head` looks like.
type closedWriter struct{}

func (closedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A report that could not be written must not exit 0. Rendering to stdout is
// the default path, so a swallowed error here means a cron job that pipes the
// report somewhere reports success on the weeks it delivered nothing.
func TestReportSurfacesAFailureToWriteToStdout(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("healthy", healthy, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	for _, format := range []string{"markdown", "json"} {
		t.Run(format, func(t *testing.T) {
			var stderr bytes.Buffer
			// runCLI is not usable here: it owns both buffers, and the writer is
			// the fixture.
			err := cli.Run([]string{"report", "--config", cfgPath, "--format", format}, closedWriter{}, &stderr)
			if err == nil {
				t.Fatal("want an error so the process exits 1 rather than claiming it wrote a report")
			}
			// Asserted as non-empty rather than matched: the wording belongs to
			// the renderer and pinning it here would break on a reword.
			if stderr.Len() == 0 {
				t.Error("the failure has to reach the user, not only the exit status")
			}
		})
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
	if !strings.Contains(stdout, "REPO") {
		t.Errorf("want a header row, got %q", stdout)
	}
	// Each value is checked against its own row. Searching the whole table for
	// "ok" and "60.0%" passes even when those sit on the wrong repo's line.
	wantInRow(t, stdout, "healthy", "ok", "60.0%")
	wantInRow(t, stdout, "never", "never collected")
	if strings.Contains(rowFor(t, stdout, "never"), "60.0%") {
		t.Error("the uncollected repo is carrying the collected one's coverage")
	}
}

// "Never ran" and "ran and broke every time" are the two states this subcommand
// exists to tell apart, because one means run collect and the other means go fix
// your build. LatestSnapshot returns nil for both, so consulting only it renders
// them identically.
func TestReposDistinguishesNeverCollectedFromAlwaysFailing(t *testing.T) {
	dir := t.TempDir()
	// Ingest mode pointed at a profile that is not there, so every run fails.
	broken := repoDir(t, dir, "broken", "")
	never := repoDir(t, dir, "never", "")
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("broken", broken, "missing.out"),
		ingestRepoEntry("never", never, "coverage.out")))

	if _, _, err := runCLI(t, "collect", "--config", cfgPath, "--repo", "broken"); err == nil {
		t.Fatal("want a non-zero status from a repo that could not be collected")
	}

	stdout, stderr, err := runCLI(t, "repos", "--config", cfgPath)
	if err != nil {
		t.Fatalf("repos: %v (stderr: %s)", err, stderr)
	}

	brokenRow := rowFor(t, stdout, "broken")
	neverRow := rowFor(t, stdout, "never")
	if !strings.Contains(brokenRow, string(store.StatusFailed)) {
		t.Errorf("want the failing repo shown as failed, got %q", brokenRow)
	}
	if !strings.Contains(brokenRow, "UTC") {
		t.Errorf("want the time of the last attempt on the failing repo's row, got %q", brokenRow)
	}
	if !strings.Contains(neverRow, "never") {
		t.Errorf("want the uncollected repo shown as never collected, got %q", neverRow)
	}
	if strings.Contains(neverRow, string(store.StatusFailed)) {
		t.Errorf("a repo that never ran has not failed, got %q", neverRow)
	}
	// The point of the whole test: the two rows must not read the same.
	if strings.TrimSpace(strings.TrimPrefix(brokenRow, "broken")) ==
		strings.TrimSpace(strings.TrimPrefix(neverRow, "never")) {
		t.Errorf("the two states render identically:\n%s\n%s", brokenRow, neverRow)
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
