# Contributing

Thanks for looking. This is a small tool and the bar for a change is just that it
passes the gate and fits the style below.

The repository is public and CI runs on every pull request, so "the pull
request" below means one. It can still mean a commit message, since some
changes land as a direct commit to main. Where it says to paste command output
into one, paste it into whichever you are writing. The point is the output, not
the venue.

## Build and run the gate

```sh
make build     # ./bin/repo-metrics
make check     # build, vet, test, lint, in that order
```

`make check` is the pre-commit gate. Run it and paste the output in the pull
request rather than saying it passed.

CI runs those same four steps in the same order and then some, so a green
`make check` is necessary and not sufficient. Three differences are worth knowing
before a red CI surprises you:

- `make test` is `go test ./...`. CI runs `go test ./... -race`, so a data race
  fails there and passes here. If you touched anything concurrent, run
  `GOWORK=off go test ./... -race` yourself first.
- CI runs `go mod tidy -diff`, which `make check` does not. If it fails, run
  `make tidy` and commit the result. It is a real gate rather than tidiness for
  its own sake: this tool reads the direct-versus-indirect classification in
  `go.mod` to count outdated dependencies, so an untidy file makes the tool
  report a wrong number about its own repo.
- CI also produces a real coverage profile and a real `go test -json` stream and
  runs the two opt-in cross-check tests against them. Those tests skip in
  `make test`, which has nothing to hand them. To run them locally, point them
  at artifacts you made:

  ```sh
  GOWORK=off go test ./internal/collect/golang/ ./internal/delta/ ./cmd/repo-metrics/ \
    -coverpkg=./internal/collect/golang/... \
    -coverprofile=/tmp/coverage.out -json > /tmp/test-stream.json
  REPO_METRICS_TEST_PROFILE=/tmp/coverage.out \
  REPO_METRICS_TEST_PROFILE_DIR=$PWD \
  REPO_METRICS_TEST_JSON=/tmp/test-stream.json \
    GOWORK=off go test -count=1 ./internal/collect/golang/ \
    -run 'TestAgainstGoToolCover|TestTestJSONAgainstRealStream' -v
  ```

  `-count=1` matters: the test cache will otherwise hand back a pass from a run
  against a different profile.

  Each package in that selection earns its place. `internal/collect/golang` is
  the code under test. `cmd/repo-metrics` has no test files, which is what makes
  the toolchain emit the `[no test files]` marker the JSON cross-check counts.
  `internal/delta` links the package under test transitively, so its test binary
  re-emits every instrumented block and the profile arrives with more data lines
  than distinct blocks, 318 over 159 on the run this was last measured from.
  `TestAgainstGoToolCover` logs that pair, and it moves as the code does, so
  trust the run over this sentence. Without `internal/delta` exactly one binary
  emits counters, no block appears twice, and the dedup logic the cross-check
  exists to protect is never exercised: a profile like that passes the percentage
  comparison even with the merge replaced by a sum, because doubling both halves
  of a ratio changes nothing.

Individual targets are `build`, `test`, `vet`, `lint`, `fmt`, `tidy`, `clean`.

## GOWORK=off

The Makefile exports `GOWORK=off`, and you need it on any toolchain command you
run outside `make`:

```sh
GOWORK=off go test ./internal/delta/ -v
```

The reason: this module is meant to build standalone, and it is commonly checked
out next to a `go.work` that does not list it. When that happens every command
fails with `directory prefix . does not contain modules listed in go.work`, which
reads like a broken checkout and is not. Do not fix it by adding this module to
that workspace. Opting out is the fix, and it also keeps your local numbers
identical to CI's.

## Linting

`golangci-lint` is pinned to **v2.12.2**, in CI and in what you should install
locally. `.golangci.yml` is a v2 config file. Run it under v1 and it fails with a
message about your Go version being too low, which is not the problem and sends
you off debugging entirely the wrong thing. If you see that error, check
`golangci-lint --version` before anything else.

## House style

- **Standard library first.** There are exactly two direct dependencies,
  `modernc.org/sqlite` and `github.com/goccy/go-yaml`, and both are load-bearing.
  A new one needs a reason in the pull request that says what it buys that the
  standard library cannot. No CLI framework, no test framework, no assertion
  library. `flag` and `testing` are fine.
