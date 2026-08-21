package cli

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// starter is one ecosystem's starter config: the file init writes, and what it
// says about having chosen that one.
type starter struct {
	// ecosystem is what a person would call what was found.
	ecosystem string

	// evidence is the marker file that decided it, and is empty when nothing
	// decided anything. That is the difference between "this is a Python repo"
	// and "nothing here identified itself, so here is the Go starter", which
	// are different sentences and one of them is an invitation to edit.
	evidence string

	// body is the file to write.
	body string
}

// starterProbes is the detection table, in precedence order.
//
// Exactly one entry wins. A directory carrying markers for two ecosystems still
// gets ONE live repo entry: every repo entry needs a name of its own and all of
// them would point at this same directory, so a second one would be a second
// repo measuring the first one's code under another name, and every report from
// then on counts that checkout twice.
//
// The marker names are written here once and read twice, by the probe and by
// the sentence init prints when it found none of them. Written out a second
// time, that sentence is what goes stale the day a marker is added.
var starterProbes = []struct {
	ecosystem string
	files     []string

	// config takes the probed directory because one of them looks in it again:
	// the TypeScript starter reads which lockfile is actually there.
	config func(dir string) string
}{
	{"Go", []string{"go.mod"}, goStarterConfig},
	{"Python", []string{"pyproject.toml", "setup.cfg"}, pythonStarterConfig},
	{"TypeScript", []string{"package.json", "bun.lock"}, typescriptStarterConfig},
}

// chooseStarter picks the starter for dir, the directory the config is being
// written into.
//
// It stats the markers rather than reading them. They are evidence that
// somebody works in this language here, and nothing inside one of them would
// change which starter is right.
func chooseStarter(dir string) starter {
	for _, probe := range starterProbes {
		for _, name := range probe.files {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				continue
			}
			return starter{ecosystem: probe.ecosystem, evidence: name, body: probe.config(dir)}
		}
	}
	// Nothing identified the directory, so the first probe is the fallback and
	// evidence stays empty. That is what makes runInit say so rather than claim
	// a detection that did not happen.
	fallback := starterProbes[0]
	return starter{ecosystem: fallback.ecosystem, body: fallback.config(dir)}
}

// starterMarkers lists every file chooseStarter looks for, for the message it
// prints when none of them was there.
func starterMarkers() string {
	names := make([]string, 0, len(starterProbes))
	for _, probe := range starterProbes {
		names = append(names, probe.files...)
	}
	return strings.Join(names, ", ")
}

// describeDir names a directory the way a sentence wants it. filepath.Dir of a
// bare "repo-metrics.yaml" is ".", and "no go.mod in ." reads like a bug in the
// message rather than like an answer.
func describeDir(dir string) string {
	if dir == "." {
		return "the current directory"
	}
	return dir
}

// artifactPath names a file for a generated step to write, outside whatever
// repo that step is measuring.
//
// A relative artifact path is resolved against the repo being measured, so the
// obvious coverage.out is a file created INSIDE that checkout. Unless the repo
// gitignores it, the second collection onward finds an uncommitted change,
// which sets git_dirty and earns every later snapshot a warning saying its
// numbers belong to no commit. That is true of a genuinely dirty tree and false
// here, so the warning stops meaning anything exactly when it would be useful.
//
// The temp directory rather than a collection directory of this tool's own,
// which is what a scheduled fleet actually wants: nothing here creates
// directories, and neither does the -coverprofile flag, so a starter naming a
// directory nobody has made yet would be a starter whose live step fails on the
// first run. The temp directory is already there. The generated file says to
// repoint it, because this bakes in one machine's temp directory and a config
// committed to git is read on another.
//
// None of this decides whether the config loads. An artifact path is not
// checked at load time, since the file it names is usually something a later
// command produces.
func artifactPath(name string) string {
	return filepath.Join(os.TempDir(), "repo-metrics-"+name)
}

