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

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}
