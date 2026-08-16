package collect_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

const sampleProfile = `mode: set
example.com/m/alpha/a.go:1.1,3.2 8 1
example.com/m/alpha/a.go:5.1,6.2 2 0
example.com/m/beta/b.go:1.1,2.2 10 0
`

func repoDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeProfile drops a coverage profile and backdates it, standing in for one
// left behind by an earlier run.
func writeProfile(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(sampleProfile), 0o600); err != nil {
		t.Fatalf("writing profile: %v", err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("backdating profile: %v", err)
		}
	}
	return path
}

// coverageStep is the ordinary single-step shape: run something, read the
// profile it wrote. No command means ingest mode.
func coverageStep(command ...string) config.Signal {
	return config.Signal{
		Name:           "coverage",
		Command:        command,
		Artifact:       "coverage.out",
		ArtifactFormat: config.FormatGoCoverprofile,
		Timeout:        config.Duration(30 * time.Second),
		MaxAge:         config.Duration(24 * time.Hour),
	}
}

func repo(dir string, command ...string) config.Repo {
	return config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{coverageStep(command...)}}
}

func collectOnce(t *testing.T, r config.Repo) collect.Result {
	t.Helper()
	return collect.Collect(context.Background(), r, time.Now())
}

func diagText(res collect.Result) string {
	var b strings.Builder
	for _, d := range res.Diagnostics {
		b.WriteString(string(d.Severity))
		b.WriteString(": ")
		b.WriteString(d.Message)
		b.WriteString("\n")
	}
	return b.String()
}

func metric(t *testing.T, res collect.Result, key, scope string) float64 {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Key == key && m.Scope == scope {
			return m.Value
		}
	}
	t.Fatalf("metric %s/%s not found in %+v", key, scope, res.Metrics)
	return 0
}

func hasMetric(res collect.Result, key string) bool {
	for _, m := range res.Metrics {
		if m.Key == key {
			return true
		}
	}
	return false
}

// The headline case, drawn from a real repository.
//
// `make coverage-all` there is declared .PHONY with no rule: it prints "Nothing
// to be done" and exits 0 while writing nothing, and a months-old profile sits
// at the configured path. A collector that trusts exit code 0 would report that
// stale file as today's coverage indefinitely, with no error anywhere.
func TestPhantomCommandIsFailedNotStaleData(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 90*24*time.Hour)

	// `true` is the moral equivalent: exits 0, does nothing.
	res := collectOnce(t, repo(dir, "true"))

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want %q. A stale profile was accepted as current data.",
			res.Snapshot.Status, store.StatusFailed)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("a failed collection recorded %d metrics; it must record none", len(res.Metrics))
	}
	if !strings.Contains(diagText(res), "wrote no fresh") {
		t.Errorf("diagnostics do not explain the staleness:\n%s", diagText(res))
	}
}

func TestCommandThatWritesTheProfileSucceeds(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)

	res := collectOnce(t, repo(dir, "cp", src, filepath.Join(dir, "coverage.out")))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if got := metric(t, res, collect.KeyCoveredStmts, "example.com/m/alpha"); got != 8 {
		t.Errorf("alpha covered: got %v, want 8", got)
	}
	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("alpha total: got %v, want 10", got)
	}
	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/beta"); got != 10 {
		t.Errorf("beta total: got %v, want 10", got)
	}
	if res.Snapshot.Duration <= 0 {
		t.Error("collection duration was not recorded")
	}
	if len(res.Collected) != 1 || res.Collected[0] != "coverage" {
		t.Errorf("Collected: got %v, want just the coverage signal", res.Collected)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed: got %v, want none", res.Failed)
	}
}

// A red suite still produces a real profile, and those numbers are worth
// keeping. Throwing them away would blank out coverage history for exactly the
// weeks when someone most wants to look at it.
func TestFailingSuiteStillRecordsCoverage(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")

	res := collectOnce(t, repo(dir, "sh", "-c", "cp "+src+" "+dst+" && echo boom >&2 && exit 3"))

	if res.Snapshot.Status != store.StatusPartial {
		t.Fatalf("Status: got %q, want partial. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("coverage was discarded on a failing suite: alpha total %v", got)
	}
	if !strings.Contains(diagText(res), "exited 3") {
		t.Errorf("exit code not reported:\n%s", diagText(res))
	}
}

func TestTimeoutIsFailed(t *testing.T) {
	dir := repoDir(t)
	r := repo(dir, "sleep", "30")
	r.Signals[0].Timeout = config.Duration(200 * time.Millisecond)

	res := collectOnce(t, r)

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if !strings.Contains(diagText(res), "timeout") {
		t.Errorf("diagnostics do not mention the timeout:\n%s", diagText(res))
	}
}

func TestMissingBinaryIsFailed(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 0)

	res := collectOnce(t, repo(dir, "/nonexistent/binary"))

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if res.Snapshot.Error == "" {
		t.Error("a failed snapshot recorded no error text")
	}
}

