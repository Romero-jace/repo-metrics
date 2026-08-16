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

// runStep executes one configured step and parses whatever it leaves behind.
func runStep(ctx context.Context, repo config.Repo, step config.Signal, index int, now time.Time) stepResult {
	res := stepResult{Name: step.Name}

	artifact := step.Artifact
	if artifact != "" && !filepath.IsAbs(artifact) {
		artifact = filepath.Join(repo.Path, artifact)
	}

	var (
		stdoutPath string
		runRes     *run.Result
		// artifactUnusable is set when the artifact cannot be trusted, so it is
		// skipped while the step's other sources still get their turn. A nil
		// Diagnostic means the artifact is fine.
		artifactUnusable *Diagnostic
	)
	before := statFile(artifact)

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

		if artifact != "" && !isFresh(before, statFile(artifact), runRes.StartedAt) {
			// The artifact is unusable, and only the artifact. This used to
			// abandon the whole step, which threw away a perfectly parsed
			// go-test-json stream because a sibling parser's file was missing:
			// the mirror of the case parseAll exists to prevent, in the one
			// direction it did not cover. A typo in artifact: was enough.
			//
			// The check itself stays: a stale file at that path DOES exist and
			// WOULD parse, and reporting months-old numbers as today's is the
			// failure this whole tool is built to refuse.
			d := errorf(
				"command exited %d but wrote no fresh %s at %s. "+
					"A target that is declared .PHONY with no rule exits 0 without doing anything, "+
					"and any stale artifact already at that path would otherwise be reported as current",
				runRes.ExitCode, step.ArtifactFormat, artifact)
			artifactUnusable = &d
		}
		if runRes.ExitCode != 0 && !step.NonZeroExitIsNormal() {
			// The command is unhappy but its output is real, so the numbers
			// stand and the snapshot says they were taken under protest.
			res.Degraded = true
			res.Diagnostics = append(res.Diagnostics, warnf(
				"command exited %d; its measurements were still collected. stderr: %s",
				runRes.ExitCode, truncate(runRes.Stderr, 2000)))
		}
	} else {
		// Ingest mode: something else produces the artifact on its own schedule,
		// so age is the only freshness signal available.
		if !before.exists {
			return res.fail(errorf("no artifact at %s and no command configured to produce one", artifact))
		}
		if isStale(before, time.Duration(step.MaxAge), now) {
			res.Degraded = true
			res.Diagnostics = append(res.Diagnostics, warnf(
				"artifact at %s is %s old, past the %s limit, so these numbers are stale",
				artifact, now.Sub(before.mtime).Round(time.Minute), time.Duration(step.MaxAge)))
		}
	}

	res.parseAll(ctx, repo, step, artifact, stdoutPath, runRes, now, artifactUnusable)

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
	artifact, stdoutPath string, runRes *run.Result, now time.Time,
	artifactUnusable *Diagnostic,
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
	switch {
	case artifactUnusable != nil:
		failures = append(failures, *artifactUnusable)
	case step.ArtifactFormat != "":
		sources = append(sources, pending{step.ArtifactFormat, artifact, "the " + string(step.ArtifactFormat) + " artifact"})
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
