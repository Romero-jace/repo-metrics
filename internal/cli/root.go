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
		// Said out loud, ahead of the usage block, because main turns the
		// returned error into an exit status and never prints it. Without this
		// a typo produced seventy lines of usage and no statement of what was
		// wrong with what was typed. Explanation first and usage after is the
		// order parseFlags already uses, and the order Go's own flag package
		// uses for an undefined flag.
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		if strings.HasPrefix(args[0], "-") {
			// The likeliest way to land here without a typo, since Go's flag
			// package cannot take flags before the subcommand.
			_, _ = fmt.Fprintf(stderr, "flags go after the subcommand, as in: repo-metrics collect %s\n", args[0])
		}
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
		// config.ErrNoConfigFile rather than os.ErrNotExist. A repo path that is
		// not there also satisfies os.ErrNotExist, through the joined validation
		// errors, so this hint used to tell an operator who had just mistyped a
		// path to overwrite their own config. init then refuses without --force,
		// and with it would have destroyed their repo list.
		if errors.Is(err, config.ErrNoConfigFile) {
			_, _ = fmt.Fprintf(stderr, "run `repo-metrics init` to write a starter config at %s\n", path)
		}
		return nil, err
	}
	return cfg, nil
}

// databasePath is the database a subcommand should open: the --database flag
// when one was given, and the config's otherwise.
//
// An override rather than a replacement, because the config still names the
// repos and their signals. The case it exists for is a scratch collection, a
// coverage floor measured once, which must not be written into the history a
// schedule is keeping. Without it that means copying an entire config file to
// change one line, which is how two configs drift apart.
//
// An explicitly empty value reads as absent rather than as a request to open a
// file with no name, which is not a path anything could open.
func databasePath(cfg *config.Config, override string) string {
	if override != "" {
		return override
	}
	return cfg.Database
}

func openStore(path string, stderr io.Writer) (*store.Store, error) {
	st, err := store.Open(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return nil, err
	}
	return st, nil
}

// coverageSummaries renders one clause per coverage unit a snapshot measured.
//
// A clause each rather than one number, because the two units are never summed:
// a Go profile counts statements and an LCOV tracefile counts lines, and several
// statements on one source line collapse to one line there. A repo normally
// fills exactly one of them, so this is usually a single clause. A Go service
// emitting a tracefile beside its profile gets both, and neither stands in for
// the other.
//
// Until this it could only say statements, so a repo measured through LCOV
// finished with a progress line that named the step and carried no figure at
// all. On a mixed fleet that reads as a collection which did not work, which is
// the opposite of what it had just done.
//
// Read through delta rather than by summing metric keys here, so the presence
// rule is applied once and the sum still happens before the division: a repo's
// coverage is total covered over total, never the mean of the per-scope rates.
func coverageSummaries(metrics []store.Metric) []string {
	var out []string
	for _, id := range delta.CoverageSignals() {
		counts, measured := delta.CoverageCountsFor(metrics, id)
		// Only when there is a denominator. Coverage of zero statements is not
		// zero percent, and this line is the first place anyone would read it as
		// one.
		if !measured || counts.Total == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%.1f%% of %d %s",
			counts.Pct(), counts.Total, delta.CoverageCountNoun(id)))
	}
	return out
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
	signals := strings.Join(delta.SignalNames(), ", ")
	// Two renderings of one list, both derived. The synopsis wants the compact
	// alternation a usage line is read in, and the flag detail below wants a
	// sentence. Neither is typed out: this line used to be spelled three ways,
	// one of them hand written, and the comma form was read on a real fleet as
	// though both formats could be passed at once.
	formatSyntax := strings.Join(report.Formats(), "|")
	formatChoice := report.FormatChoice()

	// A failed write to the caller's own writer is not actionable, so the
	// error is deliberately discarded here and everywhere else we print.
	_, _ = fmt.Fprint(w, `repo-metrics: track coverage, test health, lint findings and dependency
staleness across a pile of repos, and say what got worse this week.

usage:
  repo-metrics init    [--config FILE] [--force]
  repo-metrics collect [--config FILE] [--database FILE] [--repo NAME]
  repo-metrics report  [--config FILE] [--database FILE] [--window 7d | --against REF]
                       [--out FILE] [--format `+formatSyntax+`] [--repo NAME] [--section NAME]
  repo-metrics repos   [--config FILE] [--database FILE] [--format `+formatSyntax+`]
  repo-metrics history --repo NAME [--config FILE] [--database FILE] [--signal NAME]
                       [--since 90d] [--format `+formatSyntax+`]
  repo-metrics version

Flags go AFTER the subcommand, the way git and docker take them:

  repo-metrics collect --config repo-metrics.yaml

init flags:
  --config FILE   where to write the starter config (default repo-metrics.yaml)
  --force         overwrite an existing file instead of refusing

collect flags:
  --config FILE   config to read (default repo-metrics.yaml)
  --database FILE database to write instead of the one the config names. The
                  config still supplies the repos, so this changes where the
                  snapshot lands and nothing else. It is what a scratch
                  collection wants: a floor to measure once, taken without
                  writing into the history a schedule is keeping.
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
  --database FILE database to read instead of the one the config names
  --window DUR    how far back the baseline sits, like 7d or 36h.
                  Defaults to the window in the config, which itself defaults to 7d.
  --against REF   compare against one named snapshot instead of whichever one a
                  window back. REF is a commit sha, abbreviated to at least 7
                  characters, or a snapshot id. Needs --repo, since a snapshot
                  belongs to one repo, and cannot be combined with --window,
                  which is an instruction for picking the thing this already
                  picked. It is what makes the movers and culprits sections
                  usable as a measuring stick rather than only as a weekly cron:
                  with it you get a delta and the packages behind it from two
                  snapshots and no history at all.
  --out FILE      write the report here instead of to stdout
  --format FMT    `+formatChoice+` (default `+string(report.FormatMarkdown)+`)
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
  --database FILE database to read instead of the one the config names
  --format FMT    `+formatChoice+` (default `+string(report.FormatMarkdown)+`)

history flags:
  --config FILE   config to read (default repo-metrics.yaml)
  --database FILE database to read instead of the one the config names
  --repo NAME     which repo to chart. Required: there is no sensible history
                  of nine repos at once, so this is an error rather than a
                  silent pick.
  --signal NAME   which measurement to chart. One of: `+signals+`.
                  Defaults to whichever coverage unit the repo actually
                  recorded: statement coverage from a Go profile, or
                  coverage_lines from an LCOV tracefile. It says on stderr when
                  it picks the second. Naming one explicitly is never
                  overridden, so --signal coverage on a repo that records lines
                  charts the empty series and says what it does record.
  --since DUR     how far back to look, like 90d or 26w. Default is 90d, which
                  is deliberately not the report's window: that one is the
                  offset to a baseline, and a week of history is not a trend.
  --format FMT    `+formatChoice+` (default `+string(report.FormatMarkdown)+`)

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