func TestUnparseableProfileIsFailed(t *testing.T) {
	dir := repoDir(t)
	path := filepath.Join(dir, "coverage.out")
	if err := os.WriteFile(path, []byte("this is not a coverage profile\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
}

// Ingest mode: something else produces the artifact, so age is the only
// freshness signal there is. The numbers still get recorded, but flagged.
func TestIngestModeFlagsStaleArtifact(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 72*time.Hour)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusPartial {
		t.Fatalf("Status: got %q, want partial. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if !strings.Contains(diagText(res), "stale") {
		t.Errorf("staleness not surfaced:\n%s", diagText(res))
	}
	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("stale numbers should still be recorded: got %v", got)
	}
}

func TestIngestModeAcceptsFreshArtifact(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", time.Hour)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
}

func TestIngestModeMissingArtifactIsFailed(t *testing.T) {
	res := collectOnce(t, repo(repoDir(t)))

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if !strings.Contains(diagText(res), "no command configured") {
		t.Errorf("diagnostics do not explain the situation:\n%s", diagText(res))
	}
}

// Test counts and the untested-package count come from the captured stdout
// stream, which coverage alone cannot provide.
func TestStdoutStreamProducesTestMetrics(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")

	stream := strings.Join([]string{
		`{"Action":"pass","Package":"example.com/m/alpha","Test":"TestOne","Elapsed":0.01}`,
		`{"Action":"fail","Package":"example.com/m/alpha","Test":"TestTwo","Elapsed":0.02}`,
		`{"Action":"fail","Package":"example.com/m/alpha","Elapsed":0.5}`,
		`{"Action":"output","Package":"example.com/m/beta","Output":"?   \texample.com/m/beta\t[no test files]\n"}`,
		`{"Action":"skip","Package":"example.com/m/beta"}`,
	}, "\n")

	r := repo(dir, "sh", "-c", "cp "+src+" "+dst+" && cat "+writeFile(t, dir, "stream.json", stream))
	r.Signals[0].StdoutFormat = config.FormatGoTestJSON

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	if got := metric(t, res, collect.KeyTestCount, "example.com/m/alpha"); got != 2 {
		t.Errorf("alpha test count: got %v, want 2", got)
	}
	if got := metric(t, res, collect.KeyTestFailed, "example.com/m/alpha"); got != 1 {
		t.Errorf("alpha failed: got %v, want 1", got)
	}
	if got := metric(t, res, collect.KeyTestDurationMS, "example.com/m/alpha"); got != 500 {
		t.Errorf("alpha duration: got %v, want 500", got)
	}
	if got := metric(t, res, collect.KeyPkgWithoutTest, ""); got != 1 {
		t.Errorf("packages without tests: got %v, want 1", got)
	}
}

// Losing one of a step's parsers costs that parser's measurements. It must not
// cost the ones that worked.
//
// This used to be spelled out as a special case for the one pairing that
// existed. It is now the general rule: a step survives as long as one of its
// parsers completes.
func TestUnusableStdoutDowngradesButKeepsCoverage(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")

	r := repo(dir, "sh", "-c", "cp "+src+" "+dst+" && echo 'not json'")
	r.Signals[0].StdoutFormat = config.FormatGoTestJSON

	res := collectOnce(t, r)

	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("coverage lost along with the stream: got %v", got)
	}
	if !strings.Contains(diagText(res), "could not read the go-test-json output") {
		t.Errorf("the loss was not disclosed:\n%s", diagText(res))
	}
	// The step still counts as collected, because coverage came back, and the
	// loss shows up as degradation rather than as a missing signal.
	if len(res.Failed) != 0 {
		t.Errorf("Failed: got %v, want none: the step produced coverage", res.Failed)
	}
	if res.Snapshot.Status != store.StatusPartial {
		t.Errorf("Status: got %q, want partial: something was lost", res.Snapshot.Status)
	}
}

