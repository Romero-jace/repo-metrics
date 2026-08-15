package collect_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// gitConfigOff isolates the helper's git invocations from the machine running
// them. Without it an ambient ~/.gitconfig can rename the initial branch out
// from under the branch assertion, or sign the commit and fail on a machine
// whose signing key is not unlocked. Neither has anything to do with the code
// under test, and both would look like a real failure.
var gitConfigOff = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}

// requireGit skips rather than fails when git is absent.
//
// This repo is meant to be handed to another organization and built on machines
// nobody here controls, so the unit suite declines to hard-require git. The
// message names what went unverified, because a skip that reads like a pass is
// the same disease as a zero that reads like a measurement.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH (%v): SHA, branch, and dirty-tree extraction in gitMetadata went UNVERIFIED in this run", err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitConfigOff...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitCheckout builds a clean one-commit checkout on branch main, with the
// coverage profile already committed.
//
// The profile has to be tracked: an untracked file in the working tree makes
// `git status --porcelain` non-empty, which would report the clean tree as
// dirty and quietly invert the assertion this whole helper exists to support.
func gitCheckout(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := repoDir(t)
	// -c init.defaultBranch=main so the branch assertion does not depend on
	// the git version's default or on an operator's preference.
	git(t, dir, "-c", "init.defaultBranch=main", "init")
	writeProfile(t, dir, "coverage.out", 0)
	writeFile(t, dir, "README.md", "tracked file, modified by the dirty-tree test\n")
	git(t, dir, "add", "-A")
	// Identity on the command line so a machine with no configured user can
	// still commit, and no signing config can reach in.
	git(t, dir,
		"-c", "user.name=repo-metrics test",
		"-c", "user.email=test@example.invalid",
		"commit", "-m", "initial")
	return dir
}

// A real checkout has to yield the SHA and branch that git itself reports, and
// they have to survive onto the snapshot. Every collect test before this one
// used a bare t.TempDir(), so this half of gitMetadata had never executed.
func TestGitCheckoutRecordsSHAAndBranch(t *testing.T) {
	dir := gitCheckout(t)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	// Ground truth from git, not a shape check: a SHA that is 40 hex
	// characters of the wrong commit would pass a regexp and be useless.
	if want := git(t, dir, "rev-parse", "HEAD"); res.Snapshot.GitSHA != want {
		t.Errorf("GitSHA: got %q, want %q", res.Snapshot.GitSHA, want)
	}
	if res.Snapshot.GitBranch != "main" {
		t.Errorf("GitBranch: got %q, want %q", res.Snapshot.GitBranch, "main")
	}
	if res.Snapshot.GitDirty {
		t.Error("GitDirty: got true on a freshly committed tree, want false")
	}
	if strings.Contains(diagText(res), "git metadata unavailable") {
		t.Errorf("a real checkout produced the not-a-checkout warning:\n%s", diagText(res))
	}
}

// git_dirty is stored on every snapshot and has never once been observed true,
// so this pins the value rather than merely running the branch.
func TestDirtyTreeIsRecordedDirty(t *testing.T) {
	dir := gitCheckout(t)

	// A tracked file is modified rather than a new one added: `git status`
	// honors ignore rules and status.showUntrackedFiles, so an untracked file
	// can be invisible on someone else's machine. A modified tracked file
	// always shows.
	writeFile(t, dir, "README.md", "tracked file, now modified\n")

	res := collectOnce(t, repo(dir))

	if !res.Snapshot.GitDirty {
		t.Errorf("GitDirty: got false with a modified tracked file, want true. git status said %q",
			git(t, dir, "status", "--porcelain"))
	}
	// The dirty flag must not come at the cost of the rest of the metadata.
	if res.Snapshot.GitSHA == "" {
		t.Error("GitSHA is empty on a dirty checkout")
	}
	if res.Snapshot.GitBranch != "main" {
		t.Errorf("GitBranch: got %q, want %q", res.Snapshot.GitBranch, "main")
	}
}

// Detached HEAD is what a CI checkout normally looks like, and `rev-parse
// --abbrev-ref HEAD` answers "HEAD" there rather than failing. The SHA is still
// the real one, which is the part that has to keep working.
func TestDetachedHeadStillRecordsSHA(t *testing.T) {
	dir := gitCheckout(t)
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "--detach", head)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.GitSHA != head {
		t.Errorf("GitSHA: got %q, want %q", res.Snapshot.GitSHA, head)
	}
	if res.Snapshot.GitBranch != "HEAD" {
		t.Errorf("GitBranch on a detached head: got %q, want %q", res.Snapshot.GitBranch, "HEAD")
	}
}

// A checkout with no commits at all: `rev-parse HEAD` fails, so the whole
// lookup degrades. The repo is still measurable, which is the rule the package
// doc states.
func TestEmptyCheckoutDegradesToWarning(t *testing.T) {
	requireGit(t)

	dir := repoDir(t)
	git(t, dir, "-c", "init.defaultBranch=main", "init")
	writeProfile(t, dir, "coverage.out", 0)

	res := collectOnce(t, repo(dir))

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if res.Snapshot.GitSHA != "" || res.Snapshot.GitBranch != "" || res.Snapshot.GitDirty {
		t.Errorf("a commitless checkout produced metadata: sha=%q branch=%q dirty=%v",
			res.Snapshot.GitSHA, res.Snapshot.GitBranch, res.Snapshot.GitDirty)
	}
	if !strings.Contains(diagText(res), "git metadata unavailable") {
		t.Errorf("the missing metadata was not disclosed:\n%s", diagText(res))
	}
}

// The profile lives inside the checkout in these tests, so confirm the path
// resolution still works from there and the numbers are the same ones the
// non-git tests get. Cheap, and it catches a checkout-only regression in the
// relative-path join.
func TestGitCheckoutStillProducesMetrics(t *testing.T) {
	dir := gitCheckout(t)

	res := collectOnce(t, repo(dir))

	if got := metric(t, res, collect.KeyTotalStmts, "example.com/m/alpha"); got != 10 {
		t.Errorf("alpha total: got %v, want 10", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "coverage.out")); err != nil {
		t.Errorf("the committed profile went missing: %v", err)
	}
}
