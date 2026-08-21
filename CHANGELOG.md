# Changelog

One `## vX.Y.Z` section per release, newest at the top, so the next one goes
directly under this paragraph. The notes under a heading describe the release
rather than replaying the commits in it.

## v0.2.0

The first release shaped by running this against a fleet rather than by
reasoning about one. Ten mixed Go, Python and TypeScript repos, used to set
coverage floors, which is a different job from the weekly cron the tool was
built for and found the places where only the cron had been catered for.

**Breaking, and the reason for the minor bump.** Two changes need a look before
upgrading.

- `repos --format json` spells its coverage rate `value` rather than `pct`, so
  one consumer walks a measurement out of any payload without knowing which it is
  holding. Any v0.1.0 script reading `repos[].coverage.pct` reads null now.
- The database is schema version 2. A v0.2.0 file cannot be read by a v0.1.0
  binary, which the version guard reports rather than misreading. Upgrading is
  automatic and one-way; the wrapper script backs the database up before every
  run for the reason that has always applied, which is that snapshots cannot be
  re-collected.

**Coverage is findable in whichever unit a repo records.** `history` defaulted to
statement coverage, which is Go's, so a repo measured through an LCOV tracefile
answered with a run of nulls on snapshots that had collected perfectly well, and
you had to already know to ask for `coverage_lines`. It now charts what the repo
records and says so on stderr; naming a signal explicitly is never overridden.
`repos` publishes and prints both units, and the `collect` progress line names
the unit it measured. Its third coverage-less phrase is now `not measured`
rather than `no coverage`, which is the word `history` already used for that
state.

**A baseline you name.** `report --against <sha|snapshot-id>` compares against
one chosen snapshot instead of whichever one a window back, so two collections
are enough for movers, culprits and package churn. It needs `--repo`, refuses to
run beside `--window`, and is exempt from the rule that stops a long-lapsed repo
leading the report, since that rule infers nobody was watching from a large gap
and a baseline somebody chose is not that.

**Collection matches how people re-measure.** `--jobs N` collects several repos
at once, buffering each repo's output into a block so the table does not shred;
the default is 1 and its output is byte for byte what it was. `--repo` repeats.
`--signal` narrows to named steps, and says on every run that the snapshot it
writes is narrower than the config and will read as unmeasured for everything
skipped, which is not recoverable.

**A red suite stops looking like a broken collection.** Snapshots record whether
a run was degraded, so `report --section problems` says which of the two each row
is: numbers taken under protest, or nothing collected. The column is nullable and
null on every snapshot written before this release, because nothing recorded it
then.

**`init` writes the starter for the language it finds**, probing for `go.mod`,
`pyproject.toml` or `setup.cfg`, and `package.json` or `bun.lock`, and saying
which it detected. The generated configs write their artifacts outside the
checkout, so a second collection does not set `git_dirty` on a repo you do not
own.

**Smaller things.** `--database PATH` on every command that opens one, so a
scratch collection does not need a second config. `report --fail-on
problems|movers` exits 1 on what it finds, since `report` otherwise exits 0
whatever it found. A new `show` command prints everything one repo's newest
snapshot recorded, with the counts behind the coverage rates. `history` points
carry `covered` and `total`. The LCOV parser names Cobertura and JUnit XML when
one is handed to it, rather than only saying the input is not a tracefile.
`--format` help reads as a choice rather than as a list you could pass both of.

Known limits are unchanged from v0.1.0 and are listed below, with one addition:
the thresholds are still first guesses. This release is the first fleet feedback
acted on, not the calibration.

## v0.1.0

Tagged 2026-08-18. `repo-metrics version` reads the stamp the Go toolchain
writes into the binary rather than a constant kept in the source, so tagging was
the whole release: a build from the tag reports `repo-metrics v0.1.0` with
nothing to bump anywhere and no version literal left in the tree that could
disagree with it. A build from an untagged commit still reports a `v0.0.0-`
pseudo-version naming the commit it came from, which is the same mechanism
telling the truth about a checkout that is not a release.

First release, and the repository is public, so the audience is anybody who
finds it rather than a list of people who were handed a checkout.
Nobody has run a previous version, so there is nothing to list as changed or
fixed. What follows is what the tool does and what it deliberately does not do.

repo-metrics runs the commands a repo's config lists, one per signal, reads the
artifacts and the streams they leave behind, and stores counts in a SQLite file.
A repo routinely has several: a coverage run, a linter, a module listing.
`report` then says
what moved against the newest snapshot from a window back. The thing it is built
to refuse is a number nobody measured: a package that covered none of its
statements and a package nothing ever looked at are different answers, and
reporting both as zero is worse than admitting the second.

