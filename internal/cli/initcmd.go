package cli

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// starterConfig is what init writes.
//
// Every tunable in it is filled in from the config package's own defaults rather
// than written out again as a literal. Duplicating them means the day someone
// changes a default, the file this tool writes quietly disagrees with the tool
// that wrote it, and the new user's first config is already wrong.
var starterConfig = fmt.Sprintf(
	starterConfigFormat,
	config.DefaultDatabase,
	defaultWindowText(),
	config.DefaultMinStatements,
	config.DefaultMinRepoDelta,
	config.FormatGoCoverprofile,
	config.FormatGoTestJSON,
	config.FormatSARIF,
	config.FormatGoListModules,
	config.FormatGoCoverprofile,
)

// defaultWindowText renders the default reporting window for the config file.
//
// Hours rather than days, even though the window is really a week: durations in
// this file go through Go's time.ParseDuration, whose largest unit is the hour,
// so a literal "7d" here would make the starter config fail to load. The
// --window flag is parsed separately and does understand 7d.
func defaultWindowText() string {
	if config.DefaultWindow%time.Hour == 0 {
		return fmt.Sprintf("%dh", config.DefaultWindow/time.Hour)
	}
	// Not a whole number of hours, so let the Duration spell itself. Its String
	// round-trips through time.ParseDuration, which is all the file needs.
	return config.DefaultWindow.String()
}

// starterConfigFormat is the file with its tunables left as verbs, filled in by
// starterConfig above. The verbs are database, window, min_statements,
// min_repo_delta, and then the five format names, in that order.
//
// The format names come from the config package rather than being typed here
// for the same reason the tunables do: a starter config naming a format the
// validator rejects would make the tool's own output fail to load.
//
// The first repo entry is live and points at the current directory rather than
// at a placeholder like /path/to/your-repo. config.Load requires at least one
// repo and stats every path, so a starter file whose examples are all commented
// out, or all fictional, does not load: the first thing a new user would see is
// a validation error from a file the tool itself had just written. Keep one
// entry real when editing this.
const starterConfigFormat = `# repo-metrics config.
#
# This file is the only thing that knows which repos exist. The tool has no repo
# discovery and no forge API, so whatever generates this list is a separate job.

database: %s

# How far back to look for a baseline when reporting. --window overrides it.
#
# This is a week, written in hours. Durations in this file go through Go's
# time.ParseDuration, whose largest unit is the hour, so the "7d" the --window
# flag accepts is a load error here. 168h and 7d are the same thing.
window: %s

# Packages smaller than this stay out of the culprit ranking. A three-statement
# helper swinging from 0 to 100 percent is not news.
min_statements: %d

# How far a repo's coverage has to move, in percentage points, to lead the report.
min_repo_delta: %v

# Each repo carries a list of signals: one entry per thing to measure. A signal
# either runs a command and reads what it left behind, or reads an artifact
# something else produced. One command can feed two parsers, which is why the
# coverage entry below also names a stdout_format: ` + "`go test`" + ` yields the profile
# and the test counts from a single run.

repos:
  # This entry points at the current directory so the file works as written.
  # Point it somewhere you actually care about and give it a better name.
  - name: this-repo
    path: .
    # env reaches every signal below, and is also the environment the toolchain
    # fingerprint is taken under. A repo inside a go.work workspace measures
    # different code than the same repo with GOWORK=off, so this is part of what
    # makes two snapshots comparable.
    # env:
    #   GOWORK: "off"
    signals:
      - name: coverage
        command: ["go", "test", "./...", "-json", "-coverpkg=./...", "-coverprofile=coverage.out"]
        artifact: coverage.out
        artifact_format: %s
        stdout_format: %s
        timeout: 10m

      # Lint findings, read as SARIF so this entry is the same shape whatever
      # the repo is written in: golangci-lint, eslint, ruff, semgrep and clippy
      # all emit it. Commented out because it needs golangci-lint on your PATH,
      # and a starter config should not fail on a machine that lacks it.
      #
      # The two max-issues flags are not optional if you want a measurement.
      # golangci-lint caps output at 50 per linter and 3 per repeated message by
      # DEFAULT, so without them the number you track is a cap. Measured on a
      # real repo: 623 findings capped, 2760 uncapped, same run.
      # - name: lint
      #   command: ["golangci-lint", "run", "--output.sarif.path", "stdout",
      #             "--max-issues-per-linter", "0", "--max-same-issues", "0"]
      #   stdout_format: %s
      #   timeout: 5m

      # Dependency staleness: how many there are, how old the pins are, and how
      # many direct ones have a newer version. The -u is what makes the toolchain
      # look for updates. With GOPROXY off the update count is deliberately NOT
      # recorded, because an unchecked proxy and a fully current repo produce
      # identical output and only one of them is good news.
      - name: dependencies
        command: ["go", "list", "-m", "-u", "-json", "all"]
        stdout_format: %s
        timeout: 3m

  # Ingest only: no command, so it parses whatever CI already left on disk.
  # max_age is the freshness limit, past which the numbers get reported as stale
  # rather than presented as current. Uncomment and point it at a real checkout.
  # Note that every path here has to exist, or the config will not load.
  # - name: built-by-ci
  #   path: /srv/checkouts/built-by-ci
  #   signals:
  #     - name: coverage
  #       artifact: artifacts/coverage.out
  #       artifact_format: %s
  #       max_age: 24h
`

func runInit(args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("init", stderr)
	path := set.String("config", defaultConfigPath, "where to write the starter config")
	force := set.Bool("force", false, "overwrite an existing config instead of refusing")
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	// O_EXCL rather than stat-then-create: one syscall decides it, so there is
	// no window in which a file appears between the check and the write, and
	// nothing can clobber a config full of hand-edited repos.
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}

	f, err := os.OpenFile(*path, flags, 0o644)
	if err != nil {
		if errors.Is(err, iofs.ErrExist) {
			_, _ = fmt.Fprintf(stderr, "%s already exists. Pass --force to overwrite it.\n", *path)
			return fmt.Errorf("config %s already exists", *path)
		}
		_, _ = fmt.Fprintf(stderr, "could not create %s: %v\n", *path, err)
		return err
	}

	if _, err := io.WriteString(f, starterConfig); err != nil {
		_ = f.Close()
		_, _ = fmt.Fprintf(stderr, "could not write %s: %v\n", *path, err)
		return err
	}
	// Checked rather than deferred and discarded: a short write surfaces at
	// close, and swallowing it would report success for a truncated config.
	if err := f.Close(); err != nil {
		_, _ = fmt.Fprintf(stderr, "could not finish writing %s: %v\n", *path, err)
		return err
	}

	_, _ = fmt.Fprintf(stdout, "wrote %s\n", *path)
	_, _ = fmt.Fprint(stdout, "edit the repo list in it, then run: repo-metrics collect\n")
	return nil
}