// defaultWindowText renders the default reporting window for the config file.
//
// Days, because the window really is a week and the file can now say so. This
// used to render hours, and explained itself: durations in the file went
// through Go's time.ParseDuration, whose largest unit is the hour, so a literal
// "7d" made the starter config fail to load. The file and the flags share one
// parser now, so the workaround goes with the asymmetry that forced it.
func defaultWindowText() string {
	const day = 24 * time.Hour
	if config.DefaultWindow%day == 0 {
		return fmt.Sprintf("%dd", config.DefaultWindow/day)
	}
	// Not a whole number of days, so let the Duration spell itself. Its String
	// round-trips through the parser, which is all the file needs.
	return config.DefaultWindow.String()
}

// starterHeader is the half of the file every starter shares: the tunables, and
// the two facts about paths that hold whatever language the repo is written in.
//
// Every tunable in it is filled in from the config package's own defaults rather
// than written out again as a literal. Duplicating them means the day someone
// changes a default, the file this tool writes quietly disagrees with the tool
// that wrote it, and the new user's first config is already wrong. Shared rather
// than repeated per ecosystem for the same reason one step further out: three
// copies of that Sprintf is three places to miss.
func starterHeader() string {
	return fmt.Sprintf(
		starterHeaderFormat,
		config.DefaultDatabase,
		defaultWindowText(),
		config.DefaultMinStatements,
		config.DefaultMinRepoDelta,
	)
}

// starterHeaderFormat is everything above the first repo entry. Its verbs are
// database, window, min_statements and min_repo_delta, in that order.
//
// Nothing here or in any repos section below may contain a literal percent
// sign, which would be read as a verb and shift every argument after it.
const starterHeaderFormat = `# repo-metrics config.
#
# This file is the only thing that knows which repos exist. The tool has no repo
# discovery and no forge API, so whatever generates this list is a separate job.
#
# init chose this starter by looking for a marker file in the directory it wrote
# to, and said on stderr which one it found. What it wrote measures one repo,
# the one you ran it in, and is a starting point rather than an answer.

database: %s

# How far back to look for a baseline when reporting. --window overrides it.
#
# Durations here read the same as they do on the flags: Go's duration syntax
# plus w for weeks and d for days, so 7d, 168h and 1w all mean this one week.
window: %s

# Packages smaller than this stay out of the culprit ranking. A three-statement
# helper swinging from 0 to 100 percent is not news.
min_statements: %d

# How far a repo's coverage has to move, in percentage points, to lead the report.
# One number for both coverage signals, statement and line alike.
min_repo_delta: %v

# Each repo carries a list of signals: one entry per thing to measure. A signal
# either runs a command and reads what it left behind, or reads an artifact
# something else produced. One command can feed two parsers, which is why the
# first entry below reads more than one thing out of a single test run.
#
# Every artifact a command writes is named here by an absolute path OUTSIDE the
# repo being measured, and that is deliberate. A relative artifact path resolves
# against the repo, so coverage.out is a file created inside that checkout, and
# anything left in a measured tree that nobody gitignored shows up from the
# second collection onward as an uncommitted change. That sets git_dirty, and
# every snapshot after it carries a warning saying its numbers belong to no
# commit, which is true of a genuinely dirty tree and false here.
#
# They point into this machine's temp directory because a starter has to work as
# written and nothing creates directories for you: a path under a collection
# directory you have not made yet is a step that fails on its first run. Before
# you schedule this, make one and repoint them, and give each repo entry
# filenames of its own. A committed config carrying one machine's temp directory
# is not portable, and two entries sharing one filename overwrite each other.
#
# What moving them does not cover is everything else a run leaves behind in a
# checkout you also work in: this config, the database beside it, and whatever
# the test runner writes for itself, such as .coverage or __pycache__. Keep the
# first two somewhere else once this is scheduled, and gitignore the third in
# each measured repo. Measured, not assumed: git_dirty is set from git status
# --porcelain, which counts an untracked file exactly as it counts an edited one.

repos:
`

