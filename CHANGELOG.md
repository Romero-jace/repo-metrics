# Changelog

One `## vX.Y.Z` section per release, newest at the top, so the next one goes
directly under this paragraph. The notes under a heading describe the release
rather than replaying the commits in it.

## v0.1.0

Not tagged yet. `repo-metrics version` reads the stamp the Go toolchain writes
into the binary rather than a constant kept in the source, so until the tag
exists a build from this checkout reports a `v0.0.0-` pseudo-version naming the
commit it came from. Once `v0.1.0` is tagged the same command reports
`repo-metrics v0.1.0` with nothing to bump anywhere, and the release date is the
only thing left to add here.

First public release. Nobody has run a previous one, so there is nothing to list
as changed or fixed. What follows is what the tool does and what it deliberately
does not do.

repo-metrics runs the commands a repo's config lists, one per signal, reads the
artifacts and the streams they leave behind, and stores counts in a SQLite file.
A repo routinely has several: a coverage run, a linter, a module listing.
`report` then says
what moved against the newest snapshot from a window back. The thing it is built
to refuse is a number nobody measured: a package that covered none of its
statements and a package nothing ever looked at are different answers, and
reporting both as zero is worse than admitting the second.

- **Thirteen signals**: coverage, tests, failing tests, packages without tests,
  skipped tests, total test time, lint findings, lint errors, suppressed
  findings, outdated dependencies, dependencies, median dependency age, and
  collection time. Five of them earn a column in the every-repo table; the other
  eight are still in the JSON payload.
- **Five commands, plus `version`.** `init` writes a starter config, `collect`
  runs each repo's steps and stores a snapshot, `report` says what moved, `repos`
  lists what the database holds for each configured repo, and `history` charts
  one signal for one repo over time.
- **Markdown and JSON** from `report`, `repos` and `history` alike. In the JSON a
  measurement nothing took is `null` rather than `0`, and in the markdown it is
  words rather than a figure: "not measured", "not collected", "not comparable",
  "no baseline yet". Which of those it says is itself the finding.
- **One binary and one file.** Two direct dependencies, `modernc.org/sqlite` and
  `github.com/goccy/go-yaml`. The SQLite driver is pure Go, so the binary builds
  and runs under `CGO_ENABLED=0`, and there is no daemon, no server and nothing
  running between collections. The database is a path in the config file.

Known limits, which are the reasons to hold off rather than reasons it is broken:

- **Coverage, test results and dependency staleness are Go-only today.** Three of
  the four formats it can read are Go toolchain output: `go-coverprofile`,
  `go-test-json` and `go-list-modules`. Pointing it at a repo in another language
  gets you the three lint signals and how long collecting them took.
- **Lint findings are the exception.** They are read as SARIF, which
  golangci-lint, eslint, ruff, semgrep and clippy all emit, so that half is not
  language specific and one parser covers every linter.
- **It has not been run against a fleet for long enough to have opinions about
  it.** The thresholds deciding which repos lead the report, and which signals
  are allowed to nominate one at all, are first guesses written down where they
  can be argued with. They are not calibrated against anything.