// The point of the whole rewrite: one repo can measure several things, and one
// of them failing costs only its own numbers.
func TestOneFailingSignalDoesNotCostTheOthers(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")

	stream := `{"Action":"pass","Package":"example.com/m/alpha","Test":"TestOne","Elapsed":0.01}`

	// The broken step sits BETWEEN two working ones, deliberately. With it first
	// there is nothing collected yet for a discard-everything bug to discard, so
	// the old behavior of nilling every metric on any failure would survive this
	// test unnoticed. Confirmed by mutation: moved to the front, it does.
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		coverageStep("cp", src, dst),
		// The command exits 0 and writes nothing, which is the phantom case.
		{
			Name:           "broken",
			Command:        []string{"true"},
			Artifact:       "never-written.out",
			ArtifactFormat: config.FormatGoCoverprofile,
			Timeout:        config.Duration(30 * time.Second),
		},
		{
			Name:         "tests",
			Command:      []string{"cat", writeFile(t, dir, "stream.json", stream)},
			StdoutFormat: config.FormatGoTestJSON,
			Timeout:      config.Duration(30 * time.Second),
		},
	}}

	res := collectOnce(t, r)

	if res.Snapshot.Status != store.StatusPartial {
		t.Fatalf("Status: got %q, want partial. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("coverage was lost to an unrelated signal's failure: got %v", got)
	}
	if got := metric(t, res, collect.KeyTestCount, "example.com/m/alpha"); got != 1 {
		t.Errorf("test counts were lost to an unrelated signal's failure: got %v", got)
	}
	if strings.Join(res.Collected, ",") != "coverage,tests" {
		t.Errorf("Collected: got %v, want coverage and tests", res.Collected)
	}
	if strings.Join(res.Failed, ",") != "broken" {
		t.Errorf("Failed: got %v, want just broken", res.Failed)
	}
	// Every diagnostic has to say which signal it came from. With one step the
	// source was obvious; with three a bare message names nothing.
	if !strings.Contains(diagText(res), "broken: command exited 0 but wrote no fresh") {
		t.Errorf("the diagnostic does not name the signal it came from:\n%s", diagText(res))
	}
}

// A stale or missing artifact costs the artifact's parser and nothing else.
//
// This shipped broken and was found by review. The freshness check returned
// before any parsing happened, so a step reading both a coverage profile and a
// test stream lost the whole stream because the profile was not where the config
// said. That is the mirror of the case parseAll exists to prevent, in the one
// direction nothing covered, and a typo in artifact: was enough to trigger it.
func TestAStaleArtifactCostsOnlyItsOwnParser(t *testing.T) {
	dir := repoDir(t)
	stream := strings.Join([]string{
		`{"Action":"pass","Package":"example.com/m/alpha","Test":"TestOne","Elapsed":0.01}`,
		`{"Action":"fail","Package":"example.com/m/alpha","Test":"TestTwo","Elapsed":0.02}`,
	}, "\n")
	// The command really does write a profile, just not where the config looks.
	writeProfile(t, dir, "coverage.out", 0)

	step := coverageStep("cat", writeFile(t, dir, "stream.json", stream))
	step.Artifact = "cover.out" // the typo
	step.StdoutFormat = config.FormatGoTestJSON

	res := collectOnce(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}})

	if got := metric(t, res, collect.KeyTestCount, "example.com/m/alpha"); got != 2 {
		t.Errorf("test count: got %v, want 2. The stream parsed fine and was discarded because a sibling parser's file was missing.", got)
	}
	if !hasMetric(res, collect.KeyPkgWithoutTest) {
		t.Error("the marker for all five test signals was dropped, so every one of them reads as never measured")
	}
	// Coverage is the thing that genuinely could not be measured, so it must be
	// absent rather than zero.
	if hasMetric(res, collect.KeyTotalStmts) {
		t.Error("coverage was recorded from an artifact the freshness check rejected")
	}
	if res.Snapshot.Status != store.StatusPartial {
		t.Errorf("Status: got %q, want partial: something was measured and something was lost", res.Snapshot.Status)
	}
	if !strings.Contains(diagText(res), "wrote no fresh") {
		t.Errorf("the missing artifact was not disclosed:\n%s", diagText(res))
	}
}

// The control for the test above, and the one the whole tool is built around: a
// step whose ONLY source is the missing artifact still fails outright. Loosening
// the freshness check rather than narrowing it would have reported a months-old
// profile as today's number.
func TestAStaleArtifactStillFailsAStepThatHasNothingElse(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 90*24*time.Hour)

	res := collectOnce(t, repo(dir, "true"))

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("a stale profile produced %d metrics", len(res.Metrics))
	}
}

// Every step failing is a failed snapshot carrying nothing, not a partial one.
func TestEverySignalFailingIsAFailedSnapshot(t *testing.T) {
	dir := repoDir(t)

	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		coverageStep(),
		{
			Name:           "other",
			Artifact:       "also-missing.out",
			ArtifactFormat: config.FormatGoCoverprofile,
			Timeout:        config.Duration(30 * time.Second),
		},
	}}

	res := collectOnce(t, r)

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("a failed collection recorded %d metrics; it must record none", len(res.Metrics))
	}
	if len(res.Collected) != 0 {
		t.Errorf("Collected: got %v, want none", res.Collected)
	}
}

