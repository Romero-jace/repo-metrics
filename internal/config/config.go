// Package config loads and validates the repo list and per-repo collection
// settings.
//
// This package is the seam between the tool and whatever knows about your
// organization. repo-metrics has no repo discovery, no forge API, and no notion
// of an org: it reads this file and nothing else. Anything that enumerates
// repositories is a separate concern whose only job is to write this file.
//
// Loading is a four-stage pipeline: start from defaults, unmarshal the file
// over them, expand ${ENV_VAR} references, then validate. Starting from
// defaults is what lets every field have one without pointer gymnastics or
// omitempty tricks.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Defaults. Exported so tests and the init subcommand can reference them
// instead of duplicating literals.
const (
	DefaultDatabase      = "./repo-metrics.db"
	DefaultMinStatements = 20
	DefaultMinRepoDelta  = 0.5
	DefaultWindow        = 7 * 24 * time.Hour
	DefaultTimeout       = 10 * time.Minute
	DefaultMaxAge        = 24 * time.Hour
)

// StdoutGoTestJSON parses the command's stdout as a `go test -json` event
// stream. It is the only supported value today; an empty StdoutFormat means
// stdout is ignored and only the coverage profile is parsed.
const StdoutGoTestJSON = "go-test-json"

// Config is the whole file.
type Config struct {
	Database string `yaml:"database"`
	// Window is the default reporting window, overridable per invocation.
	Window Duration `yaml:"window"`
	// MinStatements is the noise floor for culprit ranking. A three-statement
	// package swinging from 0 to 100 percent is not news.
	MinStatements int `yaml:"min_statements"`
	// MinRepoDelta is how far a repo's coverage must move, in percentage
	// points, before it earns a place in the report's movers section.
	MinRepoDelta float64 `yaml:"min_repo_delta"`
	Repos        []Repo  `yaml:"repos"`
}

// Repo is one tracked repository.
//
// Command is optional. With one, repo-metrics runs it and then parses the
// artifacts it leaves behind. Without one, repo-metrics parses whatever is
// already on disk, which is how it consumes something CI produced.
type Repo struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
	// Coverprofile is relative to Path unless absolute.
	Coverprofile string `yaml:"coverprofile"`
	// Command is an argv slice, never a shell string. Nothing here is passed
	// through a shell, so quoting and word splitting cannot surprise anyone.
	Command      []string `yaml:"command"`
	StdoutFormat string   `yaml:"stdout_format"`
	// Env is added to the command's environment as KEY=VALUE. It exists
	// because there is no shell to put a `VAR=x` prefix in front of: without
	// it, a repo needing GOWORK=off has to smuggle it into the argv as
	// `env GOWORK=off go test ...`, which works but reads like a workaround
	// because it is one.
	Env     map[string]string `yaml:"env"`
	Timeout Duration          `yaml:"timeout"`
	// MaxAge applies only in ingest mode (no Command). An artifact older than
	// this is reported as stale rather than presented as a current number.
	MaxAge Duration `yaml:"max_age"`
}

// Duration is a time.Duration that unmarshals from a YAML string like "10m".
type Duration time.Duration

// UnmarshalYAML implements goccy/go-yaml's BytesUnmarshaler.
func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// Defaults returns a Config with every top-level default applied. Per-repo
// defaults cannot live here because repos is a slice, so they are filled in by
// normalize after unmarshaling.
func Defaults() *Config {
	return &Config{
		Database:      DefaultDatabase,
		Window:        Duration(DefaultWindow),
		MinStatements: DefaultMinStatements,
		MinRepoDelta:  DefaultMinRepoDelta,
	}
}

// Load reads, expands, and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is operator-supplied by design
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	cfg.expandEnv()
	cfg.normalize()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: invalid %s: %w", path, err)
	}
	return cfg, nil
}

