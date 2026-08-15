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
  GOWORK=off go test ./internal/collect/golang/ ./cmd/repo-metrics/ \
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
dimension it lives on, a package import path today and plausibly a test name or a
linter name later. Adding one should not need a migration. If yours seems to,
that is worth discussing in an issue before you write it.
