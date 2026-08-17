package config_test

import (
	"os"
	"path/filepath"
	"regexp"
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
  - {name: svc, path: $REPO, signals: [{name: c, command: ["go", "test"], stdout_format: not-a-real-format}]}
`,
			want: "unknown stdout_format",
		},
		{
			name: "unknown artifact format",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.xml, artifact_format: not-a-real-format}]}
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
			// The message stopped naming the two sources when a step could read
			// several artifacts: it can now collide with itself without stdout
			// being involved at all.
			want: "reads sarif twice",
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
		{
			// The likeliest way to get an entry wrong is writing the shorthand's
			// key names inside the list, which strict mode reports as two unknown
			// fields rather than as the wrong shape. These say which two words the
			// entry wants.
			name: "artifacts entry with no path",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: t, command: ["pytest"], artifacts: [{format: junit-xml}]}
`,
			want: "needs a path saying which file to read",
		},
		{
			name: "artifacts entry with no format",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: t, command: ["pytest"], artifacts: [{path: j.xml}]}
`,
			want: "needs a format saying how to read it",
		},
		{
			name: "artifacts entry naming an unreadable format",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: t, command: ["pytest"], artifacts: [{path: j.xml, format: not-a-real-format}]}
`,
			want: "unknown format",
		},
		{
			// Either precedence rule would be a config doing something other than
			// what it says, and no operator writing both meant one of them.
			name: "both artifact spellings at once",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: t, command: ["pytest"], artifact: j.xml, artifact_format: junit-xml, artifacts: [{path: c.info, format: lcov}]}
`,
			want: "names both artifacts and the single artifact/artifact_format pair",
		},
		{
			// Two entries for one file parse it twice and write its metrics twice,
			// which the collector answers by dropping the whole step.
			name: "the same file listed twice",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: t, command: ["pytest"], artifacts: [{path: j.xml, format: junit-xml}, {path: j.xml, format: junit-xml}]}
`,
			want: "reads j.xml twice",
		},
		{
			// Two files of one non-repeatable format collide with each other
			// whether they sit in one step or two, so the per-signal rule has to
			// catch the in-one-step case the repeatability sweep cannot see.
			name: "two artifacts of the same format in one step",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: t, command: ["c"], artifacts: [{path: a.info, format: lcov}, {path: b.info, format: lcov}]}
`,
			want: "reads lcov twice",
		},
		{
			// goccy discards a key nothing declares, so a misspelled fingerprint
			// would load clean and leave the repo unidentified forever. That is the
			// same silent-absence shape the field exists to close, arriving through
			// the config file instead of the collector.
			name: "misspelled fingerprint key",
			body: `
repos:
  - name: svc
    path: $REPO
    fingerprnt: ["node", "--version"]
    signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]
`,
			want: "Write it as fingerprint",
		},
		{
			name: "plural fingerprint key",
			body: `
repos:
  - name: svc
    path: $REPO
    fingerprints: ["node", "--version"]
    signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]
`,
			want: "Write it as fingerprint",
		},
		{
			// Not skipped. exec passes an empty element through as an empty
			// argument, so the probe that ran would differ from the one in the file.
			name: "empty element in the fingerprint argv",
			body: `
repos:
  - name: svc
    path: $REPO
    fingerprint: ["node", ""]
    signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]
`,
			want: "fingerprint[1] is empty",
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

// A leftover repo-level key beside a real signals list is not the old shape,
// and the rest of the repo still has to be checked.
//
// migrationError fires on any old repo-level field, and validateSignals used to
// return on it, which skipped every other check for that repo: duplicate signal
// names, each signal's own validation, and the cross-signal format collision.
// This config has four problems and reported one, and that one told the
// operator their file was the old single-command shape when it plainly was not.
// env genuinely is repo-level in the new shape, so "timeouts must be repo-level
// too" is an ordinary mistake to make.
func TestOldKeysBesideRealSignalsStillValidateTheSignals(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    timeout: 5m
    signals:
      - {name: coverage, artifact: c.out, artifact_format: not-a-format}
      - {name: coverage, command: ["go", "test"], stdout_format: also-not-a-format}
`))
	if err == nil {
		t.Fatal("Load accepted an invalid config")
	}
	msg := err.Error()

	for _, want := range []string{
		"timeout",                 // the leftover repo-level key
		"duplicate signal name",   // both steps are called coverage
		"unknown artifact_format", // and each names a format that does not exist
		"unknown stdout_format",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q:\n%s", want, msg)
		}
	}

	// The diagnosis has to match the file, which has a signals list.
	if !strings.Contains(msg, "already has a signals list") {
		t.Errorf("the error does not say what is actually wrong with this repo:\n%s", msg)
	}
	// And no pasteable replacement block, which migrationError assembles from
	// the old fields alone: pasting it over this repo would throw both of its
	// real signals away.
	if strings.Contains(msg, "Rewrite it as") {
		t.Errorf("the error offers a rewrite that would discard the signals this repo already has:\n%s", msg)
	}
}

