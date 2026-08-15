# repo-metrics

Tracks code-quality metrics across a bunch of Git repos at once, keeps history in
SQLite, and tells you what got worse this week.

Coverage tools are per-repo and live in a tab nobody opens. Nobody reads a
dashboard daily. Everybody reads "coverage in repo X dropped 4 points, package Y
is why." That report is the whole point of this thing.

Self-hosted, one binary, no account, no SaaS.

## Status

The core is built: the Go collector, SQLite storage, delta computation against a
baseline snapshot, and the markdown and JSON report. It has not been run against
a fleet for long enough to have opinions about it yet, and the only collector is
the Go one, so treat it as working but young.

## Install

```sh
go install github.com/Romero-jace/repo-metrics/cmd/repo-metrics@latest
```

Or from a checkout, which is what you want if you plan to change anything:

```sh
make build     # ./bin/repo-metrics
```

Two dependencies, both load-bearing: `modernc.org/sqlite`, which is a pure-Go
SQLite so there is no CGO and no system library to install, and
`github.com/goccy/go-yaml` for the config.

## Getting started

```sh
repo-metrics init                 # writes a commented repo-metrics.yaml
$EDITOR repo-metrics.yaml         # point it at repos you actually have
repo-metrics collect              # one pass over all of them, then exits
repo-metrics report               # markdown to stdout
```

`init` writes a config that already works: the first repo entry points at the
current directory, so you can run the other three commands immediately and see
real output before editing anything. Every path in the file is checked when it
loads, so a config full of `/path/to/your-repo` placeholders is a load error
rather than a mystery later.

The report needs two snapshots to compare, so the first one has nothing to say
about deltas. Collect again tomorrow and it will.

## The four commands

| command | what it is for |
|---|---|
| `init` | write a starter config |
| `collect` | run each repo's command, parse what it produced, store a snapshot |
| `report` | compare the newest snapshot against one from a window ago |
| `repos` | list every configured repo and when it was last collected |

`repos` is the one that is not obvious. It answers "did the cron job actually
run", and it lists repos the database has never heard of alongside the ones it
has, because a repo that quietly stopped being collected looks exactly like a
repo that has not changed.

```
REPO         LAST COLLECTED        STATUS           COVERAGE
api          2026-08-15 23:47 UTC  ok               80.0%
worker       2026-08-15 23:47 UTC  ok               80.0%
old-service  never                 never collected  -
```

## Configuring it

```yaml
database: ./repo-metrics.db
repos:
  - name: my-service
    path: /path/to/my-service
    coverprofile: coverage.out
    command: ["go", "test", "./...", "-json", "-coverpkg=./...", "-coverprofile=coverage.out"]
    stdout_format: go-test-json
    timeout: 10m

  - name: built-in-ci
    path: /srv/checkouts/built-in-ci
    coverprofile: artifacts/coverage.out
    max_age: 24h
    # no command, so it just reads what CI already wrote
```

There is a fully commented version at
[`examples/repo-metrics.yaml`](examples/repo-metrics.yaml), with every field
annotated with what it does and what happens if you leave it out.

`--window` takes Go's duration syntax plus a `d` suffix for days, so `7d`,
`168h`, and `1d12h` all work. The duration fields in the config file are
stricter: `window:`, `timeout:`, and `max_age:` go through Go's
`time.ParseDuration`, whose largest unit is the hour, so a week there has to be
written `168h` and `7d` is a load error.

## Running it on a schedule

There is no daemon. `collect` does one pass and exits, so cron or launchd owns
the cadence and you can always just run it by hand:

```
0 6 * * *  repo-metrics collect ; repo-metrics report --out /srv/report.md
```

That is a `;` and not an `&&` on purpose. `collect` exits 1 when any single repo
failed, which is the design: it keeps going so one unreachable repo does not
cost you the other nine. Chaining with `&&` would let that one repo cancel the
report you actually wanted.

[`examples/`](examples/) has a ready-made launchd agent for macOS and the
equivalent crontab line for Linux.

## Asking it narrower questions

`--format json` gives you the same report as data, and two flags narrow what you
get. This matters most if the thing reading the report is a script or an AI
agent, because the whole report is usually far more than the question needs.

```sh
repo-metrics report --format json                      # everything
repo-metrics report --format json --section movers     # just what changed
repo-metrics report --format json --section problems   # just what failed to collect
repo-metrics report --format json --repo my-service    # just one repo
```

Measured on a three-repo config with `tiktoken` (`cl100k_base`), so these are
counts rather than estimates. Yours will differ with your fleet and how much
moved:

| what you ask for | tokens |
|---|---|
| the whole report | 688 |
| `--section movers` | 205 |
| `--section problems` | 166 |

`--section` works on markdown too, since a person narrowing to what moved is
just as reasonable as a machine doing it.

### What the JSON promises

Two things, and both exist because the same bug kept happening: something that
was never measured getting published as a measurement of zero.

**Numbers live inside nullable groups.** A repo that measured nothing does not
have a coverage percentage of zero, it has no coverage object at all. A whole
row, nothing left out:

