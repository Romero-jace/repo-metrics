package cli_test

import (
	"strings"
	"testing"
)

// Three spellings, because people type all three and there is no reason to make
// anyone guess which one this tool prefers.
func TestVersionSpellings(t *testing.T) {
	for _, spelling := range []string{"version", "--version", "-version"} {
		t.Run(spelling, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, spelling)
			if err != nil {
				t.Fatalf("asking for the version is not a failure, got %v (stderr: %s)", err, stderr)
			}
			if !strings.HasPrefix(stdout, "repo-metrics") {
				t.Errorf("want the version on stdout, got %q", stdout)
			}
			// The usage text is the tell for the unknown-command path. Printing
			// it here would mean the spelling fell through to default, which
			// still exits 0 for the caller and looks fine from a script.
			if strings.Contains(stderr, "usage:") {
				t.Errorf("%s printed the usage text, so it was not recognized:\n%s", spelling, stderr)
			}
		})
	}
}

// Version output is something people paste into bug reports, so it goes to
// stdout where it can be piped, and it carries the toolchain and platform
// because that is the first thing anyone reading the report asks.
func TestVersionIsUsableInABugReport(t *testing.T) {
	stdout, stderr, err := runCLI(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if stderr != "" {
		t.Errorf("want nothing on stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, "built with go") {
		t.Errorf("want the Go version this was built with, got %q", stdout)
	}
	if strings.Contains(stdout, "—") {
		t.Error("version output contains an em dash")
	}
}

// Every other subcommand rejects an argument it does not understand rather than
// ignoring it, because printing the answer and exiting 0 tells a caller their
// flag was honored. version is not an exception just because it is small.
func TestVersionRejectsArguments(t *testing.T) {
	stdout, stderr, err := runCLI(t, "version", "--format", "json")
	if err == nil {
		t.Fatalf("want an error for an argument version does not take, got stdout %q", stdout)
	}
	if !strings.Contains(stderr, "--format") {
		t.Errorf("the error does not name the argument it rejected: %q", stderr)
	}
}

// A command the help never mentions cannot be found by anyone, which is the
// same argument the section list makes for itself further down this package.
func TestUsageMentionsVersion(t *testing.T) {
	_, stderr, _ := runCLI(t, "help")
	if !strings.Contains(stderr, "repo-metrics version") {
		t.Errorf("the usage text does not list the version command:\n%s", stderr)
	}
}