// An explicit zero is a request, and used to be answered with a default.
//
// max_age: 0s asks for no staleness limit and silently got 24 hours: the
// default was filled in for any zero MaxAge before validation ran, and an
// absent key and a written "0s" both arrive as Duration(0). Defaults are
// applied by Signal.UnmarshalYAML now, which only runs for a step the file
// actually carries, so the two are distinguishable and the explicit one is
// rejected rather than overruled.
//
// timeout: 0s was the same bug one field over. validate has always rejected a
// non-positive timeout, and for a zero that check could never fire.
func TestExplicitZeroDurationsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "max_age of zero",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile, max_age: 0s}]}
`,
			want: []string{"max_age must be positive", "omit it for the default"},
		},
		{
			name: "timeout of zero",
			body: `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile, timeout: 0s}]}
`,
			want: []string{"timeout must be positive", "omit it for the default"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(write(t, tc.body))
			if err == nil {
				t.Fatal("Load accepted a duration of zero, so the operator's request was silently replaced by a default")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not mention %q: %v", want, err)
				}
			}
		})
	}

	// The other half, without which rejecting every zero would just be a
	// different silent wrong answer: an omitted key still gets its default.
	cfg, err := config.Load(write(t, `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`))
	if err != nil {
		t.Fatalf("Load rejected a config that omits both durations: %v", err)
	}
	s := onlySignal(t, cfg)
	if got, want := time.Duration(s.MaxAge), config.DefaultMaxAge; got != want {
		t.Errorf("an omitted max_age is %s, want the default %s", got, want)
	}
	if got, want := time.Duration(s.Timeout), config.DefaultTimeout; got != want {
		t.Errorf("an omitted timeout is %s, want the default %s", got, want)
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
  - {name: alpha, path: $REPO, signals: [{name: c, command: ["go", "test"], stdout_format: not-a-real-format}]}
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

// UsesGoToolchain decides whether the collector asks `go env` to identify a
// repo's toolchain, so it has to answer from the formats rather than from
// anything an operator could get wrong.
//
// The lint-only case is the one that matters. It used to be fingerprinted with
// `go env` like every other repo, which on a machine with Go installed succeeds
// and records the ambient Go version for a repo with no Go in it at all.
func TestUsesGoToolchainReadsTheFormats(t *testing.T) {
	repoWith := func(sigs ...config.Signal) config.Repo {
		return config.Repo{Name: "svc", Path: "/tmp", Signals: sigs}
	}
	lint := config.Signal{Name: "lint", Command: []string{"eslint", "."}, StdoutFormat: config.FormatSARIF}
	tests := config.Signal{Name: "tests", Command: []string{"go", "test"}, StdoutFormat: config.FormatGoTestJSON}
	cover := config.Signal{Name: "cover", Artifact: "c.out", ArtifactFormat: config.FormatGoCoverprofile}

	cases := []struct {
		name string
		repo config.Repo
		want bool
	}{
		{"lint only", repoWith(lint), false},
		{"no signals at all", repoWith(), false},
		{"go test on stdout", repoWith(tests), true},
		{"go coverage as an artifact", repoWith(cover), true},
		// The mixed case is the whole point of scanning every step rather than
		// the first: a polyglot repo running eslint beside `go test` is still
		// measured under a Go toolchain, and checking only signals[0] would miss it.
		{"lint first, go second", repoWith(lint, tests), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.repo.UsesGoToolchain(); got != tc.want {
				t.Errorf("UsesGoToolchain() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every format has to say which toolchain it belongs to, or nothing does, and a
// format added without an answer silently means "not Go" — which routes its
// repos to the unidentified branch while looking configured.
func TestEveryFormatDeclaresItsToolchain(t *testing.T) {
	var owned int
	for _, name := range config.Formats() {
		f := config.Format(name)
		if f.Toolchain() != "" {
			owned++
		}
		if tc := f.Toolchain(); tc != "" && tc != config.ToolchainGo {
			t.Errorf("format %q claims toolchain %q, which the collector has no probe for", name, tc)
		}
	}
	// SARIF is language-agnostic by design, so at least one format must answer
	// empty and at least one must not. Without both halves this passes on a table
	// where every entry says the same thing.
	if owned == 0 || owned == len(config.Formats()) {
		t.Errorf("%d of %d formats name a toolchain; expected a mix, since SARIF names none and Go's formats all do",
			owned, len(config.Formats()))
	}
}

// Two JUnit steps in one repo is the case the format exists to allow: a repo
// running pytest beside vitest. It is legal because every row that parser writes
// is prefixed with the step's own name, which is the same property that earns
// SARIF its repeatability.
func TestTwoJUnitStepsAreAllowedInOneRepo(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: py, artifact: py.xml, artifact_format: junit-xml}
      - {name: web, artifact: web.xml, artifact_format: junit-xml}
`))
	if err != nil {
		t.Fatalf("two JUnit steps rejected: %v", err)
	}
}

