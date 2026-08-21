// Package collect runs a repo's configured collection steps, checks that they
// actually produced something, parses the artifacts, and assembles a snapshot.
//
// The governing rule is that failure degrades rather than aborting. One
// unreachable repo must not cost you the other nine, and inside a repo one
// broken step must not cost you the others, so every failure path produces a
// snapshot with a status and a diagnostic instead of an error that unwinds the
// run.
package collect

import (
	"bytes"
	"context"
	"fmt"
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
	// Collected and Failed name the steps that did and did not produce
	// measurements, so the progress line can say which measurement is missing
	// rather than only that something is.
	Collected []string
	Failed    []string
}

// Collect runs one repo end to end. It does not return an error: everything
// that can go wrong is recorded on the snapshot.
func Collect(ctx context.Context, repo config.Repo, now time.Time) Result {
	started := time.Now()

	res := Result{Snapshot: store.Snapshot{
		CollectedAt: now,
		Status:      store.StatusOK,
	}}

	sha, branch, dirty, gitDiags := gitMetadata(ctx, repo.Path)
	res.Snapshot.GitSHA, res.Snapshot.GitBranch, res.Snapshot.GitDirty = sha, branch, dirty
	res.Diagnostics = append(res.Diagnostics, gitDiags...)

	// The repo's configured environment has to reach this too, not just the
	// commands. Fingerprinting the ambient environment while measuring under a
	// different one records the wrong side of the very boundary the fingerprint
	// exists to mark. It is taken once per repo, from the repo's own env rather
	// than any one step's, because it is what tells the report whether two
	// snapshots are comparable at all.
	var envDiag []Diagnostic
	res.Snapshot.Env, envDiag = envFingerprint(ctx, repo, envPairs(repo.Env))
	res.Diagnostics = append(res.Diagnostics, envDiag...)

	var degraded bool
	seen := make(map[metricKey]string)

	for i, step := range repo.Signals {
		// Cancellation is checked between steps, so a signal that arrives during
		// a long test run stops this repo without discarding what already ran.
		if ctx.Err() != nil {
			res.Diagnostics = append(res.Diagnostics, errorf(
				"%s: canceled before this signal ran", step.Name))
			res.Failed = append(res.Failed, step.Name)
			continue
		}

		out := runStep(ctx, repo, step, i, now)
		res.Diagnostics = append(res.Diagnostics, out.Diagnostics...)

		if out.OK {
			if dup := firstDuplicate(seen, out.Metrics); dup != nil {
				// Left to the database this collides on the metrics primary key
				// and rolls back the INSERT, taking every other step's numbers
				// with it. Config validation rejects the shapes that can cause it,
				// so reaching here means something got past that, and the cheapest
				// correct answer is to lose this step rather than the snapshot.
				//
				// The two cases get different words. A collision against an
				// earlier step names that step; a collision inside one step's own
				// output has no earlier step to name, and saying "already written
				// by " with nothing after it is worse than saying nothing.
				culprit := "within its own output"
				if owner := seen[*dup]; owner != "" {
					culprit = "already written by " + owner
				}
				res.Diagnostics = append(res.Diagnostics, errorf(
					"%s: would record %s a second time, %s, so its measurements were dropped rather than losing the whole snapshot",
					step.Name, dup, culprit))
				res.Failed = append(res.Failed, step.Name)
				continue
			}
			for _, m := range out.Metrics {
				seen[metricKey{m.Key, m.Scope}] = step.Name
			}
			res.Metrics = append(res.Metrics, out.Metrics...)
			res.Collected = append(res.Collected, step.Name)
			degraded = degraded || out.Degraded
			continue
		}
		res.Failed = append(res.Failed, step.Name)
	}

	return res.finish(started, degraded)
}

// metricKey is the metrics table's primary key, minus the snapshot.
type metricKey struct {
	key   string
	scope string
}

func (k metricKey) String() string {
	if k.scope == "" {
		return k.key
	}
	return k.key + " for " + k.scope
}

// firstDuplicate reports the first metric a step would write over, or nil.
//
// It checks the whole batch before anything is committed, so a step that
// collides on its last row does not leave the rows before it behind. Half a
// step's numbers are not a measurement of anything.
func firstDuplicate(seen map[metricKey]string, metrics []store.Metric) *metricKey {
	batch := make(map[metricKey]bool, len(metrics))
	for _, m := range metrics {
		k := metricKey{m.Key, m.Scope}
		if _, taken := seen[k]; taken || batch[k] {
			return &k
		}
		batch[k] = true
	}
	return nil
}

