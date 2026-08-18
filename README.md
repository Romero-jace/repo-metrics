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

Eight formats, three of them not tied to a language at all. SARIF carries lint
findings from golangci-lint, eslint, ruff, semgrep, clippy and CodeQL; JUnit XML
carries test results from pytest, vitest, jest and go-junit-report; LCOV carries
coverage from istanbul, nyc and coverage.py. A `package-lock.json` or a
`bun.lock` gives the dependency count. So a Python or TypeScript service records
coverage, test counts, failures, skips, test time, lint findings and how many
packages it pulls in.

Two things are still Go-only, and both for the same reason: no other toolchain's
output can express them.

**`untested_packages`.** A JUnit document lists the suites that RAN. Nothing in
it reveals a source file nobody wrote a test for, and neither pytest nor vitest
enumerates one. A repo measured that way reports the signal as null rather than
as zero, which is the true answer: nobody counted.

**Dependency age and the outdated count.** `go list -m -json` answers all three
dependency questions from one stream. Elsewhere only the count is recoverable:
no JavaScript lockfile records when a version was published, and knowing whether
a newer one exists needs a registry. Both come back null.

Note what is *not* read to get that count. `npm outdated` and its siblings
resolve through the installed tree, so on a checkout where nothing has been
installed they return an empty result and exit 0 — a repo nobody installed and a
repo with nothing outdated produce identical output. That is the same trap
`GOPROXY=off` sets for the Go path. The lockfile has no such state: it either
lists the packages or it is not a lockfile.

Coverage comes in two units and they are never mixed. A Go profile counts
statements; LCOV counts lines, and several statements on one source line collapse
to one line there. They are separate signals, reported separately, and a repo
normally fills exactly one of them.

One thing to know before you trust a report rather than merely read it: the
thresholds that decide which repos lead it, and which signals are allowed to
nominate one at all, are first guesses. They are written down where they can be
argued with — `min_statements` and `min_repo_delta` in the config, and a
per-signal floor in the code — but they have not been calibrated against a real
fleet over real time, because no fleet has run long enough yet to calibrate them.
What that means in practice is that the "what moved" section is a starting
proposal about what deserves attention, not a settled one. The per-repo table
under it is unfiltered, and the history command is unfiltered, so nothing is
hidden from you by a threshold you disagree with. "How it decides what to tell
you" below has the reasoning behind each cutoff.

## Install

This repository is private, so build it from a checkout:

```sh
git clone git@github.com:Romero-jace/repo-metrics.git
cd repo-metrics
make build     # ./bin/repo-metrics
```

That is the whole install. The binary is self-contained and CGO-free, so you can
also build once and copy `./bin/repo-metrics` onto the PATH of any machine with
the same OS and architecture, without giving that machine access to this
repository at all.

`go install` works too, but not out of the box: the module proxy and the checksum
database cannot read a private repository, so both have to be told to skip it,
and git has to be able to authenticate.

```sh
go env GOPRIVATE                                  # check before you set it
go env -w GOPRIVATE=github.com/Romero-jace/*      # see the warning below first
git config --global url."git@github.com:Romero-jace/".insteadOf "https://github.com/Romero-jace/"
go install github.com/Romero-jace/repo-metrics/cmd/repo-metrics@latest
```

`GOPRIVATE` is one comma-separated list and `go env -w` replaces all of it, so if
you already have private modules from somewhere else that command silently cuts
them off. Read the existing value first and write the union:

```sh
go env -w GOPRIVATE=github.com/someone-else/*,github.com/Romero-jace/*
```

Without it you get a checksum-database error that reads like the module is
corrupt rather than unreachable, which is a misleading enough failure to be worth
naming here.

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

