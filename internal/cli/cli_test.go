package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/Romero-jace/repo-metrics/internal/cli"
	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/report"
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

// ingestRepoEntry is a repo with one coverage signal and no command, so
// collection just reads whatever is on disk. That keeps these tests hermetic:
// no test suite gets run.
func ingestRepoEntry(name, path, artifact string) string {
	return fmt.Sprintf(
		"  - name: %s\n    path: %q\n    signals:\n      - name: coverage\n        artifact: %s\n        artifact_format: %s\n",
		name, path, artifact, config.FormatGoCoverprofile)
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

// hasRowFor is rowFor's negative. rowFor fails when a row is missing, which is
// the wrong shape for asserting that a narrowed report left a repo out, and a
// plain Contains over the document cannot be used either: "moved" is a repo
// name here and also a word in the "What moved" heading.
func hasRowFor(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		if firstCell(line) == name {
			return true
		}
	}
	return false
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

// reportCoverage and reportTests are the nested measurement groups.
//
// They are decoded through pointers everywhere they appear, which is the whole
// reason the report nests them: a repo that measured nothing has no group at
// all, and a struct of plain floats would decode that absence straight back
// into 0.0. That is this project's recurring bug, and a test suite that reads
// the wire the unsafe way cannot catch it.
type reportCoverage struct {
	Value   float64  `json:"value"`
	Covered int      `json:"covered"`
	Total   int      `json:"total"`
	Delta   *float64 `json:"delta"`
}

// reportTests is the shape every signal but coverage renders, since they all
// share one SignalView on the wire. What the value means is answered once by the
// envelope's signal catalog rather than by a per-signal key name.
type reportTests struct {
	Value float64  `json:"value"`
	Delta *float64 `json:"delta"`
}

// reportRepo is the subset of a rendered JSON repo row these tests assert on.
//
// The report's JSON is the assertion target rather than its markdown prose
// wherever the question is what this command put into the report, because the
// prose is the renderer's wording and can be reworded without the numbers
// moving.
type reportRepo struct {
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	CollectedAt string          `json:"collected_at"`
	Coverage    *reportCoverage `json:"coverage"`
	Tests       *reportTests    `json:"tests"`
	HasSnapshot bool            `json:"has_snapshot"`
	HasBaseline bool            `json:"has_baseline"`
	// Degraded is three-state, so a pointer: null is a snapshot from before
	// anything recorded whether its run was clean, and decoding that into false
	// would assert something nobody checked.
	Degraded *bool  `json:"degraded"`
	Error    string `json:"error"`
}

// reportDoc holds the three sections as pointers to slices rather than slices.
//
// Not because a plain slice cannot see the difference. json.Unmarshal leaves a
// []reportRepo nil for a JSON null and allocates an empty one for [], so a nil
// check over a plain slice would in fact tell a withheld section from a
// rendered but empty one. That was measured, not assumed.
//
// The reason is that a nil slice stays silently readable. listedIn over a
// withheld section answers false, so every assertion of the shape "the other
// repo is not under problems" would pass because the problems section was
// missing altogether, which is the assertion holding for the opposite of its
// stated reason. A *[]reportRepo cannot be read without going through rowsOf,
// and rowsOf fails on a section that was never rendered.
//
// Length checks are separately useless here and are not used: len is 0 for
// null and for [] alike.
type reportDoc struct {
	Section string    `json:"section"`
	Scope   *scopeDoc `json:"scope"`
	// BaselineRef is what the caller named the baseline as, null when the window
	// picked it. A pointer because those are different answers about the same
	// two snapshots, and an empty string would have to be known to mean one.
	BaselineRef *string       `json:"baseline_ref"`
	Repos       *[]reportRepo `json:"repos"`
	Movers      *[]reportRepo `json:"movers"`
	Problems    *[]reportRepo `json:"problems"`
}

// scopeDoc is the envelope's statement of which repos the answer covers.
//
// It is a pointer for the same reason the three sections are, and against a
// sharper hazard: json.Unmarshal ignores a key the struct has no field for, so a
// value-typed Scope here would decode a report that never carried one into a
// struct full of zeroes, and every assertion below would pass while proving
// nothing. Going through scopeOf makes that a named failure instead.
type scopeDoc struct {
	// Repo is nil on an unnarrowed run. A string field would flatten that into
	// "", which is a repo name as far as any consumer can tell.
	Repo       *string `json:"repo"`
	Selected   int     `json:"selected"`
	Configured int     `json:"configured"`
}

// scopeOf unwraps the scope a report must always carry. See rowsOf.
func scopeOf(t *testing.T, doc reportDoc) scopeDoc {
	t.Helper()
	if doc.Scope == nil {
		t.Fatal("the report carries no scope at all, so nothing in it says which repos the answer covers")
	}
	return *doc.Scope
}

func decodeReport(t *testing.T, out string) reportDoc {
	t.Helper()
	var doc reportDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unparseable report json: %v\n%s", err, out)
	}
	return doc
}

// rowsOf unwraps a section that was rendered. A caller reaching for rows always
// means the rendered case, so a withheld section is a named failure here rather
// than a nil dereference somewhere further down.
func rowsOf(t *testing.T, rows *[]reportRepo, section string) []reportRepo {
	t.Helper()
	if rows == nil {
		t.Fatalf("the report withheld the %s section entirely", section)
	}
	return *rows
}