// Two steps writing the same metric key collide on the metrics table's primary
// key, and the INSERT that fails takes every other step's numbers with it.
//
// Config validation rejects the shapes that can cause this, so reaching here
// means something got past that. The cheapest correct answer is to lose the
// second step rather than the snapshot, and to say so.
func TestDuplicateMetricKeysCostOneSignalRatherThanTheSnapshot(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)

	// Two steps reading the same profile under different names, which config
	// validation would reject. Built by hand precisely because it would.
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		{
			Name:           "first",
			Command:        []string{"cp", src, filepath.Join(dir, "one.out")},
			Artifact:       "one.out",
			ArtifactFormat: config.FormatGoCoverprofile,
			Timeout:        config.Duration(30 * time.Second),
		},
		{
			Name:           "second",
			Command:        []string{"cp", src, filepath.Join(dir, "two.out")},
			Artifact:       "two.out",
			ArtifactFormat: config.FormatGoCoverprofile,
			Timeout:        config.Duration(30 * time.Second),
		},
	}}

	res := collectOnce(t, r)

	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("the first signal's numbers were lost: got %v", got)
	}
	if strings.Join(res.Failed, ",") != "second" {
		t.Errorf("Failed: got %v, want just the colliding signal", res.Failed)
	}
	if !strings.Contains(diagText(res), "a second time") {
		t.Errorf("the collision was not explained:\n%s", diagText(res))
	}
	// One row per key and scope, or the insert this guard exists to protect
	// would still fail.
	seen := map[[2]string]bool{}
	for _, m := range res.Metrics {
		k := [2]string{m.Key, m.Scope}
		if seen[k] {
			t.Fatalf("duplicate metric %v survived the guard", k)
		}
		seen[k] = true
	}
}

// An environment fingerprint has to be recorded even when the directory is not
// a Go module, or snapshots become silently incomparable.
func TestEnvFingerprintIsAlwaysRecorded(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 0)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Env == "" {
		t.Error("no environment fingerprint recorded")
	}
	if !strings.HasPrefix(res.Snapshot.Env, "go=") {
		t.Errorf("unexpected fingerprint shape: %q", res.Snapshot.Env)
	}
}

// A directory that is not a git checkout is still measurable.
func TestNonGitDirectoryStillCollects(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 0)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if res.Snapshot.GitSHA != "" {
		t.Errorf("GitSHA: got %q, want empty outside a checkout", res.Snapshot.GitSHA)
	}
	// The other two fields are stored on every snapshot too, so leaving them
	// unasserted is how a fabricated branch name or a stuck dirty flag would
	// get in without anything turning red.
	if res.Snapshot.GitBranch != "" {
		t.Errorf("GitBranch: got %q, want empty outside a checkout", res.Snapshot.GitBranch)
	}
	if res.Snapshot.GitDirty {
		t.Error("GitDirty: got true outside a checkout, where there is no tree to be dirty")
	}
	if !strings.Contains(diagText(res), "git metadata unavailable") {
		t.Errorf("the missing metadata was not disclosed:\n%s", diagText(res))
	}
}

// The env field exists so a repo needing GOWORK=off does not have to smuggle it
// into the argv as `env GOWORK=off go test ...`. What matters is that the value
// reaches the subprocess, so the command here refuses to produce a profile
// without it and writes what it actually saw to disk.
func TestConfiguredEnvReachesTheCommand(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")
	seen := filepath.Join(dir, "seen.txt")

	script := "printf '%s' \"$RM_TEST_ENV\" > " + seen +
		" && test -n \"$RM_TEST_ENV\" && cp " + src + " " + dst

	t.Run("from the repo block", func(t *testing.T) {
		r := repo(dir, "sh", "-c", script)
		r.Env = map[string]string{"RM_TEST_ENV": "gowork-off"}

		res := collectOnce(t, r)

		if res.Snapshot.Status != store.StatusOK {
			t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
		}
		got, err := os.ReadFile(seen) //nolint:gosec // path is our own temp file
		if err != nil {
			t.Fatalf("reading what the subprocess saw: %v", err)
		}
		if string(got) != "gowork-off" {
			t.Errorf("subprocess saw RM_TEST_ENV=%q, want %q", got, "gowork-off")
		}
	})

	// A step's own env has to reach the subprocess too, and win where the two
	// disagree. Otherwise the per-step block is decoration.
	t.Run("a signal's own env wins over the repo's", func(t *testing.T) {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clearing the profile: %v", err)
		}
		r := repo(dir, "sh", "-c", script)
		r.Env = map[string]string{"RM_TEST_ENV": "from-repo"}
		r.Signals[0].Env = map[string]string{"RM_TEST_ENV": "from-signal"}

		res := collectOnce(t, r)

		if res.Snapshot.Status != store.StatusOK {
			t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
		}
		got, err := os.ReadFile(seen) //nolint:gosec // path is our own temp file
		if err != nil {
			t.Fatalf("reading what the subprocess saw: %v", err)
		}
		if string(got) != "from-signal" {
			t.Errorf("subprocess saw RM_TEST_ENV=%q, want the signal's value", got)
		}
	})

	// The anti-vacuity control. Without this the cases above would still pass if
	// the variable were exported by the test process itself rather than threaded
	// through the config.
	t.Run("without it the same command fails", func(t *testing.T) {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clearing the profile: %v", err)
		}
		res := collectOnce(t, repo(dir, "sh", "-c", script))

		if res.Snapshot.Status != store.StatusFailed {
			t.Errorf("Status: got %q, want failed. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
		}
	})
}

