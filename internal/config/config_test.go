package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// write drops a config file in a temp dir alongside a real repo directory, and
// returns the config path. Validation checks that repo paths exist, so tests
// need a genuine directory rather than a made-up string.
func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "svc")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body = strings.ReplaceAll(body, "$REPO", repoDir)
	path := filepath.Join(dir, "repo-metrics.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// onlySignal returns the first repo's only step, failing loudly rather than
// panicking if the shape is not what the test set up.
func onlySignal(t *testing.T, cfg *config.Config) config.Signal {
	t.Helper()
	if len(cfg.Repos) != 1 || len(cfg.Repos[0].Signals) != 1 {
		t.Fatalf("want one repo with one signal, got %+v", cfg.Repos)
	}
	return cfg.Repos[0].Signals[0]
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - name: coverage
        artifact: coverage.out
        artifact_format: go-coverprofile
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database != config.DefaultDatabase {
		t.Errorf("Database: got %q, want the default %q", cfg.Database, config.DefaultDatabase)
	}
	if cfg.MinStatements != config.DefaultMinStatements {
		t.Errorf("MinStatements: got %d, want %d", cfg.MinStatements, config.DefaultMinStatements)
	}
	if cfg.MinRepoDelta != config.DefaultMinRepoDelta {
		t.Errorf("MinRepoDelta: got %v, want %v", cfg.MinRepoDelta, config.DefaultMinRepoDelta)
	}
	if got, want := time.Duration(cfg.Window), config.DefaultWindow; got != want {
		t.Errorf("Window: got %s, want %s", got, want)
	}

	// The per-step defaults matter more than they did: they used to be filled in
	// once per repo, and now every entry in the list needs its own.
	s := onlySignal(t, cfg)
	if got, want := time.Duration(s.Timeout), config.DefaultTimeout; got != want {
		t.Errorf("signal Timeout: got %s, want %s", got, want)
	}
	if got, want := time.Duration(s.MaxAge), config.DefaultMaxAge; got != want {
		t.Errorf("signal MaxAge: got %s, want %s", got, want)
	}
}

// File values must win over defaults, including per-step ones, and durations
// have to come back as real durations rather than strings.
func TestLoadOverridesDefaults(t *testing.T) {
	path := write(t, `
database: /tmp/other.db
min_statements: 5
min_repo_delta: 2.5
window: 336h
repos:
  - name: svc
    path: $REPO
    signals:
      - name: coverage
        command: ["go", "test", "./...", "-coverprofile=out/cover.out"]
        artifact: out/cover.out
        artifact_format: go-coverprofile
        stdout_format: go-test-json
        timeout: 90s
        max_age: 1h30m
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database != "/tmp/other.db" {
		t.Errorf("Database: got %q", cfg.Database)
	}
	if cfg.MinStatements != 5 {
		t.Errorf("MinStatements: got %d, want 5", cfg.MinStatements)
	}
	if cfg.MinRepoDelta != 2.5 {
		t.Errorf("MinRepoDelta: got %v, want 2.5", cfg.MinRepoDelta)
	}
	if got := time.Duration(cfg.Window); got != 336*time.Hour {
		t.Errorf("Window: got %s, want 336h", got)
	}

	s := onlySignal(t, cfg)
	if got := time.Duration(s.Timeout); got != 90*time.Second {
		t.Errorf("Timeout: got %s, want 90s", got)
	}
	if got := time.Duration(s.MaxAge); got != 90*time.Minute {
		t.Errorf("MaxAge: got %s, want 1h30m", got)
	}
	if len(s.Command) != 4 || s.Command[0] != "go" {
		t.Errorf("Command round-trip failed: %q", s.Command)
	}
	if s.ArtifactFormat != config.FormatGoCoverprofile {
		t.Errorf("ArtifactFormat: got %q", s.ArtifactFormat)
	}
	if s.StdoutFormat != config.FormatGoTestJSON {
		t.Errorf("StdoutFormat: got %q", s.StdoutFormat)
	}
}

// Secrets and machine-specific paths belong in the environment, not the config
// file, so ${VAR} has to expand in the fields that would carry them. The
// expansion walk now has to descend into the signals list, which is exactly the
// kind of thing a restructure drops.
func TestLoadExpandsEnvVars(t *testing.T) {
	t.Setenv("RM_TEST_PROFILE", "from-env.out")

	path := write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - name: coverage
        artifact: ${RM_TEST_PROFILE}
        artifact_format: go-coverprofile
        command: ["go", "test", "-coverprofile=${RM_TEST_PROFILE}"]
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := onlySignal(t, cfg)
	if s.Artifact != "from-env.out" {
		t.Errorf("Artifact not expanded: got %q", s.Artifact)
	}
	if got := s.Command[2]; got != "-coverprofile=from-env.out" {
		t.Errorf("Command not expanded: got %q", got)
	}
}

// env exists so a repo needing GOWORK=off does not have to smuggle it into the
// argv as `env GOWORK=off go test ...`. Values expand like the other
// machine-specific fields; keys do not, since letting the environment decide
// which variable gets set is a different and much stranger feature.
func TestLoadParsesEnvMap(t *testing.T) {
	t.Setenv("RM_TEST_FLAGS", "-mod=mod")

	path := write(t, `
repos:
  - name: svc
    path: $REPO
    env:
      GOWORK: "off"
      GOFLAGS: ${RM_TEST_FLAGS}
    signals:
      - name: coverage
        command: ["go", "test", "./..."]
        artifact: coverage.out
        artifact_format: go-coverprofile
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	env := cfg.Repos[0].Env
	if got := env["GOWORK"]; got != "off" {
		t.Errorf("env GOWORK: got %q, want %q", got, "off")
	}
	if got := env["GOFLAGS"]; got != "-mod=mod" {
		t.Errorf("env GOFLAGS not expanded: got %q, want %q", got, "-mod=mod")
	}
	if len(env) != 2 {
		t.Errorf("env has %d entries, want 2: %v", len(env), env)
	}
}

// A step's env is merged over the repo's, not instead of it. The repo block is
// where GOWORK=off belongs, since it also fixes the toolchain fingerprint, and a
// step adding one variable of its own must not drop it.
func TestSignalEnvMergesOverTheRepos(t *testing.T) {
	path := write(t, `
repos:
  - name: svc
    path: $REPO
    env:
      GOWORK: "off"
      GOFLAGS: "-mod=mod"
    signals:
      - name: coverage
        command: ["go", "test", "./..."]
        artifact: coverage.out
        artifact_format: go-coverprofile
        env:
          GOFLAGS: "-count=1"
          GOMAXPROCS: "4"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	merged := onlySignal(t, cfg).MergedEnv(cfg.Repos[0].Env)

	want := map[string]string{"GOWORK": "off", "GOFLAGS": "-count=1", "GOMAXPROCS": "4"}
	for k, v := range want {
		if merged[k] != v {
			t.Errorf("merged env %s: got %q, want %q", k, merged[k], v)
		}
	}
	if len(merged) != len(want) {
		t.Errorf("merged env has %d entries, want %d: %v", len(merged), len(want), merged)
	}
	// The repo's own map must not be mutated by the merge, or the second step in
	// the list would inherit the first step's overrides.
	if got := cfg.Repos[0].Env["GOFLAGS"]; got != "-mod=mod" {
		t.Errorf("merging wrote back into the repo env: GOFLAGS is now %q", got)
	}
}

// Nothing configured anywhere has to stay nil rather than becoming an empty map
// that then has to be distinguished from a nil one everywhere downstream.
func TestMergedEnvIsNilWhenNothingIsConfigured(t *testing.T) {
	cfg, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - name: coverage
        artifact: coverage.out
        artifact_format: go-coverprofile
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repos[0].Env != nil {
		t.Errorf("Env: got %v, want nil when the file says nothing", cfg.Repos[0].Env)
	}
	if got := onlySignal(t, cfg).MergedEnv(cfg.Repos[0].Env); got != nil {
		t.Errorf("MergedEnv: got %v, want nil when neither level configured anything", got)
	}
}

// A non-zero exit means findings for some tools and failure for others, and the
// answer is a property of the format rather than a knob an operator sets. A step
// that mixes the two cannot claim the exemption: the exit could be either cause.
func TestNonZeroExitIsNormalNeedsEveryFormatToAgree(t *testing.T) {
	cover := config.Signal{ArtifactFormat: config.FormatGoCoverprofile}
	if cover.NonZeroExitIsNormal() {
		t.Error("a go test step treats a non-zero exit as normal, so a red suite would go unreported")
	}
	// A step declaring nothing is not exempt either, or an unparsed step would
	// swallow its command's failure.
	if (config.Signal{}).NonZeroExitIsNormal() {
		t.Error("a step with no formats claimed the exemption")
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no repos at all",
			body: "database: /tmp/x.db\n",
			want: "no repos",
		},
		{
			name: "duplicate repo names",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`,
			want: "duplicate repo name",
		},
		{
			name: "repo path does not exist",
			body: `
repos:
  - {name: svc, path: /nope/not/here, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`,
			want: "is unusable",
		},
		{
			// A repo with an empty signals list measures nothing, which would
			// otherwise store a clean snapshot with no metrics in it.
			name: "no signals",
			body: `
repos:
  - {name: svc, path: $REPO}
`,
			want: "nothing to measure",
		},
		{
			name: "signal without a name",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{artifact: c.out, artifact_format: go-coverprofile}]}
`,
			want: "name is required",
		},
		{
			name: "duplicate signal names within a repo",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: c, artifact: c.out, artifact_format: go-coverprofile}
      - {name: c, command: ["go", "test"], stdout_format: go-test-json}
`,
			want: "duplicate signal name",
		},
		{
			// Without a format there is nothing to parse, so running a command
			// would burn a full test suite and record nothing.
			name: "no format at all",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, command: ["go", "test", "./..."]}]}