// coveragePct reads a repo's measured coverage percentage.
//
// It fails rather than returning zero when the group is absent. A helper that
// answered 0 for a repo that measured nothing would put this project's
// recurring bug inside the test suite that exists to catch it.
func coverageValue(t *testing.T, r reportRepo) float64 {
	t.Helper()
	if r.Coverage == nil {
		t.Fatalf("no coverage group on %q, so nothing was measured about it", r.Name)
	}
	return r.Coverage.Value
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

// The error this returns only becomes an exit status: main prints nothing. So a
// typo used to produce seventy lines of usage and no statement of the problem,
// and this test asserted only that the usage appeared, which is the half that
// was never missing.
func TestRunUnknownSubcommand(t *testing.T) {
	t.Run("a mistyped subcommand", func(t *testing.T) {
		_, stderr, err := runCLI(t, "collct")
		if err == nil {
			t.Error("want an error for an unknown subcommand")
		}
		if !strings.Contains(stderr, "usage:") {
			t.Errorf("want usage on stderr, got %q", stderr)
		}
		// What was typed has to appear, or the reader is left to spot the
		// difference between their command line and a page of usage.
		if !strings.Contains(stderr, "collct") {
			t.Errorf("want the rejected command named on stderr, got %q", stderr)
		}
		if !strings.Contains(stderr, "unknown command") {
			t.Errorf("want stderr to say what was wrong, got %q", stderr)
		}
	})

	// The same path, reached without a typo: Go's flag package cannot take
	// flags before the subcommand, so this lands in the same branch and is
	// worth a word of its own.
	t.Run("a flag before the subcommand", func(t *testing.T) {
		_, stderr, err := runCLI(t, "--config", "repo-metrics.yaml", "collect")
		if err == nil {
			t.Error("want an error for a flag before the subcommand")
		}
		if !strings.Contains(stderr, "--config") {
			t.Errorf("want the rejected token named on stderr, got %q", stderr)
		}
		if !strings.Contains(stderr, "flags go after the subcommand") {
			t.Errorf("want the order explained on stderr, got %q", stderr)
		}
	})
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

	// The window is spelled the way the documents teach it. This assertion used
	// to be its exact opposite: "window: 7d" had to be absent, because the
	// config file went through time.ParseDuration, whose largest unit is the
	// hour, so the file this tool writes had to say 168h while every document
	// and the --window help text said 7d. Both now read through one parser, and
	// a starter config that cannot say what the tool recommends is the thing
	// worth catching.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !strings.Contains(string(contents), "window: 7d") {
		t.Errorf("want the window written in days, got:\n%s", contents)
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

// Nothing was printed while a repo was being collected.
//
// collectOne's line lands after that repo's signals have all run and been
// stored, so a cold three-repo run was 78 seconds of silence and the starter
// config allows ten minutes per signal. On nine repos there was no way to tell
// working from hung, or to see which repo it was on.
func TestCollectAnnouncesEachRepoBeforeItStarts(t *testing.T) {
	dir := t.TempDir()
	alpha := repoDir(t, dir, "alpha", sampleProfile)
	beta := repoDir(t, dir, "beta", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s",
		dbPath,
		ingestRepoEntry("alpha", alpha, "coverage.out"),
		ingestRepoEntry("beta", beta, "coverage.out")))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath)
	if err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	for i, name := range []string{"alpha", "beta"} {
		starting := fmt.Sprintf("collecting %s (%d of 2)", name, i+1)
		at := strings.Index(stdout, starting)
		if at < 0 {
			t.Fatalf("nothing announced %s while it was being collected:\n%s", name, stdout)
		}
		// Before its own completion line, not after. A line printed once the
		// repo is done is the one that was already there.
		if done := strings.Index(stdout, rowFor(t, stdout, name)); done < at {
			t.Errorf("%s was announced only after it finished, which says nothing while it runs:\n%s", name, stdout)
		}
	}

	// The completion lines survive and still say what happened: the new line
	// says what is starting, the old one says how it went.
	wantInRow(t, stdout, "alpha", "ok", "60.0%")
	wantInRow(t, stdout, "beta", "ok", "60.0%")
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
		"database: %q\nrepos:\n  - name: stale\n    path: %q\n    signals:\n"+
			"      - name: coverage\n        artifact: coverage.out\n        artifact_format: %s\n        max_age: 24h\n",
		dbPath, stale, config.FormatGoCoverprofile))

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
	// A partial snapshot says what it cost. This assertion used to be the
	// opposite, on the grounds that a warning is not an error, and it was
	// pinning a real gap: the field stayed empty, and the report listed the repo
	// under Collection problems as a bare name with nothing after the status
	// word. Every repo with one failing test is in that state, since a non-zero
	// exit degrades the step.
	//
	// The severity distinction still holds and is not what this field carries. A
	// clean snapshot picks up nothing here, because a diagnostic only lands in
	// it when it actually cost the snapshot something, and anything that did has
	// already moved the status off ok.
	if !strings.Contains(snap.Error, "stale") {
		t.Errorf("a partial snapshot has to say what it cost, got %q", snap.Error)
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

	rows := rowsOf(t, decodeReport(t, stdout).Repos, "repos")
	if len(rows) != 1 {
		t.Fatalf("want one repo in the json, got %d", len(rows))
	}
	got := jsonRepo(t, rows, "healthy")
	if coverageValue(t, got) != 60 {
		t.Errorf("json = %+v, want healthy at 60", *got.Coverage)
	}
	// The inner null: measured, but with nothing to compare against. It is a
	// different statement from the outer one, which is why the group has to be
	// there and only the delta absent.
	if got.HasBaseline || got.Coverage.Delta != nil {
		t.Errorf("want a null delta with only one snapshot, got %+v", *got.Coverage)
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

	doc := decodeReport(t, stdout)
	repos := rowsOf(t, doc.Repos, "repos")
	if !listedIn(repos, "broken") {
		t.Fatalf("the broken repo dropped out of the report entirely: %s", stdout)
	}
	got := jsonRepo(t, repos, "broken")
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
	// A failed run stored no metrics, so there is nothing measured to publish.
	// Absent groups are what make that unreadable as a repo sitting at zero: a
	// consumer that reaches into either one gets an error instead of a default.
	if got.Coverage != nil {
		t.Errorf("a failed run measured no coverage, but the report published %+v", *got.Coverage)
	}
	if got.Tests != nil {
		t.Errorf("a failed run counted no tests, but the report published %+v", *got.Tests)
	}

	// It must not lead the report on a fabricated cliff either.
	if listedIn(rowsOf(t, doc.Movers, "movers"), "broken") {
		t.Errorf("a crashed test command is not this week's biggest move: %+v", *doc.Movers)
	}
	if !listedIn(rowsOf(t, doc.Problems, "problems"), "broken") {
		t.Error("want the failing repo under collection problems")
	}

	// The healthy repo is untouched by any of this.
	if healthy := jsonRepo(t, repos, "healthy"); coverageValue(t, healthy) != 60 {
		t.Errorf("healthy repo = %+v, want 60 percent", *healthy.Coverage)
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
	repos := rowsOf(t, doc.Repos, "repos")

	// Present at all. This is the half that fails if the branch just skips.
	got := jsonRepo(t, repos, "pending")

	// And not fabricated. Every one of these is what a head the command invented
	// would flip.
	if got.HasSnapshot {
		t.Error("has_snapshot is true for a repo with no row in the database, so a consumer would chart its zeros")
	}
	// The structural half of the same claim, and the stronger one. has_snapshot
	// is a boolean a consumer has to remember to check; an absent group cannot be
	// read as a number at all. Nothing was measured here, so nothing numeric may
	// exist anywhere on this row for a defaulting accessor to turn into a zero.
	if got.Coverage != nil {
		t.Errorf("a repo nobody has ever collected published coverage %+v", *got.Coverage)
	}
	if got.Tests != nil {
		t.Errorf("a repo nobody has ever collected published a test count %+v", *got.Tests)
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
	if listedIn(rowsOf(t, doc.Movers, "movers"), "pending") {
		t.Error("a repo that was never collected cannot be this week's biggest move")
	}

	// Anti-vacuity control: the collected repo still carries its own real
	// numbers, so a change that flattened every row would not pass this.
	if healthy := jsonRepo(t, repos, "collected"); !healthy.HasSnapshot || coverageValue(t, healthy) != 60 {
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
	repos := rowsOf(t, doc.Repos, "repos")
	brokeRow := jsonRepo(t, repos, "broken")
	neverRow := jsonRepo(t, repos, "registered")

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
	movers := rowsOf(t, doc.Movers, "movers")
	if listedIn(movers, "broken") || listedIn(movers, "registered") {
		t.Errorf("a repo with no numbers cannot be a mover: %+v", movers)
	}
}

// narrowingFixture is a three-repo database where all three report sections
// have something in them: moved cleared the mover threshold, steady was
// collected and has no baseline, and broken failed every run so it lands under
// collection problems.
//
// All three matter. A fixture where a section came back empty anyway would make
// "--section movers withheld the repos table" indistinguishable from "there
// were no repos to show", and the assertion would hold for the wrong reason.
//
// It returns the config path. Everything else lives under the test's own temp
// directory, which is cleaned up with it.
func narrowingFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	moved := repoDir(t, dir, "moved", sampleProfile)
	steady := repoDir(t, dir, "steady", sampleProfile)
	// Ingest mode pointed at a profile that is not there, so every run fails.
	broken := repoDir(t, dir, "broken", "")
	dbPath := filepath.Join(dir, "metrics.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s%s%s",
		dbPath,
		ingestRepoEntry("moved", moved, "coverage.out"),
		ingestRepoEntry("steady", steady, "coverage.out"),
		ingestRepoEntry("broken", broken, "missing.out")))

	ctx := context.Background()
	seed := openStore(t, dbPath)
	repoID, err := seed.UpsertRepo(ctx, "moved", moved)
	if err != nil {
		t.Fatalf("seeding the repo: %v", err)
	}
	// A fortnight back at 6 of 15 statements covered, against the fixture's 3 of
	// 5, so the move is 40.0 to 60.0 and clears the default 0.5 threshold. The
	// third package is only in the baseline, so it also gives the every-repo
	// section its package-churn block to render.
	if _, err := seed.InsertSnapshot(ctx, store.Snapshot{
		RepoID:      repoID,
		CollectedAt: time.Now().Add(-14 * 24 * time.Hour),
		Status:      store.StatusOK,
	}, []store.Metric{
		{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/a", Value: 0},
		{Key: collect.KeyTotalStmts, Scope: "example.com/demo/a", Value: 4},
		{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/b", Value: 1},
		{Key: collect.KeyTotalStmts, Scope: "example.com/demo/b", Value: 1},
		{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/gone", Value: 5},
		{Key: collect.KeyTotalStmts, Scope: "example.com/demo/gone", Value: 10},
	}); err != nil {
		t.Fatalf("seeding a baseline snapshot: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed store: %v", err)
	}

	// broken fails, so a non-zero status here is the fixture working rather than
	// a problem. Asserting on it is what stops a change that made broken collect
	// cleanly from silently emptying the problems section.
	if _, _, err := runCLI(t, "collect", "--config", cfgPath); err == nil {
		t.Fatal("want the failing repo to make collect exit non-zero, otherwise there are no collection problems to narrow to")
	}
	return cfgPath
}

// An agent asking about one repo should pay for one repo. The narrowing happens
// before anything is read out of the database, so the other repos are not
// fetched, compared, and then dropped.
func TestReportNarrowsToOneRepo(t *testing.T) {
	cfgPath := narrowingFixture(t)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json", "--repo", "steady")
	if err != nil {
		t.Fatalf("report --repo: %v (stderr: %s)", err, stderr)
	}
	doc := decodeReport(t, stdout)
	repos := rowsOf(t, doc.Repos, "repos")

	if len(repos) != 1 {
		t.Fatalf("want exactly the one repo that was asked for, got %d: %+v", len(repos), repos)
	}
	// Anti-vacuity control: the surviving repo still carries its real numbers, so
	// a change that narrowed by emptying every row would not pass this.
	if got := jsonRepo(t, repos, "steady"); coverageValue(t, got) != 60 {
		t.Errorf("the one repo asked for = %+v, want 60 percent", got)
	}
	// Narrowing has to reach every section, not just the table. A --repo run
	// still listing broken under collection problems is reporting on a repo
	// nobody asked about, which is the cost the flag exists to remove.
	if listedIn(rowsOf(t, doc.Problems, "problems"), "broken") {
		t.Error("--repo steady still reported the broken repo under collection problems")
	}
	if listedIn(rowsOf(t, doc.Movers, "movers"), "moved") {
		t.Error("--repo steady still reported the other repo as a mover")
	}

	// The markdown narrows too, bound to its own row rather than searched for
	// over the whole document.
	md, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "steady")
	if err != nil {
		t.Fatalf("report --repo (markdown): %v (stderr: %s)", err, stderr)
	}
	wantInRow(t, md, "steady", "60.0%")
	for _, gone := range []string{"moved", "broken"} {
		if hasRowFor(md, gone) {
			t.Errorf("--repo steady still has a table row for %q:\n%s", gone, md)
		}
		// The movers write-up gives a repo an h3 rather than a table row, so it
		// needs its own check: a row assertion alone would miss it entirely.
		if strings.Contains(md, "### "+gone) {
			t.Errorf("--repo steady still wrote up %q under what moved:\n%s", gone, md)
		}
	}
}

// The failure this whole round is about.
//
// "No repo failed to collect" and "the one repo I asked about did not fail to
// collect" are wildly different findings, and before the scope field they were
// the same bytes: an empty problems list. An agent that ran --repo steady and
// reported the fleet healthy would have been reading the tool exactly as written.
//
// So the two runs are rendered side by side here and the test is that they can be
// told apart, rather than that either one says something in particular.
func TestANarrowedAllClearIsNotAFleetWideAllClear(t *testing.T) {
	cfgPath := narrowingFixture(t)

	narrowed, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json", "--section", "problems", "--repo", "steady")
	if err != nil {
		t.Fatalf("narrowed problems report: %v (stderr: %s)", err, stderr)
	}
	whole, stderr, err := runCLI(t, "report", "--config", cfgPath, "--format", "json", "--section", "problems")
	if err != nil {
		t.Fatalf("fleet-wide problems report: %v (stderr: %s)", err, stderr)
	}

	narrowedDoc, wholeDoc := decodeReport(t, narrowed), decodeReport(t, whole)

	// The premise: the narrowed run really does come back clean, because steady
	// is fine. Without this the test could pass on a fixture where both runs
	// listed broken and nothing was ever ambiguous.
	if rows := rowsOf(t, narrowedDoc.Problems, "problems"); len(rows) != 0 {
		t.Fatalf("--repo steady --section problems reported %+v, want nothing: steady collects cleanly, and the ambiguity this test is about only exists when it does", rows)
	}
	if !listedIn(rowsOf(t, wholeDoc.Problems, "problems"), "broken") {
		t.Fatal("the fleet-wide run does not report the broken repo, so there is no fleet-wide finding for the narrowed run to be confused with")
	}

	got := scopeOf(t, narrowedDoc)
	if got.Repo == nil || *got.Repo != "steady" {
		t.Errorf("scope.repo = %v, want %q. Nothing else in a clean narrowed report says which repo it is clean about.", got.Repo, "steady")
	}
	if got.Selected != 1 || got.Configured != 3 {
		t.Errorf("scope selected/configured = %d/%d, want 1/3. The report has to say how much of the config it is not covering.", got.Selected, got.Configured)
	}

	// And the unnarrowed run has to be readable as unnarrowed, or the fix only
	// works for callers who already know they narrowed.
	fleet := scopeOf(t, wholeDoc)
	if fleet.Repo != nil {
		t.Errorf("scope.repo = %q on a fleet-wide run, want null", *fleet.Repo)
	}
	if fleet.Selected != fleet.Configured {
		t.Errorf("scope selected/configured = %d/%d on a fleet-wide run, want them equal: that equality is how a consumer knows it is seeing everything", fleet.Selected, fleet.Configured)
	}
}

// The same distinction in the format a person reads. A narrowed markdown report
// that just says nothing failed is as misleading as the JSON was.
func TestMarkdownSaysWhatACleanProblemsSectionIsCleanAbout(t *testing.T) {
	cfgPath := narrowingFixture(t)

	md, stderr, err := runCLI(t, "report", "--config", cfgPath, "--section", "problems", "--repo", "steady")
	if err != nil {
		t.Fatalf("narrowed problems report: %v (stderr: %s)", err, stderr)
	}

	// The heading has to be there at all. It used to be gated on the list being
	// non-empty, so asking specifically about problems and having none rendered a
	// document with no problems section in it, which answers a question with
	// silence.
	if !strings.Contains(md, "## Collection problems") {
		t.Errorf("--section problems rendered no problems section:\n%s", md)
	}
	if !strings.Contains(md, "Nothing failed to collect.") {
		t.Errorf("--section problems does not say that nothing failed:\n%s", md)
	}
	if !strings.Contains(md, "Only covering the repo `steady`") {
		t.Errorf("a narrowed report does not say what it is narrowed to:\n%s", md)
	}
	if strings.Contains(md, "broken") {
		t.Errorf("--repo steady mentions the repo that did fail:\n%s", md)
	}
}

// A full report gains the same positive statement, which is information rather
// than filler for a tool whose entire job is noticing that a repo stopped
// reporting.
func TestAFleetWideReportStatesThatNothingFailed(t *testing.T) {
	dir := t.TempDir()
	clean := repoDir(t, dir, "clean", sampleProfile)
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		filepath.Join(dir, "metrics.db"), ingestRepoEntry("clean", clean, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}
	md, stderr, err := runCLI(t, "report", "--config", cfgPath)
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}

	if !strings.Contains(md, "## Collection problems") {
		t.Errorf("a clean report has no collection problems section, so it never says the collection worked:\n%s", md)
	}
	if !strings.Contains(md, "Nothing failed to collect.") {
		t.Errorf("a clean report does not say that nothing failed:\n%s", md)
	}
}

// A name that is not in the config is an error, never an empty report. An empty
// report is the worst possible answer to a typo, and worse for an agent than
// for a person: nothing in it says "you asked about a repo I have never heard
// of", so it reads as a clean week.
func TestReportRejectsARepoNameThatIsNotConfigured(t *testing.T) {
	cfgPath := narrowingFixture(t)

	// A near miss rather than nonsense, since a typo is the case this guards.
	// "stedy" is deliberately not a substring of "steady", so the assertion
	// below cannot be satisfied by the configured-repo list that follows it.
	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "stedy")
	if err == nil {
		t.Fatal("want an error rather than an empty report, which an agent reads as an answer")
	}
	if !strings.Contains(stderr, "stedy") {
		t.Errorf("want the name that was asked for quoted back, got %q", stderr)
	}
	// Naming the repo is not enough on its own: it does not say whether the name
	// is misspelled or the config is the wrong one. Listing what is configured
	// answers both.
	for _, configured := range []string{"moved", "steady", "broken"} {
		if !strings.Contains(stderr, configured) {
			t.Errorf("want %q listed among the configured repos, got %q", configured, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("want no report at all on a rejected run, got %q", stdout)
	}
}

// Each --section value renders that section and withholds the others.
//
// The assertions are on whether the key came back null, never on how many rows
// it has. A withheld section is null and a rendered but empty one is [], and
// both are length zero, so a length check would hold whether or not narrowing
// works at all.
func TestReportSectionRendersOnlyWhatWasAskedFor(t *testing.T) {
	cfgPath := narrowingFixture(t)

	for _, tc := range []struct {
		section                 string
		movers, repos, problems bool
	}{
		{"all", true, true, true},
		{"movers", true, false, false},
		{"repos", false, true, false},
		{"problems", false, false, true},
	} {
		t.Run(tc.section, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, "report", "--config", cfgPath,
				"--format", "json", "--section", tc.section)
			if err != nil {
				t.Fatalf("report --section %s: %v (stderr: %s)", tc.section, err, stderr)
			}
			doc := decodeReport(t, stdout)

			// The document says which question it answered. Without it a consumer
			// holding a null cannot tell "you did not ask" from "nothing to say".
			if doc.Section != tc.section {
				t.Errorf("section = %q, want %q", doc.Section, tc.section)
			}
			for _, part := range []struct {
				name     string
				rows     *[]reportRepo
				rendered bool
			}{
				{"movers", doc.Movers, tc.movers},
				{"repos", doc.Repos, tc.repos},
				{"problems", doc.Problems, tc.problems},
			} {
				if (part.rows != nil) != part.rendered {
					t.Errorf("--section %s: %s rendered = %v, want %v",
						tc.section, part.name, part.rows != nil, part.rendered)
				}
			}

			// Anti-vacuity control: whatever was rendered still carries its rows,
			// so a change that narrowed by returning an empty document would fail
			// here rather than pass everything above.
			if tc.movers {
				jsonRepo(t, rowsOf(t, doc.Movers, "movers"), "moved")
			}
			if tc.repos {
				jsonRepo(t, rowsOf(t, doc.Repos, "repos"), "steady")
			}
			if tc.problems {
				jsonRepo(t, rowsOf(t, doc.Problems, "problems"), "broken")
			}
		})
	}
}

// --section applies to markdown as well, because a person narrowing to movers is
// as reasonable as an agent doing it.
//
// Headings are the assertion target: they are what a reader navigates by, and
// binding to them says which sections are present without pinning the prose
// underneath.
func TestReportSectionAppliesToMarkdownToo(t *testing.T) {
	cfgPath := narrowingFixture(t)

	headings := map[string]string{
		"movers":   "## What moved",
		"repos":    "## Every repo",
		"problems": "## Collection problems",
	}

	t.Run("all", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "report", "--config", cfgPath)
		if err != nil {
			t.Fatalf("report: %v (stderr: %s)", err, stderr)
		}
		// The control for every subtest below: unnarrowed, all three are there.
		for section, heading := range headings {
			if !strings.Contains(stdout, heading) {
				t.Errorf("an unnarrowed report is missing the %s section:\n%s", section, stdout)
			}
		}
	})

	for section, own := range headings {
		t.Run(section, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--section", section)
			if err != nil {
				t.Fatalf("report --section %s: %v (stderr: %s)", section, err, stderr)
			}
			if !strings.Contains(stdout, own) {
				t.Errorf("want the %s section, got:\n%s", section, stdout)
			}
			for other, heading := range headings {
				if other == section {
					continue
				}
				if strings.Contains(stdout, heading) {
					t.Errorf("--section %s also rendered the %s section:\n%s", section, other, stdout)
				}
			}
		})
	}

	// The file path renders separately from the stdout path, so a section that
	// only ever reached stdout would leave --out quietly writing the whole
	// report. Anyone scripting this writes to a file.
	t.Run("and to a file", func(t *testing.T) {
		outPath := filepath.Join(t.TempDir(), "movers.md")
		if _, stderr, err := runCLI(t, "report", "--config", cfgPath,
			"--section", "movers", "--out", outPath); err != nil {
			t.Fatalf("report --section movers --out: %v (stderr: %s)", err, stderr)
		}
		written, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("reading the written report: %v", err)
		}
		if !strings.Contains(string(written), headings["movers"]) {
			t.Errorf("the written file is missing the movers section:\n%s", written)
		}
		if strings.Contains(string(written), headings["repos"]) {
			t.Errorf("--section movers --out wrote the whole report to the file:\n%s", written)
		}
	})

	// The package-churn block belongs to the every-repo section rather than to a
	// section of its own, which is a shape decision worth pinning: it is the one
	// heading whose home is not obvious from its name.
	t.Run("package churn travels with repos", func(t *testing.T) {
		const churn = "## Packages that came and went"
		repos, _, err := runCLI(t, "report", "--config", cfgPath, "--section", "repos")
		if err != nil {
			t.Fatalf("report --section repos: %v", err)
		}
		if !strings.Contains(repos, churn) {
			t.Errorf("--section repos dropped the package churn:\n%s", repos)
		}
		movers, _, err := runCLI(t, "report", "--config", cfgPath, "--section", "movers")
		if err != nil {
			t.Fatalf("report --section movers: %v", err)
		}
		if strings.Contains(movers, churn) {
			t.Errorf("--section movers carried the package churn:\n%s", movers)
		}
	})
}