// goStarterConfig is the Go starter, and also the fallback for a directory that
// identified itself as nothing. It ignores the probed directory: everything it
// needs to know is that a go.mod was there, or that nothing was.
//
// Adding an example to any repos section means adding BOTH a verb and its
// argument, at matching indexes. Miss the argument and every later format name
// shifts one slot: sarif lands as the dependencies step's stdout_format. It
// compiles, and the file it writes still loads, because every value is a valid
// format name and only the pairing is wrong. TestInitPinsEachFormatNameToItsKey
// and TestEveryStarterPinsEachFormatNameToItsKey are what catch that;
// config.Load cannot.
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
// entry real in every one of these.
func goStarterConfig(string) string {
	cover := artifactPath("this-repo-coverage.out")
	return starterHeader() + fmt.Sprintf(
		goStarterRepos,
		"-coverprofile="+cover,
		cover,
		config.FormatGoCoverprofile,
		config.FormatGoTestJSON,
		config.FormatSARIF,
		config.FormatGoListModules,
		config.FormatGoCoverprofile,
		config.FormatJUnitXML,
		config.FormatLCOV,
		config.FormatSARIF,
	)
}

// goStarterRepos is the Go repo list. Its verbs are the coverprofile flag, the
// profile's path, and then the format names, in that order.
const goStarterRepos = `  # This entry points at the current directory so the file works as written.
  # Point it somewhere you actually care about and give it a better name.
  #
  # A dot is the directory repo-metrics runs in, which is the directory init
  # probed as long as you let --config default. Passing --config somewhere else
  # separates the two, and this path follows the tool rather than the config.
  #
  # Its two live signals run the Go toolchain, so a directory that is not a Go
  # module still loads and collect still exits 0, and what you get is a PARTIAL
  # snapshot carrying nothing you can use: the coverage step runs, finds no
  # instrumented packages and records none, the dependency step cannot find a
  # go.mod and records nothing, and the whole thing arrives as a wall of
  # warnings rather than as an error. If that is where you are, init said so on
  # stderr, and the commented entries at the bottom of this file are the shape
  # to copy. Note their fingerprint lines, which a repo running no Go format
  # needs and a Go repo gets for free.
  - name: this-repo
    path: .
    # env reaches every signal below, and is also the environment the toolchain
    # fingerprint is taken under. A repo inside a go.work workspace measures
    # different code than the same repo with GOWORK=off, so this is part of what
    # makes two snapshots comparable.
    # env:
    #   GOWORK: "off"
    signals:
      # -count=1 defeats the Go test cache, on purpose. Without it a second
      # collection with nothing changed is served from the cache, and the
      # per-package durations test_time sums collapse: three packages of a real
      # repo went from 2.851s to 0.017s across two runs with no edit between
      # them. Coverage is unharmed either way, because -coverprofile is rewritten
      # even on a fully cached run, so test_time is the only thing at stake.
      #
      # The cost is that every collection now pays for a whole test run, which on
      # a large repo is minutes. That is the trade taken here: collection is
      # scheduled rather than interactive, and a duration nobody actually spent
      # is the kind of number this tool would rather not record at all. Delete
      # the flag if you would sooner have cheap collections and read test_time as
      # noise.
      - name: coverage
        command: ["go", "test", "./...", "-json", "-count=1", "-coverpkg=./...", %q]
        artifact: %q
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
  #       artifact: /srv/repo-metrics/artifacts/built-by-ci/coverage.out
  #       artifact_format: %s
  #       max_age: 24h

  # A repo that is not written in Go. Lint needs no parser of its own, because
  # SARIF is not tied to a language, and tests and coverage are read from the
  # formats every runner emits: JUnit XML and LCOV. Run init inside one of these
  # and it writes this shape live instead of commented.
  #
  # Note the shape of the test step. One pytest run writes BOTH files, so both
  # are listed under artifacts and each is held to the same check: it has to have
  # been written by this run. Reading the second one in a separate step with no
  # command also works, and quietly swaps that check for a 24 hour age limit, so
  # a profile left over from yesterday is accepted as today's measurement.
  # - name: python-service
  #   path: /srv/checkouts/python-service
  #   fingerprint: ["python3", "--version"]
  #   signals:
  #     - name: tests
  #       command: ["pytest", "--junitxml=/srv/repo-metrics/artifacts/python-service/junit.xml",
  #                 "--cov=.", "--cov-report=lcov:/srv/repo-metrics/artifacts/python-service/coverage.lcov"]
  #       artifacts:
  #         - {path: /srv/repo-metrics/artifacts/python-service/junit.xml, format: %s}
  #         - {path: /srv/repo-metrics/artifacts/python-service/coverage.lcov, format: %s}
  #       timeout: 10m
  #
  #     - name: lint
  #       command: ["ruff", "check", "--output-format", "sarif"]
  #       stdout_format: %s
  #       timeout: 5m
`

