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

// Repo is one tracked repository and the list of things to measure about it.
type Repo struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`

	// Env is added to every step's environment as KEY=VALUE, and is also the
	// environment the snapshot's toolchain fingerprint is taken under.
	//
	// It exists because there is no shell to put a `VAR=x` prefix in front of:
	// without it, a repo needing GOWORK=off has to smuggle it into the argv as
	// `env GOWORK=off go test ...`, which works but reads like a workaround
	// because it is one.
	//
	// It stays at the repo level even though steps have their own env, because
	// the fingerprint is taken once per repo and is what tells the report whether
	// two snapshots are comparable at all. Under a per-step env alone, whichever
	// step happened to be fingerprinted would decide that for every signal.
	Env map[string]string `yaml:"env"`

	// Signals is what to measure. One entry per command, or per artifact in
	// ingest mode.
	Signals []Signal `yaml:"signals"`

	// The old single-command shape. These are read only so that a config
	// written against it fails with instructions rather than loading into an
	// empty signals list. See migrationError.
	OldCoverprofile string   `yaml:"coverprofile"`
	OldCommand      []string `yaml:"command"`
	OldStdoutFormat string   `yaml:"stdout_format"`
	OldTimeout      Duration `yaml:"timeout"`
	OldMaxAge       Duration `yaml:"max_age"`
}

// Signal is one collection step: something to run, and how to read what it
// leaves behind.
//
// The name is the one the config file uses, and it is deliberately not exactly
// what the report calls a signal. One step can produce several reported signals:
// a `go test` step yields coverage, test counts, failures, skips and timings
// from a single run, because the toolchain gives them up together. The step is
// the unit of execution and of failure; the reported signal is the unit of
// measurement.
type Signal struct {
	// Name labels this step in diagnostics and in the progress line. It has to
	// be unique within the repo, and it is what a person reads when a step goes
	// wrong, so give it the name of the thing rather than of the tool.
	Name string `yaml:"name"`

	// Command is an argv slice, never a shell string. Nothing here is passed
	// through a shell, so quoting and word splitting cannot surprise anyone.
	//
	// It is optional. With one, repo-metrics runs it and then parses what it
	// left behind. Without one, repo-metrics parses whatever is already on disk,
	// which is how it consumes something CI produced.
	Command []string `yaml:"command"`

	// Artifact is a file the step produces or finds, relative to the repo path
	// unless absolute. ArtifactFormat says how to read it.
	Artifact       string `yaml:"artifact"`
	ArtifactFormat Format `yaml:"artifact_format"`

	// StdoutFormat says how to read the command's standard output. It needs a
	// command, since there is no stdout without one.
	StdoutFormat Format `yaml:"stdout_format"`

	// Env is merged over the repo's env for this step only.
	Env map[string]string `yaml:"env"`

	// Timeout bounds this step. It is per step rather than per repo, so adding a
	// second measurement does not silently halve the time the first one gets.
	Timeout Duration `yaml:"timeout"`

	// MaxAge applies only in ingest mode (no Command). An artifact older than
	// this is reported as stale rather than presented as a current number.
	//
	// Omit it for the default. There is no way to write "no limit": a
	// non-positive max_age is rejected, because a zero used to be read as an
	// absent key and answered with the 24 hour default. See UnmarshalYAML.
	MaxAge Duration `yaml:"max_age"`
}

// HasCommand reports whether this step runs something, as opposed to reading an
// artifact somebody else produced.
func (s Signal) HasCommand() bool { return len(s.Command) > 0 }

// Formats lists the parsers this step declares, in a fixed order.
func (s Signal) Formats() []Format {
	var out []Format
	if s.ArtifactFormat != "" {
		out = append(out, s.ArtifactFormat)
	}
	if s.StdoutFormat != "" {
		out = append(out, s.StdoutFormat)
	}
	return out
}

// NonZeroExitIsNormal reports whether a non-zero exit from this step means
// findings rather than failure.
//
// Every format on the step has to agree. A step that writes a coverage profile
// and a SARIF log from one command is not covered by lint's exemption: the
// non-zero exit could be either the linter finding something or the suite going
// red, and treating that as normal would silence the red suite.
func (s Signal) NonZeroExitIsNormal() bool {
	declared := s.Formats()
	if len(declared) == 0 {
		return false
	}
	for _, f := range declared {
		if !f.NonZeroExitIsNormal() {
			return false
		}
	}
	return true
}

// MergedEnv is the step's environment: the repo's, with the step's over it.
func (s Signal) MergedEnv(repo map[string]string) map[string]string {
	if len(repo) == 0 && len(s.Env) == 0 {
		return nil
	}
	out := make(map[string]string, len(repo)+len(s.Env))
	maps.Copy(out, repo)
	maps.Copy(out, s.Env)
	return out
}

// UnmarshalYAML gives a step its defaults before the file is read over it, the
// same way Defaults does for the whole config. It implements goccy/go-yaml's
// BytesUnmarshaler.
//
// Defaulting has to happen here rather than in a pass afterwards, because
// afterwards there is nothing left to tell "max_age: 0s" written on purpose
// from the key being absent: both are Duration(0), and filling in the default
// for a zero silently gave an operator asking for no staleness limit 24 hours.
// Done this way, an absent key keeps the default and an explicit zero survives
// to validate, which rejects it.
//
// Only Signal gets this. The same trick on Repo would set OldTimeout and
// OldMaxAge non-zero, and migrationError would then fire on every repo in every
// config.
func (s *Signal) UnmarshalYAML(b []byte) error {
	// A local type so the decode below does not call this method again.
	type signalFields Signal
	fields := signalFields{
		Timeout: Duration(DefaultTimeout),
		MaxAge:  Duration(DefaultMaxAge),
	}
	if err := yaml.Unmarshal(b, &fields); err != nil {
		return err
	}
	*s = Signal(fields)
	return nil
}

// Defaults returns a Config with every top-level default applied. Per-step
// defaults cannot live here because repos is a slice, so they are applied by
// Signal.UnmarshalYAML as each step is read.
func Defaults() *Config {
	return &Config{
		Database:      DefaultDatabase,
		Window:        Duration(DefaultWindow),
		MinStatements: DefaultMinStatements,
		MinRepoDelta:  DefaultMinRepoDelta,
	}
}

// ErrNoConfigFile marks the config file itself being absent, as opposed to a
// path named inside a config that was read perfectly well.
//
// Both used to be indistinguishable to a caller: a missing repo path is an
// os.Stat error joined into the validation errors, and errors.Is recurses
// through a joined error, so both satisfied errors.Is(err, os.ErrNotExist).
// The `repo-metrics init` hint keyed off that told an operator whose repo path
// was wrong to overwrite the config they had just edited.
var ErrNoConfigFile = errors.New("no config file")

// Load reads, expands, and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is operator-supplied by design
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config: %s does not exist: %w", path, ErrNoConfigFile)
		}
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	cfg.expandEnv()

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
		expandEnvMap(r.Env)
		for j := range r.Signals {
			s := &r.Signals[j]
			s.Artifact = os.ExpandEnv(s.Artifact)
			for k := range s.Command {
				s.Command[k] = os.ExpandEnv(s.Command[k])
			}
			expandEnvMap(s.Env)
		}
	}
}

// expandEnvMap expands values only. A ${VAR} in a key would let the environment
// decide which variable gets set, and an unset one would produce the empty key
// that validate rejects.
func expandEnvMap(env map[string]string) {
	for k, v := range env {
		env[k] = os.ExpandEnv(v)
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
			// The error is reported rather than assumed to be absence. A
			// permission problem on an intermediate directory came back as "path
			// does not exist", which sends someone looking for a missing checkout
			// that is sitting right there.
			problems = append(problems, fmt.Errorf("%s: path %s is unusable: %w", label, r.Path, err))
		} else if !info.IsDir() {
			problems = append(problems, fmt.Errorf("%s: path %s is not a directory", label, r.Path))
		}

		problems = append(problems, checkEnv(label, r.Env)...)
		problems = append(problems, r.validateSignals(label)...)
	}

	return errors.Join(problems...)
}

// validateSignals checks a repo's step list, including the migration error for
// a config still written in the old single-command shape.
func (r Repo) validateSignals(label string) []error {
	var problems []error

	migrating := r.migrationError(label)
	if migrating != nil {
		problems = append(problems, migrating)
	}

	// The migration error suppresses "nothing to measure" and nothing else. It
	// would bury the rewrite instructions under a complaint about their own
	// consequence, which is why that suppression is deliberate. Returning early
	// on it was not: a config carrying one leftover repo-level key beside a
	// perfectly good signals list had every other check skipped, so duplicate
	// signal names, bad formats and format collisions all went unreported while
	// the one error printed claimed the file was in the old shape.
	if len(r.Signals) == 0 && migrating == nil {
		problems = append(problems, fmt.Errorf("%s: no signals configured, so there is nothing to measure", label))
	}

	names := make(map[string]bool, len(r.Signals))
	// used counts how many steps declared each format, so a collision on the
	// metrics table's primary key is caught here rather than at insert time,
	// where it would cost the whole snapshot.
	used := make(map[Format]int, len(r.Signals))

	for i, s := range r.Signals {
		sub := fmt.Sprintf("%s: signals[%d]", label, i)
		if s.Name != "" {
			sub = fmt.Sprintf("%s: %s", label, s.Name)
		}

		switch {
		case s.Name == "":
			problems = append(problems, fmt.Errorf("%s: name is required", sub))
		case names[s.Name]:
			problems = append(problems, fmt.Errorf("%s: duplicate signal name", sub))
		default:
			names[s.Name] = true
		}

		problems = append(problems, s.validate(sub)...)

		// Counted once per signal, however many of its sources name the format.
		// Repeatable means two SIGNALS may share a format, never that one signal
		// may read the same format twice: both reads would be scoped by the same
		// step name and collide with each other. Counting sources here let
		// `artifact_format: sarif` beside `stdout_format: sarif` validate, and
		// every collection then dropped the step.
		seen := make(map[Format]bool, 2)
		for _, f := range s.Formats() {
			if seen[f] {
				problems = append(problems, fmt.Errorf(
					"%s: reads %s from both its artifact and its stdout, which would write the same metric keys twice under one signal name. Pick one source", sub, f))
				continue
			}
			seen[f] = true
			used[f]++
		}
	}

	for _, f := range formatOrder {
		if used[f] > 1 && !f.Repeatable() {
			problems = append(problems, fmt.Errorf(
				"%s: %d signals read %s, and repo-metrics cannot tell whether their metric keys would collide. "+
					"One repo normally has one of these, so combine them into a single signal", label, used[f], f))
		}
	}
	return problems
}

// validate checks one step in isolation.
func (s Signal) validate(label string) []error {
	var problems []error

	if s.ArtifactFormat != "" {
		if err := checkFormat(label, "artifact_format", s.ArtifactFormat); err != nil {
			problems = append(problems, err)
		}
		if s.Artifact == "" {
			problems = append(problems, fmt.Errorf("%s: artifact_format needs an artifact to read", label))
		}
	}
	if s.StdoutFormat != "" {
		if err := checkFormat(label, "stdout_format", s.StdoutFormat); err != nil {
			problems = append(problems, err)
		}
		if !s.HasCommand() {
			problems = append(problems, fmt.Errorf("%s: stdout_format needs a command to capture stdout from", label))
		}
	}
	if s.Artifact != "" && s.ArtifactFormat == "" {
		// Otherwise the step would run its command, check the artifact is fresh,
		// and then read nothing out of it.
		problems = append(problems, fmt.Errorf("%s: artifact needs an artifact_format saying how to read it", label))
	}
	if len(s.Formats()) == 0 {
		problems = append(problems, fmt.Errorf(
			"%s: no artifact_format and no stdout_format, so this signal would run and record nothing", label))
	}
	if !s.HasCommand() && s.Artifact == "" {
		problems = append(problems, fmt.Errorf(
			"%s: no command to run and no artifact to read", label))
	}

	problems = append(problems, checkEnv(label, s.Env)...)

	if s.Timeout <= 0 {
		problems = append(problems, fmt.Errorf("%s: timeout must be positive, omit it for the default of %s", label, DefaultTimeout))
	}
	// Rejected at zero and not only below it, which is what makes an operator
	// who wanted no staleness limit say so out loud. This used to accept
	// "max_age: 0s" and hand back the 24 hour default, because the default was
	// filled in for any zero before this ran. See Signal.UnmarshalYAML.
	//
	// Nothing a config can express reaches collect with a non-positive max_age
	// now. The maxAge <= 0 arm in internal/collect's isStale is therefore for
	// direct callers of that package only.
	if s.MaxAge <= 0 {
		problems = append(problems, fmt.Errorf("%s: max_age must be positive, omit it for the default of %s", label, DefaultMaxAge))
	}
	return problems
}

// checkEnv validates an env block's keys.
//
// Sorted so a config with several bad env keys reports them in the same order
// every run. Map order would make the message shuffle between runs and the
// failure harder to talk about.
func checkEnv(label string, env map[string]string) []error {
	var problems []error
	for _, k := range slices.Sorted(maps.Keys(env)) {
		switch {
		case k == "":
			problems = append(problems, fmt.Errorf("%s: env has an empty key", label))
		case strings.Contains(k, "="):
			// The runner joins these as KEY=VALUE, so a key carrying its own "="
			// would silently set a different variable than the one written in the
			// file.
			problems = append(problems, fmt.Errorf("%s: env key %q cannot contain \"=\"", label, k))
		}
	}
	return problems
}

// migrationError explains a config written against the old single-command shape.
//
// goccy ignores unknown keys, so without this a config from the previous version
// loads with an empty signals list and fails with "no signals configured", which
// is true and tells the operator nothing about why their working file stopped
// working. The old fields are read purely so this message can be produced.
func (r Repo) migrationError(label string) error {
	var old []string
	if r.OldCoverprofile != "" {
		old = append(old, "coverprofile")
	}
	if len(r.OldCommand) > 0 {
		old = append(old, "command")
	}
	if r.OldStdoutFormat != "" {
		old = append(old, "stdout_format")
	}
	if r.OldTimeout != 0 {
		old = append(old, "timeout")
	}
	if r.OldMaxAge != 0 {
		old = append(old, "max_age")
	}
	if len(old) == 0 {
		return nil
	}

	// A repo that already has signals is not in the old shape, so it does not
	// get told that it is, and it does not get a replacement block either: the
	// block is assembled from the old fields alone, so pasting it over a repo
	// with real signals would throw those signals away. What is wrong here is
	// narrower, and so is the instruction.
	if len(r.Signals) > 0 {
		return fmt.Errorf(
			"%s: %s %s at the repo level, which is where the old single-command shape put %s. "+
				"This repo already has a signals list, so %s ignored. Each one belongs in a signal now: "+
				"move it there, or delete it. env is the only setting that is still repo-level",
			label, strings.Join(old, ", "), pluralIs(len(old)), pluralThem(len(old)), pluralAre(len(old)))
	}

	// The example is filled in from what they actually wrote, so it can be
	// pasted rather than translated.
	example := "      - name: coverage\n"
	if len(r.OldCommand) > 0 {
		example += fmt.Sprintf("        command: [%s]\n", strings.Join(quoteAll(r.OldCommand), ", "))
	}
	if r.OldCoverprofile != "" {
		example += fmt.Sprintf("        artifact: %s\n", r.OldCoverprofile)
		example += fmt.Sprintf("        artifact_format: %s\n", FormatGoCoverprofile)
	}
	if r.OldStdoutFormat != "" {
		example += fmt.Sprintf("        stdout_format: %s\n", r.OldStdoutFormat)
	}
	if r.OldTimeout != 0 {
		example += fmt.Sprintf("        timeout: %s\n", r.OldTimeout)
	}
	if r.OldMaxAge != 0 {
		example += fmt.Sprintf("        max_age: %s\n", r.OldMaxAge)
	}

	return fmt.Errorf(
		"%s: %s %s at the repo level, which is the old single-command shape. "+
			"A repo now carries a list of signals so it can measure more than coverage. Rewrite it as:\n\n"+
			"  - name: %s\n    path: %s\n    signals:\n%s",
		label, strings.Join(old, ", "), pluralIs(len(old)), r.Name, r.Path, example)
}

func pluralIs(n int) string {
	if n == 1 {
		return "is set"
	}
	return "are set"
}

func pluralThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func pluralAre(n int) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = fmt.Sprintf("%q", a)
	}
	return out
}
