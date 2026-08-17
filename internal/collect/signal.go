package collect

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/run"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// stepResult is one configured step's outcome.
//
// A step is the unit of failure. One step going wrong costs its own
// measurements and nothing else, which is the repo-level version of the rule
// that one unreachable repo must not cost you the other nine.
type stepResult struct {
	Name        string
	Metrics     []store.Metric
	Diagnostics []Diagnostic

	// OK reports whether at least one of this step's parsers ran to completion.
	//
	// Parse success rather than metric count, and the difference is load bearing:
	// a coverage profile carrying only its "mode: set" header parses perfectly
	// and yields no metrics at all. That is a step that worked and measured
	// nothing, which is a finding. Counting metrics here would call it a failure
	// and mark the snapshot degraded every week the repo instruments nothing.
	OK bool

	// Degraded reports that this step produced usable numbers with a caveat: a
	// red suite, a stale artifact, or one of several parsers failing.
	Degraded bool

	// Duration is the step's command wall time, zero in ingest mode.
	Duration time.Duration
}

// artifactState is one of a step's declared files: where it is, how to read it,
// what it looked like before the command ran, and why it cannot be trusted.
//
// The last two are per artifact rather than per step, which is the point. A step
// can name several files from one command, and each is judged on its own: a
// stale coverage profile must not cost a test report the same run wrote.
type artifactState struct {
	config.Artifact
	// before is the file as it was before the command ran, so freshness can be
	// judged as a difference rather than against a clock.
	before fileState
	// unusable is why this file may not be read, or nil when it is fine.
	unusable *Diagnostic
}

// runStep executes one configured step and parses whatever it leaves behind.
func runStep(ctx context.Context, repo config.Repo, step config.Signal, index int, now time.Time) stepResult {
	res := stepResult{Name: step.Name}

	// One entry per file this step reads, resolved and stat'd before anything
	// runs. Both the resolution and the before-state are per artifact: a step can
	// name several, and each is judged on its own.
	arts := make([]artifactState, 0, len(step.ArtifactList()))
	for _, a := range step.ArtifactList() {
		path := a.Path
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(repo.Path, path)
		}
		arts = append(arts, artifactState{
			Artifact: config.Artifact{Path: path, Format: a.Format},
			before:   statFile(path),
		})
	}

	var (
		stdoutPath string
		runRes     *run.Result
	)

	if step.HasCommand() {
		var (
			cleanup func()
			err     error
		)
		// Named by index rather than by step name: names come from the config
		// file, and a name containing a path separator would put this write
		// somewhere other than the temp directory we are responsible for.
		stdoutPath, cleanup, err = stdoutSink(step, index)
		if err != nil {
			return res.fail(errorf("%v", err))
		}
		defer cleanup()

		var diags []Diagnostic
		runRes, diags, err = execute(ctx, repo, step, stdoutPath)
		res.Diagnostics = append(res.Diagnostics, diags...)
		if err != nil {
			return res.finish()
		}
		res.Duration = runRes.Duration

		for i := range arts {
			a := &arts[i]
			if a.Path == "" || isFresh(a.before, statFile(a.Path), runRes.StartedAt) {
				continue
			}
			// This artifact is unusable, and only this one. The verdict is per
			// artifact rather than per step for the same reason it was already per
			// source: abandoning the step threw away a perfectly parsed
			// go-test-json stream because a sibling parser's file was missing, and
			// a typo in artifact: was enough. A step reading two files gets the
			// same treatment between them, so one stale profile does not cost a
			// test report written by the same command.
			//
			// The check itself stays: a stale file at that path DOES exist and
			// WOULD parse, and reporting months-old numbers as today's is the
			// failure this whole tool is built to refuse.
			d := errorf(
				"command exited %d but wrote no fresh %s at %s. "+
					"A target that is declared .PHONY with no rule exits 0 without doing anything, "+
					"and any stale artifact already at that path would otherwise be reported as current",
				runRes.ExitCode, a.Format, a.Path)
			a.unusable = &d
		}
		if runRes.ExitCode != 0 && !step.NonZeroExitIsNormal() {
			// The command is unhappy but its output is real, so the numbers
			// stand and the snapshot says they were taken under protest.
			res.Degraded = true
			// The stderr clause only when there is stderr. A great many tools
			// say everything they have to say on stdout, and "stderr: " with
			// nothing after it is a sentence that trails off in the middle of
			// the report.
			detail := ""
			if s := strings.TrimSpace(runRes.Stderr); s != "" {
				detail = ". stderr: " + truncate(s, 2000)
			}
			res.Diagnostics = append(res.Diagnostics, degradef(
				"command exited %d; its measurements were still collected%s",
				runRes.ExitCode, detail))
		}
	} else {
		// Ingest mode: something else produces the artifact on its own schedule,
		// so age is the only freshness signal available.
		for i := range arts {
			a := &arts[i]
			if !a.before.exists {
				// Per artifact rather than failing the step outright, which is what
				// this did when a step could only name one file. With one artifact
				// the outcome is unchanged: the failure reaches parseAll, nothing
				// else parses, so OK stays false, the diagnostic keeps its error
				// severity, and the step is failed exactly as before. With several
				// it costs only its own measurements.
				d := errorf("no artifact at %s and no command configured to produce one", a.Path)
				a.unusable = &d
				continue
			}
			if isStale(a.before, time.Duration(step.MaxAge), now) {
				res.Degraded = true
				res.Diagnostics = append(res.Diagnostics, degradef(
					"artifact at %s is %s old, past the %s limit, so these numbers are stale",
					a.Path, now.Sub(a.before.mtime).Round(time.Minute), time.Duration(step.MaxAge)))
			}
		}
	}

	res.parseAll(ctx, repo, step, arts, stdoutPath, runRes, now)

	// The runner has measured this since the day it was written and nothing has
	// ever read it. It goes in the step's own batch rather than being appended by
	// the caller, so the duplicate-key guard sees it along with everything else
	// the step produced, and so a step that failed carries no timing: its metrics
	// are dropped whole, and half a step's numbers are not a measurement.
	if res.OK && step.HasCommand() {
		res.Metrics = append(res.Metrics, store.Metric{
			Key:   KeySignalDurationMS,
			Scope: step.Name,
			Value: float64(res.Duration.Milliseconds()),
		})
	}
	return res.finish()
}

