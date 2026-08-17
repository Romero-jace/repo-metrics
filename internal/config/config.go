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

	// Fingerprint is an argv that prints something identifying this repo's
	// toolchain, like ["node", "--version"]. Its trimmed stdout becomes the
	// snapshot's fingerprint, which is what lets the report refuse to diff two
	// snapshots taken under different runtimes.
	//
	// Optional, and only needed where the tool cannot work it out. A repo running
	// any Go format is fingerprinted with `go env GOVERSION GOWORK` without being
	// asked. A repo running none of them has no toolchain this tool can name, and
	// records that it does not know rather than guessing: see
	// collect.envFingerprint. This is the way to tell it.
	//
	// An argv slice for the same reason Signal.Command is one. There is no shell,
	// so quoting cannot surprise anyone, and a probe that needs a pipe to produce
	// one line is a script the repo should own rather than something to embed here.
	Fingerprint []string `yaml:"fingerprint"`

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

	// Near misses for Fingerprint, read only so a typo fails loudly.
	//
	// goccy discards a key nothing declares, so `fingerprnt:` or `finger_print:`
	// would load clean and leave the repo silently fingerprinted as unidentified
	// forever. That is the same shape as the bug this whole field exists to fix,
	// arriving through the config file instead of the collector. See
	// misspelledFingerprintError.
	TypoFingerprnt   []string `yaml:"fingerprnt"`
	TypoFingerPrint  []string `yaml:"finger_print"`
	TypoFingerprints []string `yaml:"fingerprints"`
}