// An unknown section is rejected, not quietly rounded down to the whole report.
// A typo that renders everything is the same lie as one that renders nothing:
// the caller asked a narrow question and got an answer to a different one.
func TestReportRejectsAnUnknownSection(t *testing.T) {
	cfgPath := narrowingFixture(t)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--section", "mvoers")
	if err == nil {
		t.Fatal("want an error rather than a silent fallback to the whole report")
	}
	if !strings.Contains(stderr, "mvoers") {
		t.Errorf("want the rejected name quoted back, got %q", stderr)
	}
	// Every valid name, read from the parser rather than typed out here, so this
	// keeps holding when a section is added.
	for _, valid := range report.Sections() {
		if !strings.Contains(stderr, valid) {
			t.Errorf("want %q offered as a valid section, got %q", valid, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("want no report at all on a rejected run, got %q", stdout)
	}
}

// The two flags are independent, so asking for one repo's movers has to narrow
// on both axes at once. Neither one quietly winning is the failure here.
func TestReportRepoAndSectionCompose(t *testing.T) {
	cfgPath := narrowingFixture(t)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath,
		"--format", "json", "--repo", "moved", "--section", "movers")
	if err != nil {
		t.Fatalf("report --repo --section: %v (stderr: %s)", err, stderr)
	}
	doc := decodeReport(t, stdout)

	if doc.Section != "movers" {
		t.Errorf("section = %q, want movers", doc.Section)
	}
	if doc.Repos != nil {
		t.Errorf("--section movers still rendered the repos table: %+v", *doc.Repos)
	}
	if doc.Problems != nil {
		t.Errorf("--section movers still rendered collection problems: %+v", *doc.Problems)
	}
	movers := rowsOf(t, doc.Movers, "movers")
	if len(movers) != 1 {
		t.Fatalf("want just the one repo asked for, got %d: %+v", len(movers), movers)
	}
	// And it is still a real answer: the repo that was asked for, with the move
	// it actually made, rather than an empty envelope that satisfies both filters.
	got := jsonRepo(t, movers, "moved")
	if coverageValue(t, got) != 60 {
		t.Errorf("moved = %+v, want 60 percent", got)
	}
	if got.Coverage.Delta == nil {
		t.Fatalf("a mover with no delta is not a mover: %+v", *got.Coverage)
	}
	if *got.Coverage.Delta != 20 {
		t.Errorf("delta = %v points, want the +20 the fixture seeded", *got.Coverage.Delta)
	}
}

// The usage text is the entire discoverability story for --section, since this
// tool deliberately ships no MCP server describing its flags. A section the
// parser accepts and the help never mentions cannot be found by anyone.
func TestUsageListsEverySection(t *testing.T) {
	_, stderr, _ := runCLI(t, "help")

	if !strings.Contains(stderr, "--section") {
		t.Fatalf("the usage text does not mention --section at all:\n%s", stderr)
	}
	// Read from the parser, and asserted as the joined list rather than name by
	// name: "all" and "repos" both occur in the surrounding prose, so checking
	// them individually would pass on text that has nothing to do with the flag.
	if want := strings.Join(report.Sections(), ", "); !strings.Contains(stderr, want) {
		t.Errorf("want the valid sections %q listed in the usage text:\n%s", want, stderr)
	}
	// report's --repo needs more than a mention in the synopsis line, which
	// collect's --repo already puts there. What a caller has to know is the part
	// that differs from a plain filter: an unknown name fails rather than
	// producing an empty report they would read as a clean week.
	if !strings.Contains(stderr, "not an empty report") {
		t.Errorf("the usage text does not say what report --repo does with an unknown name:\n%s", stderr)
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
		rows := rowsOf(t, decodeReport(t, stdout).Repos, "repos")
		if got := jsonRepo(t, rows, "healthy"); coverageValue(t, got) != 60 {
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
		rows := rowsOf(t, decodeReport(t, string(written)).Repos, "repos")
		if got := jsonRepo(t, rows, "healthy"); coverageValue(t, got) != 60 {
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

// A repo path that is not there is not a missing config, and must not be
// answered as one.
//
// Both used to satisfy errors.Is(err, os.ErrNotExist): the config file through
// os.ReadFile, a repo path through the os.Stat that validation joins into its
// error list, which errors.Is recurses through. So a config the operator had
// just edited was answered with "run repo-metrics init to write a starter
// config", which refuses without --force and with it would have overwritten
// their repo list. The starter config's own commented-out example points at a
// path that does not exist, so this is a first-hour stumble.
func TestAMissingRepoPathDoesNotSuggestOverwritingTheConfig(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "not-checked-out")
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		filepath.Join(dir, "metrics.db"), ingestRepoEntry("gone", absent, "coverage.out")))

	for _, sub := range []string{"collect", "report", "repos"} {
		t.Run(sub, func(t *testing.T) {
			_, stderr, err := runCLI(t, sub, "--config", cfgPath)
			if err == nil {
				t.Fatal("want an error for a repo path that is not there")
			}
			// The real problem still has to be reported, or this passes by
			// saying nothing at all.
			if !strings.Contains(stderr, absent) {
				t.Errorf("want the unusable repo path named on stderr, got %q", stderr)
			}
			if strings.Contains(stderr, "repo-metrics init") {
				t.Errorf("a bad repo path was answered by telling the operator to overwrite their config: %q", stderr)
			}
		})
	}
}

// Every format name in the starter config sits under the key it belongs to.
//
// The template is a Sprintf with positional verbs, and most of them are %s
// consuming a config.Format, which is a string type. Insert an example without
// inserting its argument at the matching index and every later format name
// shifts one slot — sarif becomes the dependencies step's stdout_format. It
// still compiles, and config.Load still accepts the result, because every value
// is a valid format name and only the pairing is wrong.
//
// So neither the compiler nor the validator nor the existing round-trip test
// covers this edit. This does.
func TestInitPinsEachFormatNameToItsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo-metrics.yaml")
	if err := cli.Run([]string{"init", "--config", path}, io.Discard, io.Discard); err != nil {
		t.Fatalf("init: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the starter config: %v", err)
	}
	got := string(body)

	for _, want := range []string{
		"artifact_format: " + string(config.FormatGoCoverprofile),
		"stdout_format: " + string(config.FormatGoTestJSON),
		"stdout_format: " + string(config.FormatSARIF),
		"stdout_format: " + string(config.FormatGoListModules),
		"format: " + string(config.FormatJUnitXML),
		"format: " + string(config.FormatLCOV),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the starter config does not pair %q; a verb and its argument are out of step", want)
		}
	}

	// The control: a format name appearing under the WRONG key is what the shift
	// produces, and it has to be detectable. go-list-modules is only ever a
	// stdout format, so seeing it as an artifact_format means the slots moved.
	if strings.Contains(got, "artifact_format: "+string(config.FormatGoListModules)) {
		t.Error("go-list-modules appears as an artifact_format, which is the signature of a shifted verb")
	}
	if strings.Contains(got, "artifact_format: "+string(config.FormatSARIF)) {
		t.Error("sarif appears as an artifact_format, which is the signature of a shifted verb")
	}
}