// Several variables at once, because one is the case where a missing sort
// cannot show. The ordering itself is asserted in envpairs_internal_test.go:
// a shell rebuilds its exported environment in hash order, so `env` inside
// `sh -c` cannot observe the order the argv env was handed over in.
func TestSeveralConfiguredVarsAllArrive(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")
	dump := filepath.Join(dir, "vars.txt")

	r := repo(dir, "sh", "-c",
		"printf '%s,%s,%s' \"$RM_MANY_A\" \"$RM_MANY_B\" \"$RM_MANY_C\" > "+dump+
			" && cp "+src+" "+dst)
	r.Env = map[string]string{"RM_MANY_C": "three", "RM_MANY_A": "one", "RM_MANY_B": "two"}

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	got, err := os.ReadFile(dump) //nolint:gosec // path is our own temp file
	if err != nil {
		t.Fatalf("reading what the subprocess saw: %v", err)
	}
	if string(got) != "one,two,three" {
		t.Errorf("subprocess saw %q, want %q", got, "one,two,three")
	}
}

// No env configured must not mean an empty non-nil slice or a replaced
// environment: the subprocess still needs PATH to find anything at all.
func TestNoConfiguredEnvLeavesTheEnvironmentAlone(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")
	dump := filepath.Join(dir, "path.txt")

	res := collectOnce(t, repo(dir, "sh", "-c",
		"printf '%s' \"$PATH\" > "+dump+" && cp "+src+" "+dst))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	got, err := os.ReadFile(dump) //nolint:gosec // path is our own temp file
	if err != nil {
		t.Fatalf("reading PATH as the subprocess saw it: %v", err)
	}
	if len(got) == 0 {
		t.Error("the subprocess inherited an empty PATH")
	}
}

// A step with no artifact at all is legal: the measurement is entirely in the
// command's stdout. The freshness guard has nothing to check and must not
// invent something to fail on.
func TestStdoutOnlySignalNeedsNoArtifact(t *testing.T) {
	dir := repoDir(t)
	stream := `{"Action":"pass","Package":"example.com/m/alpha","Test":"TestOne","Elapsed":0.01}`

	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{{
		Name:         "tests",
		Command:      []string{"cat", writeFile(t, dir, "stream.json", stream)},
		StdoutFormat: config.FormatGoTestJSON,
		Timeout:      config.Duration(30 * time.Second),
	}}}

	res := collectOnce(t, r)

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if got := metric(t, res, collect.KeyTestCount, "example.com/m/alpha"); got != 1 {
		t.Errorf("alpha test count: got %v, want 1", got)
	}
	// Coverage was never asked for, so it must be absent rather than zero.
	if hasMetric(res, collect.KeyTotalStmts) {
		t.Error("a tests-only signal recorded coverage statements out of nowhere")
	}
}

// A profile with nothing instrumented parses fine and yields no metrics. That is
// a step that worked and measured nothing, which is a finding, not a failure.
// Counting metrics rather than parses would call it broken.
func TestHeaderOnlyProfileIsCollectedNotFailed(t *testing.T) {
	dir := repoDir(t)
	if err := os.WriteFile(filepath.Join(dir, "coverage.out"), []byte("mode: set\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if len(res.Metrics) != 0 {
		t.Errorf("a header-only profile produced %d metrics, want none", len(res.Metrics))
	}
	if strings.Join(res.Collected, ",") != "coverage" {
		t.Errorf("Collected: got %v, want the coverage signal", res.Collected)
	}
	if !strings.Contains(diagText(res), "no instrumented packages") {
		t.Errorf("the empty profile was not disclosed:\n%s", diagText(res))
	}
}

// sampleSARIF is the shape golangci-lint v2 actually emits: one run, a driver
// carrying only a name, and no rules array to resolve severities against.
//
// Three results, one of them suppressed. Active findings are therefore 2, errors
// 1, suppressed 1, and the suppressed one is deliberately at error level so a
// parser that folded suppressions into the totals would report 2 errors and be
// caught here.
const sampleSARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"golangci-lint"}},"results":[
  {"ruleId":"errcheck","level":"error","message":{"text":"unchecked error"}},
  {"ruleId":"govet","level":"warning","message":{"text":"shadowed variable"}},
  {"ruleId":"staticcheck","level":"error","message":{"text":"triaged"},"suppressions":[{"kind":"inSource"}]}
]}]}`

// cleanSARIF is a linter that ran and found nothing. It is the case the whole
// marker contract exists for: without a row it is byte-identical to a repo
// nobody lints, and a repo with nothing left to fix is the good news this tool
// should be able to report.
const cleanSARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"golangci-lint"}},"results":[]}]}`

