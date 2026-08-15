// Command repo-metrics collects code-quality metrics across a configured set of
// Git repositories, stores snapshots in SQLite, and reports week-over-week deltas.
//
// It is deliberately a one-shot binary: "collect" does a single pass and exits,
// so cadence is launchd's or cron's problem rather than a daemon's.
package main

import (
	"os"

	"github.com/Romero-jace/repo-metrics/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