// --database moves where a run reads and writes, and moves nothing else.
//
// The case it exists for is a coverage floor measured once, which must not land
// in the history a schedule is keeping. Before it, that meant copying a whole
// config file to change one line, which is how two configs drift apart.
//
// Two failures are worth telling apart, so there are two assertions. Confirmed
// by mutation: with databasePath returning cfg.Database unconditionally, the
// override is never opened, the scratch file comes back empty and repoByName
// fails first. The stat further down catches the other shape, a run that honors
// the override and touches the config's database anyway, which would sail past
// every assertion above it.
func TestDatabaseFlagOverridesTheConfigAndLeavesItAlone(t *testing.T) {
	dir := t.TempDir()
	healthy := repoDir(t, dir, "healthy", sampleProfile)
	configured := filepath.Join(dir, "configured.db")
	scratch := filepath.Join(dir, "scratch.db")

	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		configured, ingestRepoEntry("healthy", healthy, "coverage.out")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--database", scratch); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	// The snapshot is in the override.
	st := openStore(t, scratch)
	repo := repoByName(t, st, "healthy")
	snap, err := st.LatestSnapshot(context.Background(), repo.ID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("nothing was stored in the database named by --database")
	}

	// And the config's database was never touched. Checked with a stat rather
	// than by opening it, since store.Open creates what it is pointed at and
	// would manufacture the very file this asserts is absent.
	if _, err := os.Stat(configured); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the config's database at %s exists, so --database did not stand in for it (stat: %v)",
			configured, err)
	}

	// The read side has to honor it too, or a scratch collection is write-only.
	stdout, stderr, err := runCLI(t, "repos", "--config", cfgPath, "--database", scratch, "--format", "json")
	if err != nil {
		t.Fatalf("repos: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stdout, `"has_snapshot":true`) {
		t.Errorf("repos --database did not read the scratch database, got %s", stdout)
	}
}