// expandEnv resolves ${VAR} and $VAR against the environment in the fields that
// plausibly carry machine-specific or secret values. Secrets belong in the
// environment rather than in a file that gets committed.
func (c *Config) expandEnv() {
	c.Database = os.ExpandEnv(c.Database)
	for i := range c.Repos {
		r := &c.Repos[i]
		r.Path = os.ExpandEnv(r.Path)
		r.Coverprofile = os.ExpandEnv(r.Coverprofile)
		for j := range r.Command {
			r.Command[j] = os.ExpandEnv(r.Command[j])
		}
		// Values only. A ${VAR} in a key would let the environment decide
		// which variable gets set, and an unset one would produce the empty
		// key that validate rejects.
		for k, v := range r.Env {
			r.Env[k] = os.ExpandEnv(v)
		}
	}
}

// normalize fills in per-repo defaults left unset by the file.
func (c *Config) normalize() {
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Timeout == 0 {
			r.Timeout = Duration(DefaultTimeout)
		}
		if r.MaxAge == 0 {
			r.MaxAge = Duration(DefaultMaxAge)
		}
	}
}

// validate collects every problem rather than stopping at the first, because
// fixing a multi-repo config one error per run is miserable.
func (c *Config) validate() error {
	var problems []error

	if c.Database == "" {
		problems = append(problems, errors.New("database path is empty"))
	}
	if c.Window <= 0 {
		problems = append(problems, errors.New("window must be positive"))
	}
	if c.MinStatements < 0 {
		problems = append(problems, errors.New("min_statements cannot be negative"))
	}
	if len(c.Repos) == 0 {
		problems = append(problems, errors.New("no repos configured"))
	}

	seen := make(map[string]bool, len(c.Repos))
	for i, r := range c.Repos {
		label := r.Name
		if label == "" {
			label = fmt.Sprintf("repos[%d]", i)
		}

		switch {
		case r.Name == "":
			problems = append(problems, fmt.Errorf("%s: name is required", label))
		case seen[r.Name]:
			problems = append(problems, fmt.Errorf("%s: duplicate repo name", label))
		default:
			seen[r.Name] = true
		}

		if r.Path == "" {
			problems = append(problems, fmt.Errorf("%s: path is required", label))
		} else if info, err := os.Stat(r.Path); err != nil {
			problems = append(problems, fmt.Errorf("%s: path %s does not exist", label, r.Path))
		} else if !info.IsDir() {
			problems = append(problems, fmt.Errorf("%s: path %s is not a directory", label, r.Path))
		}

		// Without a coverprofile there is nothing to parse, so running a
		// command would burn a whole test suite and record nothing.
		if r.Coverprofile == "" {
			problems = append(problems, fmt.Errorf("%s: coverprofile is required", label))
		}

		if r.StdoutFormat != "" && r.StdoutFormat != StdoutGoTestJSON {
			problems = append(problems, fmt.Errorf(
				"%s: unknown stdout_format %q, want %q or empty", label, r.StdoutFormat, StdoutGoTestJSON))
		}
		if r.StdoutFormat != "" && len(r.Command) == 0 {
			problems = append(problems, fmt.Errorf(
				"%s: stdout_format needs a command to capture stdout from", label))
		}

		// Sorted so a config with several bad env keys reports them in the
		// same order every run. Map order would make the message shuffle
		// between runs and the failure harder to talk about.
		for _, k := range slices.Sorted(maps.Keys(r.Env)) {
			switch {
			case k == "":
				problems = append(problems, fmt.Errorf("%s: env has an empty key", label))
			case strings.Contains(k, "="):
				// The runner joins these as KEY=VALUE, so a key carrying its
				// own "=" would silently set a different variable than the
				// one written in the file.
				problems = append(problems, fmt.Errorf("%s: env key %q cannot contain \"=\"", label, k))
			}
		}

		if r.Timeout <= 0 {
			problems = append(problems, fmt.Errorf("%s: timeout must be positive", label))
		}
		if r.MaxAge < 0 {
			problems = append(problems, fmt.Errorf("%s: max_age cannot be negative", label))
		}
	}

	return errors.Join(problems...)
}
