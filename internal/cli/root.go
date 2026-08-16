// Package cli is a thin shell over the config, collect, store, delta, and report
// packages. It uses only the standard library, no cobra, to keep this module's
// dependency surface to the two libraries it genuinely needs.
//
// Global flags go after the subcommand (repo-metrics collect --config x.yaml),
// which is how git, docker, and kubectl behave. Go's flag package stops at the
// first positional argument, so the reverse order cannot work without a manual
// pre-pass, and a tool that accepts only one of the two orders should accept the
// one users already have in their fingers.
//
// Run is the only exported symbol, and everything printed here goes to the
// writers Run was handed rather than to os.Stdout. That is what makes the whole
// command surface testable in process.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// defaultConfigPath is where every subcommand looks unless told otherwise.
const defaultConfigPath = "repo-metrics.yaml"

// Run dispatches a subcommand. args excludes the program name. A non-nil return
// becomes exit status 1, so every path that returns one has already explained
// itself on stderr: the error text is for the caller, not for the user.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("no command given")
	}

	// A collect run over a dozen repos takes minutes, so ctrl-C or a TERM from
	// launchd has to stop it between repos rather than killing it mid-write.
	// Cancellation propagates into the subprocess runner as well.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "collect":
		return runCollect(ctx, args[1:], stdout, stderr)
	case "report":
		return runReport(ctx, args[1:], stdout, stderr)
	case "repos":
		return runRepos(ctx, args[1:], stdout, stderr)
	case "history":
		return runHistory(ctx, args[1:], stdout, stderr)
	// Spelled three ways because people type all three, and a version check
	// that answers "unknown command" is a small insult at the exact moment
	// someone is trying to tell you what they are running. Deliberately not -v:
	// that letter is worth keeping free for a verbose flag.
	case "version", "--version", "-version":
		return runVersion(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stderr)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// newFlagSet builds a subcommand flag set that reports through the caller's
// writer and never exits the process. flag.ExitOnError would call os.Exit from
// inside a library function, which defeats the injectable writers entirely.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	return set
}

// parseFlags reports whether the caller should carry on. A -h is a successful
// outcome rather than a failure, so it stops the command without an error and
// without exit status 1.
//
// No subcommand takes a positional argument, and leftovers are rejected rather
// than ignored. Go's flag package stops at the first non-flag token, so
// `collect myrepo --config prod.yaml` would otherwise parse no flags at all,
// fall back to the default config, quietly collect a different set of repos
// than the one asked for, and exit 0. That is the silent wrong answer this tool
// exists to refuse.
func parseFlags(set *flag.FlagSet, args []string, stderr io.Writer) (proceed bool, err error) {
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stderr)
			return false, nil
		}
		return false, err
	}
	if set.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s takes no positional arguments, got %q\n", set.Name(), set.Arg(0))
		printUsage(stderr)
		return false, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	return true, nil
}

// loadConfig reads the config and explains itself on stderr, since the error
// this returns only ever becomes an exit status.
func loadConfig(path string, stderr io.Writer) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "run `repo-metrics init` to write a starter config at %s\n", path)
		}
		return nil, err
	}
	return cfg, nil
}

func openStore(path string, stderr io.Writer) (*store.Store, error) {
	st, err := store.Open(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return nil, err
	}
	return st, nil
}

// coverageTotals sums statement counts across a snapshot's packages.
//
// The sum has to happen before the division: a repo's coverage is total covered
// over total statements, not the mean of the per-package rates, so a big package
// counts for more than a small one. Repo-level metrics carry an empty scope and
// are not coverage at all, hence the skip.
func coverageTotals(metrics []store.Metric) (covered, total int) {
	for _, m := range metrics {
		if m.Scope == "" {
			continue
		}
		switch m.Key {
		case collect.KeyCoveredStmts:
			covered += int(m.Value)
		case collect.KeyTotalStmts:
			total += int(m.Value)
		}
	}
	return covered, total
}