// finish settles the snapshot's status from what the steps did.
//
// The three statuses mean what store's own documentation says they mean, which
// this is finally in a position to make true: failed is a snapshot carrying
// nothing trustworthy, partial is some signals collected and some not, ok is
// everything asked for.
func (r Result) finish(started time.Time, degraded bool) Result {
	switch {
	case len(r.Collected) == 0:
		r.Snapshot.Status = store.StatusFailed
		// Nothing was trustworthy, so nothing is kept. A partial write here is
		// how a repo ends up reporting a coverage cliff that is really a crashed
		// command.
		r.Metrics = nil
	case len(r.Failed) > 0 || degraded:
		r.Snapshot.Status = store.StatusPartial
	default:
		r.Snapshot.Status = store.StatusOK
	}

	// Recorded on every snapshot this binary writes, including false, because
	// the absent case has to keep meaning "written before anything was
	// checking". A run that collected nothing is left unrecorded: it has no
	// numbers to have taken under protest, so neither answer is true of it.
	if len(r.Collected) > 0 {
		r.Snapshot.Degraded = &degraded
	}

	r.Snapshot.Duration = time.Since(started)
	r.Snapshot.Error = snapshotReason(r.Diagnostics)
	return r
}

// snapshotReason is what a snapshot records about why it is not clean.
//
// An error first and alone, since that is a snapshot carrying nothing
// trustworthy and there is no more to say. Otherwise every diagnostic that
// actually cost the snapshot something, in the order they happened.
//
// This whole function is the fix for a real gap: it used to look for errors
// alone, so a snapshot degraded by a warning stored no reason at all, and the
// report listed the repo under Collection problems as a bare name with nothing
// after the status word. Every repo with one failing test is in that state,
// because a non-zero exit degrades the step.
//
// All of them rather than the first, because the first is usually the least
// informative. A broken test build makes the command exit non-zero AND makes the
// counts partial, and the exit code is noticed first while the reason a person
// can act on arrives second. Ranking them would mean inventing a scale; saying
// both costs a line and loses nothing.
//
// It deliberately does not fall back to ordinary warnings. A clean snapshot
// routinely carries several (a repo that is not a git checkout, a proxy nobody
// consulted) and none of them is a reason for anything. Only a diagnostic that
// says it cost something explains a snapshot that is not ok, which is also why an
// ok snapshot picks up nothing here: nothing degrading can be present without the
// status having moved.
func snapshotReason(diags []Diagnostic) string {
	for _, d := range diags {
		if d.Severity == SeverityError {
			return d.Message
		}
	}
	var reasons []string
	for _, d := range diags {
		if d.Degrades {
			reasons = append(reasons, d.Message)
		}
	}
	return strings.Join(reasons, "; ")
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

// EnvUnidentified is the fingerprint recorded when nothing established which
// toolchain a measurement was taken under.
//
// It is a value the delta layer recognizes rather than an ordinary string,
// because two of them must not compare as the same toolchain. That is the whole
// defect this constant replaces: the old placeholder was a plain "go=unknown",
// so a repo that failed the probe on both snapshots read as "the toolchain did
// not change", which is a fact nobody established being published as one.
const EnvUnidentified = "unidentified"

// envUnknownLegacy is the placeholder written before EnvUnidentified existed.
//
// Snapshots carrying it are still in databases and still get compared, so it has
// to keep meaning what it meant. Recognizing it here rather than rewriting rows
// also keeps the migration out of this change: a stored fingerprint is evidence
// about a run that already happened, and editing it would be editing the record.
const envUnknownLegacy = "go=unknown"

// EnvIsUnidentified reports whether a stored fingerprint names no toolchain.
//
// The empty string counts. A snapshot written by something that never set the
// column, or a hand-built fixture, has established nothing either, and failing
// closed is the answer everywhere else in this tool.
func EnvIsUnidentified(env string) bool {
	return env == "" || env == EnvUnidentified || env == envUnknownLegacy
}

// envFingerprint identifies the toolchain a measurement was taken with.
//
// Go workspaces are what made this necessary: a repo inside a go.work compiles
// against its siblings' working trees, while the same repo with GOWORK=off
// compiles against the versions pinned in go.mod. Same command, same commit,
// different source, different coverage. Recording the fingerprint is what lets
// the report refuse to diff across that boundary silently.
//
// Which probe to run is derived rather than assumed, and that is the part that
// changed. This used to run `go env` against every repo unconditionally, which
// is wrong in both directions on a repo with no Go in it. Where Go is installed
// the probe SUCCEEDS and records the ambient Go version, a string that cannot
// move when the repo's actual runtime does and does move when someone upgrades
// Go on the collector; where it is not, it fails and records a placeholder that
// compares equal to every other failure. Either way a Node or Python upgrade
// between two snapshots goes unflagged, in the one field that exists to flag it.
//
// So: an explicit probe wins, a repo running any Go format gets the Go probe,
// and anything else records that nothing identified it. The last branch is the
// point. Refusing to name a toolchain is worth more than naming the wrong one,
// for the same reason the module parser records no update count rather than a
// flattering zero.
func envFingerprint(ctx context.Context, repo config.Repo, env []string) (string, []Diagnostic) {
	switch {
	case len(repo.Fingerprint) > 0:
		return probeFingerprint(ctx, repo, env)
	case repo.UsesGoToolchain():
		return goFingerprint(ctx, repo.Path, env)
	}
	return EnvUnidentified, []Diagnostic{warnf(
		"no toolchain fingerprint: this repo runs no Go-format step and configures no fingerprint command, " +
			"so nothing identifies the runtime its numbers were measured under and an upgrade between two " +
			"snapshots will not be flagged. Set fingerprint on the repo to fix it, for instance " +
			`fingerprint: ["node", "--version"]`)}
}

// probeFingerprint runs the operator's own toolchain probe.
//
// Output is collapsed to a single space-separated line, so a probe that prints
// several lines produces one stable fingerprint rather than one that depends on
// how the runtime formats its version banner this release.
//
// A probe that runs and prints nothing is treated as a failure rather than as an
// empty fingerprint. `command -v` style probes exit 0 with no output when they
// find nothing, and an empty answer stored as the fingerprint would compare
// equal to every other empty answer, which is the defect this whole function
// exists to close.
func probeFingerprint(ctx context.Context, repo config.Repo, env []string) (string, []Diagnostic) {
	var buf bytes.Buffer
	res, err := run.Command(ctx, run.Options{
		Dir:     repo.Path,
		Args:    repo.Fingerprint,
		Env:     env,
		Timeout: gitTimeout,
		Stdout:  &buf,
	})
	name := filepath.Base(repo.Fingerprint[0])
	switch {
	case err != nil:
		return EnvUnidentified, []Diagnostic{warnf(
			"toolchain fingerprint unavailable: the configured probe %s could not run: %v", name, err)}
	case res.ExitCode != 0:
		return EnvUnidentified, []Diagnostic{warnf(
			"toolchain fingerprint unavailable: the configured probe %s exited %d", name, res.ExitCode)}
	}
	out := strings.Join(strings.Fields(buf.String()), " ")
	if out == "" {
		return EnvUnidentified, []Diagnostic{warnf(
			"toolchain fingerprint unavailable: the configured probe %s exited 0 but printed nothing", name)}
	}
	return name + "=" + out, nil
}

// goFingerprint asks the Go toolchain to identify itself.
//
// The failure is disclosed rather than swallowed. gitMetadata, the sibling
// best-effort probe, has always warned when it came up empty, and this one
// silently returned a placeholder.
func goFingerprint(ctx context.Context, dir string, env []string) (string, []Diagnostic) {
	var buf bytes.Buffer
	res, err := run.Command(ctx, run.Options{
		Dir:     dir,
		Args:    []string{"go", "env", "GOVERSION", "GOWORK"},
		Env:     env,
		Timeout: gitTimeout,
		Stdout:  &buf,
	})
	switch {
	case err != nil:
		return EnvUnidentified, []Diagnostic{warnf(
			"toolchain fingerprint unavailable: %v, so a workspace change between two snapshots will not be flagged", err)}
	case res.ExitCode != 0:
		return EnvUnidentified, []Diagnostic{warnf(
			"toolchain fingerprint unavailable: go env exited %d, so a workspace change between two snapshots will not be flagged", res.ExitCode)}
	}
	return fingerprintFrom(buf.String()), nil
}

// fingerprintFrom parses `go env GOVERSION GOWORK` output.
//
// Split out from the subprocess so the parsing can be tested directly, which
// matters because the parsing is where this went wrong: `go env GOWORK` prints
// one of three things and only one of them is a path. The literal "off" when
// the workspace is disabled, the go.work path when one is active, and the empty
// string when there is none. Reading "any non-empty value means a workspace is
// on" reported GOWORK=off as gowork=on, the exact inverse of the truth for the
// one case this fingerprint exists to detect. Measured against Go 1.26.
func fingerprintFrom(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	version := runtime.Version()
	if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
		version = strings.TrimSpace(lines[0])
	}

	workspace := "off"
	if len(lines) > 1 {
		switch strings.TrimSpace(lines[1]) {
		case "", "off":
			workspace = "off"
		default:
			workspace = "on"
		}
	}
	return fmt.Sprintf("go=%s;gowork=%s", version, workspace)
}
