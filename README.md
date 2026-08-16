# repo-metrics

Tracks code-quality metrics across a bunch of Git repos at once, keeps history in
SQLite, and tells you what got worse this week.

Coverage tools are per-repo and live in a tab nobody opens. Nobody reads a
dashboard daily. Everybody reads "coverage in repo X dropped 4 points, package Y
is why." That report is the whole point of this thing.

Self-hosted, one binary, no account, no SaaS.

## Status

The core is built: multi-signal collection, SQLite storage, delta computation
against a baseline snapshot, history over the whole series, and the markdown and
JSON report. It has not been run against a fleet for long enough to have opinions
about it yet, so treat it as working but young.

Coverage and test counts are read from Go's own formats. Lint findings are read
as SARIF, which golangci-lint, eslint, ruff, semgrep, clippy and CodeQL all emit,
so that one is not Go-specific at all.

That cuts both ways, and it is worth saying plainly which way. SARIF is the only
one of the four formats that is not Go's own, so a TypeScript or Python service
collects lint findings, lint errors, lint suppressions and the time collection
took, and reports null for coverage, for every test count and for every
dependency signal. Those go through parsers that read a Go coverage profile, a
`go test -json` stream and `go list -m -json`, and there is no second parser
behind any of them yet.

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
current directory, so you can run the rest immediately and see real output before
editing anything. Every repo `path:` is checked when the config loads, so a file
full of `/path/to/your-repo` placeholders is a load error rather than a mystery
later. An `artifact:` path is not checked then, because the file it names is
usually something a later command produces.

The report needs two snapshots to compare, and the second one has to be far
enough back. The baseline is the newest snapshot at or before your window, which
defaults to seven days, so collecting again tomorrow still leaves you with no
baseline: yesterday is too recent to qualify, not too old. Either wait out the
window, or pass `--window 1d` to compare against yesterday.

## The commands

| command | what it is for |
|---|---|
| `init` | write a starter config |
| `collect` | run each repo's signals, parse what they produced, store a snapshot |
| `report` | compare the newest snapshot against one from a window ago |
| `history` | one repo, one signal, every snapshot in a range |
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

`history` is the one that reads back what is already in the database. `report`
only ever compares two snapshots, so it tells you how far a number moved and over
what span, and nothing at all about the shape in between:

```sh
repo-metrics history --repo api --signal coverage --since 90d
```

A run that failed stays in the series rather than being filtered out, because a
gap in collection is the finding. Dropping those points draws a straight line
through the week nobody was looking, and drawing them at zero turns a crashed
test command into a coverage cliff:

```
| when | Coverage | status |
| --- | --- | --- |
| 2026-08-01 06:00 UTC | 83.6% | ok |
| 2026-08-08 06:00 UTC | not collected | failed |
| 2026-08-15 06:00 UTC | 57.4% | ok |
```

The words in that middle cell are load-bearing. `not collected` is a run that
failed outright, and `not measured` is a run that succeeded without producing
this particular signal, which are different problems and are never given the
same words. Under the table, a Collection problems section prints the error
behind each run that reported one, matched to the rows by timestamp, so a row
like that one is never a dead end. A repo that has only ever collected cleanly
does not grow the heading at all.

There is also `repo-metrics version`, which does no work and takes no flags.
Built from a clean checkout of this repository, which carries no tag yet, it
says:

```
repo-metrics v0.0.0-20260816181544-8eba617e536d
built with go1.26.5 for darwin/amd64
```

That first line is the pseudo-version the toolchain derives when there is no tag
to use, so it reads as the commit's date and hash. Installed from a tag it is
the tag, `repo-metrics v0.1.0` and nothing else.

Nothing has to be bumped to keep that honest. The Go toolchain stamps the
version and the commit into every binary it builds, so this reads that back
rather than keeping a second copy that can drift from it. A binary built from a
modified tree says `plus uncommitted changes`, because a commit hash from a
dirty checkout names code that never ran. A binary built with no stamping at all
says it does not know, rather than printing a confident-looking `devel`. A
version string is a measurement too.

## What it measures

Thirteen signals. Twelve of them are whatever three configured commands turn out
to yield, and the thirteenth is the collector timing those commands itself. You
configure the commands; you do not pick the signals.

| signal | unit | from |
|---|---|---|
| `coverage` | percent | coverage profile |
| `tests` | count | `go test -json` stream |
| `test_failures` | count | same stream |
| `test_skipped` | count | same stream |
| `untested_packages` | count | same stream |
| `test_time` | duration | same stream |
| `lint_findings` | count | SARIF log |
| `lint_errors` | count | same log |
| `lint_suppressed` | count | same log |
| `dependencies` | count | `go list -m -json` |
| `dependency_age` | days | same stream |
| `outdated_dependencies` | count | same stream, with `-u` |
| `collect_time` | duration | the runner's own clock |

