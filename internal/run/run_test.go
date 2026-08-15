package run_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/run"
)

// The tests drive real subprocesses by re-executing this test binary with
// helperEnv set, which keeps them honest about pipes, exit codes, and signals
// without depending on whatever shell utilities happen to exist on the box.
const helperEnv = "REPO_METRICS_HELPER_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		helperMain(mode)
		return
	}
	os.Exit(m.Run())
}

func helperMain(mode string) {
	switch mode {
	case "ok":
		_, _ = fmt.Fprint(os.Stdout, "hello stdout")
		fmt.Fprint(os.Stderr, "hello stderr")
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "it went badly")
		os.Exit(3)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "cwd":
		wd, _ := os.Getwd()
		_, _ = fmt.Fprint(os.Stdout, wd)
		os.Exit(0)
	case "env":
		_, _ = fmt.Fprint(os.Stdout, os.Getenv("REPO_METRICS_TEST_VAR"))
		os.Exit(0)
	case "noisy-stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 200<<10))
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q", mode)
		os.Exit(99)
	}
}

func helper(t *testing.T, mode string) run.Options {
	t.Helper()
	return run.Options{
		Dir:     t.TempDir(),
		Args:    []string{os.Args[0]},
		Env:     []string{helperEnv + "=" + mode},
		Timeout: 30 * time.Second,
	}
}

func TestCommandCapturesStdoutAndStderr(t *testing.T) {
	var stdout bytes.Buffer
	opts := helper(t, "ok")
	opts.Stdout = &stdout

	res, err := run.Command(context.Background(), opts)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", res.ExitCode)
	}
	if got := stdout.String(); got != "hello stdout" {
		t.Errorf("stdout: got %q", got)
	}
	if !strings.Contains(res.Stderr, "hello stderr") {
		t.Errorf("stderr: got %q", res.Stderr)
	}
	if res.StartedAt.IsZero() {
		t.Error("StartedAt was not recorded; the freshness check depends on it")
	}
	if res.TimedOut {
		t.Error("TimedOut set on a command that finished on its own")
	}
}

// A failing test suite is a normal, informative outcome that still leaves a
// usable coverage profile behind. If Command reported it as an error the caller
// could not tell it apart from a missing binary.
func TestNonZeroExitIsAResultNotAnError(t *testing.T) {
	res, err := run.Command(context.Background(), helper(t, "fail"))
	if err != nil {
		t.Fatalf("Command returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode: got %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "it went badly") {
		t.Errorf("stderr not captured: %q", res.Stderr)
	}
}

func TestTimeoutKillsTheCommand(t *testing.T) {
	opts := helper(t, "sleep")
	opts.Timeout = 200 * time.Millisecond

	start := time.Now()
	res, err := run.Command(context.Background(), opts)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut was not set on a command that exceeded its timeout")
	}
	if elapsed > 20*time.Second {
		t.Errorf("timeout did not take effect: waited %s for a 200ms budget", elapsed)
	}
}

func TestWorkingDirectoryIsHonored(t *testing.T) {
	var stdout bytes.Buffer
	opts := helper(t, "cwd")
	opts.Stdout = &stdout

	if _, err := run.Command(context.Background(), opts); err != nil {
		t.Fatalf("Command: %v", err)
	}
	// macOS reports /var as /private/var, so compare on the suffix.
	if got := stdout.String(); !strings.HasSuffix(got, opts.Dir) {
		t.Errorf("cwd: got %q, want it to end with %q", got, opts.Dir)
	}
}

func TestExtraEnvReachesTheCommand(t *testing.T) {
	var stdout bytes.Buffer
	opts := helper(t, "env")
	opts.Env = append(opts.Env, "REPO_METRICS_TEST_VAR=visible")
	opts.Stdout = &stdout

	if _, err := run.Command(context.Background(), opts); err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := stdout.String(); got != "visible" {
		t.Errorf("env var: got %q, want %q", got, "visible")
	}
}

// Stderr only ever lands in a diagnostic message, so a runaway suite must not
// be able to balloon the database through it.
func TestStderrIsCapped(t *testing.T) {
	res, err := run.Command(context.Background(), helper(t, "noisy-stderr"))
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(res.Stderr) > 128<<10 {
		t.Errorf("stderr not capped: got %d bytes", len(res.Stderr))
	}
	if !strings.Contains(res.Stderr, "truncated") {
		t.Error("truncation was not disclosed in the captured stderr")
	}
}

// Discarding stdout has to be safe, since a repo with no stdout_format has no
// reason to keep it.
func TestNilStdoutIsDiscarded(t *testing.T) {
	res, err := run.Command(context.Background(), helper(t, "ok"))
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", res.ExitCode)
	}
}

func TestBadInvocationsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		opts run.Options
		want string
	}{
		{"no args", run.Options{Dir: "/tmp", Timeout: time.Second}, "empty command"},
		{"no dir", run.Options{Args: []string{"true"}, Timeout: time.Second}, "working directory"},
		{"no timeout", run.Options{Args: []string{"true"}, Dir: "/tmp"}, "timeout"},
		{
			"missing binary",
			run.Options{Args: []string{"/nonexistent/binary"}, Dir: "/tmp", Timeout: time.Second},
			"starting",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run.Command(context.Background(), tc.opts)
			if err == nil {
				t.Fatal("Command accepted an invalid invocation")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