func lintStep(name, path string) config.Signal {
	return config.Signal{
		Name:         name,
		Command:      []string{"cat", path},
		StdoutFormat: config.FormatSARIF,
		Timeout:      config.Duration(30 * time.Second),
	}
}

func TestSARIFStepCountsFindings(t *testing.T) {
	dir := repoDir(t)
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		lintStep("lint", writeFile(t, dir, "report.sarif", sampleSARIF)),
	}}

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	// Scoped by the step's name rather than repo-level, which is what lets two
	// linters coexist in one repo.
	if got := metric(t, res, collect.KeyLintFindings, "lint"); got != 2 {
		t.Errorf("active findings: got %v, want 2", got)
	}
	if got := metric(t, res, collect.KeyLintErrors, "lint"); got != 1 {
		t.Errorf("error-level findings: got %v, want 1; the suppressed error must not be counted", got)
	}
	if got := metric(t, res, collect.KeyLintSuppressed, "lint"); got != 1 {
		t.Errorf("suppressed: got %v, want 1", got)
	}
}

// A linter that ran and found nothing measured zero. That has to reach the
// database as a row, because the alternative is indistinguishable from a repo
// nobody lints, and the two call for opposite reactions.
func TestCleanLintRunStoresAMeasuredZero(t *testing.T) {
	dir := repoDir(t)
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		lintStep("lint", writeFile(t, dir, "clean.sarif", cleanSARIF)),
	}}

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if got := metric(t, res, collect.KeyLintFindings, "lint"); got != 0 {
		t.Errorf("findings: got %v, want a measured 0", got)
	}
}

// golangci-lint exits 1 whenever it finds anything, and so do eslint, ruff and
// clippy. Treating that as degradation would mark every snapshot partial forever
// on any repo that has a single outstanding nit, and the status field would stop
// meaning anything.
//
// `go test` is the opposite case and is asserted separately by
// TestFailingSuiteStillRecordsCoverage: a red suite IS worth flagging.
func TestALinterExitingNonZeroIsNotDegradation(t *testing.T) {
	dir := repoDir(t)
	sarifPath := writeFile(t, dir, "report.sarif", sampleSARIF)

	step := lintStep("lint", sarifPath)
	step.Command = []string{"sh", "-c", "cat " + sarifPath + " && exit 1"}
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}

	res := collectOnce(t, r)

	if res.Snapshot.Status != store.StatusOK {
		t.Errorf("Status: got %q, want ok. A linter reporting findings is the measurement, not a failure.\nDiagnostics:\n%s",
			res.Snapshot.Status, diagText(res))
	}
	if got := metric(t, res, collect.KeyLintFindings, "lint"); got != 2 {
		t.Errorf("findings: got %v, want 2", got)
	}
}

// SARIF is the one repeatable format, because a polyglot repo genuinely runs two
// linters. Two steps have to sum rather than collide on the metrics primary key.
func TestTwoLintStepsSumRatherThanCollide(t *testing.T) {
	dir := repoDir(t)
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		lintStep("go", writeFile(t, dir, "go.sarif", sampleSARIF)),
		lintStep("js", writeFile(t, dir, "js.sarif", cleanSARIF)),
	}}

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed: got %v, want none: two lint steps must not collide", res.Failed)
	}
	if got := metric(t, res, collect.KeyLintFindings, "go"); got != 2 {
		t.Errorf("go findings: got %v, want 2", got)
	}
	// The clean one still stores its zero, which is the whole reason the rows are
	// scoped: a repo whose JavaScript is clean and whose Go is not must not read
	// as either one alone.
	if got := metric(t, res, collect.KeyLintFindings, "js"); got != 0 {
		t.Errorf("js findings: got %v, want a measured 0", got)
	}
}

// A crashed linter's partial output must not read as a clean repo. The parser
// refuses it, and the step fails rather than storing a flattering zero.
func TestTruncatedSARIFFailsRatherThanReportingZero(t *testing.T) {
	dir := repoDir(t)
	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		lintStep("lint", writeFile(t, dir, "broken.sarif", `{"version":"2.1.0","runs":[{"tool":`)),
	}}

	res := collectOnce(t, r)

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if hasMetric(res, collect.KeyLintFindings) {
		t.Error("a truncated analysis log produced a findings count, which would read as a clean repo")
	}
}

// moduleStream is what `go list -m -u -json all` writes: a run of concatenated
// JSON objects, NOT an array, with the main module first.
//
// One dependency, with a publish timestamp and an available update, so the
// median age is that one module's age and the outdated count is 1. The main
// module is excluded from every count: a repo is not one of its own
// dependencies.
const moduleStream = `{"Path":"example.com/m","Main":true,"Dir":"/tmp/m"}
{"Path":"github.com/a/b","Version":"v1.2.3","Time":"2024-01-02T00:00:00Z","Update":{"Path":"github.com/a/b","Version":"v1.3.0"}}
`

