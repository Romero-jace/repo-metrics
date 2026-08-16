# Contributing

Thanks for looking. This is a small tool and the bar for a change is just that it
passes the gate and fits the style below.

## Build and run the gate

```sh
make build     # ./bin/repo-metrics
make check     # build, vet, test, lint, in that order
```

`make check` is the pre-commit gate. Run it and paste the output in the pull
request rather than saying it passed.

CI runs those same four steps in the same order and then some, so a green
`make check` is necessary and not sufficient. Two differences are worth knowing
before a red CI surprises you:

- `make test` is `go test ./...`. CI runs `go test ./... -race`, so a data race
  fails there and passes here. If you touched anything concurrent, run
  `GOWORK=off go test ./... -race` yourself first.
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
  re-emits every instrumented block and the profile arrives with 282 data lines
  over 141 distinct spans. Without it exactly one binary emits counters, no span
  appears twice, and the dedup logic the cross-check exists to protect is never
  exercised: a profile like that passes the percentage comparison even with the
  merge replaced by a sum, because doubling both halves of a ratio changes
  nothing.

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

- **Standard library first.** There are exactly two dependencies,
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
has found and fixed nine times.

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

## Adding a signal

A signal is what the report publishes. It is not the same as a config `signals:`
entry, which is a collection step: one `go test` step yields six reported
signals, because the toolchain gives them up together.

Seven places, and four drift guards will fail the build if you miss one, so the
list is a shortcut rather than the enforcement:

1. `internal/collect/keys.go` holds the metric key, with a comment saying which one
   is the marker and under what conditions it is written.
2. Whatever parser in `internal/collect/parse.go` emits it. Emit the marker
   unconditionally once the parse succeeds.
3. `internal/delta/signal.go` holds the registry entry: id, label, unit, direction,
   marker, marker scope, extractor, whether it may nominate a repo as a mover,
   and `ScopeSetMustMatch` if its scope names the apparatus rather than the
   subject.
4. `internal/report/view.go` needs a field on `RepoView`, a case in `group()`, and a
   line in `buildRepo`. Named fields rather than a map, because the field census
   collapses list-element paths and a map forces one value type on every entry.
   All three parts are guarded, by `TestEverySignalReachesTheWire` and
   `TestEverySignalIsPublishedUnderItsRegistryID` in
   `internal/report/wiring_test.go`, which also needs a row in its `signalFills`
   table. That table is hand-written for the same reason the censuses are:
   deriving it from the struct would be deriving it from the thing it guards.
5. `internal/report/degraded_test.go` needs three census entries (the group and its
   two measurements) and the pinned measurement set below them.
6. `internal/delta/signal_test.go` needs a fixture saying what the collector stores
   when it looked and found zero, and what it stores when it never looked.
7. A fixture in `degraded_test.go` that renders the group filled at least once
   and null at least once, or the census reports it as never demonstrated
   nullable.

Direction has no neutral option on purpose. A signal nobody will commit to a
direction for renders as "up" rather than "worse", and then a reader has to know
on their own whether rising numbers are bad news. Pick one.

`Nominates` is opt-in. A signal that moves on every run makes every repo a mover
every week and buries the ones that matter, so anything sensitive to machine load
or to the calendar should be reported and never lead. If it moves in proportion
to its own size, give it a `MinMoveFraction` rather than a fixed floor.

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
   policy, and an entry in `formatOrder`.
2. `internal/collect/parse.go` holds the parser and its entry in `parsers`.

The two policy flags are facts about the tools that emit a format rather than
preferences an operator holds, which is why they are here and not in the config
file. `NonZeroExitIsNormal` is true for anything whose producers exit non-zero to
report findings, which is every linter. `Repeatable` is true only when two steps
in one repo can both use it without their metric keys colliding, which requires
the parser to scope its rows by step.