Every one of them can be null, and null always means the same thing: nothing
measured it. Three of them are worth a word about why they are separate.

**`lint_suppressed` is not part of `lint_findings`.** Counting suppressed
findings against a repo would make it look worse for having triaged them, which
is the opposite of the incentive this tool should create. It is tracked on its
own because a rising suppression count is its own finding.

**`outdated_dependencies` is direct dependencies only.** Bumping an indirect
module is a consequence of bumping the direct one that pulls it in, so a headline
built on the total reports work that is not really there.

**`outdated_dependencies` is often null when the other two dependency signals are
not.** With `GOPROXY=off`, or when any module fails to resolve, `go list -m -u`
exits 0, writes nothing to stderr, and reports an update on nothing. "Every
dependency is current" and "nothing was ever checked" are byte-identical output,
so the count is not recorded at all and the run says why. The module count and
the age aggregate need no network and are unaffected.

`collect_time` is wall clock per signal, which is not `test_time`. That one sums
the per-package durations `go test` reports, counting parallel packages more than
once: it is machine work. `collect_time` is time somebody waited.

`test_time` also mostly measures Go's test cache, which is worth knowing before
charting it. A package `go test` did not rerun comes back marked `(cached)` and
reports a near-zero elapsed time, and this signal sums whatever the stream says.
Two
collections of the same repo here, with nothing changed between them, summed to
34.27s and then 588ms. Neither number is wrong and neither is about the code, so
a chart of this signal shows swings that correspond to nothing that happened in
the repo. That, plus load on whatever machine ran the suite, is why `test_time`
is one of the signals that can never nominate a repo as a mover: it is on the
wire and in `history` for anyone who wants it, and it is never the reason a repo
leads the report.

## Configuring it

```yaml
database: ./repo-metrics.db
repos:
  - name: my-service
    path: /path/to/my-service
    env:
      GOWORK: "off"
    signals:
      - name: coverage
        command: ["go", "test", "./...", "-json", "-coverpkg=./...", "-coverprofile=coverage.out"]
        artifact: coverage.out
        artifact_format: go-coverprofile
        stdout_format: go-test-json
        timeout: 10m

      - name: lint
        command: ["golangci-lint", "run", "--output.sarif.path", "stdout",
                  "--max-issues-per-linter", "0", "--max-same-issues", "0"]
        stdout_format: sarif
        timeout: 5m

      - name: dependencies
        command: ["go", "list", "-m", "-u", "-json", "all"]
        stdout_format: go-list-modules
        timeout: 3m

  - name: built-in-ci
    path: /srv/checkouts/built-in-ci
    signals:
      - name: coverage
        artifact: artifacts/coverage.out
        artifact_format: go-coverprofile
        max_age: 24h
        # no command, so it just reads what CI already wrote
```

Each repo carries a list of signals: one entry per thing to measure. A signal
either runs a command and reads what it left behind, or reads an artifact
something else produced. One command can feed two parsers, which is why the
coverage entry names both an `artifact_format` and a `stdout_format`: `go test`
yields the profile and the test counts from a single run.

A signal is also the unit of failure. One going wrong costs its own numbers and
nothing else, and the snapshot comes back `partial` rather than `failed`.

Those two `--max-issues` flags on the lint entry are not decoration.
golangci-lint caps its output at 50 issues per linter and 3 per repeated message
by default, so without them the number you track over time is a cap, and a repo
that doubles its findings charts as flat. Measured on one real repo: 623 findings
capped, 2760 uncapped, same run.

You do not have to tell it that a linter exiting 1 is normal. That is a fact
about the tools that emit SARIF rather than a preference, so it is built in, and
`go test` exiting non-zero is still treated as the red suite it is.

There is a fully commented version at
[`examples/repo-metrics.yaml`](examples/repo-metrics.yaml), with every field
annotated with what it does and what happens if you leave it out.