// Every subcommand that opens a database takes the flag.
//
// Hand-written rather than derived, because the thing worth catching is a fifth
// subcommand added later that opens a store and forgets it. Deriving the list
// from the subcommands that call openStore would make this agree automatically,
// and automatic agreement proves nothing.
func TestEverySubcommandThatOpensADatabaseTakesTheFlag(t *testing.T) {
	for _, cmd := range []string{"collect", "report", "repos", "history"} {
		t.Run(cmd, func(t *testing.T) {
			_, stderr, err := runCLI(t, cmd, "-h")
			if err != nil {
				t.Fatalf("%s -h: %v", cmd, err)
			}
			if !strings.Contains(stderr, "-database") {
				t.Errorf("%s does not offer --database:\n%s", cmd, stderr)
			}
		})
	}
}

// initInto runs init in a fresh directory holding the named marker files, and
// hands back the directory, the config path, and what init said on stderr.
func initInto(t *testing.T, markers ...string) (dir, path, stderr string) {
	t.Helper()
	dir = t.TempDir()
	for _, name := range markers {
		// Empty files, because the probe stats these rather than reading them.
		writeFile(t, filepath.Join(dir, name), "")
	}
	path = filepath.Join(dir, "repo-metrics.yaml")
	_, stderr, err := runCLI(t, "init", "--config", path)
	if err != nil {
		t.Fatalf("init into a directory holding %v: %v (stderr: %s)", markers, err, stderr)
	}
	return dir, path, stderr
}