// pythonStarterConfig is the starter for a directory carrying a pyproject.toml
// or a setup.cfg. It ignores the probed directory: which of the two markers was
// there says nothing about how the suite is run.
func pythonStarterConfig(string) string {
	junit := artifactPath("this-repo-junit.xml")
	cover := artifactPath("this-repo-coverage.lcov")
	return starterHeader() + fmt.Sprintf(
		pythonStarterRepos,
		"--junitxml="+junit,
		"--cov-report=lcov:"+cover,
		junit,
		config.FormatJUnitXML,
		cover,
		config.FormatLCOV,
		config.FormatSARIF,
		config.FormatLCOV,
	)
}

// pythonStarterRepos is the Python repo list. Its verbs are the two pytest
// output flags, then each artifact path paired with its format, then the lint
// format, then the ingest example's format.
const pythonStarterRepos = `  # This entry points at the current directory so the file works as written.
  # Point it somewhere you actually care about and give it a better name.
  #
  # A dot is the directory repo-metrics runs in, which is the directory init
  # probed as long as you let --config default. Passing --config somewhere else
  # separates the two, and this path follows the tool rather than the config.
  #
  # Both live steps need their tool on your PATH, pytest and ruff. A signal is
  # the unit of failure, so a missing one costs its own measurements and leaves
  # the rest of the snapshot alone: you get a partial snapshot that says which
  # step went wrong, rather than a failed run or a confident zero.
  - name: this-repo
    path: .

    # No step here reads a format the Go toolchain produces, so without this
    # nothing would identify what these numbers were measured under, and a
    # runtime upgrade between two snapshots would go unflagged. A repo running a
    # Go format is fingerprinted with go env without being asked; every other
    # repo says here what to ask instead, and the probe's trimmed stdout becomes
    # the snapshot's fingerprint. Point it at the interpreter that actually runs
    # the suite, which under a virtualenv is not the one on your PATH.
    fingerprint: ["python3", "--version"]

    # env reaches every signal below, and is also the environment the
    # fingerprint is taken under, so a suite measured inside a virtualenv and
    # the same suite measured outside one are not claimed to be comparable.
    # env:
    #   VIRTUAL_ENV: /srv/venvs/this-repo

    signals:
      # One pytest run writes BOTH files, so both are listed under artifacts
      # rather than split across two steps. That is not only tidier: every
      # artifact in this list is held to the same check, which is that this run
      # wrote it. A second step with no command would read the coverage file
      # perfectly well and quietly swap that check for a 24 hour age limit, so a
      # profile left over from yesterday would be accepted as today's
      # measurement.
      #
      # Two things about the numbers. pytest counts every parametrize case as
      # its own test, where go test counts top-level functions and folds
      # subtests in, so counts from two languages are not comparable to each
      # other. And LCOV counts LINES where a Go profile counts statements: they
      # are stored and reported as separate signals and never summed.
      #
      # What a JUnit document cannot answer is how many source files carry no
      # tests at all, because it lists the suites that RAN. That signal comes
      # back unmeasured rather than zero, which is the distinction this whole
      # tool is built on.
      - name: tests
        command: ["pytest", %q, "--cov=.", %q]
        artifacts:
          - {path: %q, format: %s}
          - {path: %q, format: %s}
        timeout: 15m

      # Lint findings, read as SARIF so this entry is the same shape whatever
      # the repo is written in: ruff, golangci-lint, eslint, semgrep and clippy
      # all emit it. ruff exits 1 when it finds something, and that is read as
      # findings rather than as failure. It comes from the format rather than
      # from anything set here, so there is nothing to configure for it.
      - name: lint
        command: ["ruff", "check", "--output-format", "sarif"]
        stdout_format: %s
        timeout: 5m

  # Ingest only: no command, so it parses whatever CI already left on disk.
  # max_age is the freshness limit, past which the numbers get reported as stale
  # rather than presented as current. Uncomment and point it at a real checkout.
  # Note that every path here has to exist, or the config will not load.
  # - name: built-by-ci
  #   path: /srv/checkouts/built-by-ci
  #   fingerprint: ["python3", "--version"]
  #   signals:
  #     - name: coverage
  #       artifact: /srv/repo-metrics/artifacts/built-by-ci/coverage.lcov
  #       artifact_format: %s
  #       max_age: 24h
`