- **Tests live in an external test package**, `package foo_test`. It keeps the
  tests honest about what the package actually exports. Reach for an internal
  test only when the thing under test genuinely cannot be exercised from outside,
  and say so in the file.
- **Check every error.** `errcheck` is on. When ignoring one is deliberate, write
  it out: `_, _ = fmt.Fprint(w, ...)` or `defer func() { _ = f.Close() }()`.
- **US spelling.** `misspell` is on with the US locale, so "behavior", "labeled",
  "canceled".
- **Comments say why, not what.** If a line guards against a specific failure,
  name the failure. `// dedup blocks` is noise; `// under -coverpkg the same
  block appears once per test binary, so summing inflates the total` is the
  comment worth having.
- **No em dashes in anything a user reads.** Report output, help text, README.
  Commas, colons, or two sentences instead.
- `//nolint` needs both the specific linter and an explanation. `nolintlint` will
  reject a bare one.

## Adding a metric

New metrics go in as **counts, never a precomputed percentage**. A repo's
coverage is total covered statements over total statements, which is not the mean
of the per-package percentages, so a percentage column bakes that error into the
database permanently and makes correct rollups impossible afterward. Store the
numerator and the denominator, derive the rate at query time.

The `metrics` table is long and narrow on purpose: `(snapshot_id, metric_key,
scope, value)`. A new metric is a new `metric_key`, and `scope` is whatever
dimension it lives on: a package import path for coverage, a collection step's
name for lint findings and timings. Adding one should not need a migration. If
yours seems to, that is worth discussing in an issue before you write it.

`(snapshot_id, metric_key, scope)` is the primary key. Two rows colliding on it
fail the INSERT and roll back the whole snapshot, taking every other signal's
numbers with it, so a metric whose scope is not unique per snapshot needs a
scope that makes it so. That is why lint findings are filed under the step's
name: a repo can run golangci-lint and eslint as two steps, and repo-level rows
would collide.

## The marker contract

This is the load-bearing rule in the codebase and the one thing a registry entry
cannot check for itself, so it is written down here.

**A parser that looked at a signal must emit its marker key, even when the answer
is zero.** Presence of that row is the only thing separating "measured, and the
answer is none" from "nothing ever looked". Everything downstream reads presence
and never value, so a package covering none of its statements, a suite with zero
failures and a clean lint run all count as measured.

Get this wrong and the signal is either permanently unmeasured, or permanently
reported as a confident zero. The second is worse, and it is the bug this project
has found and fixed eight times.

There are three shapes in use, and the third is the one that gets copied wrong:

**One repo-level marker for a family.** `pkg.without_tests` is the marker for
five signals: test count, failures, skips, untested packages, and test time. They
come out of one parsed stream, so either the parser read the test output or it
did not. There is no state where it counted the passes but not the failures, and
they null and fill together.

**One scoped marker for a family.** `lint.findings` is the marker for the three
lint signals, at the step's scope rather than the repo's, for the collision
reason above. Same logic otherwise: one parsed log, one answer about whether it
was read.

**Separate markers for signals from one stream.** The three dependency signals
come out of a single `go list -m` stream and each has its OWN marker, which looks
redundant beside the two shapes above and is not. They are measurable under
different conditions: the module count needs nothing, the age aggregate needs the
modules to carry publish timestamps, and the outdated count needs the module
proxy to have actually been consulted. A shared marker would claim all three were
measured whenever any of them was, which would publish an unchecked proxy as
"zero outdated dependencies".

The rule of thumb: share a marker only when there is no possible input where one
of the signals is knowable and another is not. If you can describe such an input,
they need separate markers.

## The other half: what the scope names

A marker says something was measured. It does not say the same things were
measured on both sides of a comparison, and for a signal whose value is a sum
across scoped rows that is a second question with its own answer.

The distinction is what the scope column NAMES.

**The scope names the subject.** Coverage breaks down by package, and a package
appearing or vanishing is a real change in the repo. The report has designed
answers for it: culprit ranking, added and removed lists. These signals compare
freely.

**The scope names the apparatus.** Lint findings and collection time break down
by the collection step that produced them, because a repo can run two linters.
When that set changes, the sum covers something different, and subtracting it
from last week's is not a measurement of anything. A step that crashed did not
make the repo better; a step newly switched on did not make it worse. Both of
those shipped, and both are pinned now.