`init` writes a config that already works **if you run it inside a Go module**:
the first repo entry points at the current directory, and its two live signals
are `go test` and `go list`, so you can run the rest immediately and see real
output before editing anything. Run it anywhere else and it still loads, and
`collect` still exits 0 — what you get is a *partial* snapshot with nothing in it
worth reading: the coverage step finds no instrumented packages and records none,
the dependency step cannot find a `go.mod` and records nothing, and the result
arrives as a wall of warnings rather than as a clean failure. That is not the
tool declining to measure your language; it is a
starter config written for the language the tool is written in. Every
language-neutral format named under [Status](#status) is reachable from a config
you write yourself, and the Python and TypeScript entries in
[`examples/repo-metrics.yaml`](examples/repo-metrics.yaml) are the ones to copy —
note in particular their `fingerprint:` lines, which a non-Go repo needs and a Go
repo gets for free.

Two smaller things worth knowing before the first run. Every repo `path:` is
checked when the config loads, so a file full of `/path/to/your-repo`
placeholders is a load error rather than a mystery later; an `artifact:` path is
not checked then, because the file it names is usually something a later command
produces. And every artifact written to a relative path lands inside the repo
being measured — the generated coverage entry writes `coverage.out` there, and
the examples write into `reports/`. Whatever you do not gitignore will show up as
an uncommitted change from the second run onward, which sets the `git_dirty` flag
and earns every future snapshot a warning saying its numbers belong to no commit.
The alternative is to point the command's output flag and the matching
`artifact:` at an absolute path outside the tree, which costs one line and makes
the dirty-tree warning mean something again.

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

Two more phrases show up in a change column rather than a value, and they are
about the comparison rather than about the measurement. `no baseline yet` means
the signal was measured today and there is no earlier snapshot far enough back to
compare it against — the usual cause is simply that you started collecting less
than a window ago, and it clears itself once history catches up rather than
indicating anything wrong. `not comparable` is stronger: two snapshots exist, and
something about them makes the difference meaningless, most often that the set of
steps producing the signal changed between them, or that the environment
fingerprint moved. A signal newly switched on did not make the repo worse, and a
number measured under a different toolchain is not a delta, so in both cases the
tool declines to subtract rather than publishing an artifact of your own config
change.

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

Fourteen signals. Thirteen of them are whatever the configured commands turn out
to yield, and the fourteenth is the collector timing those commands itself. You
configure the commands; you do not pick the signals.

| signal | unit | from |
|---|---|---|
| `coverage` | percent | Go coverage profile, in statements |
| `coverage_lines` | percent | LCOV tracefile, in lines |
| `tests` | count | `go test -json` stream, or a JUnit report |
| `test_failures` | count | same |
| `test_skipped` | count | same |
| `untested_packages` | count | `go test -json` stream only |
| `test_time` | duration | same as `tests` |
| `lint_findings` | count | SARIF log |
| `lint_errors` | count | same log |
| `lint_suppressed` | count | same log, only if it carries `suppressions` |
| `dependencies` | count | `go list -m -json`, or a lockfile |
| `dependency_age` | days | `go list -m -json` only |
| `outdated_dependencies` | count | `go list -m -json` only, with `-u` |
| `collect_time` | duration | the runner's own clock |

Every one of them can be null, and null always means the same thing: nothing
measured it. Four of them are worth a word about why they are separate.

**`coverage` and `coverage_lines` are different units and are never summed.** A
Go profile counts statements, an LCOV tracefile counts lines, and several
statements on one source line collapse to one line. Adding the two denominators
together would produce a percentage that is arithmetically well formed and
describes nothing. A polyglot repo can fill both; neither ever stands in for the
other.

**`lint_suppressed` is not part of `lint_findings`.** Counting suppressed
findings against a repo would make it look worse for having triaged them, which
is the opposite of the incentive this tool should create. It is tracked on its
own because a rising suppression count is its own finding.

**And it is unmeasured on most repos, deliberately.** A SARIF `suppressions`
array is the only way a document can report a finding that was raised and then
silenced, and almost no linter writes one: golangci-lint drops a `//nolint`
finding before it reaches the log, and so do ruff for `# noqa`, rubocop for
`# rubocop:disable`, detekt for `@Suppress` and clippy for `#[allow]`. Only ESLint
via `@microsoft/eslint-formatter-sarif` and Roslyn emit the array at all.

So a document carrying no suppressions is the same bytes whether the repo
suppresses nothing or the linter cannot say, and nothing is recorded rather than a
zero. That is the one count in this tool where zero is never stored: `lint_findings`
of 0 is a real measurement of a clean run, and `lint_suppressed` of 0 is not a
measurement of anything. What it costs is real and worth stating — a repo that
deletes its last suppression reads as unmeasured rather than as zero, so that
improvement cannot be reported. There is no version of this that reports the
improvement without also fabricating the zero.

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

  - name: web-app
    path: /path/to/web-app
    fingerprint: ["node", "--version"]
    signals:
      - name: tests
        command: ["vitest", "run", "--reporter=junit", "--outputFile=reports/junit.xml",
                  "--coverage", "--coverage.reporter=lcov",
                  "--coverage.reportsDirectory=reports/cov"]
        artifacts:
          - {path: reports/junit.xml, format: junit-xml}
          - {path: reports/cov/lcov.info, format: lcov}
        timeout: 20m

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
something else produced.

One command can feed several parsers, and there are two ways it does. The
coverage entry above names both an `artifact_format` and a `stdout_format`,
because `go test` yields the profile as a file and the test counts on stdout. A
command that writes several *files* uses `artifacts:` instead:

```yaml
      - name: tests
        command: ["pytest", "--junitxml=j.xml", "--cov-report=lcov:c.info"]
        artifacts:
          - {path: j.xml, format: junit-xml}
          - {path: c.info, format: lcov}
```

`artifact:` with `artifact_format:` is the one-file shorthand for the same thing,
and naming both spellings on one step is an error rather than a precedence rule.

Listing both files here rather than reading the second one in a step of its own
is not only tidier. Every artifact a step names is held to the same check — this
run has to have written it — while a step with no command only asks whether the
file is younger than `max_age`. Split across two steps, the second file gets the
weaker check, and a profile left over from yesterday is accepted as today's
measurement.

Keys this tool does not read are rejected rather than ignored, at every level of
the file. A typo that loads clean and does nothing is the same failure the tool
exists to refuse, arriving through the config instead of through a parser.

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
the cadence and you can always just run it by hand.

What you should not do is put the two commands in the crontab directly. This
file used to recommend exactly that — `collect ; report`, with `;` rather than
`&&` on the grounds that one unreachable repo should not veto the whole report.
That reasoning was right and its price was fatal: chaining with `;` makes the
job's status `report`'s alone, and **`report` exits 0 unconditionally**. It exits
0 against a database with nothing in it, having rendered a report with nothing in
it. So the scheduled job could not fail, and a collection that stopped working
would go on looking healthy indefinitely — which is the precise failure this
tool exists to catch, reproduced in its own recommended deployment.

Use [`examples/repo-metrics-daily.sh`](examples/repo-metrics-daily.sh) instead.
It resolves that rather than reverting it: it decides the exit code from what the
database actually stored, runs the report regardless, and then exits with the
code it decided — `1` nothing stored, `2` a repo failed, `3` the database is
corrupt, `4` the data is stale, `5` a run was already in progress. It also backs
the database up before each run, because snapshots cannot be re-collected: they
measured a working tree at a commit that has since moved on.

[`examples/`](examples/) wraps that script in a ready-made launchd agent for
macOS and a crontab line for Linux, and [`examples/README.md`](examples/README.md)
has the full reasoning, the prerequisites, and what each exit code means.

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
 "coverage": null, "coverage_lines": null, "tests": null,
 "test_failures": null, "test_skipped": null,
 "untested_packages": null, "test_time": null, "lint_findings": null,
 "lint_errors": null, "lint_suppressed": null, "dependencies": null,
 "outdated_dependencies": null, "dependency_age": null, "collect_time": null,
 "has_snapshot": true, "has_baseline": false, "env_changed": false,
 "env_unknown": false, "git_dirty": false, "moved_by": null,
 "error": "coverage: no artifact at /srv/legacy/coverage.out and no command configured to produce one"}
