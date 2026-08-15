# repo-metrics

Tracks code-quality metrics across a bunch of Git repos at once, keeps history in
SQLite, and tells you what got worse this week.

Coverage tools are per-repo and live in a tab nobody opens. Nobody reads a
dashboard daily. Everybody reads "coverage in repo X dropped 4 points, package Y
is why." That report is the whole point of this thing.

Self-hosted, one binary, no account, no SaaS.

## Status

Early. The scaffold builds and the design is settled. Collectors, storage, and
the report are being built out now. Not usable yet.

## How it will work

You give it a config listing the repos you care about:

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

Then you run it on whatever schedule you like:

```sh
repo-metrics collect                          # one pass, then exits
repo-metrics report --window 7d --out report.md
```

There is no daemon. `collect` does one pass and exits, so cron or launchd owns
the cadence and you can always just run it by hand:

```
0 6 * * *  repo-metrics collect && repo-metrics report --out /srv/report.md
```

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
- **No dashboard, no web server.** Static output only.
- **No plugin system.** There is a `Collector` interface so this does not stay
  Go-only forever, but one interface is not a registry and does not need to be.

## Building

The Makefile sets `GOWORK=off` because this module lives next to a Go workspace
it is deliberately not part of. It is meant to build standalone.

```sh
make build     # ./bin/repo-metrics
make check     # build, vet, test, lint
```

## License

Apache-2.0. See [LICENSE](LICENSE).