Signals in the second group set `ScopeSetMustMatch` on their registry entry.
`delta.Compare` then refuses the comparison when the sets differ, the report says
"not comparable" rather than printing a number, and the repo is not nominated as
a mover on the strength of it. Set it when the scope names how the measurement
was taken, and leave it off when the scope names what was measured.

The fingerprint is taken over the signal's MARKER key, not over the key its
extractor reads: `Side.measure` calls `s.scopeKey(sig.Marker)`. For every signal
that sets the flag today those are either the same key, or two keys one parser
writes together at one scope, so it is right by circumstance rather than by
construction. `lint_errors` and `lint_suppressed` fingerprint `lint.findings`
while summing `lint.errors` and `lint.suppressed`, and that holds only because
the SARIF parser writes all three keys under the same step's scope in one pass.
A signal whose marker and value keys could carry different scope sets would
fingerprint the wrong one, and then compare two sums that covered different
things, which is the exact failure the flag exists to refuse. If you set it,
check that the marker's scope set really is the value's.

A second rule sits next to that one and asks a different question, so a new
signal has to pick between them. `ScopeSetMustMatch` asks whether both sides
covered the same things. `PartialWhen` asks whether either side covered
everything it claims to, which a scope fingerprint cannot see: the contribution
that went missing leaves no row to be missing from the set. A package whose test
binary would not compile is exactly that shape. It is still a package, it still
has however many tests it has, and the stream carries no count for any of them,
so the repo's total is short by a number nobody knows.

`PartialWhen` names a repo-scoped metric key whose non-zero value says this
signal's value is a floor rather than a total. The five test signals set it to
`test.build_failed`, which the `go test -json` parser writes whenever the stream
parses, zero included. `delta.Compare` refuses the comparison when either side is
a floor, and the value is still published, because it is real: what it cannot do
is be subtracted from another week's. An absent row reads as zero, so a snapshot
written before the key existed still compares, which is the right answer for a
snapshot taken when nothing was checking. Nothing stops a signal setting both
flags, and none does today.

## Adding a signal

A signal is what the report publishes. It is not the same as a config `signals:`
entry, which is a collection step: one `go test` step yields six reported
signals, because the toolchain gives them up together.

Eight places. Four drift guards fail the build if you miss one of places 4 to 7,
and a fifth does if the signal takes a column in the every-repo table, so for
that stretch the list is a shortcut rather than the enforcement. Places 1 and 3
need no guard, because skipping either is not a thing you can do quietly: a
metric key nothing declares will not compile, and a signal with no registry entry
is not a signal. That leaves two of the eight genuinely unguarded, and they are
the two worth slowing down for.

Place 2 is one of them, which follows from the marker contract being the one
thing a registry entry cannot check for itself. Declare a marker key and forget
to make the parser write it, and no test in this repo notices: the fixtures that
stand in for collector output are hand written, so they satisfy a marker no
parser emits just as readily as one that is emitted. Confirmed by probe. A marker
key was pointed at a name nothing writes, every fixture updated to match, and
build, vet, the whole suite and the linter all stayed green while that signal
published null for a repo that genuinely had dozens. That is the
permanently-unmeasured half of the bug at the top of this file, reachable by
following this list and skipping only place 2. Place 8 is the other unguarded
one, for its own reason, given there.

1. `internal/collect/keys.go` holds the metric key, with a comment saying which one
   is the marker and under what conditions it is written.
2. Whatever parser in `internal/collect/parse.go` emits it. Emit the marker
   unconditionally once the parse succeeds.
3. `internal/delta/signal.go` holds the registry entry: id, label, unit, direction,
   marker, marker scope, extractor, whether it may nominate a repo as a mover,
   `ScopeSetMustMatch` if its scope names the apparatus rather than the subject,
   and `PartialWhen` if some input can leave its value a floor rather than a
   total.
4. `internal/report/view.go` needs a field on `RepoView`, a case in `group()`, and a
   line in `buildRepo`. Named fields rather than a map, because the field census
   collapses list-element paths and a map forces one value type on every entry.
   All three parts are guarded, by `TestEverySignalReachesTheWire` and
   `TestEverySignalIsPublishedUnderItsRegistryID` in
   `internal/report/wiring_test.go`, which also needs a row in its `signalFills`
   table. That table is hand-written for the same reason the censuses are:
   deriving it from the struct would be deriving it from the thing it guards.