```

Twenty-four keys, and every repo row in a report payload has all of them. `error`
is the twenty-fifth and the only key on this row that is omitted rather than
nulled, since it is there when there is something to say and gone when there is
not.

`env_unknown` sits beside `env_changed` and is not redundant with it. A false
`env_changed` reads as reassurance, and for a repo whose toolchain nothing ever
identified it would be a reassurance nobody gave: two snapshots that both failed
to name a toolchain carry the same placeholder and would otherwise compare as
unchanged. The two are mutually exclusive. A history point carries an `error` of its own on the same terms, and those
two are the only omitted keys anywhere on the wire. A repo that
collected cleanly is the same shape with the groups filled in. The rows `repos
--format json` returns are a different and much smaller shape, since that command
answers a different question, and so is what `history` returns. Both are spelled
out at the end of this section.

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

Thirteen of the fourteen groups are exactly those two keys. Coverage is the
exception and carries five more, which is where the culprit ranking below
actually lives. Walking signals still needs no per-signal knowledge, because
`value` and `delta` are on this group too and mean what they mean everywhere
else, but a consumer that wants the packages behind a coverage move reads them
from here:

```json
{"value": 89.33459178857952, "delta": -1.7215452237896613,
 "covered": 1893, "total": 2119,
 "culprits": [
   {"scope": "github.com/Romero-jace/repo-metrics/internal/retention",
    "state": "added", "from_pct": null, "to_pct": 0,
    "contribution_points": -1.023403438150794, "units": 24},
   {"scope": "github.com/Romero-jace/repo-metrics/internal/config",
    "state": "changed", "from_pct": 95.27896995708154, "to_pct": 88.8,
    "contribution_points": -0.7224966985755685, "units": 250},
   {"scope": "github.com/Romero-jace/repo-metrics/internal/goingaway",
    "state": "removed", "from_pct": 87.5, "to_pct": null,
    "contribution_points": 0.020546058294868885, "units": 24}],
 "added_scopes": ["github.com/Romero-jace/repo-metrics/internal/retention"],
 "removed_scopes": ["github.com/Romero-jace/repo-metrics/internal/goingaway"]}