// UsesGoToolchain reports whether any of this repo's steps reads a format that
// the Go toolchain produces.
//
// It is what decides whether `go env` is worth asking. Derived from the formats
// rather than configured, on the same reasoning as the -u sniff in the module
// parser: a setting can disagree with what the repo actually runs, and the
// formats cannot disagree with themselves.
func (r Repo) UsesGoToolchain() bool {
	for _, s := range r.Signals {
		for _, f := range s.Formats() {
			if f.Toolchain() == ToolchainGo {
				return true
			}
		}
	}
	return false
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
	//
	// The one-file shorthand. A step reading several files uses Artifacts below;
	// these two stay because most steps read one and `artifact: coverage.out` is
	// what that should look like. Read them through ArtifactList, never directly.
	Artifact       string `yaml:"artifact"`
	ArtifactFormat Format `yaml:"artifact_format"`

	// Artifacts is several files from one run, each with its own format.
	//
	// It exists because a single command routinely writes more than one:
	// `pytest --junitxml=j.xml --cov-report=lcov:c.info` produces a test report
	// and a coverage profile together, and so does vitest.
	//
	// Before this, the only way to read the second file was a second step with no
	// command, reading whatever the first step's run had left on disk. That works,
	// and it quietly costs the freshness check: a step with a command requires its
	// artifact to have CHANGED during the run, while a step without one only asks
	// whether the file is younger than max_age. So the second file was held to a
	// 24 hour window instead of to the run that supposedly produced it, and a
	// profile left over from yesterday was accepted as today's measurement.
	//
	// Mutually exclusive with the shorthand above rather than merged with it.
	// Merging would let one step declare the same format twice by accident, which
	// the per-signal dedup rule then rejects, on a config that named each file
	// once.
	Artifacts []Artifact `yaml:"artifacts"`

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

// Artifact is one file a step leaves behind, paired with how to read it.
//
// The pairing is the point. As two loose fields on Signal it was an invariant
// somebody had to check, and the check existed: an artifact with no format made
// a step run its command, verify the file was fresh, and then read nothing out
// of it. As one struct it is a shape, and the invariant cannot be violated.
type Artifact struct {
	// Path is relative to the repo unless absolute, like the shorthand field.
	Path string `yaml:"path"`
	// Format says which parser reads it.
	Format Format `yaml:"format"`
}

// HasCommand reports whether this step runs something, as opposed to reading an
// artifact somebody else produced.
func (s Signal) HasCommand() bool { return len(s.Command) > 0 }

// ArtifactList is every file this step reads, whichever spelling declared them.
//
// It RETURNS the shorthand rather than copying it into Artifacts, and that is
// load bearing rather than tidy. A copy would leave both shapes populated, so
// Formats would report the format twice and the per-signal dedup rule would
// reject a config that named the file once. Returning one or the other makes
// declaring both an error to be caught rather than a state to be reconciled.
func (s Signal) ArtifactList() []Artifact {
	if s.Artifact != "" || s.ArtifactFormat != "" {
		return []Artifact{{Path: s.Artifact, Format: s.ArtifactFormat}}
	}
	return s.Artifacts
}

// Formats lists the parsers this step declares, in a fixed order.
func (s Signal) Formats() []Format {
	var out []Format
	for _, a := range s.ArtifactList() {
		if a.Format != "" {
			out = append(out, a.Format)
		}
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
	// The strict option has to be repeated here, and forgetting it is invisible.
	//
	// This is a fresh decode with its own options, so the DisallowUnknownField
	// passed in Load does not reach it. Without this line EVERY key inside a
	// signals: entry escapes the unknown-key check while the rest of the file is
	// covered, which is worse than not being strict at all: the check appears to
	// be on and is off exactly where the most keys are.
	if err := yaml.UnmarshalWithOptions(b, &fields, yaml.DisallowUnknownField()); err != nil {
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
	// Strict, so a key this tool does not read is an error rather than silence.
	//
	// goccy discards an unrecognized key by default, which makes a typo the exact
	// failure this whole project is built against: `fingerprnt:` loads clean and
	// the repo is fingerprinted as unidentified forever, `artifcts:` loads clean
	// and the step reads nothing. Both look like a working config.
	//
	// The Old* and Typo* fields on Repo are declared, so they are known keys and
	// still produce their own better messages. This catches everything nobody
	// thought to name in advance.
	if err := yaml.UnmarshalWithOptions(data, cfg, yaml.DisallowUnknownField()); err != nil {
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
		// Same treatment as a step's command, and for the same reason: the probe
		// for a toolchain installed under a version manager lives at a path that
		// differs per machine.
		for k := range r.Fingerprint {
			r.Fingerprint[k] = os.ExpandEnv(r.Fingerprint[k])
		}
		for j := range r.Signals {
			s := &r.Signals[j]
			s.Artifact = os.ExpandEnv(s.Artifact)
			// Both spellings, because this runs BEFORE validation and nothing
			// downstream expands anything. ArtifactList is read at validate and
			// collect time, by which point expansion is over, so a ${VAR} in an
			// artifacts: entry that was not expanded here reaches the collector as
			// literal text while the same variable in the shorthand works. That is
			// one shape of a config silently doing something other than what it
			// says, in the walk whose own test comment warns that a restructure
			// drops it.
			for k := range s.Artifacts {
				s.Artifacts[k].Path = os.ExpandEnv(s.Artifacts[k].Path)
			}
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
		problems = append(problems, checkFingerprint(label, r)...)
		problems = append(problems, r.validateSignals(label)...)
	}

	return errors.Join(problems...)
}

// checkFingerprint validates the toolchain probe argv, and rejects the spellings
// that would otherwise be discarded in silence.
//
// An empty argv element is rejected rather than dropped: exec passes it through
// as an empty argument, and a probe that silently ran a different command than
// the one in the file is exactly the kind of disagreement this field exists to
// prevent.
func checkFingerprint(label string, r Repo) []error {
	var problems []error

	for _, typo := range []struct {
		key  string
		argv []string
	}{
		{"fingerprnt", r.TypoFingerprnt},
		{"finger_print", r.TypoFingerPrint},
		{"fingerprints", r.TypoFingerprints},
	} {
		if len(typo.argv) > 0 {
			problems = append(problems, fmt.Errorf(
				"%s: %q is not a key this tool reads, and unknown keys are discarded without a word. Write it as fingerprint", label, typo.key))
		}
	}

	for i, arg := range r.Fingerprint {
		if arg == "" {
			problems = append(problems, fmt.Errorf(
				"%s: fingerprint[%d] is empty, which would pass an empty argument to the probe rather than being skipped", label, i))
		}
	}
	return problems
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
		//
		// The message no longer names the two sources, because there are no longer
		// only two: a step reading several artifacts can collide with itself
		// without stdout being involved at all.
		seen := make(map[Format]bool, len(s.Formats()))
		for _, f := range s.Formats() {
			if seen[f] {
				problems = append(problems, fmt.Errorf(
					"%s: reads %s twice, which would write the same metric keys twice under one signal name. Read it once, or split the step in two", sub, f))
				continue
			}
			seen[f] = true
			used[f]++
		}
	}

	for _, f := range formatOrder {
		if used[f] > 1 && !f.Repeatable() {
			// The advice deliberately does not say "combine them into a single
			// signal" any more. That used to be safe to suggest because a step
			// could only read one artifact, so it read as "you cannot do this".
			// Now it is an instruction an operator can follow, and the per-signal
			// rule above rejects the result: two files of one format collide with
			// each other whether they sit in one step or two.
			problems = append(problems, fmt.Errorf(
				"%s: %d signals read %s, and repo-metrics cannot tell whether their metric keys would collide. "+
					"One repo normally has one of these, so read it once, or measure them in separate repos", label, used[f], f))
		}
	}

	problems = append(problems, checkRepoScopedCollisions(label, r)...)
	return problems
}

// checkRepoScopedCollisions rejects two steps that would write the same
// repo-level row.
//
// The loop above counts usage per format, which answers whether one format
// appears twice and nothing else. That was a complete question while every
// repo-scoped key belonged to exactly one format. It stops being one as soon as
// two DIFFERENT formats can measure the same thing: a `go test -json` step
// beside a JUnit step both want to say how many packages carry no tests, the
// metrics primary key is (snapshot, key, scope), and the per-format count sees
// nothing wrong with it.
//
// Caught here rather than at collection because the collector's answer is to
// fail the second step, every run, forever. That is loud, but it is a config
// error being rediscovered nightly instead of once, at load, by the code that
// already knows the shapes.
func checkRepoScopedCollisions(label string, r Repo) []error {
	var problems []error
	owner := make(map[string]string)

	for _, s := range r.Signals {
		if s.Name == "" {
			continue
		}
		for _, f := range s.Formats() {
			for _, key := range f.RepoScopedKeys() {
				if first, taken := owner[key]; taken && first != s.Name {
					problems = append(problems, fmt.Errorf(
						"%s: %s and %s would both record %s for the whole repo, which is one row and cannot hold two numbers. "+
							"Keep one of them, or measure them in separate repos",
						label, first, s.Name, key))
					continue
				}
				owner[key] = s.Name
			}
		}
	}
	return problems
}

// validate checks one step in isolation.
func (s Signal) validate(label string) []error {
	var problems []error

	// Declaring both spellings is an error rather than a precedence rule. Either
	// answer would be a config doing something other than what it says, and there
	// is no reading of a file naming both under which the operator meant one.
	if len(s.Artifacts) > 0 && (s.Artifact != "" || s.ArtifactFormat != "") {
		problems = append(problems, fmt.Errorf(
			"%s: names both artifacts and the single artifact/artifact_format pair. Use one or the other", label))
	}

	if s.ArtifactFormat != "" {
		if err := checkFormat(label, "artifact_format", s.ArtifactFormat); err != nil {
			problems = append(problems, err)
		}
		if s.Artifact == "" {
			problems = append(problems, fmt.Errorf("%s: artifact_format needs an artifact to read", label))
		}
	}
	if s.Artifact != "" && s.ArtifactFormat == "" {
		// Otherwise the step would run its command, check the artifact is fresh,
		// and then read nothing out of it. The list spelling cannot reach this
		// state at all, since a path and a format are one value there.
		problems = append(problems, fmt.Errorf("%s: artifact needs an artifact_format saying how to read it", label))
	}

	seenPath := make(map[string]bool, len(s.Artifacts))
	for i, a := range s.Artifacts {
		where := fmt.Sprintf("%s: artifacts[%d]", label, i)
		// Named separately rather than as one "incomplete entry" message, because
		// the likeliest way to get here is writing the shorthand's key names inside
		// the list, and the fix is to know which two words the entry wants.
		if a.Path == "" {
			problems = append(problems, fmt.Errorf("%s: needs a path saying which file to read", where))
		}
		switch a.Format {
		case "":
			problems = append(problems, fmt.Errorf("%s: needs a format saying how to read it", where))
		default:
			if err := checkFormat(where, "format", a.Format); err != nil {
				problems = append(problems, err)
			}
		}
		if a.Path != "" && seenPath[a.Path] {
			// Two entries for one file would parse it twice and write its metrics
			// twice, which the collector answers by dropping the whole step.
			problems = append(problems, fmt.Errorf("%s: reads %s twice", where, a.Path))
		}
		seenPath[a.Path] = true
	}

	if s.StdoutFormat != "" {
		if err := checkFormat(label, "stdout_format", s.StdoutFormat); err != nil {
			problems = append(problems, err)
		}
		if !s.HasCommand() {
			problems = append(problems, fmt.Errorf("%s: stdout_format needs a command to capture stdout from", label))
		}
	}
	if len(s.Formats()) == 0 {
		problems = append(problems, fmt.Errorf(
			"%s: no artifact_format and no stdout_format, so this signal would run and record nothing", label))
	}
	if !s.HasCommand() && len(s.ArtifactList()) == 0 {
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