`,
			want: "record nothing",
		},
		{
			name: "artifact without a format",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, stdout_format: go-test-json, command: ["go", "test"]}]}
`,
			want: "artifact_format saying how to read it",
		},
		{
			name: "artifact format without an artifact",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact_format: go-coverprofile, command: ["go", "test"]}]}
`,
			want: "needs an artifact to read",
		},
		{
			name: "stdout format without a command",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, stdout_format: go-test-json}]}
`,
			want: "needs a command to capture stdout from",
		},
		{
			name: "unknown stdout format",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, command: ["go", "test"], stdout_format: junit-xml}]}
`,
			want: "unknown stdout_format",
		},
		{
			name: "unknown artifact format",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.xml, artifact_format: lcov}]}
`,
			want: "unknown artifact_format",
		},
		{
			// Two steps writing the same metric keys collide on the metrics
			// table's primary key, and the INSERT that fails takes every other
			// step's numbers down with it.
			name: "two signals reading the same non-repeatable format",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: unit, command: ["go", "test", "./..."], stdout_format: go-test-json}
      - {name: integration, command: ["go", "test", "-tags=integration", "./..."], stdout_format: go-test-json}
`,
			want: "cannot tell whether their metric keys would collide",
		},
		{
			// Repeatable means two SIGNALS may share a format, never that one
			// signal may read it from both of its sources: both reads are scoped
			// by the same step name and collide with each other. This validated
			// before, and every collection then dropped the step at the
			// duplicate-key guard.
			name: "one signal reading a repeatable format from both sources",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: lint, command: ["golangci-lint", "run"], artifact: out.sarif, artifact_format: sarif, stdout_format: sarif}
`,
			want: "from both its artifact and its stdout",
		},
		{
			name: "negative timeout",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile, timeout: -5s}]}