Durations read the same everywhere: Go's duration syntax plus a `w` suffix for
weeks and a `d` suffix for days. `7d`, `168h`, `1w` and `1d12h` all work, on the
`--window` and `--since` flags and in the `window:`, `timeout:` and `max_age:`
fields of the config file alike. Larger units come first, so `1w3d12h` reads the
way `1h30m` does and `3d1w` is an error rather than a guess.

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
repo-metrics history --repo my-service --format json   # one signal over time
repo-metrics repos --format json                       # did the cron run
```

Measured on a three-repo config with `tiktoken` (`cl100k_base`), so these are
counts rather than estimates. Yours will differ with your fleet and how much
moved:

| what you ask for | tokens |
|---|---|
| the whole report | 1882 |
| `--repo one-of-three` | 1245 |
| `--section movers` | 791 |
| `--section problems` | 522 |
| `history` for one repo | 192 |
| `repos` | 158 |

284 of those tokens is the signal catalog, which is worth knowing before you
narrow: `--section problems` is 522 tokens and 284 of them are the legend. It
says what each measurement is called, what unit it is in, and which direction is
good news, which is how a consumer reads `"value": 214000` and knows it is
milliseconds and that lower is better without the key saying so on every row of
every repo. Paying once per response rather than once per number is why adding
eleven signals roughly doubled these counts instead of multiplying them.

The catalog rides on `report --format json` and only there, whatever `--section`
or `--repo` you narrow to. `repos` has no `signals` key at all, and `history`
carries a single `signal` object describing the one measurement it charted, 20
tokens rather than 284. That is the other half of why those two are so much
cheaper than any slice of the report.

`--section` works on markdown too, since a person narrowing to what moved is
just as reasonable as a machine doing it.

### What the JSON promises

Two things, and both exist because the same bug kept happening: something that
was never measured getting published as a measurement of zero.

**Numbers live inside nullable groups.** A repo that measured nothing does not
have a coverage percentage of zero, it has no coverage object at all. Here is
one such row rendered whole, so the key set is the one a consumer really gets:

```json
{"name": "legacy", "status": "failed", "collected_at": "2026-08-15 23:47 UTC",
 "baseline_collected_at": null,
 "coverage": null, "tests": null, "test_failures": null, "test_skipped": null,
 "untested_packages": null, "test_time": null, "lint_findings": null,
 "lint_errors": null, "lint_suppressed": null, "dependencies": null,
 "outdated_dependencies": null, "dependency_age": null, "collect_time": null,
 "has_snapshot": true, "has_baseline": false, "env_changed": false,
 "git_dirty": false, "moved_by": null,
 "error": "coverage: no artifact at /srv/legacy/coverage.out and no command configured to produce one"}
```

Twenty-two keys, and every repo row in a report payload has all of them. `error`
is the twenty-third and the only one that is omitted rather than nulled, since it
is there when there is something to say and gone when there is not. A repo that
collected cleanly is the same shape with the groups filled in. The rows `repos
--format json` returns are a different and much smaller shape, since that command
answers a different question.

That shape is deliberate. If those fields were merely omitted, a consumer writing
`row.coverage_pct ?? 0` would turn an absent measurement straight back into a
measured zero. Reaching into a null object throws instead, which is the point.
There are two levels of it: a null `coverage` means nothing was measured, while a
null `delta` inside a present `coverage` means it was measured and there is no
baseline to compare it against.

Every measured group carries `value` and `delta`, whatever the signal is:

```json
{"lint_findings": {"value": 2857, "delta": -14},
 "outdated_dependencies": null,
 "dependency_age": {"value": 199.33, "delta": null}}
```

`value` and `delta` rather than `count` and `count_change`, so a consumer walking
signals needs no per-signal key knowledge. What each one means is answered once
by the catalog on the envelope. The row above is a repo whose lint findings fell
by 14, whose dependency ages were measured with no baseline to compare against,
and whose outdated count was not measured at all, most likely because the module
proxy was never consulted.

Twelve of the thirteen groups are exactly those two keys. Coverage is the
exception and carries five more, which is where the culprit ranking below
actually lives. Walking signals still needs no per-signal knowledge, because
`value` and `delta` are on this group too and mean what they mean everywhere
else, but a consumer that wants the packages behind a coverage move reads them
from here:

```json
{"value": 89.33459178857952, "delta": -1.7215452237896613,
 "covered": 1893, "total": 2119,
 "culprits": [
   {"package": "github.com/Romero-jace/repo-metrics/internal/retention",
    "state": "added", "from_pct": null, "to_pct": 0,
    "contribution_points": -1.023403438150794, "statements": 24},
   {"package": "github.com/Romero-jace/repo-metrics/internal/config",
    "state": "changed", "from_pct": 95.27896995708154, "to_pct": 88.8,
    "contribution_points": -0.7224966985755685, "statements": 250},
   {"package": "github.com/Romero-jace/repo-metrics/internal/goingaway",
    "state": "removed", "from_pct": 87.5, "to_pct": null,
    "contribution_points": 0.020546058294868885, "statements": 24}],
 "added_packages": ["github.com/Romero-jace/repo-metrics/internal/retention"],
 "removed_packages": ["github.com/Romero-jace/repo-metrics/internal/goingaway"]}