// A Go test step beside a JUnit step is legal too, and this is the pairing the
// repo-scoped-key guard had to be written carefully enough to permit.
//
// go-test-json owns two repo-level rows; junit-xml owns none, so their sets do
// not intersect and nothing collides. A guard that had simply forbidden two test
// formats in one repo would have blocked the polyglot case it was written for.
func TestAGoTestStepAndAJUnitStepCoexist(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: go-tests, command: ["go", "test", "-json", "./..."], stdout_format: go-test-json}
      - {name: web-tests, artifact: junit.xml, artifact_format: junit-xml}
`))
	if err != nil {
		t.Fatalf("a Go test step beside a JUnit step was rejected: %v", err)
	}
}

// Two steps that would write the same repo-level row are rejected at load,
// rather than being discovered on every collection when the second one fails.
//
// The per-format count above cannot see this: it answers whether one format
// appears twice, and these are two different formats. Today the only way to
// reach it is two go-test-json steps, which the per-format rule also catches, so
// this asserts the collision message specifically rather than merely that
// something failed.
func TestStepsWritingTheSameRepoScopedRowAreRejected(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: first, command: ["go", "test", "-json", "./..."], stdout_format: go-test-json}
      - {name: second, command: ["go", "test", "-json", "./x/..."], stdout_format: go-test-json}
`))
	if err == nil {
		t.Fatal("Load accepted two steps that would write the same repo-level row")
	}
	if !strings.Contains(err.Error(), "pkg.without_tests") {
		t.Errorf("the error does not name the colliding row, so nobody can act on it: %v", err)
	}
	if !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Errorf("the error does not name both steps: %v", err)
	}
}

// A key this tool does not read is an error, at every level of the file.
//
// goccy discards an unrecognized key by default, which turns a typo into the
// exact failure this project is built against: the file loads, the tool runs,
// and the setting the operator wrote does nothing. `fingerprnt:` leaves a repo
// unidentified forever; `artifcts:` leaves a step reading nothing.
//
// The signals case is the one that matters. Signal implements its own
// UnmarshalYAML and re-decodes with a fresh decoder, so the strict option passed
// in Load does not reach it. Without repeating the option there, every key
// inside a signals entry escapes the check while the rest of the file is
// covered, which is worse than not being strict at all: the guard looks on and
// is off exactly where the most keys are.
func TestLoadRejectsUnknownKeysAtEveryLevel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "top level",
			body: `
databse: /tmp/x.db
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`,
			want: "databse",
		},
		{
			name: "repo level",
			body: `
repos:
  - name: svc
    path: $REPO
    pth: /somewhere/else
    signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]
`,
			want: "pth",
		},
		{
			name: "inside a signal",
			body: `
repos:
  - name: svc
    path: $REPO
    signals:
      - {name: c, artifact: c.out, artifact_format: go-coverprofile, artifcts: []}
`,
			want: "artifcts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(write(t, tc.body))
			if err == nil {
				t.Fatal("Load accepted a config carrying a key this tool does not read")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending key %q", err, tc.want)
			}
		})
	}

	// The control: a config using only real keys still loads. Without it, a
	// decoder that rejected everything would pass every case above.
	if _, err := config.Load(write(t, `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`)); err != nil {
		t.Errorf("a config using only real keys was rejected: %v", err)
	}
}