`,
			want: "timeout",
		},
		{
			// An empty key becomes "=VALUE" in the child's environment, which is
			// not a variable and not an error anyone would ever see.
			name: "empty env key on a repo",
			body: `
repos:
  - name: svc
    path: $REPO
    env:
      "": off
    signals:
      - {name: c, artifact: c.out, artifact_format: go-coverprofile}
`,
			want: "empty key",
		},
		{
			// The same rule has to hold one level down, or the per-step block
			// becomes the way around it.
			name: "empty env key on a signal",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - name: c
        command: ["go", "test"]
        stdout_format: go-test-json
        env:
          "": off
`,
			want: "empty key",
		},
		{
			// "A=B": "c" would join to "A=B=c" and set A to "B=c", silently
			// setting a different variable than the one written in the file.
			name: "env key containing an equals sign",
			body: `
repos:
  - name: svc
    path: $REPO
    env:
      "GOWORK=off": "1"
    signals:
      - {name: c, artifact: c.out, artifact_format: go-coverprofile}
`,
			want: "cannot contain",
		},
		{
			name: "unparseable duration",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile, timeout: "ten minutes"}]}
`,
			want: "duration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(write(t, tc.body))
			if err == nil {
				t.Fatal("Load accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A config written against the previous shape has to say so.
//
// goccy ignores keys it does not recognize, so without the deprecated fields
// this file would load into an empty signals list and fail with "nothing to
// measure", which is true and explains nothing. The old fields exist purely so
// this message can be produced.
func TestOldSingleCommandShapeExplainsItself(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    coverprofile: coverage.out
    command: ["go", "test", "./...", "-coverprofile=coverage.out"]
    stdout_format: go-test-json
    timeout: 10m
`))
	if err == nil {
		t.Fatal("Load accepted a config in the old shape")
	}
	msg := err.Error()

	for _, want := range []string{
		"old single-command shape",
		// The instructions have to name the new keys, or the operator is told
		// what is wrong and not what to write.
		"signals:",
		"artifact_format: go-coverprofile",
		"stdout_format: go-test-json",
		// Filled in from what they actually wrote, so the block can be pasted
		// rather than translated.
		"-coverprofile=coverage.out",
		"timeout: 10m",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the migration error does not mention %q:\n%s", want, msg)
		}
	}

	// It must not also complain about the consequence, which would bury the
	// instructions under a second error about their own effect.
	if strings.Contains(msg, "nothing to measure") {
		t.Errorf("the migration error is buried under a complaint about its own consequence:\n%s", msg)
	}
}