// liveRepo loads a generated config and returns the one live repo entry.
//
// Loading it is half the assertion. A starter the tool cannot load is worse
// than no starter, since the first thing a new user would see is a validation
// error from a file this program just wrote. The count is the other half: a
// directory carrying two ecosystems' markers still gets ONE live entry, because
// two would point at the same directory under two names and every report from
// then on would count that checkout twice.
func liveRepo(t *testing.T, path string) config.Repo {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config init wrote does not load: %v", err)
	}
	if len(cfg.Repos) != 1 {
		t.Fatalf("want exactly one live repo entry, got %d", len(cfg.Repos))
	}
	return cfg.Repos[0]
}

// signalByName finds one collection step by the name an operator reads, rather
// than by position, so reordering a starter's steps fails cleanly here instead
// of asserting against the wrong one.
func signalByName(t *testing.T, repo config.Repo, name string) config.Signal {
	t.Helper()
	for _, s := range repo.Signals {
		if s.Name == name {
			return s
		}
	}
	names := make([]string, 0, len(repo.Signals))
	for _, s := range repo.Signals {
		names = append(names, s.Name)
	}
	t.Fatalf("repo %s has no %q step, only %v", repo.Name, name, names)
	return config.Signal{}
}

// liveTools names the program each live step invokes, which is the shortest
// unambiguous statement of which starter was written.
func liveTools(repo config.Repo) []string {
	var tools []string
	for _, s := range repo.Signals {
		if s.HasCommand() {
			tools = append(tools, s.Command[0])
		}
	}
	return tools
}