// typescriptStarterConfig is the starter for a directory carrying a
// package.json or a bun.lock.
//
// It is the one starter that looks at the directory again, for the lockfile.
func typescriptStarterConfig(dir string) string {
	junit := artifactPath("this-repo-junit.xml")
	coverDir := artifactPath("this-repo-coverage")
	lock := chooseLockfile(dir)
	return starterHeader() + fmt.Sprintf(
		typescriptStarterRepos,
		"--outputFile="+junit,
		"--coverage.reportsDirectory="+coverDir,
		junit,
		config.FormatJUnitXML,
		// The one artifact whose path is not what the flag says. The lcov
		// reporter is told a directory and writes lcov.info into it.
		filepath.Join(coverDir, "lcov.info"),
		config.FormatLCOV,
		config.FormatSARIF,
		lock.name,
		lock.format,
	)
}

// lockfileChoice is a lockfile to read and the format that reads it.
//
// One value rather than two, because the pairing is the part that has to stay
// true: npm-lockfile parses nothing out of a bun.lock, and both formats write
// deps.total at repo scope, so a config naming one of each is rejected at load
// rather than discovered when the second step fails every night.
type lockfileChoice struct {
	name   string
	format config.Format
}

// chooseLockfile picks which lockfile the dependencies step reads: whichever is
// actually in dir, falling back to npm's, since npm is what writes one for a
// package.json nobody chose a package manager for.
//
// The count comes from reading the file rather than from asking a package
// manager, which is the whole point of these two formats. bun outdated and npm
// outdated resolve through the installed tree, so on a checkout where nothing
// has been installed they answer exactly as they do for a repo with nothing
// outdated.
func chooseLockfile(dir string) lockfileChoice {
	if _, err := os.Stat(filepath.Join(dir, "bun.lock")); err == nil {
		return lockfileChoice{name: "bun.lock", format: config.FormatBunLockfile}
	}
	return lockfileChoice{name: "package-lock.json", format: config.FormatNPMLockfile}
}

