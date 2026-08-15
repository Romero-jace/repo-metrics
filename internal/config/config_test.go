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

func TestLoadAppliesDefaults(t *testing.T) {
	path := write(t, `
repos:
  - name: svc
    path: $REPO
    coverprofile: coverage.out
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

	r := cfg.Repos[0]
	if got, want := time.Duration(r.Timeout), config.DefaultTimeout; got != want {
		t.Errorf("repo Timeout: got %s, want %s", got, want)
	}
	if got, want := time.Duration(r.MaxAge), config.DefaultMaxAge; got != want {
		t.Errorf("repo MaxAge: got %s, want %s", got, want)
	}
}

// File values must win over defaults, including per-repo ones, and durations
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
    coverprofile: out/cover.out
    command: ["go", "test", "./...", "-coverprofile=out/cover.out"]
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

	r := cfg.Repos[0]
	if got := time.Duration(r.Timeout); got != 90*time.Second {
		t.Errorf("Timeout: got %s, want 90s", got)
	}
	if got := time.Duration(r.MaxAge); got != 90*time.Minute {
		t.Errorf("MaxAge: got %s, want 1h30m", got)
	}
	if len(r.Command) != 4 || r.Command[0] != "go" {
		t.Errorf("Command round-trip failed: %q", r.Command)
	}
	if r.StdoutFormat != config.StdoutGoTestJSON {
		t.Errorf("StdoutFormat: got %q", r.StdoutFormat)
	}
}

// Secrets and machine-specific paths belong in the environment, not the config
// file, so ${VAR} has to expand in the fields that would carry them.
func TestLoadExpandsEnvVars(t *testing.T) {
	t.Setenv("RM_TEST_PROFILE", "from-env.out")

	path := write(t, `
repos:
  - name: svc
    path: $REPO
    coverprofile: ${RM_TEST_PROFILE}
    command: ["go", "test", "-coverprofile=${RM_TEST_PROFILE}"]
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repos[0].Coverprofile != "from-env.out" {
		t.Errorf("Coverprofile not expanded: got %q", cfg.Repos[0].Coverprofile)
	}
	if got := cfg.Repos[0].Command[2]; got != "-coverprofile=from-env.out" {
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
    coverprofile: coverage.out
    command: ["go", "test", "./..."]
    env:
      GOWORK: "off"
      GOFLAGS: ${RM_TEST_FLAGS}
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

// A repo without an env block must not come back with an empty map that then
// has to be distinguished from a nil one everywhere downstream.
func TestLoadLeavesEnvNilWhenAbsent(t *testing.T) {
	cfg, err := config.Load(write(t, `
repos:
  - name: svc
    path: $REPO
    coverprofile: coverage.out
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repos[0].Env != nil {
		t.Errorf("Env: got %v, want nil when the file says nothing", cfg.Repos[0].Env)
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
  - {name: svc, path: $REPO, coverprofile: c.out}
  - {name: svc, path: $REPO, coverprofile: c.out}
`,
			want: "duplicate",
		},
		{
			name: "repo path does not exist",
			body: `
repos:
  - {name: svc, path: /nope/not/here, coverprofile: c.out}
`,
			want: "does not exist",
		},
		{
			// Without a coverprofile there is nothing to parse, so running a
			// command would burn a full test suite and record nothing.
			name: "no coverprofile",
			body: `
repos:
  - {name: svc, path: $REPO, command: ["go", "test", "./..."]}
`,
			want: "coverprofile",
		},
		{
			name: "unknown stdout format",
			body: `
repos:
  - {name: svc, path: $REPO, coverprofile: c.out, stdout_format: junit-xml}
`,
			want: "stdout_format",
		},
		{
			name: "negative timeout",
			body: `
repos:
  - {name: svc, path: $REPO, coverprofile: c.out, timeout: -5s}
`,
			want: "timeout",
		},
		{
			// An empty key becomes "=VALUE" in the child's environment, which
			// is not a variable and not an error anyone would ever see.
			name: "empty env key",
			body: `
repos:
  - name: svc
    path: $REPO
    coverprofile: c.out
    command: ["go", "test"]
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
    coverprofile: c.out
    command: ["go", "test"]
    env:
      "GOWORK=off": "1"
`,
			want: "cannot contain",
		},
		{
			name: "unparseable duration",
			body: `
repos:
  - {name: svc, path: $REPO, coverprofile: c.out, timeout: "ten minutes"}
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

// Every problem in one pass. Fixing a config one error per run is miserable
// when the file lists a dozen repos.
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	_, err := config.Load(write(t, `
repos:
  - {name: "", path: $REPO, coverprofile: c.out}
  - {name: svc, path: /nope/not/here, coverprofile: c.out}
`))
	if err == nil {
		t.Fatal("Load accepted an invalid config")
	}
	msg := err.Error()
	if !strings.Contains(msg, "name") || !strings.Contains(msg, "does not exist") {
		t.Errorf("want both problems reported, got: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}