```json
{"name": "legacy", "status": "failed", "collected_at": "2026-08-15 23:47 UTC",
 "coverage": null, "tests": null,
 "has_snapshot": true, "has_baseline": false, "env_changed": false,
 "error": "no coverage profile at /srv/legacy/coverage.out and no command configured to produce one"}
```

That shape is deliberate. If those fields were merely omitted, a consumer writing
`row.coverage_pct ?? 0` would turn an absent measurement straight back into a
measured zero. Reaching into a null object throws instead, which is the point.
There are two levels of it: a null `coverage` means nothing was measured, while
a `delta_points` of null inside a present `coverage` means it was measured and
there is no baseline to compare it against.

**Every report says what it covers.** A section you did not ask for comes back
`null` rather than `[]`, so "not requested" is distinguishable from "nothing to
report", and a `scope` object says which repos the answer is about:

```json
{"generated_at": "2026-08-15 23:47 UTC", "window_days": 7, "section": "problems",
 "scope": {"repo": "api", "selected": 1, "configured": 3},
 "movers": null, "repos": null, "problems": []}
```

That one says every repo it looked at collected cleanly, and that it only looked
at one of the three you configured. Without the scope object it would be
byte-identical to the answer meaning "none of your three repos failed", which is
a much stronger claim. `selected == configured` is how you know you are seeing
all of it.

Nothing here needs the flags to be discovered ahead of time: `repo-metrics
report --help` lists them all.

## How it decides what to tell you

The report leads with which repos moved, and under each one, which packages are
why. Picking those packages is the only genuinely interesting decision the tool
makes, so here is how it works.

For each package, it recomputes the repo's overall coverage with that one package
held at its old numbers and everything else left at today's. The gap between that
made-up figure and the real one is what that package contributed, in percentage
points of the repo. Packages are ranked by the size of that gap.

The obvious alternative, ranking by how much each package's own percentage moved,
sounds equivalent and is not. Say a three-statement helper goes from 0 percent to
100 percent. That is the biggest percentage swing in the repo by a mile, so it
tops the list, and it is worth nothing: three statements against a repo of tens of
thousands does not move the repo number at all. Meanwhile a large package sliding
four points quietly accounts for the entire drop you are trying to explain, and
sits below the helper. Ranking by contribution puts them the right way round,
because it is measuring the thing the headline is actually about.

There is a size floor on top of that, `min_statements`, default 20. Tiny packages
are left out of the ranking entirely rather than trusted to sort themselves to the
bottom.

Left out means left out: there is no appendix, and a small package whose coverage
merely moved is not named anywhere in the report. It is not ignored, though. Its
statements still count toward its repo's coverage number in the per-repo table,
which is where the headline figure comes from, and if it was added or deleted it
is still listed among the packages that came and went. The floor applies to the
"which package is why" ranking and to nothing else.

## Design notes

**It stores counts, not percentages.** Repo coverage is total covered statements
over total statements, which is not the average of the per-package percentages.
Storing a percentage column would bake that error in permanently.

**It does not trust a file path.** If it runs a command for you, the artifact has
to be newer than the command that supposedly produced it. This is not paranoia:
a real repo out there has a `make coverage-all` target that is declared `.PHONY`
with no rule, so it prints "Nothing to be done", exits 0, and writes nothing,
while a months-old coverage profile sits at the path it would have written to.
Anything that trusts exit code 0 reports that stale file as today's number
forever, and nothing anywhere logs an error.

**It records the toolchain it measured with.** Go workspaces can silently swap a
dependency between a local working tree and a pinned version, which changes
coverage for reasons that have nothing to do with your code. Snapshots carry a
fingerprint so you do not get to diff across that boundary without being told.

## What it deliberately does not do

- **No export plumbing.** It writes markdown and JSON. If you want it in Notion
  or Linear, the official MCP servers already do that better than a bespoke
  integration would.
- **No MCP server of its own either.** A CLI costs nothing until you invoke it,
  while an MCP tool schema sits in the context window whether or not anyone uses
  it. This tool is stateless and one-shot, its state is a SQLite file rather than
  anything a server has to hold, and its output was already JSON, so a protocol
  would not have been relieving anyone of a parsing burden. What MCP genuinely
  buys is discoverability, and `--help` buys most of that for free.
- **No dashboard, no web server.** Static output only.
- **No coverage floor, and no ratchet.** The report says what moved and leaves the
  judgment to you. A percentage gate punishes an honest deletion of covered code
  and rewards writing low-value tests to clear a bar, which is the exact confusion
  this tool argues against everywhere else.
- **No plugin system.** There is a `Collector` interface so this does not stay
  Go-only forever, but one interface is not a registry and does not need to be.

## Building

The Makefile sets `GOWORK=off` because this module lives next to a Go workspace
it is deliberately not part of. It is meant to build standalone.

```sh
make build     # ./bin/repo-metrics
make check     # build, vet, test, lint
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the rest: the pinned linter version, the
house style, and why a new metric goes in as counts rather than a percentage.

## License

Apache-2.0. See [LICENSE](LICENSE).