// modulePinnedAt is the one dependency's publish date, and moduleNow is the
// collection time, so the expected age is arithmetic rather than a magic number.
var (
	modulePinnedAt = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	moduleNow      = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// The runner has measured every command's wall clock since it was written and
// nothing ever read it. Now it is a metric, scoped by step, so a repo running
// four of them can see which one got slower.
func TestEachSignalRecordsItsOwnCommandDuration(t *testing.T) {
	dir := repoDir(t)
	src := writeProfile(t, dir, "src.out", 0)
	dst := filepath.Join(dir, "coverage.out")

	r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
		coverageStep("sh", "-c", "sleep 0.2 && cp "+src+" "+dst),
		lintStep("lint", writeFile(t, dir, "clean.sarif", cleanSARIF)),
	}}

	res := collectAt(t, r, time.Now())
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	// Per step, not per repo. A single repo-level number would say a collection
	// got slower without saying which part of it did.
	slow := metric(t, res, collect.KeySignalDurationMS, "coverage")
	fast := metric(t, res, collect.KeySignalDurationMS, "lint")
	if slow < 150 {
		t.Errorf("the coverage step slept 200ms and recorded %v ms", slow)
	}
	if slow <= fast {
		t.Errorf("the deliberately slow step recorded %v ms and the fast one %v ms, so this is not measuring per-step time at all",
			slow, fast)
	}

	// The snapshot's own duration covers the whole collection including the git
	// subprocesses and the toolchain fingerprint, so it is a different number and
	// has to be at least the sum of the parts.
	if res.Snapshot.Duration.Milliseconds() < int64(slow+fast) {
		t.Errorf("snapshot duration %v is less than the sum of its steps (%v ms), so one of the two is measuring the wrong thing",
			res.Snapshot.Duration, slow+fast)
	}
}

// Ingest mode runs nothing, so there is no wall time to record. A zero here
// would be a duration nobody measured, and over a baseline that ran a real
// command it would render as the collection having got dramatically faster.
func TestIngestModeRecordsNoDuration(t *testing.T) {
	dir := repoDir(t)
	writeProfile(t, dir, "coverage.out", 0)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if hasMetric(res, collect.KeySignalDurationMS) {
		t.Error("a duration was recorded for a signal that never ran a command")
	}
}

// depsStep runs a command that prints a canned module stream.
//
// The stream is canned rather than produced by a real `go list`, which would
// make these tests depend on the network and on this repo's own module graph.
// But the -u still has to be genuinely present in the argv, because that is what
// repo-metrics reads to decide whether updates were checked, so it is passed as
// a positional argument to sh: `sh -c 'cat FILE' -u` sets $0 to -u and runs the
// cat. Faking that by rewriting the argv would test the fake instead.
func depsStep(t *testing.T, dir, stream string, withUpdateFlag bool) config.Signal {
	t.Helper()
	argv := []string{"sh", "-c", "cat " + writeFile(t, dir, "modules.json", stream)}
	if withUpdateFlag {
		argv = append(argv, "-u")
	}
	return config.Signal{
		Name:         "deps",
		Command:      argv,
		StdoutFormat: config.FormatGoListModules,
		Timeout:      config.Duration(30 * time.Second),
	}
}

func collectAt(t *testing.T, r config.Repo, now time.Time) collect.Result {
	t.Helper()
	return collect.Collect(context.Background(), r, now)
}

func TestModuleStreamCountsDependencies(t *testing.T) {
	dir := repoDir(t)
	step := depsStep(t, dir, moduleStream, true)
	// A proxy that is set to anything but off. The value is never dialed: it is
	// read to answer whether the toolchain was in a position to check at all.
	step.Env = map[string]string{"GOPROXY": "https://proxy.example"}

	res := collectAt(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}, moduleNow)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 1 {
		t.Errorf("dependencies: got %v, want 1; the main module must not count as its own dependency", got)
	}
	wantDays := moduleNow.Sub(modulePinnedAt).Hours() / 24
	if got := metric(t, res, collect.KeyDepsAgeMedianDays, ""); got != wantDays {
		t.Errorf("median age: got %v days, want %v", got, wantDays)
	}
	if got := metric(t, res, collect.KeyDepsOutdatedDirect, ""); got != 1 {
		t.Errorf("outdated direct dependencies: got %v, want 1", got)
	}
}