// init writes the starter for the workspace it is writing into, rather than the
// Go one every time.
//
// The Go starter's two live steps run the Go toolchain. A Python or TypeScript
// repo handed it gets a config that loads, collects, exits 0 and records
// nothing usable: the coverage step finds no instrumented packages and the
// dependency step finds no go.mod, so what arrives is a partial snapshot and a
// wall of warnings. That is a working config that measures nothing, which is
// the failure this tool is organized against, reached through the file the tool
// itself wrote.
func TestInitWritesTheStarterForTheWorkspaceItProbes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		markers   []string
		ecosystem string
		evidence  string
		// runs is a program only this starter's live steps invoke.
		runs string
		// absent is another starter's test program. Without it the assertion
		// passes on everything the three starters share, and the Go fallback
		// would satisfy every case in this table.
		absent string
	}{
		{"go.mod", []string{"go.mod"}, "Go", "go.mod", "go", "pytest"},
		{"pyproject.toml", []string{"pyproject.toml"}, "Python", "pyproject.toml", "pytest", "vitest"},
		{"setup.cfg", []string{"setup.cfg"}, "Python", "setup.cfg", "pytest", "vitest"},
		{"package.json", []string{"package.json"}, "TypeScript", "package.json", "vitest", "pytest"},
		{"bun.lock", []string{"bun.lock"}, "TypeScript", "bun.lock", "vitest", "pytest"},
		// Precedence, and the reason for it. A directory carrying all three
		// markers gets one starter rather than a live entry per ecosystem.
		{"all three", []string{"go.mod", "pyproject.toml", "package.json"}, "Go", "go.mod", "go", "pytest"},
		{"python before typescript", []string{"pyproject.toml", "package.json"}, "Python", "pyproject.toml", "pytest", "vitest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path, stderr := initInto(t, tc.markers...)
			repo := liveRepo(t, path)

			tools := liveTools(repo)
			if !slices.Contains(tools, tc.runs) {
				t.Errorf("the live steps run %v, want the %s starter, which runs %s", tools, tc.ecosystem, tc.runs)
			}
			if slices.Contains(tools, tc.absent) {
				t.Errorf("the live steps run %v, which is another ecosystem's starter", tools)
			}

			// Saying which and why is the point: a starter chosen silently is
			// one nobody knows to correct.
			for _, want := range []string{"detected", tc.ecosystem, tc.evidence, dir} {
				if !strings.Contains(stderr, want) {
					t.Errorf("init said %q, which does not name %q", stderr, want)
				}
			}

			// Both collection modes stay visible in the file someone is about
			// to edit, whichever starter it is, even though only one of them
			// can be live.
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading back: %v", err)
			}
			if !strings.Contains(string(body), "command:") {
				t.Error("want a command-mode example")
			}
			if !strings.Contains(string(body), "max_age") {
				t.Error("want an ingest-mode example with a max_age")
			}
		})
	}
}

// A directory that identified itself as nothing gets the Go starter and is told
// so, naming every marker that was looked for.
//
// The alternative is what this used to do: write the Go starter in silence, so
// the first evidence that it was a guess is a collection that exits 0 having
// measured nothing.
func TestInitSaysWhenNothingIdentifiedTheWorkspace(t *testing.T) {
	dir, path, stderr := initInto(t)

	if tools := liveTools(liveRepo(t, path)); !slices.Contains(tools, "go") {
		t.Errorf("the fallback's live steps run %v, want the Go starter", tools)
	}
	for _, want := range []string{"go.mod", "pyproject.toml", "setup.cfg", "package.json", "bun.lock", dir} {
		if !strings.Contains(stderr, want) {
			t.Errorf("init said %q, which does not name the marker %q it looked for", stderr, want)
		}
	}
	// The control. This sentence and the detection sentence are alternatives,
	// so a run that printed both would mean the Go starter reports a detection
	// it did not make, and the word would stop meaning anything.
	if strings.Contains(stderr, "detected") {
		t.Errorf("init reported a detection for a directory carrying no marker: %q", stderr)
	}
}

// Every format name in every starter sits under the key it belongs to.
//
// The templates are Sprintf calls with positional verbs, and most of them are
// %s consuming a config.Format, which is a string type. Insert an example
// without inserting its argument at the matching index and every later format
// name shifts one slot: sarif lands as another step's stdout_format. It still
// compiles, and config.Load still accepts the result, because every value is a
// valid format name and only the pairing is wrong.
//
// TestInitPinsEachFormatNameToItsKey covers the Go starter by reading the text,
// which is what a commented-out example needs. This reads the LOADED config
// instead, so a format under the wrong key is a different field of a different
// step rather than a string somewhere else in the file.
func TestEveryStarterPinsEachFormatNameToItsKey(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		_, path, _ := initInto(t, "go.mod")
		repo := liveRepo(t, path)

		cov := signalByName(t, repo, "coverage")
		if cov.ArtifactFormat != config.FormatGoCoverprofile {
			t.Errorf("coverage artifact_format is %q, want %q", cov.ArtifactFormat, config.FormatGoCoverprofile)
		}
		if cov.StdoutFormat != config.FormatGoTestJSON {
			t.Errorf("coverage stdout_format is %q, want %q", cov.StdoutFormat, config.FormatGoTestJSON)
		}

		deps := signalByName(t, repo, "dependencies")
		if deps.StdoutFormat != config.FormatGoListModules {
			t.Errorf("dependencies stdout_format is %q, want %q", deps.StdoutFormat, config.FormatGoListModules)
		}
		if arts := deps.ArtifactList(); len(arts) != 0 {
			t.Errorf("the dependencies step reads %v, but its whole answer comes from stdout", arts)
		}
	})

	t.Run("python", func(t *testing.T) {
		_, path, _ := initInto(t, "pyproject.toml")
		repo := liveRepo(t, path)
		assertOneRunWritesBothReports(t, repo)

		if lint := signalByName(t, repo, "lint"); lint.StdoutFormat != config.FormatSARIF {
			t.Errorf("lint stdout_format is %q, want %q", lint.StdoutFormat, config.FormatSARIF)
		}
	})

	t.Run("typescript", func(t *testing.T) {
		_, path, _ := initInto(t, "package.json")
		repo := liveRepo(t, path)
		assertOneRunWritesBothReports(t, repo)

		if lint := signalByName(t, repo, "lint"); lint.StdoutFormat != config.FormatSARIF {
			t.Errorf("lint stdout_format is %q, want %q", lint.StdoutFormat, config.FormatSARIF)
		}

		deps := signalByName(t, repo, "dependencies")
		if deps.HasCommand() {
			t.Errorf("the lockfile step runs %v; bun outdated and npm outdated answer a checkout nobody installed exactly as they answer a current one, which is why this reads the file", deps.Command)
		}
		if deps.ArtifactFormat != config.FormatNPMLockfile {
			t.Errorf("dependencies artifact_format is %q, want %q", deps.ArtifactFormat, config.FormatNPMLockfile)
		}
	})
}

// assertOneRunWritesBothReports pins the two artifacts one test run produces.
// A test report and a coverage profile are separate measurements out of a
// single command, which is what the artifacts longhand exists for: listed there
// rather than split across two steps, each is held to the check that this run
// wrote it instead of to a 24 hour age limit.
func assertOneRunWritesBothReports(t *testing.T, repo config.Repo) {
	t.Helper()
	tests := signalByName(t, repo, "tests")
	if tests.StdoutFormat != "" {
		t.Errorf("the tests step names a stdout_format %q, but both of its measurements come from files", tests.StdoutFormat)
	}
	arts := tests.ArtifactList()
	if len(arts) != 2 {
		t.Fatalf("want a test report and a coverage profile from one run, got %d artifacts: %v", len(arts), arts)
	}
	if arts[0].Format != config.FormatJUnitXML {
		t.Errorf("the first artifact is %q, want %q", arts[0].Format, config.FormatJUnitXML)
	}
	if arts[1].Format != config.FormatLCOV {
		t.Errorf("the second artifact is %q, want %q", arts[1].Format, config.FormatLCOV)
	}
}