// parseAll reads every source this step declared.
//
// The step survives as long as ONE parser completes. That is what keeps losing
// the test stream from also costing coverage, which used to be spelled out as a
// special case for the one pairing that existed and is now the general rule.
func (r *stepResult) parseAll(
	ctx context.Context, repo config.Repo, step config.Signal,
	arts []artifactState, stdoutPath string, runRes *run.Result, now time.Time,
) {
	type pending struct {
		format config.Format
		path   string
		what   string
	}

	env := envPairs(step.MergedEnv(repo.Env))
	var (
		sources  []pending
		failures []Diagnostic
	)

	// An unusable artifact is a source that failed before it was read, so it
	// joins the failures rather than the sources. It then gets exactly the same
	// escalate-or-downgrade treatment as a parse error: fatal to the step only if
	// nothing else in it worked.
	//
	// The decision is inside the loop, which is the whole difference from the
	// version that handled one artifact. Hoisted out, one unusable file would
	// remove every artifact source in the step, so a stale coverage profile would
	// silently take a test report written by the same command with it.
	for _, a := range arts {
		if a.unusable != nil {
			failures = append(failures, *a.unusable)
			continue
		}
		if a.Format == "" {
			continue
		}
		// Named by path rather than only by format. With one artifact "the
		// go-coverprofile artifact" identified it; with several, two sources of
		// one step have to be told apart in a diagnostic.
		sources = append(sources, pending{a.Format, a.Path, fmt.Sprintf("the %s artifact at %s", a.Format, a.Path)})
	}
	if step.StdoutFormat != "" {
		sources = append(sources, pending{step.StdoutFormat, stdoutPath, "the " + string(step.StdoutFormat) + " output"})
	}

	for _, src := range sources {
		parse, err := parserFor(src.format)
		if err != nil {
			failures = append(failures, errorf("%v", err))
			continue
		}
		metrics, diags, err := parse(ctx, source{
			Path: src.path,
			Repo: repo,
			Step: step,
			Env:  env,
			Run:  runRes,
			Now:  now,
		})
		r.Diagnostics = append(r.Diagnostics, diags...)
		// A parser can report that it read its input and still lost something,
		// which is a different thing from the read failing. Only the parser is
		// in a position to know, so it says so on the diagnostic rather than
		// this loop guessing from the severity: plenty of warnings here are the
		// designed answer rather than a loss.
		for _, d := range diags {
			if d.Degrades {
				r.Degraded = true
			}
		}
		if err != nil {
			failures = append(failures, errorf("could not read %s, so its measurements are missing: %v", src.what, err))
			continue
		}
		r.OK = true
		r.Metrics = append(r.Metrics, metrics...)
	}

	// Whether a parse failure is fatal depends entirely on whether anything else
	// in this step worked, so the verdict cannot be reached until they have all
	// been tried.
	for _, f := range failures {
		if r.OK {
			r.Degraded = true
			f.Severity = SeverityWarn
			f.Degrades = true
		}
		r.Diagnostics = append(r.Diagnostics, f)
	}
}

