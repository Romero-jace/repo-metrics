# AGENTS.md

Notes for an AI coding agent. [CONTRIBUTING.md](CONTRIBUTING.md) is the real
document and this one does not repeat it. This is a router, plus the things a
model gets wrong by reflex rather than by ignorance.

## The one idea

repo-metrics collects code-quality signals across a bunch of Git repos, keeps
history in SQLite, and says what got worse. One decision explains most of the
rest of the design: it refuses to report a number nobody measured. A package
that covered none of its statements and a package nothing ever looked at are
different answers, and publishing both as `0` is the bug this codebase is
organized against. That is why a parser emits its marker key even when the count
is zero, why the database stores counts rather than percentages, and why every
**measurement** on the JSON wire sits inside a group that can be null.

Measurement is the load-bearing word. An input, meaning something the caller
asked for rather than something the tool went and found, is deliberately bare and
ungrouped: `window_days` and `since_days` are what you requested, and nothing
measured them or can fail to. The censuses enforce that split in both directions,
so moving an input into a nullable group fails the build exactly as putting a
measurement outside one does. Make a change without holding this in your head and
it will look reasonable and be wrong.

## Read this before touching that

| Doing | Read first (`##` names are CONTRIBUTING.md headings) |
|---|---|
| Adding or changing a reported signal | `## Adding a signal`, then `## The marker contract` and `## The other half: what the scope names`. Code: `internal/collect/keys.go`, `internal/collect/parse.go`, `internal/delta/signal.go`, `internal/report/view.go`. |
| Adding a metric key | `## Adding a metric`. Counts, never a precomputed percentage. |
| Adding a config format | `## Adding a format`. Both halves land together: `internal/config/format.go` and `internal/collect/parse.go`. |
| Getting the extractor right | `### The extractor and the marker answer different questions`, inside `## Adding a signal`. Read it even if you think you know: it is the one field the suite cannot catch you on, and getting it wrong publishes a confident zero. |
| Changing a JSON payload | The closing paragraphs of `## Adding a signal`, after its `###` subsection. Three payloads, four censuses: `internal/report/view.go` guarded by `degraded_test.go`, `internal/report/repos.go` and `history.go` guarded by `payload_census_test.go`. |
| Changing CLI output | No CONTRIBUTING section for this. Flag order is in the package doc at `internal/cli/root.go`; which stream carries what is in the doc comment on `Run` in the same file, and at length under `## Streams and exit codes` in the README. Format and section names are single lists in `internal/report/format.go` and `section.go`. Markdown lives in `internal/report/report.md.tmpl` and `history.md.tmpl`. |

## The gate

`make check` is build, vet, test, lint, in that order, and the Makefile exports
`GOWORK=off` itself. A green one is necessary and not sufficient: CI also runs
`go test ./... -race`, `go mod tidy -diff`, and two cross-check tests that skip
locally because `make test` has no artifacts to hand them.
`## Build and run the gate` has the local recipe for the race run and the two
cross-checks. For the third, run `GOWORK=off go mod tidy -diff`, which reports
without rewriting; `make tidy` is the fix for a red one, not the check.

Two more traps:

- **Outside `make`, every toolchain command needs `GOWORK=off`.** This module is
  commonly checked out beside a `go.work` that does not list it, and the failure
  reads like a broken checkout: `directory prefix . does not contain modules
  listed in go.work`. Do not fix it by joining that workspace. (`## GOWORK=off`)
- **golangci-lint is pinned to v2.12.2.** `.golangci.yml` is a v2 config, and v1
  rejects it with a message about your Go version being too low, which sends you
  off debugging entirely the wrong thing. Check `golangci-lint --version` before
  anything else. (`## Linting`)

## What models get wrong by reflex

`## House style` in CONTRIBUTING is the list, and it is short: standard library
first, external `_test` packages, every error checked, US spelling, no em dashes,
`//nolint` with a linter and a reason. Read it once rather than take a summary of
it from here. Three of those bite a model harder than a person, so they get a
line each.

- **Paste real command output. Do not assert that tests pass.** CONTRIBUTING
  asks for the output in the pull request, or the commit message when there is no
  pull request, and it matters more here than in most repos: a tool whose whole
  thesis is "do not report a measurement you did not take" cannot ship a claim
  nobody ran.
- **No em dashes.** Default model prose is full of them, and three tests already
  fail on one: `TestNoEmDashes` (`internal/report/report_test.go`) for rendered
  report markdown, `TestUsageHasNoEmDash` (`internal/cli/cli_test.go`) for usage
  text, `TestVersionIsUsableInABugReport` (`internal/cli/version_test.go`) for
  `--version`. Those three cover some of what a user reads, not all of it: the
  `repos` and `history` renderings have no such test, and neither do these
  documents or the code comments. Use commas, colons, or two sentences.
- **Reach for a dependency and stop.** There are exactly two and both are
  load-bearing. No cobra, no testify, no assertion library. This is the rule a
  model breaks fastest, because the reflex is to import the thing that makes the
  test read nicely.

## About the tests here

They are more opinionated than most. Many carry a control case with a comment
above it naming what would still pass without it, because something did: see
`internal/collect/collect_test.go` ("Confirmed by mutation") and the header of
`internal/report/payload_census_test.go`, where a new wire field "passed the
build, the vet, the whole suite and the linter." Hold new tests to that. One that
passes with and without the change it guards is worse than none, because it reads
like coverage.
