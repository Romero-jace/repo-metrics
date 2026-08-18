# Examples

- `repo-metrics.yaml` is a commented starter config showing both collection
  modes. Copy it, edit the paths, and point `--config` at your copy. It will not
  load unedited: every repo path is checked at load time and has to be a
  directory that exists.
- `repo-metrics-daily.sh` is the wrapper you actually want a scheduler to call. It
  takes a lock, backs the database up, collects, verifies what landed, and exits
  with a distinct code per failure. See "Why not just call collect" below.
- `com.repo-metrics.daily.plist` is a macOS launchd agent that runs that wrapper
  once a day.

## Why there is no built-in scheduler

`collect` does one pass and exits, which means anything that can run a command on
a timer already schedules it, you can always just run it by hand when you want a
number now, and it drops into a CI job with no extra thought. A daemon would add
a process to babysit and buy none of that.

## launchd, on macOS

```sh
cp examples/com.repo-metrics.daily.plist ~/Library/LaunchAgents/
# Edit the paths in the copy first. It points at /usr/local/bin/repo-metrics and
# at /Users/YOUR-USER/repo-metrics, which is a placeholder you have to replace by
# hand. That is the one manual step and there is no way around it: launchd
# expands neither ~ nor $HOME, so every path in the plist has to be spelled out,
# and no example can know your home directory.
mkdir -p /Users/YOUR-USER/repo-metrics/logs

launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.repo-metrics.daily.plist
launchctl print gui/$(id -u)/com.repo-metrics.daily     # confirm it is loaded
launchctl kickstart -k gui/$(id -u)/com.repo-metrics.daily   # run it now, do not wait for 6am
```

To remove it:

```sh
launchctl bootout gui/$(id -u)/com.repo-metrics.daily
rm ~/Library/LaunchAgents/com.repo-metrics.daily.plist
```

If nothing seems to happen, read
`/Users/YOUR-USER/repo-metrics/logs/repo-metrics.err.log`. That is what the
`StandardErrorPath` key in the plist is there for, so it moves if you put the
directory somewhere else. A launchd job that fails has nowhere else to tell you.

(`launchctl load` and `unload` still work and you will see them in older writeups.
`bootstrap` and `bootout` are the current spelling and give real error messages
when something is wrong, so prefer them.)

## cron, on Linux

Same thing, one line. This section keeps `/srv/repo-metrics` while the launchd
one above uses a home directory, and that is not an oversight: `/srv` is
FHS-standard on Linux and works, but on macOS it cannot be created at all. The
system volume there has been read-only since Catalina, so `mkdir -p /srv` fails
with "Read-only file system" even under sudo, until you add an entry to
`/etc/synthetic.conf` and reboot.

Absolute paths, again, and this time because cron gets a minimal `PATH` and will
not find a binary you installed somewhere interesting. The per-repo command in
your config inherits that same `PATH`, so if it starts with `go`, set a `PATH` at
the top of the crontab rather than hoping.

```
PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
RM_BIN=/usr/local/bin/repo-metrics
RM_REPORT_OUT=/srv/repo-metrics/report.md
RM_WINDOW=7d

5 6 * * *  /srv/repo-metrics/repo-metrics-daily.sh /srv/repo-metrics/repo-metrics.yaml
```

## Why not just call collect

Because a scheduled run is judged entirely by its exit status, and the tool's exit
status cannot carry what a scheduled run needs to know.

`collect` has two exit codes. It exits 1 when any repo's snapshot came back failed
— verified, not assumed: one bad repo out of ten is enough. And it exits 0 when a
repo merely degraded, so a repo that lost three signals but kept the rest is
invisible. Those two facts together mean exit 1 cannot tell "nothing works" from
"nine of ten are fine", and exit 0 cannot tell "all good" from "half the signals
went dark".

The older version of this file put `collect ; report` in the crontab, with `;`
rather than `&&` on the grounds that one unreachable repo should not veto the whole
report. That reasoning was right, and its price was that the job's status became
`report`'s alone — and `report` exits 0 unconditionally. **The job could not fail.**

The wrapper resolves that rather than reverting it: it decides the exit code from
what the database stored, runs the report regardless, then exits with the code it
decided. Codes are `1` nothing stored, `2` a repo failed, `3` the database is
corrupt, `4` the data is stale, `5` a run was already in progress. Two more come
from the preflight rather than from the data: `6` sqlite3 is not on PATH, and `7`
the `database:` value in the config is not literal text. Both exist because the
alternative was not a failure but a confident wrong answer — see the exit-code
table at the top of the script for what each one used to be misreported as.

It also does two things the tool deliberately does not do itself:

- **Backs up the database first**, with `sqlite3 .backup`. Snapshots cannot be
  re-collected — they measured a working tree at a commit that has moved on — and
  `cp` is not safe here, because WAL is on unconditionally and a plain copy can
  miss committed transactions still living in the `-wal` file.
- **Takes a lock**, with `mkdir` rather than `flock`, since `flock` is util-linux
  and is not present on macOS. The store opens with `busy_timeout` at 0, so a
  second process touching the database mid-collection gets an immediate "database
  is locked" rather than waiting.

## Schedule the staleness check separately

This is the one that needs saying out loud, because the obvious setup does not work.

A database nobody has added to for a month still produces a report with a fresh
`generated_at`, a full table of real numbers, "Nothing failed to collect", and exit
0. Every figure in it is true and a month old. The `last collected` column does
carry the real date, so a person reading the table can see it — but nothing fails,
and nothing in the problems section mentions it, so no automation ever will.

The wrapper checks for that. But the check lives inside the collection job, and a
job that never fires never runs its own checks either. So:

```sh
repo-metrics-daily.sh check /srv/repo-metrics/repo-metrics.yaml   # writes nothing, takes no lock
```

Run that from somewhere that does not share a fate with the collection job. A
second cron entry is better than nothing but dies with the same cron. Better is
another machine, or a line in your shell profile, or simply the thing you run
before you read a report. Complete coverage needs an external dead-man's switch —
something that alarms when it stops *hearing* from you — which is a third-party
service and so deliberately not wired in here.

## 7d or 168h

Both work, and they mean the same thing. Every duration this tool reads goes
through one parser: Go's duration syntax plus a `w` suffix for weeks and a `d`
suffix for days, so `7d`, `168h`, `1w` and `1d12h` all parse.

That is true of the flags and of the config file alike. `window:`, `timeout:`
and `max_age:` in the file take exactly what `--window` and `--since` take.
Larger units come first, so `1w3d12h` reads the way `1h30m` does, while `3d1w`
is an error rather than a guess at which reading was meant.