5. `internal/report/degraded_test.go` needs three census entries (the group and its
   two measurements) and the pinned measurement set below them. A signal that
   also takes a column in the every-repo table needs a row in `pairedGroups` in
   the same file, and `TestEveryTableSignalIsPaired` is the fifth guard that
   fails without it: nothing else checks that a table signal's markdown cell and
   its JSON group agree about whether anything measured it. It also needs two more
   entries in `degradedColumns` at the top of that file, the value column and its
   change column, since that is a positional array of the table's columns in
   template order. The template builds those columns from the registry, so a sixth
   table signal widens every rendered row while the array stays the width it was,
   and `tableCells` stops the run with "row has 14 cells, want 12". That is a
   fixture being told about a change rather than a sixth guard, and it fails on
   every degraded row at once, so read the width in the message rather than
   hunting for a broken signal.
6. `internal/delta/signal_test.go` needs a fixture saying what the collector stores
   when it looked and found zero, and what it stores when it never looked. A
   signal claiming it has no reachable measured zero sets `zeroIsReachable: false`
   AND takes a row in the `zeroUnreachable` map above the fixtures, with the
   reason written out, or `TestNoSignalOptsOutOfTheMeasuredZeroCheckUnannounced`
   fails naming it. An empty reason fails too. The map is pinned empty today
   because nothing opts out, and the point of it is that the bool alone deletes
   an assertion silently while the subtest keeps reporting ok.
7. A fixture in `degraded_test.go` that renders the group filled at least once
   and null at least once, or the census reports it as never demonstrated
   nullable.
8. The docs, which nothing checks at all. `README.md` says "Fourteen signals" in
   prose, then again as "Eight of the fourteen" where it explains nominating, and
   then lists them in a table with one row each. `CHANGELOG.md` repeats the count
   and the names. The payload section of the README counts the keys on a repo
   row, currently "Twenty-four keys" with `error` as "the twenty-fifth", and the
   sample JSON right above it has to gain the key too.
   Every one of those is a number spelled as a word beside a registry that just
   moved, and every guard above stays green while all of them go stale. It is
   deliberately unguarded: a test asserting a word form, a numeral, a table row
   count and two key counts against the registry would be an odd test for this
   repo and would plausibly cost more than it protects. So grep for the count you
   are changing and fix all of it in the same commit.

### The extractor and the marker answer different questions

Item 3 hides a field with no guard behind it, and getting that one wrong
reproduces the bug at the top of this file exactly.

There are two extractors and they read different maps. `repoValue` reads
`Side.repoVal`, which `newSide` fills only from rows whose scope column is empty.
`sumOver` reads `Side.pkgSum`, which it fills only from rows that carry a scope.
Neither one falls back to the other. A key stored at one level and read at the
other is a lookup in a map that never had an entry for it, so it comes back 0,
and 0 is what gets published.

**The extractor has to match the scope its own value key is stored at. That is a
separate question from the marker's scope, and the two are independent.** The
marker only answers whether anything looked. It is often a different key
entirely, kept at a different level, and that is not a mistake to be fixed:
`tests` and `packages without tests` share `pkg.without_tests` at repo scope as
their marker, and then one of them reads `test.count` off the per-package rows
with `sumOver` while the other reads `pkg.without_tests` itself with `repoValue`.
Same marker, same `MarkerScope`, two opposite extractors, both correct. A rule
phrased as "the extractor must match `MarkerScope`" would call that pair a bug,
so do not carry that rule around.

Nothing catches the mistake. Pair `dependencies` with `sumOver(deps.total)`
instead of `repoValue(deps.total)`, change nothing else, and the whole suite is
green. `TestEverySignalDistinguishesZeroFromUnmeasured` passes, which is worth
sitting with, because it is the test written to catch this family of bug: its
measured-zero fixture for that signal stores `deps.total` with a value of 0, so
the correct extractor and the broken one both answer 0 and the assertion cannot
tell them apart, and its absent fixture stores no `deps.total` row at all, so the
marker is missing and the signal reads unmeasured either way. The fixtures are
built to exercise presence, and presence is not what broke.