// The case this whole design exists for.
//
// With GOPROXY=off the toolchain exits 0, writes nothing to stderr, streams
// every module, and reports an update on none of them. "Every dependency is
// current" and "nothing was ever checked" are byte-identical streams, so no
// parser can tell them apart and the only honest answer is to record nothing.
//
// The other two measurements are unaffected, which is why the three dependency
// signals do not share a marker.
func TestGoproxyOffRecordsNoUpdateCount(t *testing.T) {
	dir := repoDir(t)
	step := depsStep(t, dir, moduleStream, true)
	step.Env = map[string]string{"GOPROXY": "off"}

	res := collectAt(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}, moduleNow)

	if hasMetric(res, collect.KeyDepsOutdatedDirect) {
		t.Error("an outdated-dependency count was recorded from a run that never consulted the proxy. Zero and unmeasured are the same bytes here, and only one of them is true.")
	}
	if !strings.Contains(diagText(res), "GOPROXY is off") {
		t.Errorf("the reason was not disclosed:\n%s", diagText(res))
	}
	// The offline-measurable half survives. Losing it too would make the whole
	// signal worthless on a machine with no network.
	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 1 {
		t.Errorf("dependencies: got %v, want 1; the module count needs no proxy", got)
	}
	if !hasMetric(res, collect.KeyDepsAgeMedianDays) {
		t.Error("the age aggregate was dropped along with the update check, but publish timestamps are in the stream and need no network")
	}
}

// Without -u the toolchain never populates Update, so every module reads as
// current. The flag is read off the argv rather than from a config field,
// because a field can disagree with the command and then the tool records
// confident zeroes.
func TestAModuleListWithoutTheUpdateFlagRecordsNoUpdateCount(t *testing.T) {
	dir := repoDir(t)
	step := depsStep(t, dir, moduleStream, false)
	step.Env = map[string]string{"GOPROXY": "https://proxy.example"}

	res := collectAt(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}, moduleNow)

	if hasMetric(res, collect.KeyDepsOutdatedDirect) {
		t.Error("an outdated count was recorded from a command that never asked for updates")
	}
	if !strings.Contains(diagText(res), "-u") {
		t.Errorf("the reason was not disclosed:\n%s", diagText(res))
	}
	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 1 {
		t.Errorf("dependencies: got %v, want 1", got)
	}
}

// A proxy that answers for some modules and fails on others reads as FEWER
// updates available, which is worse than a hard failure because it looks like
// good news. Any unresolved module makes every count a floor rather than a
// total.
func TestUnresolvedModulesRecordNoUpdateCount(t *testing.T) {
	dir := repoDir(t)
	stream := moduleStream +
		`{"Path":"github.com/c/d","Version":"v0.1.0","Error":{"Err":"module lookup disabled"}}` + "\n"
	step := depsStep(t, dir, stream, true)
	step.Env = map[string]string{"GOPROXY": "https://proxy.example"}

	res := collectAt(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}, moduleNow)

	if hasMetric(res, collect.KeyDepsOutdatedDirect) {
		t.Error("an outdated count was recorded while a module failed to resolve, so the number is a floor being published as a total")
	}
	if !strings.Contains(diagText(res), "could not be resolved") {
		t.Errorf("the reason was not disclosed:\n%s", diagText(res))
	}
	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 2 {
		t.Errorf("dependencies: got %v, want 2; an unresolvable module is still a dependency", got)
	}
}

// A directory replacement has no version and no publish timestamp. That is a
// normal state rather than a parse failure, and it is not an age of zero.
func TestModulesWithoutTimestampsRecordNoAge(t *testing.T) {
	dir := repoDir(t)
	stream := `{"Path":"example.com/m","Main":true}
{"Path":"github.com/a/b","Replace":{"Path":"../local"}}
`
	step := depsStep(t, dir, stream, true)
	step.Env = map[string]string{"GOPROXY": "off"}

	res := collectAt(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}, moduleNow)

	if hasMetric(res, collect.KeyDepsAgeMedianDays) {
		t.Error("an age was recorded for a dependency set in which nothing carried a publish date")
	}
	if !strings.Contains(diagText(res), "publish timestamp") {
		t.Errorf("the reason was not disclosed:\n%s", diagText(res))
	}
	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 1 {
		t.Errorf("dependencies: got %v, want 1", got)
	}
}

// A truncated stream means the module set is incomplete, and every count from it
// is wrong LOW, including the count of dependencies needing an update. Reporting
// fewer stale dependencies than a repo really has is the failure this tool exists
// to refuse, so a partial stream produces no summary at all.
func TestTruncatedModuleStreamFailsRatherThanUndercounting(t *testing.T) {
	dir := repoDir(t)
	step := depsStep(t, dir, "{\"Path\":\"example.com/m\",\"Main\":true}\n{\"Path\":\"github.com/a/b\",\"Vers", true)
	step.Env = map[string]string{"GOPROXY": "https://proxy.example"}

	res := collectAt(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}}, moduleNow)

	if res.Snapshot.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want failed", res.Snapshot.Status)
	}
	if hasMetric(res, collect.KeyDepsTotal) {
		t.Error("a truncated module stream produced a dependency count, which is wrong low by an unknown amount")
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}
