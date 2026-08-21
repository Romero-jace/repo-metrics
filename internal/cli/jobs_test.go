package cli_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fleet is several ingest repos in one config, which is what --jobs and a
// repeated --repo need and what a single-repo fixture cannot exercise.
func fleet(t *testing.T, names ...string) (cfgPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "metrics.db")
	var entries strings.Builder
	for _, name := range names {
		repo := repoDir(t, dir, name, sampleProfile)
		entries.WriteString(ingestRepoEntry(name, repo, "coverage.out"))
	}
	cfgPath = writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s", dbPath, entries.String()))
	return cfgPath, dbPath
}

// collectedNames lists the repos the database holds a usable snapshot for.
func collectedNames(t *testing.T, dbPath string) []string {
	t.Helper()
	st := openStore(t, dbPath)
	repos, err := st.Repos(context.Background())
	if err != nil {
		t.Fatalf("listing repos: %v", err)
	}
	var out []string
	for _, r := range repos {
		snap, err := st.LatestSnapshot(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("LatestSnapshot: %v", err)
		}
		if snap != nil {
			out = append(out, r.Name)
		}
	}
	return out
}

// A full pass forked one collect per repo by hand on the reported fleet, because
// collect did one repo at a time with 20 to 30 minute timeouts. What it spends
// is subprocess time, so the repos can overlap.
//
// Run under -race this is also the only test that exercises the pool at all: the
// shared writers, the failed slice and the work channel are unsynchronized
// memory that a single-job run never touches concurrently.
func TestCollectWithJobsCollectsEveryRepo(t *testing.T) {
	cfgPath, dbPath := fleet(t, "alpha", "bravo", "charlie", "delta", "echo")

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--jobs", "4")
	if err != nil {
		t.Fatalf("collect --jobs 4: %v (stderr: %s)", err, stderr)
	}

	got := collectedNames(t, dbPath)
	if len(got) != 5 {
		t.Errorf("collected %v, want all 5 repos. A repo the pool dropped is exactly the silent gap this tool exists to catch", got)
	}
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("%s never appears in the progress output: %q", name, stdout)
		}
	}
}

// Above one job each repo's output is flushed as a block, so the table does not
// shred.
//
// Without the buffering the two lines of one repo are separated by whatever
// another repo wrote in between, and the aligned columns stop lining up with the
// repo they describe. This asserts adjacency rather than order: completion order
// is the honest order for a parallel run and is not fixed.
func TestCollectWithJobsKeepsEachReposOutputTogether(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha", "bravo", "charlie", "delta", "echo")

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--jobs", "4")
	if err != nil {
		t.Fatalf("collect --jobs 4: %v (stderr: %s)", err, stderr)
	}

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		start := -1
		for i, line := range lines {
			if strings.HasPrefix(line, "collecting "+name+" ") {
				start = i
				break
			}
		}
		if start < 0 {
			t.Errorf("no starting line for %s in:\n%s", name, stdout)
			continue
		}
		if start+1 >= len(lines) || !strings.HasPrefix(lines[start+1], name) {
			next := "(end of output)"
			if start+1 < len(lines) {
				next = lines[start+1]
			}
			t.Errorf("%s's completion line does not follow its starting line, so the blocks interleaved. Got %q after it", name, next)
		}
	}
}

// The default is one job and its output is what it has always been.
//
// This is the control for the buffering above. A build that buffered
// unconditionally would pass every assertion in this file except this one, and
// would move every repo's starting line to after its work, which is the opposite
// of what that line is for.
func TestCollectDefaultsToOneJobAndStreamsInConfigOrder(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha", "bravo", "charlie")

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath)
	if err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	want := []string{
		"collecting alpha (1 of 3)",
		"alpha",
		"collecting bravo (2 of 3)",
		"bravo",
		"collecting charlie (3 of 3)",
		"charlie",
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), stdout)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want it to start with %q", i, lines[i], prefix)
		}
	}
}

// --repo repeats, because a fleet re-measure is usually two or three repos.
func TestCollectRepoFlagRepeats(t *testing.T) {
	cfgPath, dbPath := fleet(t, "alpha", "bravo", "charlie")

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath,
		"--repo", "alpha", "--repo", "charlie"); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	got := collectedNames(t, dbPath)
	if strings.Join(got, ",") != "alpha,charlie" {
		t.Errorf("collected %v, want alpha and charlie and not bravo", got)
	}
}

// One bad name in a repeated flag fails the whole run, naming the bad one.
//
// Checked before anything is collected, so a typo cannot leave half a fleet
// measured and the other half not. An empty selection that exits 0 is the silent
// wrong answer this tool refuses everywhere else.
func TestCollectRejectsAnUnknownRepoBeforeCollectingAnything(t *testing.T) {
	cfgPath, dbPath := fleet(t, "alpha", "bravo")

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath,
		"--repo", "alpha", "--repo", "wroker")
	if err == nil {
		t.Fatalf("accepted a repo that is not in the config, rendering %q", stdout)
	}
	if !strings.Contains(stderr, "wroker") {
		t.Errorf("stderr does not name the typo: %q", stderr)
	}
	if !strings.Contains(stderr, "alpha") {
		t.Errorf("stderr does not list what is configured, so the message is a dead end: %q", stderr)
	}
	if got := collectedNames(t, dbPath); len(got) != 0 {
		t.Errorf("collected %v despite the bad name, so a typo half-measured the fleet", got)
	}
}

