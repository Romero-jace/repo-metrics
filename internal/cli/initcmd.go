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

// starterConfigFormat is the file with its four tunables left as verbs, filled
// in by starterConfig above. The verbs are database, window, min_statements,
// and min_repo_delta, in that order.
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

repos:
  # Mode one: run a command, then read what it wrote. This entry points at the
  # current directory so the file works as written. Point it somewhere you
  # actually care about and give it a better name.
  - name: this-repo
    path: .
    coverprofile: coverage.out
    command: ["go", "test", "./...", "-json", "-coverpkg=./...", "-coverprofile=coverage.out"]
    stdout_format: go-test-json
    timeout: 10m

  # Mode two, ingest only: no command, so it parses whatever CI already left on
  # disk. max_age is the freshness limit, past which the numbers get reported as
  # stale rather than presented as current. Uncomment and point it at a real
  # checkout. Note that every path here has to exist, or the config will not load.
  # - name: built-by-ci
  #   path: /srv/checkouts/built-by-ci
  #   coverprofile: artifacts/coverage.out
  #   max_age: 24h
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