- **Fourteen signals**: statement coverage, line coverage, tests, failing tests,
  packages without tests, skipped tests, total test time, lint findings, lint
  errors, suppressed findings, outdated dependencies, dependencies, median
  dependency age, and collection time. Six of them earn a column in the
  every-repo table; the other eight are still in the JSON payload.
- **Five commands, plus `version`.** `init` writes a starter config, `collect`
  runs each repo's steps and stores a snapshot, `report` says what moved, `repos`
  lists what the database holds for each configured repo, and `history` charts
  one signal for one repo over time.
- **Markdown and JSON** from `report`, `repos` and `history` alike. In the JSON a
  measurement nothing took is `null` rather than `0`, and in the markdown it is
  words rather than a figure: "not measured", "not collected", "not comparable",
  "no baseline yet". Which of those it says is itself the finding.
- **Eight formats, three of them language-neutral.** `sarif` for lint findings,
  `junit-xml` for test results, `lcov` for coverage, `npm-lockfile` and
  `bun-lockfile` for dependency counts, plus `go-coverprofile`,
  `go-test-json` and `go-list-modules`. Adding one is a table entry and a parser;
  a step whose format validates but cannot be parsed is impossible by
  construction, because the two lists are pinned to each other.
- **Several artifacts from one command.** `artifacts:` is a list of path-and-format
  pairs, so `pytest --junitxml=… --cov-report=lcov:…` is one step rather than two,
  and every file it names is held to the same check: this run has to have written
  it. `artifact:` with `artifact_format:` remains the one-file shorthand.
- **A toolchain fingerprint that refuses to guess.** Snapshots record what they
  were measured under, derived from the formats a repo declares, with
  `fingerprint:` for a repo the tool cannot work out. A repo where nothing
  identified it records that, rather than the ambient Go version, and the report
  says the comparison could not be made instead of implying it came back equal.
- **Unknown config keys are rejected.** A key this tool does not read fails the
  load at every level of the file, including inside a `signals:` entry. A typo
  that loads clean and does nothing is the same failure everything else here is
  built to refuse.
- **One binary and one file.** Two direct dependencies, `modernc.org/sqlite` and
  `github.com/goccy/go-yaml`. The SQLite driver is pure Go, so the binary builds
  and runs under `CGO_ENABLED=0`, and there is no daemon, no server and nothing
  running between collections. The database is a path in the config file.

Known limits, which are the reasons to hold off rather than reasons it is broken:

- **Dependency age and the outdated count are Go-only.** `go list -m -json`
  answers all three questions from one stream. Elsewhere only the count is
  recoverable: no JavaScript lockfile records a publish time, and knowing whether
  a newer version exists needs a registry. `uv.lock` is the one lockfile in either
  ecosystem that does carry publish timestamps, so Python age is reachable — and
  deliberately not taken. It is TOML, which means a third direct dependency for a
  signal that is charted and never leads a report, and the two-dependency property
  above is worth more than one more number on one ecosystem.
- **The count comes from the lockfile, never from `npm outdated`.** That command
  resolves through the installed tree, so on a checkout where nothing has been
  installed it returns an empty result and exits 0 — the same shape as the
  `GOPROXY=off` trap on the Go side, where a repo nobody checked and a repo with
  nothing outdated produce identical output.
- **`lint_suppressed` is unmeasured unless the linter reports suppressions.** A
  SARIF `suppressions` array is the only way a document can say a finding was
  raised and then silenced, and almost nothing writes one: `//nolint`, `# noqa`,
  `# rubocop:disable`, `@Suppress` and `#[allow]` all delete the finding instead.
  Only ESLint via `@microsoft/eslint-formatter-sarif` and Roslyn emit it. So no row
  is stored rather than a zero, which is the one count here where zero is never a
  measurement — and it costs the ability to report a repo clearing its last
  suppression, because that is the same document as a linter that never had
  anything to say.
- **`untested_packages` is Go-only, and permanently.** It is not a missing parser:
  a JUnit document lists the suites that ran and cannot reveal a source file
  nobody wrote a test for. Answering it elsewhere is a different measurement.
- **Coverage comes in two units that are never mixed.** A Go profile counts
  statements, LCOV counts lines, and several statements on one source line
  collapse to one line. They are separate signals under separate keys, so a repo
  percentage summing both denominators is impossible rather than discouraged.
- **Three of the eight formats are not tied to a language.** SARIF for lint
  findings, JUnit XML for test results, LCOV for coverage — so pytest, vitest,
  jest, coverage.py, istanbul, eslint and ruff are all read by one parser each,
  and a Python or TypeScript repo records the same signals a Go one does apart
  from the ones above.
- **It has not been run against a fleet for long enough to have opinions about
  it.** The thresholds deciding which repos lead the report, and which signals
  are allowed to nominate one at all, are first guesses written down where they
  can be argued with. They are not calibrated against anything.