```

`covered` and `total` are the statement counts `value` was computed from, so a
consumer can re-derive the percentage or roll several repos up without averaging
percentages. `contribution_points` is the ranking key described under "How it
decides what to tell you": percentage points of the whole repo, not of the
package, which is why a 24-statement package that arrived untested outranks a
250-statement one that slipped six and a half points. `statements` is the larger of the two
sides, which is the size the `min_statements` floor is applied to. `state` is
`changed`, `added` or `removed`, and it is what stops the two percentages being
misread: `from_pct` is null for a package that did not exist in the baseline and
`to_pct` is null for one that is gone, because a zero there would chart a
deletion as a collapse and a new package as a climb out of nothing.

`added_packages` and `removed_packages` are the same churn without the ranking,
and the floor does not apply to them: a three-statement package that appeared is
too small to be a culprit and is still named here.

All three of those lists are `null` rather than `[]` when there is nothing in
them, so a repo whose coverage held steady has a coverage group with `value` and
`delta` filled in and `"culprits": null` beside them. That is the opposite of
how the report's three top-level sections behave, and the difference is not
carrying a meaning: only the sections distinguish "you did not ask" from
"nothing to report", because only they can be unasked for.

`moved_by` on the repo row is the other half of this. It lists the signals that
made the repo lead the report, by the same ids the catalog uses, so `["coverage",
"tests", "untested_packages"]` says which three measurements qualified it. It is
null for a repo that did not move, which is every repo outside the `movers` list.

**Every report says what it covers.** A section you did not ask for comes back
`null` rather than `[]`, so "not requested" is distinguishable from "nothing to
report", and a `scope` object says which repos the answer is about:

```json
{"generated_at": "2026-08-15 23:47 UTC", "window_days": 7, "section": "problems",
 "scope": {"repo": "api", "selected": 1, "configured": 3},
 "movers": null, "repos": null, "problems": []}
```

That is the envelope with the `signals` catalog dropped out of it for length, and
it is the one place in this section where something has been left out. It says
every repo it looked at collected cleanly, and that it only looked at one of the
three you configured. Without the scope object it would be
byte-identical to the answer meaning "none of your three repos failed", which is
a much stronger claim. `selected == configured` is how you know you are seeing
all of it.

Nothing here needs the flags to be discovered ahead of time: `repo-metrics
report --help` lists them all.

## How it decides what to tell you

The report leads with which repos moved, and says which measurements moved them.
Seven of the thirteen signals are allowed to nominate a repo: `coverage`,
`tests`, `test_failures`, `untested_packages`, `lint_findings`, `lint_errors`
and `outdated_dependencies`. The other six are collected, published and
chartable, and never make a repo the headline. That is deliberate rather than an
oversight about their importance: a signal that moves on every run, like a test
suite's wall time on a shared machine, would make every repo a mover every week
and bury the ones that matter. The JSON says which ones qualified a repo in
`moved_by`, and the markdown writes one line per qualifying signal.

Under each repo in that list, whenever coverage was measured on both sides and
some package actually shifted it, the report also says which packages are why.
Picking those packages is the only genuinely interesting decision the tool makes,
so here is how it works.

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

There is one more way to be kept out of the headline, and it has nothing to do
with packages. The baseline is the newest snapshot at or before the window's
cutoff, with no floor on how far before it sits, so a repo nobody collected for
two months is compared against a two-month-old snapshot. That comparison is
still the best answer available and is still published, labeled with the span it
actually covers rather than with the window you asked for, and every repo row
carries the baseline's own timestamp in `baseline_collected_at`. What it loses,
once the gap is more than three windows, is the right to lead the report. Three
rather than two because a weekly cron that missed a single run has a
fortnight-old baseline, which is common and harmless. The case the rule exists
for is a quarter of accumulated drift outranking the repos that really moved
this week.

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

**A signal that cannot be trusted is not recorded.** The clearest case is the
dependency one. Run `go list -m -u -json all` with `GOPROXY=off` and it exits 0,
says nothing on stderr, streams every module, and reports an update on none of
them, which is byte-identical to what a perfectly current repo produces. So the
tool asks the toolchain what its proxy is set to, and if the answer means nothing
was checked, it records no update count and says why. A zero there would be
indistinguishable from good news.

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
- **No plugin system.** Formats are a closed table: a name an operator can write,
  paired with the code that reads it. Adding lcov or JUnit XML is a new entry and
  a new parser, not a registry, not a plugin API, and not a shared object anyone
  has to build.
- **No flaky-test rate and no PR velocity.** The first needs per-test rows on
  every snapshot, which is a different database and its own project. The second
  is the only signal on the original list not derivable from a local checkout,
  and reaching for a forge API would undo the "reads this file and nothing else"
  property that makes the config the whole integration surface.

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