// Every artifact a generated step writes lands outside the repo being measured,
// and the flag that writes it names the same path.
//
// Both halves matter and nothing else checks either. A relative artifact path
// resolves against the measured repo, so it creates a file inside that
// checkout: unless somebody gitignores it, the second collection onward finds
// an uncommitted change, which sets git_dirty and earns every later snapshot a
// warning saying its numbers belong to no commit. And nothing in this codebase
// couples a command's output flag to the artifact beside it, so pointing them
// at two paths runs the whole suite and then reads a file that run did not
// write.
//
// The expectation is keyed by FORMAT rather than by position, because swapping
// the two PATHS is the mistake config.Load cannot see: every format stays under
// its own key, and an assertion asking only whether each path appears somewhere
// in the argv passes, since after a swap both of them still do.
func TestEveryStarterWritesItsArtifactsOutsideTheMeasuredRepo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers []string
		// flags maps an artifact's format to the exact command-line element
		// that has to name it.
		flags map[config.Format]func(path string) string
	}{
		{"go", []string{"go.mod"}, map[config.Format]func(string) string{
			config.FormatGoCoverprofile: func(p string) string { return "-coverprofile=" + p },
		}},
		{"python", []string{"pyproject.toml"}, map[config.Format]func(string) string{
			config.FormatJUnitXML: func(p string) string { return "--junitxml=" + p },
			config.FormatLCOV:     func(p string) string { return "--cov-report=lcov:" + p },
		}},
		{"typescript", []string{"package.json"}, map[config.Format]func(string) string{
			config.FormatJUnitXML: func(p string) string { return "--outputFile=" + p },
			// The one artifact whose path is not the flag's value: vitest is
			// handed a directory and writes lcov.info inside it.
			config.FormatLCOV: func(p string) string {
				return "--coverage.reportsDirectory=" + filepath.Dir(p)
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path, _ := initInto(t, tc.markers...)
			repo := liveRepo(t, path)

			measured, err := filepath.Abs(repo.Path)
			if err != nil {
				t.Fatalf("resolving the measured repo path %q: %v", repo.Path, err)
			}

			checked := 0
			for _, s := range repo.Signals {
				// A step with no command writes nothing, so it has nothing to
				// keep out of the checkout. The lockfile the TypeScript starter
				// reads is a committed file and belongs exactly where it is.
				if !s.HasCommand() {
					continue
				}
				for _, a := range s.ArtifactList() {
					if !filepath.IsAbs(a.Path) {
						t.Errorf("step %s writes %s, which resolves inside the repo being measured", s.Name, a.Path)
						continue
					}
					if strings.HasPrefix(a.Path, measured+string(os.PathSeparator)) {
						t.Errorf("step %s writes %s, which is inside the measured repo at %s", s.Name, a.Path, measured)
					}
					want, ok := tc.flags[a.Format]
					if !ok {
						t.Errorf("step %s writes a %s artifact this test has no expected flag for", s.Name, a.Format)
						continue
					}
					if !slices.Contains(s.Command, want(a.Path)) {
						t.Errorf("step %s reads %s, but nothing in its command says %q: %v",
							s.Name, a.Path, want(a.Path), s.Command)
					}
					checked++
				}
			}
			// Without this the whole table passes on a starter whose live steps
			// stopped writing artifacts at all.
			if checked == 0 {
				t.Error("no artifact was checked, so this case asserted nothing")
			}
		})
	}
}

// A starter running no Go format carries a fingerprint, and the Go one is
// allowed to omit it.
//
// The snapshot's toolchain fingerprint is what lets the report refuse to diff
// two snapshots taken under different runtimes. A repo running a Go format is
// fingerprinted with go env without being asked, and a repo running none of
// them records that nothing identified it, forever, unless the config says what
// to ask instead. Without this line a non-Go starter is worse than the Go one
// it replaced, because it measures a real repo and cannot say what it measured
// it under.
func TestEveryNonGoStarterCarriesAFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers []string
	}{
		{"python", []string{"pyproject.toml"}},
		{"typescript", []string{"package.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path, _ := initInto(t, tc.markers...)
			repo := liveRepo(t, path)
			if repo.UsesGoToolchain() {
				t.Fatal("this starter declares a Go format, so go env identifies it and this test is asserting the wrong thing")
			}
			if len(repo.Fingerprint) == 0 {
				t.Error("no fingerprint, so every snapshot from this config records an unidentified toolchain and a runtime upgrade never shows up as one")
			}
		})
	}

	// The control, and the reason the Go starter is allowed to omit one.
	t.Run("go", func(t *testing.T) {
		_, path, _ := initInto(t, "go.mod")
		if repo := liveRepo(t, path); !repo.UsesGoToolchain() {
			t.Error("the Go starter declares no Go format, so nothing fingerprints it either and it needs a fingerprint line too")
		}
	})
}

// The TypeScript starter reads the lockfile that is actually there, paired with
// the format that parses it.
//
// The pairing is the part worth pinning. npm-lockfile finds nothing in a
// bun.lock, and hedging by declaring one step of each is not available: both
// formats write deps.total at repo scope, so config.Load rejects a repo naming
// both rather than letting two steps collide on the metrics primary key.
func TestTheTypeScriptStarterReadsTheLockfileThatIsThere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers []string
		want    config.Artifact
	}{
		{"bun", []string{"package.json", "bun.lock"}, config.Artifact{Path: "bun.lock", Format: config.FormatBunLockfile}},
		// npm is what writes a lockfile for a package.json whose owner chose no
		// package manager, so it is the fallback rather than a detection.
		{"npm", []string{"package.json"}, config.Artifact{Path: "package-lock.json", Format: config.FormatNPMLockfile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path, _ := initInto(t, tc.markers...)
			deps := signalByName(t, liveRepo(t, path), "dependencies")
			got := deps.ArtifactList()
			if len(got) != 1 {
				t.Fatalf("want one lockfile to read, got %v", got)
			}
			if got[0] != tc.want {
				t.Errorf("the dependencies step reads %+v, want %+v", got[0], tc.want)
			}
		})
	}
}

// The house style says no em dashes anywhere a user reads, and a generated
// config is read more closely than most of this tool's output, since it is the
// file someone edits. TestUsageHasNoEmDash covers the usage text; nothing
// covered this, and there are now three starters' worth of prose.
func TestNoStarterHasAnEmDash(t *testing.T) {
	for _, marker := range []string{"go.mod", "pyproject.toml", "package.json"} {
		t.Run(marker, func(t *testing.T) {
			_, path, _ := initInto(t, marker)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading back: %v", err)
			}
			if strings.Contains(string(body), "—") {
				t.Error("the starter written for this workspace contains an em dash")
			}
		})
	}
}