```

`covered` and `total` are the statement counts `value` was computed from, so a
consumer can re-derive the percentage or roll several repos up without averaging
percentages. `contribution_points` is the ranking key described under "How it
decides what to tell you": percentage points of the whole repo, not of the
package, which is why a 24-statement package that arrived untested outranks a
250-statement one that slipped six and a half points. `units` is the larger of the two
sides, which is the size the `min_statements` floor is applied to. It counts
statements under `coverage` and lines under `coverage_lines`, and is named for
neither: the same group type serves both units, and a field called `statements`
holding a line count would put a wrong-unit label at the wire, which is what the
two separate metric keys exist to prevent one layer down. `scope` is a Go import
path under `coverage` and a source file path under `coverage_lines`, for the same
reason. `state` is
`changed`, `added` or `removed`, and it is what stops the two percentages being
misread: `from_pct` is null for a package that did not exist in the baseline and
`to_pct` is null for one that is gone, because a zero there would chart a
deletion as a collapse and a new package as a climb out of nothing.

`added_scopes` and `removed_scopes` are the same churn without the ranking, and
the floor does not apply to them: a three-statement package that appeared is too
small to be a culprit and is still named here.

`coverage_lines` carries this identical shape, in lines over files. That is the
whole of what it means for a Python or TypeScript repo to be a first-class
citizen here: not just a percentage in the table, but a named file to go and look
at when the percentage moves. The two groups are never summed and never stand in
for one another, so a Go repo reports `"coverage_lines": null` and a repo measured
only through LCOV reports `"coverage": null`, each honestly.

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

**The other two payloads are their own shapes.** `repos` and `history` both take
`--format json`, and neither returns anything resembling a report row.

`repos` has two top-level keys, and each of its rows has five:

```json
{"generated_at": "2026-08-16 20:05 UTC",
 "repos": [
   {"name": "api", "status": "ok", "collected_at": "2026-08-16 20:05 UTC",
    "has_snapshot": true, "coverage": {"pct": 78, "covered": 1560, "total": 2000}},
   {"name": "worker", "status": "ok", "collected_at": "2026-08-09 06:00 UTC",
    "has_snapshot": true, "coverage": {"pct": 80, "covered": 640, "total": 800}},
   {"name": "old-service", "status": "not collected", "collected_at": null,
    "has_snapshot": false, "coverage": null}]}
```

`name`, `status`, `collected_at`, `has_snapshot`, `coverage`, and that is the
whole row: no deltas, no baseline, nothing derived from a second snapshot.
`collected_at` and `coverage` are the two nullables and they answer different
questions. A null `collected_at` means nobody has ever collected this repo, while
a null `coverage` covers that case and two more, the run that failed and the run
that succeeded without instrumenting anything. The `repos` key is always present
and always an array.

`history` has seven top-level keys:

```json
{"generated_at": "2026-08-16 20:05 UTC", "since": "2026-05-18 20:05 UTC",
 "since_days": 90,
 "scope": {"repo": "api", "selected": 1, "configured": 3},
 "signal": {"id": "coverage", "label": "Coverage", "unit": "percent",
            "direction": "higher_is_better"},
 "last_collected": "2026-08-16 20:05 UTC",
 "points": [
   {"collected_at": "2026-08-09 06:00 UTC", "status": "ok",
    "git_sha": "0ba66afc6e2a81bb25177b8a55906c041ee11a70",
    "env": "go=go1.26.5;gowork=on", "measurement": {"value": 80}},
   {"collected_at": "2026-08-16 20:05 UTC", "status": "ok",
    "git_sha": "0ba66afc6e2a81bb25177b8a55906c041ee11a70",
    "env": "go=go1.26.5;gowork=on", "measurement": {"value": 78}}]}