// fail records the step as producing nothing and drops any metrics it had
// accumulated. Half a step's numbers are not a measurement of anything.
func (r stepResult) fail(d Diagnostic) stepResult {
	r.Diagnostics = append(r.Diagnostics, d)
	r.Metrics = nil
	r.OK = false
	return r.finish()
}

// finish labels every diagnostic with the step it came from. With one step per
// repo the source was obvious; with several a bare message says nothing about
// which measurement was lost.
func (r stepResult) finish() stepResult {
	for i := range r.Diagnostics {
		r.Diagnostics[i] = r.Diagnostics[i].prefix(r.Name)
	}
	return r
}

// execute runs the step's command, reporting whether parsing can carry on. A
// non-zero exit is survivable; failing to start or timing out is not.
func execute(
	ctx context.Context, repo config.Repo, step config.Signal, stdoutPath string,
) (*run.Result, []Diagnostic, error) {
	var (
		diags  []Diagnostic
		stdout *os.File
		err    error
	)

	if stdoutPath != "" {
		if stdout, err = os.Create(stdoutPath); err != nil { //nolint:gosec // path is our own temp file
			return nil, []Diagnostic{errorf("creating stdout capture file: %v", err)}, err
		}
		defer func() { _ = stdout.Close() }()
	}

	opts := run.Options{
		Dir:     repo.Path,
		Args:    step.Command,
		Env:     envPairs(step.MergedEnv(repo.Env)),
		Timeout: time.Duration(step.Timeout),
	}
	if stdout != nil {
		opts.Stdout = stdout
	}

	runRes, err := run.Command(ctx, opts)
	if err != nil {
		return nil, append(diags, errorf("running %s: %v", strings.Join(step.Command, " "), err)), err
	}
	if runRes.TimedOut {
		err := fmt.Errorf("%s exceeded its %s timeout", step.Name, time.Duration(step.Timeout))
		return runRes, append(diags, errorf(
			"command exceeded its %s timeout and was killed", time.Duration(step.Timeout))), err
	}
	return runRes, diags, nil
}

// stdoutSink returns a temp file path for capturing stdout, or an empty path if
// this step has no stdout format configured and therefore no reason to keep it.
func stdoutSink(step config.Signal, index int) (path string, cleanup func(), err error) {
	if step.StdoutFormat == "" {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp("", "repo-metrics-")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp dir for stdout: %w", err)
	}
	return filepath.Join(dir, fmt.Sprintf("step-%d.out", index)), func() { _ = os.RemoveAll(dir) }, nil
}

// envPairs renders an environment as the KEY=VALUE slice run.Options wants,
// appended to the process environment rather than replacing it.
//
// The keys are sorted because Go randomizes map iteration order. Left unsorted,
// two identical configs would hand the subprocess its variables in a different
// order on every run, so anything sensitive to that order (a duplicate key
// resolving to whichever came last, a command that echoes its environment)
// would fail intermittently and not reproduce.
func envPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	pairs := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		pairs = append(pairs, k+"="+env[k])
	}
	return pairs
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