// Every problem in one pass. Fixing a config one error per run is miserable
// when the file lists a dozen repos.
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - {name: "", path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
  - {name: svc, path: /nope/not/here, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`))
	if err == nil {
		t.Fatal("Load accepted an invalid config")
	}
	msg := err.Error()
	if !strings.Contains(msg, "name") || !strings.Contains(msg, "is unusable") {
		t.Errorf("want both problems reported, got: %v", err)
	}
}

// Problems inside the signals list have to be reported alongside the repo's own,
// rather than the first failure stopping the walk.
func TestLoadReportsSignalProblemsFromEveryRepo(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - {name: alpha, path: $REPO, signals: [{name: c, command: ["go", "test"], stdout_format: junit-xml}]}
  - {name: beta, path: $REPO, signals: [{name: c, artifact_format: go-coverprofile}]}
`))
	if err == nil {
		t.Fatal("Load accepted an invalid config")
	}
	msg := err.Error()
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("want a problem from each repo, got: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

// The list an operator may name and the list the tool can read have to be the
// same list. This is the config half; the collect package pins the other.
func TestFormatsIsStableAndComplete(t *testing.T) {
	got := config.Formats()
	if len(got) == 0 {
		t.Fatal("Formats is empty, so the help text and every error message list nothing")
	}
	for _, name := range got {
		if !config.Format(name).Known() {
			t.Errorf("Formats lists %q, which Known rejects", name)
		}
	}
	// Stable, because it is printed in help text and in error messages, and a
	// list that shuffles between runs makes both impossible to diff.
	for range 5 {
		if second := config.Formats(); strings.Join(second, ",") != strings.Join(got, ",") {
			t.Fatalf("Formats is not stable: %v then %v", got, second)
		}
	}
}
