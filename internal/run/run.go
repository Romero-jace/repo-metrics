// Package run executes a repo's collection command with bounds on how long it
// may take, how much stderr it may produce, and how long its children may keep
// the pipe open after it exits.
//
// Nothing here goes through a shell. Commands are argv slices, so quoting and
// word splitting cannot surprise anyone, and a repo path with a space in it is
// not a security question.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const (
	// maxStderr caps captured stderr. It only ever lands in a diagnostic
	// message, so a runaway suite must not be able to balloon the database.
	maxStderr = 64 << 10

	// waitDelay bounds how long we wait after the process exits for any
	// grandchild that inherited the pipes to let go of them. Without it a
	// leaked background process keeps the read blocked indefinitely and the
	// whole collection run hangs on one repo.
	waitDelay = 5 * time.Second
)

// Options describes one command execution.
type Options struct {
	// Dir is the working directory. Required.
	Dir string
	// Args is the argv, with the program at index 0. Required.
	Args []string
	// Env is appended to the caller's environment, as KEY=VALUE.
	Env []string
	// Timeout bounds the run. Required and must be positive.
	Timeout time.Duration
	// Stdout receives the command's stdout. Nil discards it. Callers that
	// expect volume (a `go test -json` stream is easily many MB) should pass a
	// file rather than a buffer.
	Stdout io.Writer
}

// Result describes how a command finished.
//
// A non-zero ExitCode is NOT an error from Command's point of view. Tests
// failing is a normal, informative outcome that still leaves a usable coverage
// profile behind, and only the caller knows whether it means partial or failed.
// Command returns an error only when the command could not be run at all.
type Result struct {
	// StartedAt is captured immediately before the process starts, so callers
	// can reason about which files it could possibly have written.
	StartedAt time.Time
	Duration  time.Duration
	ExitCode  int
	// Stderr is captured, truncated to maxStderr.
	Stderr string
	// TimedOut reports whether the command was killed for exceeding Timeout.
	TimedOut bool
}

// Command runs opts and reports how it finished.
func Command(ctx context.Context, opts Options) (*Result, error) {
	if len(opts.Args) == 0 {
		return nil, errors.New("run: empty command")
	}
	if opts.Dir == "" {
		return nil, errors.New("run: no working directory")
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("run: timeout must be positive")
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	stderr := &cappedBuffer{limit: maxStderr}

	cmd := exec.CommandContext(ctx, opts.Args[0], opts.Args[1:]...) //nolint:gosec // argv is operator-supplied by design
	cmd.Dir = opts.Dir
	cmd.Stdout = opts.Stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), opts.Env...)
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run: starting %s in %s: %w", opts.Args[0], opts.Dir, err)
	}

	waitErr := cmd.Wait()
	res := &Result{
		StartedAt: started,
		Duration:  time.Since(started),
		Stderr:    stderr.String(),
		TimedOut:  errors.Is(ctx.Err(), context.DeadlineExceeded),
	}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		res.ExitCode = 0
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		// Not an exit status: the pipes failed, WaitDelay fired, or the
		// process could not be reaped. That is a runner problem, not a
		// verdict on the repo.
		return res, fmt.Errorf("run: waiting for %s: %w", opts.Args[0], waitErr)
	}

	return res, nil
}

// cappedBuffer accumulates up to limit bytes and silently drops the rest.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	switch {
	case room >= len(p):
		return c.buf.Write(p)
	case room > 0:
		if _, err := c.buf.Write(p[:room]); err != nil {
			return 0, err
		}
		c.dropped += len(p) - room
	default:
		c.dropped += len(p)
	}
	// Always report the full length: a short write would make the command
	// think the pipe broke.
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	s := c.buf.String()
	if c.dropped > 0 {
		s += fmt.Sprintf("\n... stderr truncated at %d bytes ...", c.limit)
	}
	return s
}
