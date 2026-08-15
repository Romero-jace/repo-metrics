// Package cli is a thin shell over the config, collect, store, delta, and report
// packages. It uses only the standard library, no cobra, to keep this module's
// dependency surface to the two libraries it genuinely needs.
//
// Global flags go after the subcommand (repo-metrics collect --config x.yaml),
// which is how git, docker, and kubectl behave. Go's flag package stops at the
// first positional argument, so the reverse order cannot work without a manual
// pre-pass, and a tool that accepts only one of the two orders should accept the
// one users already have in their fingers.
package cli

import "io"

// Run dispatches a subcommand. args excludes the program name.
//
// Subcommand wiring lands in a later step; this stub exists so the module has a
// buildable package from the first commit.
func Run(_ []string, _, _ io.Writer) error {
	return nil
}