// Several files from one run, which is the shape a single pytest or vitest
// command actually produces.
func TestLoadAcceptsSeveralArtifactsInOneStep(t *testing.T) {
	cfg, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - name: tests
        command: ["pytest", "--junitxml=j.xml", "--cov-report=lcov:c.info"]
        artifacts:
          - {path: j.xml, format: junit-xml}
          - {path: c.info, format: lcov}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s := onlySignal(t, cfg)
	got := s.ArtifactList()
	if len(got) != 2 {
		t.Fatalf("ArtifactList: got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Path != "j.xml" || got[0].Format != config.FormatJUnitXML {
		t.Errorf("first artifact: got %+v", got[0])
	}
	if got[1].Path != "c.info" || got[1].Format != config.FormatLCOV {
		t.Errorf("second artifact: got %+v", got[1])
	}

	// Formats has to see both, or the repeatability sweep and the repo-scoped
	// collision check are reasoning about half the step.
	if len(s.Formats()) != 2 {
		t.Errorf("Formats: got %v, want both artifact formats", s.Formats())
	}
}

// The shorthand is RETURNED by ArtifactList, never copied into Artifacts.
//
// A copy would leave both shapes populated, so Formats would report the format
// twice and the per-signal dedup rule would reject a config that named the file
// once. This is the assertion that keeps that from being reintroduced as a
// simplification.
func TestTheArtifactShorthandIsNotDuplicated(t *testing.T) {
	cfg, err := config.Load(write(t, `
repos:
  - {name: svc, path: $REPO, signals: [{name: c, artifact: c.out, artifact_format: go-coverprofile}]}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s := onlySignal(t, cfg)
	if got := s.ArtifactList(); len(got) != 1 || got[0].Path != "c.out" {
		t.Fatalf("ArtifactList: got %+v, want the shorthand as one entry", got)
	}
	if len(s.Artifacts) != 0 {
		t.Errorf("Artifacts was populated from the shorthand: %+v. Formats would then report the format twice", s.Artifacts)
	}
	if got := s.Formats(); len(got) != 1 {
		t.Errorf("Formats: got %v, want exactly one", got)
	}
}

// ${VAR} expands inside an artifacts entry, the same as in the shorthand.
//
// The expansion walk runs between unmarshal and validate and nothing downstream
// expands anything, so a path missed here reaches the collector as literal text
// while the identical variable in the shorthand works. That asymmetry is a
// config silently doing something other than what it says.
func TestLoadExpandsEnvVarsInsideArtifacts(t *testing.T) {
	t.Setenv("RM_TEST_REPORT", "from-env.xml")

	cfg, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    signals:
      - name: tests
        command: ["pytest"]
        artifacts:
          # Block style, not flow style: a ${VAR} inside {…} ends the mapping at
          # the variable's own closing brace.
          - path: ${RM_TEST_REPORT}
            format: junit-xml
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := onlySignal(t, cfg).ArtifactList()
	if len(got) != 1 || got[0].Path != "from-env.xml" {
		t.Errorf("artifact path not expanded: got %+v", got)
	}
}

// The shipped example config has to load.
//
// Nothing checked it before: no test read it, no Makefile target built it, no CI
// step touched it. It is 300 lines of documentation about a schema, and the only
// thing keeping it true was somebody remembering. A key renamed in this package
// would leave it describing a tool that no longer exists — and now that unknown
// keys are rejected, a stale key in it is a file that does not load at all.
//
// The repo paths are rewritten to a real temp directory, because Load stats
// every one of them and the file ships with placeholders on purpose.
func TestTheExampleConfigLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "repo-metrics.yaml"))
	if err != nil {
		t.Fatalf("reading the example config: %v", err)
	}

	dir := t.TempDir()
	body := regexp.MustCompile(`(?m)^(\s*path:) /srv/checkouts/\S+`).
		ReplaceAllString(string(raw), "${1} "+dir)
	// The first repo's path is the one the file leaves as a placeholder for the
	// reader to edit; the rest are under /srv/checkouts.
	body = strings.ReplaceAll(body, "path: /path/to/your-repo", "path: "+dir)

	path := filepath.Join(dir, "example.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the rewritten example: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the shipped example config does not load: %v", err)
	}
	if len(cfg.Repos) < 2 {
		t.Errorf("the example describes %d repos; it is meant to show several shapes", len(cfg.Repos))
	}

	// It has to actually exercise the polyglot shape, or it stops being the
	// worked example the docs point at.
	var multi int
	for _, r := range cfg.Repos {
		for _, s := range r.Signals {
			if len(s.ArtifactList()) > 1 {
				multi++
			}
		}
	}
	if multi == 0 {
		t.Error("no step in the example reads more than one artifact, so the shape this config exists to demonstrate is undocumented")
	}
}