```

`generated_at`, `since`, `since_days`, `scope`, `signal`, `last_collected` and
`points`. `signal` is the single object mentioned above rather than the report's
catalog, since a history answer charts exactly one measurement. `last_collected`
is null when the repo has never been collected at all, which is what tells an
empty `points` array meaning "nobody ever ran this" apart from one meaning
"collection stopped before the window you asked about". A point's `measurement`
is null for a run that produced nothing for this signal, which is the same rule
the report's groups follow.

`scope` is the report envelope's three keys, and there is one difference worth
knowing if you read both: `scope.repo` is always a name here, because `--repo` is
required, while the report's is null whenever you did not narrow.

One habit does not carry over, and it is worth knowing before you add to either
payload. Numbers live inside nullable groups only where they are measurements. An
input, meaning a number you supplied rather than one the tool went and found,
sits bare on the envelope on purpose: `since_days` here, `window_days` on the
report, and `scope`'s two counts on both. Nothing measured those and nothing can
fail to, so a null would say something untrue.

The rows are the safe part today. Every number on a `repos` row is inside the
nullable `coverage` group and every number on a history point is inside its
nullable `measurement`, so neither has a bare number anywhere. What is missing is
the pressure that keeps it that way: a measurement added to either payload lands
bare unless somebody puts it in a group deliberately, and then
`row.whatever ?? 0` turns an absent measurement back into a measured zero, which
is the one thing this whole format is arranged to prevent.

Nothing here needs the flags to be discovered ahead of time: `repo-metrics
report --help` lists them all.

## Streams and exit codes

Stdout carries the answer somebody asked for, the thing you would pipe
somewhere. Stderr carries what the run has to say about itself: warnings, errors
and the usage block. So `repo-metrics repos --format json | jq` works, and
whatever went wrong along the way is still in your terminal rather than in jq's
input.

`collect` is the case worth stating outright, because its answer looks like
progress. Its per-repo lines are on stdout, both the one before a repo starts and
the one when it lands, since the table of what each repo did is the thing you ran
it for. `collect > log.txt` captures those and leaves the diagnostics on your
terminal.

The binary itself only ever exits 0 or 1. There is exactly one `os.Exit` in it,
in `cmd/repo-metrics/main.go`, and every failure goes through it, so there is no
per-error code to switch on. A non-zero status that is not 1 did not come from
the program: pipe a large report into something that stops reading, and SIGPIPE
kills it with nothing on stderr while your shell reports 141.

Exit 1 always leaves at least one line on stderr. `main` never prints the error
it is handed back, so every path that returns one has already explained itself,
including the paths you would not go looking for. A ctrl-C or a launchd `TERM`
part way through a collect exits 1 after a line like `stopping early, collection
was canceled after 1 of 2 repos`.

Two spots in that surface catch people out, so they get said plainly here rather
than left to be discovered.

`help`, `-h` and `--help` print the usage block to **stderr** and exit 0, so
`repo-metrics help > help.txt` writes an empty file. Usage is not an answer
anybody asked to pipe.

`version` with any argument at all exits 1, and that includes `-h`. Every other
subcommand's `-h` exits 0. It is there because a command that takes no flags must
not print a version and let you believe your flag was honored.

Running `repo-metrics` with no arguments at all prints the same usage block that
`help` prints, byte for byte, and exits 1 rather than 0. Asking for help is a
question that got answered; naming no command is a command that was not given.

A partial collection exits 0. Only a repo that failed outright, meaning it stored
nothing worth having, makes `collect` exit 1:

```
$ repo-metrics collect
collecting api (1 of 3)
api                          ok       80.0% of 2000 statements; collected coverage
collecting worker (2 of 3)
worker                       partial  80.0% of 800 statements; collected coverage; could not collect lint
collecting legacy (3 of 3)
legacy                       failed   could not collect coverage
$ echo $?
1
```

Those six output lines are stdout. Stderr got the diagnostics from the repos that
had any, and then `1 of 3 repos failed: legacy`. `worker` came back `partial` and
cost the run nothing: drop `legacy` from the config and the same collection exits
0, with `worker` still partial and still explaining itself on stderr. That is the
`;`-rather-than-`&&` rule from further up, seen from the other end.

| command | stdout | stderr | exit |
|---|---|---|---|
| `init` | `wrote PATH`, then a one-line next step | why it refused to write | 0, or 1 if the file is already there and you did not pass `--force` |
| `collect` | one line per repo as it starts, one more when it lands | each repo's diagnostics as they happen, then a line naming the repos that failed | 0 for any number of partials, 1 if a repo failed outright |
| `report` | the report, or `wrote PATH` when `--out` is set | why it refused | 0, or 1 on a bad flag value, an unknown `--repo`, an unwritable `--out`, or a config or database problem |
| `repos` | the table or the JSON | why it refused | 0, or 1 on a bad `--format` or a config or database problem |
| `history` | the table or the JSON | why it refused | 0, or 1 on a missing or unknown `--repo`, a bad `--signal`, `--format` or `--since`, or a config or database problem |
| `version` | two lines, the version and the toolchain it was built with | the complaint about the argument | 0, or 1 given any argument at all |
| `help`, `-h`, `--help` | nothing | the usage block | 0 |
| no arguments | nothing | the usage block, and nothing else | 1 |
| an unknown command | nothing | `unknown command "x"`, then the usage block | 1 |

`--format json` is pure JSON on stdout and nothing else. All three payloads go
through one encoder, which is why the same four things hold for each of them. The
document is written in a single call, so a failure part way through encoding
yields zero bytes rather than truncated JSON. It is not indented. HTML escaping
is off, so `<`, `>` and `&` arrive as themselves rather than as the `\u003c`,
`\u003e` and `\u0026` Go's encoder writes by default. And it ends with exactly
one newline.

For `report` that holds only while `--out` is empty. With `--out PATH` the report
goes to the file and stdout gets `wrote PATH`, so `--format json --out F` leaves
stdout holding a line of prose. `repos` and `history` have no `--out` flag at
all, so for those two it holds unconditionally.

Exit 0 with an empty stderr is not the success signal, and nothing should be
written that waits for one. A clean run routinely has things to say: a repo that
is not a git checkout, a module proxy nobody consulted, a signal that ran and
found nothing worth recording. Those are `warn` lines on stderr and the run still
exits 0. Read the exit code for whether it worked and stderr for what it noticed,
and do not let a wrapper script collapse the second into the first.

## How it decides what to tell you

Everything in this section is a first guess. The cutoffs below have reasoning
behind them and no calibration under them: no fleet has yet run long enough to
tell anyone whether they are the right numbers, and they were chosen to be easy
to argue with rather than to be defended. Read them as the current proposal.

The report leads with which repos moved, and says which measurements moved them.
Eight of the fourteen signals are allowed to nominate a repo: `coverage`,
`coverage_lines`, `tests`, `test_failures`, `untested_packages`,
`lint_findings`, `lint_errors` and `outdated_dependencies`. The other six are
collected, published and
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
Storing a percentage column would bake that error in permanently. It is also why
LCOV is the coverage format rather than Cobertura XML: LCOV records counts, while
Cobertura carries per-file rates and leaves the counts to be reconstructed from
them.

**Two units are never one number.** A Go profile counts statements and an LCOV
tracefile counts lines. They are separate keys under separate signals, which
makes summing them impossible rather than merely discouraged.

**It does not trust a file path.** If it runs a command for you, every artifact
the step names has to be newer than the command that supposedly produced it. This is not paranoia:
a real repo out there has a `make coverage-all` target that is declared `.PHONY`
with no rule, so it prints "Nothing to be done", exits 0, and writes nothing,
while a months-old coverage profile sits at the path it would have written to.
Anything that trusts exit code 0 reports that stale file as today's number
forever, and nothing anywhere logs an error.

**It records the toolchain it measured with.** Go workspaces can silently swap a
dependency between a local working tree and a pinned version, which changes
coverage for reasons that have nothing to do with your code. Snapshots carry a
fingerprint so you do not get to diff across that boundary without being told.

Which probe to run is derived from the formats a repo's steps declare, and a repo
running none of Go's gets `fingerprint:` on the repo instead — an argv whose
output becomes the fingerprint, like `["node", "--version"]`. A repo with
neither records that nothing identified it, and says so, rather than answering
with whatever `go env` happens to report on the collecting machine. That is not a
hypothetical improvement: `go env` succeeds from any directory, so a TypeScript
repo used to be fingerprinted with the ambient Go version — a string that cannot
move when its actual runtime does, and does move when somebody upgrades Go on the
collector.

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
  paired with the code that reads it. LCOV and JUnit XML were added that way, as
  an entry and a parser each — not a registry, not a plugin API, and not a shared
  object anyone has to build. The table is the reason a format cannot be accepted
  by the config and then read by nothing.
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
