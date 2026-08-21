package report

import (
	"fmt"
	"strings"
)

// FailOn names a condition that should make report exit non-zero.
//
// It exists because report exits 0 unconditionally, which is documented and is
// still a trap: a scheduled job chaining collect and report takes its status
// from the last command, so a collection that stopped working goes on looking
// healthy indefinitely. That is the precise failure this tool exists to catch,
// reproduced in its own recommended deployment, and examples/repo-metrics-daily.sh
// exists to work around it by inspecting the database itself.
//
// A named selector rather than a bool for the same reason Section is one: asking
// for two things at once is not representable, and --help can list what there is
// without the flag set growing every time a new condition is worth failing on.
type FailOn string

const (
	// FailOnNothing is the default and preserves the exit code report has
	// always had. A flag that changed exit status by existing would break every
	// wrapper already parsing this command's output.
	FailOnNothing FailOn = "none"
	// FailOnProblems fails when any repo in scope did not collect cleanly.
	FailOnProblems FailOn = "problems"
	// FailOnMovers fails when any repo cleared the reporting threshold, which is
	// what a pre-merge check wants: not "did collection work" but "did anything
	// get worse".
	FailOnMovers FailOn = "movers"
)

// failOns is the one list. ParseFailOn validates against it and its error
// message is built from it, so a new condition cannot be accepted by the parser
// while going unmentioned in the error, or the other way round.
var failOns = []FailOn{FailOnNothing, FailOnProblems, FailOnMovers}

// ParseFailOn turns a command-line value into a FailOn.
//
// An unknown name is an error rather than a silent fallback to none. Quietly
// accepting --fail-on problmes and then exiting 0 would hand a pipeline the
// reassurance it asked to have withheld, which is worse than not offering the
// flag at all.
func ParseFailOn(s string) (FailOn, error) {
	for _, f := range failOns {
		if FailOn(s) == f {
			return f, nil
		}
	}
	return "", fmt.Errorf("report: unknown --fail-on %q, valid values are %s", s, failOnList())
}

// failOnList renders the valid names for an error message or help text.
func failOnList() string {
	names := make([]string, 0, len(failOns))
	for _, f := range failOns {
		names = append(names, string(f))
	}
	return strings.Join(names, ", ")
}

// FailOns lists the valid names, in the order help text should show them. It
// exists so the CLI's help cannot drift from what ParseFailOn accepts.
func FailOns() []string { return strings.Split(failOnList(), ", ") }