// typescriptStarterRepos is the TypeScript repo list. Its verbs are the two
// vitest output flags, then each artifact path paired with its format, then the
// lint format, then the lockfile paired with the format that reads it.
const typescriptStarterRepos = `  # This entry points at the current directory so the file works as written.
  # Point it somewhere you actually care about and give it a better name.
  #
  # A dot is the directory repo-metrics runs in, which is the directory init
  # probed as long as you let --config default. Passing --config somewhere else
  # separates the two, and this path follows the tool rather than the config.
  #
  # The two live commands need vitest and eslint on your PATH. A signal is the
  # unit of failure, so a missing one costs its own measurements and leaves the
  # rest of the snapshot alone: you get a partial snapshot that says which step
  # went wrong, rather than a failed run or a confident zero.
  - name: this-repo
    path: .

    # No step here reads a format the Go toolchain produces, so without this
    # nothing would identify what these numbers were measured under, and a
    # runtime upgrade between two snapshots would go unflagged. A repo running a
    # Go format is fingerprinted with go env without being asked; every other
    # repo says here what to ask instead, and the probe's trimmed stdout becomes
    # the snapshot's fingerprint.
    fingerprint: ["node", "--version"]

    signals:
      # One vitest run writes BOTH files, so both are listed under artifacts
      # rather than split across two steps. Every artifact in this list is held
      # to the same check, which is that this run wrote it. A second step with
      # no command would read the coverage file perfectly well and quietly swap
      # that check for a 24 hour age limit, so a profile left over from
      # yesterday would be accepted as today's measurement.
      #
      # The lcov reporter is given a DIRECTORY and writes lcov.info inside it,
      # which is why the flag and the artifact below are not the same string.
      # They still have to agree: nothing checks that they do.
      #
      # Set coverage.include in your vitest config, and check that it is set.
      # Without it, files with no tests are left out of the report entirely, so
      # the percentage is computed only over code that already had tests and
      # reads high for the wrong reason. That is the same trap -coverpkg guards
      # against one ecosystem over. Verified against a real repo: a full
      # 1,964-test run with the option unset wrote a ZERO-BYTE lcov.info and
      # exited 0. repo-metrics records nothing for that rather than a coverage
      # of zero, and says so, but the measurement is still lost.
      #
      # LCOV counts LINES where a Go profile counts statements. They are stored
      # and reported as separate signals and never summed. And what a JUnit
      # document cannot answer is how many source files carry no tests at all,
      # because it lists the suites that RAN, so that signal comes back
      # unmeasured rather than zero.
      - name: tests
        command: ["vitest", "run", "--reporter=junit", %q, "--coverage", "--coverage.reporter=lcov", %q]
        artifacts:
          - {path: %q, format: %s}
          - {path: %q, format: %s}
        timeout: 20m

      # Lint findings, read as SARIF so this entry is the same shape whatever
      # the repo is written in: eslint, golangci-lint, ruff, semgrep and clippy
      # all emit it. eslint exits 1 when it finds something, and that is read as
      # findings rather than as failure. It comes from the format rather than
      # from anything set here, so there is nothing to configure for it.
      - name: lint
        command: ["eslint", ".", "--format", "@microsoft/sarif"]
        stdout_format: %s
        timeout: 5m

      # The dependency count, read from the lockfile with no command at all, so
      # this entry is also what ingest mode looks like. It is deliberately not
      # bun outdated or npm outdated: those resolve through the installed tree,
      # so on a checkout where nothing has been installed they return an empty
      # result and exit 0, which is what a repo with nothing outdated returns
      # too. The lockfile either lists the packages or it is not a lockfile.
      #
      # This path is relative on purpose, unlike the artifacts above. It is a
      # committed file being read rather than a file being written, so it
      # belongs in the checkout and leaves no uncommitted change behind.
      #
      # What it cannot record is dependency_age and outdated_dependencies. No
      # JavaScript lockfile carries a publish time, and the update question needs
      # a registry. Both come back null rather than approximated.
      #
      # max_age is the freshness limit in ingest mode, and it wants to be
      # generous here: a repo that has not touched its dependencies in a month
      # has a month-old lockfile, and that is the healthy case rather than a
      # stale one.
      - name: dependencies
        artifact: %s
        artifact_format: %s
        max_age: 8760h
`

func runInit(args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("init", stderr)
	path := set.String("config", defaultConfigPath, "where to write the starter config")
	force := set.Bool("force", false, "overwrite an existing config instead of refusing")
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	// Which starter comes from the directory the config lands in rather than
	// being the Go one every time. A Python or TypeScript workspace handed the
	// Go starter gets a file that loads, collects, exits 0, and records
	// nothing: the coverage step finds no instrumented packages and the
	// dependency step finds no go.mod. That is a working config that measures
	// nothing, which is the failure this tool exists to refuse, arriving
	// through the config the tool itself wrote.
	dir := filepath.Dir(*path)
	chosen := chooseStarter(dir)

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

	if _, err := io.WriteString(f, chosen.body); err != nil {
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

	// Said after the write rather than before it, so a run that refused to
	// clobber an existing config does not also report a detection it never
	// acted on.
	if chosen.evidence == "" {
		_, _ = fmt.Fprintf(stderr, "no %s in %s, so this is the %s starter: edit its signals to match what this repo runs\n",
			starterMarkers(), describeDir(dir), chosen.ecosystem)
	} else {
		_, _ = fmt.Fprintf(stderr, "detected a %s repo: %s in %s\n",
			chosen.ecosystem, chosen.evidence, describeDir(dir))
	}

	_, _ = fmt.Fprintf(stdout, "wrote %s\n", *path)
	_, _ = fmt.Fprint(stdout, "edit the repo list in it, then run: repo-metrics collect\n")
	return nil
}
