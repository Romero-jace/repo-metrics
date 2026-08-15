// Package collect runs a repo's collection command, checks that it actually
// produced something, parses the artifacts, and assembles a snapshot.
//
// The governing rule is that failure degrades rather than aborting. One
// unreachable repo must not cost you the other nine, so every failure path
// produces a snapshot with a status and a diagnostic instead of an error that
// unwinds the whole run.
package collect

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/run"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// gitTimeout bounds the metadata lookups. They are three cheap local commands;
// if git is wedged we would rather record an unlabeled snapshot than hang.
const gitTimeout = 15 * time.Second

// Result is one repo's collection outcome.
type Result struct {
	Snapshot    store.Snapshot
	Metrics     []store.Metric
	Diagnostics []Diagnostic
}

// Collect runs one repo end to end. It does not return an error: everything
// that can go wrong is recorded on the snapshot.
func Collect(ctx context.Context, repo config.Repo, collector Collector, now time.Time) Result {
	started := time.Now()

	res := Result{Snapshot: store.Snapshot{
		CollectedAt: now,
		Status:      store.StatusOK,
	}}

	sha, branch, dirty, gitDiags := gitMetadata(ctx, repo.Path)
	res.Snapshot.GitSHA, res.Snapshot.GitBranch, res.Snapshot.GitDirty = sha, branch, dirty
	res.Diagnostics = append(res.Diagnostics, gitDiags...)
	res.Snapshot.Env = envFingerprint(ctx, repo.Path)

	profilePath := repo.Coverprofile
	if !filepath.IsAbs(profilePath) {
		profilePath = filepath.Join(repo.Path, profilePath)
	}

	artifacts := Artifacts{CoverprofilePath: profilePath}
	before := statFile(profilePath)

	if len(repo.Command) > 0 {
		stdoutPath, cleanup, err := stdoutSink(repo)
		if err != nil {
			return res.fail(started, errorf("%v", err))
		}
		defer cleanup()
		artifacts.StdoutPath = stdoutPath

		runRes, diags, ok := execute(ctx, repo, stdoutPath)
		res.Diagnostics = append(res.Diagnostics, diags...)
		if !ok {
			return res.finish(started, store.StatusFailed)
		}

		after := statFile(profilePath)
		if !isFresh(before, after, runRes.StartedAt) {
			return res.fail(started, errorf(
				"command exited %d but wrote no fresh coverage profile at %s. "+
					"A target that is declared .PHONY with no rule exits 0 without doing anything, "+
					"and any stale profile already at that path would otherwise be reported as current",
				runRes.ExitCode, profilePath))
		}
		if runRes.ExitCode != 0 {
			// The suite is red but the profile is real, so the numbers stand.
			res.Snapshot.Status = store.StatusPartial
			res.Diagnostics = append(res.Diagnostics, warnf(
				"command exited %d; coverage was still collected. stderr: %s",
				runRes.ExitCode, truncate(runRes.Stderr, 2000)))
		}
	} else {
		// Ingest mode: something else produces the artifact on its own
		// schedule, so age is the only freshness signal available.
		if !before.exists {
			return res.fail(started, errorf("no coverage profile at %s and no command configured to produce one", profilePath))
		}
		if isStale(before, time.Duration(repo.MaxAge), now) {
			res.Snapshot.Status = store.StatusPartial
			res.Diagnostics = append(res.Diagnostics, warnf(
				"coverage profile at %s is %s old, past the %s limit, so these numbers are stale",
				profilePath, now.Sub(before.mtime).Round(time.Minute), time.Duration(repo.MaxAge)))
		}
	}

	metrics, diags, err := collector.Collect(artifacts)
	res.Diagnostics = append(res.Diagnostics, diags...)
	if err != nil {
		return res.fail(started, errorf("%v", err))
	}
	res.Metrics = metrics

	return res.finish(started, res.Snapshot.Status)
}