func printUsage(w io.Writer) {
	// The section names are generated from report.Sections() rather than typed
	// out here, so a section the parser accepts cannot go unlisted and a name
	// that gets dropped cannot linger in the help. This text is the entire
	// discoverability story for the flag, since there is deliberately no MCP
	// server describing it.
	//
	// Concatenated rather than passed through Fprintf: the body below is prose
	// that people will edit, and a stray percent sign in it would otherwise turn
	// into a formatting verb.
	sections := strings.Join(report.Sections(), ", ")
	formats := strings.Join(report.Formats(), ", ")
	signals := strings.Join(delta.SignalNames(), ", ")

	// A failed write to the caller's own writer is not actionable, so the
	// error is deliberately discarded here and everywhere else we print.
	_, _ = fmt.Fprint(w, `repo-metrics: track coverage, test health, lint findings and dependency
staleness across a pile of repos, and say what got worse this week.

usage:
  repo-metrics init    [--config FILE] [--force]
  repo-metrics collect [--config FILE] [--repo NAME]
  repo-metrics report  [--config FILE] [--window 7d] [--out FILE] [--format markdown|json]
                       [--repo NAME] [--section NAME]
  repo-metrics repos   [--config FILE] [--format `+formats+`]
  repo-metrics history --repo NAME [--config FILE] [--signal NAME] [--since 90d]
                       [--format `+formats+`]
  repo-metrics version

Flags go AFTER the subcommand, the way git and docker take them:

  repo-metrics collect --config repo-metrics.yaml

init flags:
  --config FILE   where to write the starter config (default repo-metrics.yaml)
  --force         overwrite an existing file instead of refusing

collect flags:
  --config FILE   config to read (default repo-metrics.yaml)
  --repo NAME     collect just this one repo instead of all of them

  Each repo runs the signals its config lists, and a signal is the unit of
  failure: one going wrong costs its own measurements and nothing else, and the
  snapshot comes back partial rather than failed. The progress line names which
  ones landed.

  A signal that ran but could not trust what it found records nothing rather
  than a zero. The clearest case is dependency updates with GOPROXY off, where
  "everything is current" and "nothing was checked" are identical output.

report flags:
  --config FILE   config to read (default repo-metrics.yaml)
  --window DUR    how far back the baseline sits, like 7d or 36h.
                  Defaults to the window in the config, which itself defaults to 7d.
  --out FILE      write the report here instead of to stdout
  --format FMT    markdown (default) or json
  --repo NAME     report on just this one repo instead of all of them. A name
                  that is not in the config is an error, not an empty report.
                  Every report says what it covers, narrowed or not: the json
                  carries a scope object and the markdown says so under the
                  heading, so a quiet answer about one repo is never readable as
                  a quiet week across all of them.
  --section NAME  render one part of the report instead of the whole thing.
                  One of: `+sections+`. Default is all.
                    movers   what got better or worse, and by far the cheapest
                             thing to ask for
                    repos    the every-repo table, plus the packages that came
                             and went
                    problems the repos that did not report clean
                  It applies to markdown and json alike. In json, a section you
                  did not ask for comes back null rather than empty, so you can
                  tell "not requested" from "nothing to report".

repos flags:
  --config FILE   config to read (default repo-metrics.yaml)
  --format FMT    `+formats+` (default `+string(report.FormatMarkdown)+`)

history flags:
  --config FILE   config to read (default repo-metrics.yaml)
  --repo NAME     which repo to chart. Required: there is no sensible history
                  of nine repos at once, so this is an error rather than a
                  silent pick.
  --signal NAME   which measurement to chart. One of: `+signals+`.
                  Default is coverage.
  --since DUR     how far back to look, like 90d or 26w. Default is 90d, which
                  is deliberately not the report's window: that one is the
                  offset to a baseline, and a week of history is not a trend.
  --format FMT    `+formats+` (default `+string(report.FormatMarkdown)+`)

  history keeps failed runs in the series instead of filtering them out. A gap
  in collection is the finding, and a chart that silently omits its failures
  draws a straight line through the week nobody was looking.

version takes no flags. It reports the module version when this binary was
installed from a tag, the commit when it was built from a checkout, and says
plainly that it does not know when it was built with neither.

collect keeps going when a repo fails and exits 1 at the end if any did, so one
unreachable repo never costs you the other nine.
`)
}