Run the mispaired build against a real repo and it is unmistakable. Against this
checkout, whose build list `deps.total` counts in the dozens, the report came
back `"dependencies": {"value": 0, "delta": null}`, while `dependency_age`,
parsed out of the very same `go list -m -json` stream, reported a real median age
on the same row. The stream was demonstrably read, and the count still published
a confident zero. Check the extractor against the scope its key is written at, by
hand, because nothing downstream will.

Direction has no neutral option on purpose. A signal nobody will commit to a
direction for renders as "up" rather than "worse", and then a reader has to know
on their own whether rising numbers are bad news. Pick one.

`Nominates` is opt-in. A signal that moves on every run makes every repo a mover
every week and buries the ones that matter, so anything sensitive to machine load
or to the calendar should be reported and never lead. If it moves in proportion
to its own size, give it a `MinMoveFraction` rather than a fixed floor.

A signal that does nominate needs a real `MinMove`, and coverage is the wrong
entry to copy for it. Coverage leaves `MinMove: 0` because `nominate`
special-cases coverage by id and takes its floor from the config's
`min_repo_delta` instead, which is the field operators have always written.
Nothing else gets that treatment. Every other signal with a zero floor falls
through to the fallback below it, which is 1 in the signal's own unit. For a
count that is a sane default and reads like one. For a duration in milliseconds
it is no floor at all, and a nominating millisecond signal would lead the report
on a move of a few milliseconds. Nothing checks this, and no test in the repo
reads `MinMove`.

It takes three things at once, which is why it has not fired: `Nominates: true`,
a zero `MinMove` on a millisecond signal, and no `MinMoveFraction` either. Both
millisecond signals today fail two of the three, since they set
`Nominates: false` AND `MinMoveFraction: 0.25`, and that relative floor is
applied after the absolute one. So flipping `Nominates` alone on one of them
changes nothing, which is worth knowing before you conclude the floor works.
Measured on two snapshots seven minutes apart: with the fraction the repo is not
a mover, without it the same 5 millisecond move makes it lead the report.

The `repos` and `history` payloads carry their own censuses, in
`internal/report/payload_census_test.go`, separate from the report's because they
are separate wire contracts. A number added to either one lands outside any
nullable group, and a consumer writing `row.lint_findings ?? 0` turns an absent
measurement straight back into a measured zero, which is the bug at the top of
this file arriving through a door the report's census cannot see.

## Adding a format

A format is a name an operator writes in the config paired with the code that
reads it. Both halves land together or not at all: a format that validates but
cannot be parsed lets a step run a whole test suite and record silence, which is
exactly what this tool exists to refuse. `TestEveryConfiguredFormatHasAParser`
pins the two lists to each other in both directions.

1. `internal/config/format.go` holds the constant, an entry in `formats` with its
   policy, and an entry in `formatOrder`. All three, and nothing pins the map
   against the slice directly: `TestEveryConfiguredFormatHasAParser` catches a
   missing `formatOrder` entry only by count.
2. `internal/collect/parse.go` holds the parser and its entry in `parsers`. A
   parser reading a public format goes in its own package under
   `internal/collect/`, with no dependency on `collect`, so it stays a parser of
   that format rather than a piece of this tool. `sarif`, `junit` and `lcov` are
   the precedent.
3. `internal/collect/reposcoped_test.go` needs a row saying which repo-scoped
   keys the format writes, even when the answer is none.

The four policy fields are facts about the tools that emit a format rather than
preferences an operator holds, which is why they are here and not in the config
file.

- `NonZeroExitIsNormal` is true for anything whose producers exit non-zero to
  report findings, which is every linter.
- `Repeatable` is true only when two steps in one repo can both use it without
  their metric keys colliding, which requires the parser to scope its rows by
  step. `sarif` and `junit-xml` qualify; `lcov` does not, because its scope names
  a source file rather than the step that read it.
- `RepoScopedKeys` lists what the parser writes at repo scope, so validation can
  reject two steps that would write the same row. Per-format usage counting
  cannot see that, because the two steps may name different formats.
- `Toolchain` says whose output it is, which is what decides whether `go env` is
  worth running for a repo's fingerprint. Leave it empty for a format no single
  toolchain owns.

**Pick a sentinel nothing will ever implement** when a test needs an unknown
format name. `TestParserForRejectsAnUnknownFormat` used `lcov` and two config
tests used `junit-xml`; all three broke for reasons unrelated to what they were
testing on the day those became real. `not-a-real-format` is the one to copy.