// execute runs the configured command, reporting whether collection can carry
// on. A non-zero exit is survivable; failing to start or timing out is not.
func execute(ctx context.Context, repo config.Repo, stdoutPath string) (*run.Result, []Diagnostic, bool) {
	var (
		diags  []Diagnostic
		stdout *os.File
		err    error
	)

	if stdoutPath != "" {
		if stdout, err = os.Create(stdoutPath); err != nil { //nolint:gosec // path is our own temp file
			return nil, []Diagnostic{errorf("creating stdout capture file: %v", err)}, false
		}
		defer func() { _ = stdout.Close() }()
	}

	opts := run.Options{
		Dir:     repo.Path,
		Args:    repo.Command,
		Timeout: time.Duration(repo.Timeout),
	}
	if stdout != nil {
		opts.Stdout = stdout
	}

	runRes, err := run.Command(ctx, opts)
	if err != nil {
		return nil, append(diags, errorf("running %s: %v", strings.Join(repo.Command, " "), err)), false
	}
	if runRes.TimedOut {
		return runRes, append(diags, errorf(
			"command exceeded its %s timeout and was killed", time.Duration(repo.Timeout))), false
	}
	return runRes, diags, true
}

// stdoutSink returns a temp file path for capturing stdout, or an empty path if
// this repo has no stdout format configured and therefore no reason to keep it.
func stdoutSink(repo config.Repo) (path string, cleanup func(), err error) {
	if repo.StdoutFormat == "" {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp("", "repo-metrics-")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp dir for stdout: %w", err)
	}
	return filepath.Join(dir, "stdout.json"), func() { _ = os.RemoveAll(dir) }, nil
}

func (r Result) fail(started time.Time, d Diagnostic) Result {
	r.Diagnostics = append(r.Diagnostics, d)
	r.Metrics = nil
	return r.finish(started, store.StatusFailed)
}

func (r Result) finish(started time.Time, status store.Status) Result {
	r.Snapshot.Status = status
	r.Snapshot.Duration = time.Since(started)
	r.Snapshot.Error = firstError(r.Diagnostics)
	return r
}

func firstError(diags []Diagnostic) string {
	for _, d := range diags {
		if d.Severity == SeverityError {
			return d.Message
		}
	}
	return ""
}

// gitMetadata reads HEAD, the branch, and whether the tree is dirty. All three
// are best effort: a repo that is not a git checkout is still measurable.
func gitMetadata(ctx context.Context, dir string) (sha, branch string, dirty bool, diags []Diagnostic) {
	sha, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", false, []Diagnostic{warnf("git metadata unavailable: %v", err)}
	}
	branch, _ = gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	status, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err == nil {
		dirty = status != ""
	}
	return sha, branch, dirty, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	var buf bytes.Buffer
	res, err := run.Command(ctx, run.Options{
		Dir:     dir,
		Args:    append([]string{"git"}, args...),
		Timeout: gitTimeout,
		Stdout:  &buf,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git %s exited %d", strings.Join(args, " "), res.ExitCode)
	}
	return strings.TrimSpace(buf.String()), nil
}

// envFingerprint identifies the toolchain a measurement was taken with.
//
// Go workspaces make this necessary: a repo inside a go.work compiles against
// its siblings' working trees, while the same repo with GOWORK=off compiles
// against the versions pinned in go.mod. Same command, same commit, different
// source, different coverage. Recording the fingerprint is what lets the report
// refuse to diff across that boundary silently.
func envFingerprint(ctx context.Context, dir string) string {
	var buf bytes.Buffer
	res, err := run.Command(ctx, run.Options{
		Dir:     dir,
		Args:    []string{"go", "env", "GOVERSION", "GOWORK"},
		Timeout: gitTimeout,
		Stdout:  &buf,
	})
	if err != nil || res.ExitCode != 0 {
		return "go=unknown"
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	version := runtime.Version()
	if len(lines) > 0 && lines[0] != "" {
		version = lines[0]
	}
	workspace := "off"
	if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
		workspace = "on"
	}
	return fmt.Sprintf("go=%s;gowork=%s", version, workspace)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