// --signal narrows to one step, which is the case where you wanted coverage and
// not a three minute module listing.
func TestCollectSignalNarrowsAndSaysWhatItCosts(t *testing.T) {
	dir := t.TempDir()
	repo := repoDir(t, dir, "service", sampleProfile)
	writeFile(t, filepath.Join(repo, "junit.xml"), sampleJUnit)
	dbPath := filepath.Join(dir, "metrics.db")
	entry := fmt.Sprintf(
		"  - name: service\n    path: %q\n    signals:\n"+
			"      - name: coverage\n        artifact: coverage.out\n        artifact_format: go-coverprofile\n"+
			"      - name: tests\n        artifact: junit.xml\n        artifact_format: junit-xml\n",
		repo)
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s", dbPath, entry))

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--signal", "coverage")
	if err != nil {
		t.Fatalf("collect --signal coverage: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stdout, "collected coverage") {
		t.Errorf("coverage did not run: %q", stdout)
	}
	if strings.Contains(stdout, "tests") {
		t.Errorf("the tests step ran despite being filtered out: %q", stdout)
	}

	// The cost has to be said out loud. This snapshot is ok rather than partial,
	// because nothing failed, so it is a legitimate baseline forever and next
	// week's report reads unmeasured for every signal skipped here. Snapshots
	// cannot be re-collected: they measured a tree at a commit that has moved on.
	if !strings.Contains(stderr, "narrower than the config") {
		t.Errorf("nothing warned that this run writes a narrower snapshot: %q", stderr)
	}
}

// A step name that matches nothing is an error listing what there is.
func TestCollectRejectsAnUnknownSignal(t *testing.T) {
	cfgPath, dbPath := fleet(t, "alpha")

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--signal", "lint")
	if err == nil {
		t.Fatalf("accepted a step no repo declares, rendering %q", stdout)
	}
	if !strings.Contains(stderr, "lint") || !strings.Contains(stderr, "coverage") {
		t.Errorf("stderr does not name the bad step and what is available: %q", stderr)
	}
	if got := collectedNames(t, dbPath); len(got) != 0 {
		t.Errorf("collected %v despite the bad step name", got)
	}
}

// A job count below one is refused rather than clamped, since it is a number
// somebody typed and clamping answers a question they did not ask.
func TestCollectRejectsANonPositiveJobCount(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha")

	stdout, stderr, err := runCLI(t, "collect", "--config", cfgPath, "--jobs", "0")
	if err == nil {
		t.Fatalf("accepted --jobs 0, rendering %q", stdout)
	}
	if !strings.Contains(stderr, "--jobs") {
		t.Errorf("stderr does not name the flag: %q", stderr)
	}
}

// Whatever the job count, the same snapshots land.
//
// The pool shares a store handle capped at one connection, and each repo's write
// is an upsert followed by a transaction. This is the assertion that those
// interleave safely, which -race cannot answer: it watches memory, not
// transactions.
func TestCollectStoresTheSameThingAtAnyJobCount(t *testing.T) {
	serialCfg, serialDB := fleet(t, "alpha", "bravo", "charlie", "delta")
	if _, stderr, err := runCLI(t, "collect", "--config", serialCfg); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}
	parallelCfg, parallelDB := fleet(t, "alpha", "bravo", "charlie", "delta")
	if _, stderr, err := runCLI(t, "collect", "--config", parallelCfg, "--jobs", "4"); err != nil {
		t.Fatalf("collect --jobs 4: %v (stderr: %s)", err, stderr)
	}

	serial, parallel := snapshotSummary(t, serialDB), snapshotSummary(t, parallelDB)
	if serial != parallel {
		t.Errorf("a parallel collection stored something different:\n serial:   %s\n parallel: %s", serial, parallel)
	}
	if !strings.Contains(serial, "alpha=ok") {
		t.Fatalf("the fixture stored nothing worth comparing: %s", serial)
	}
}

// snapshotSummary renders what each repo's newest snapshot measured, so two
// databases can be compared without depending on ids or timestamps.
func snapshotSummary(t *testing.T, dbPath string) string {
	t.Helper()
	st := openStore(t, dbPath)
	ctx := context.Background()
	repos, err := st.Repos(ctx)
	if err != nil {
		t.Fatalf("listing repos: %v", err)
	}
	var parts []string
	for _, r := range repos {
		snap, err := st.LatestSnapshotAny(ctx, r.ID)
		if err != nil {
			t.Fatalf("LatestSnapshotAny: %v", err)
		}
		if snap == nil {
			parts = append(parts, r.Name+"=none")
			continue
		}
		metrics, err := st.MetricsFor(ctx, snap.ID)
		if err != nil {
			t.Fatalf("MetricsFor: %v", err)
		}
		parts = append(parts, fmt.Sprintf("%s=%s/%d", r.Name, snap.Status, len(metrics)))
	}
	return strings.Join(parts, " ")
}
