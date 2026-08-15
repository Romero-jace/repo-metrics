# Examples

- `repo-metrics.yaml` is a commented starter config showing both collection
  modes. Copy it, edit the paths, and point `--config` at your copy. It will not
  load unedited: every repo path is checked at load time and has to be a
  directory that exists.
- `com.repo-metrics.daily.plist` is a macOS launchd agent that runs `collect` and
  then `report` once a day.

## Why there is no built-in scheduler

`collect` does one pass and exits, which means anything that can run a command on
a timer already schedules it, you can always just run it by hand when you want a
number now, and it drops into a CI job with no extra thought. A daemon would add
a process to babysit and buy none of that.

## launchd, on macOS

```sh
cp examples/com.repo-metrics.daily.plist ~/Library/LaunchAgents/
# edit the paths in the copy first: it points at /usr/local/bin/repo-metrics
# and /srv/repo-metrics, and launchd will not expand ~ or $HOME for you
mkdir -p /srv/repo-metrics/logs

launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.repo-metrics.daily.plist
launchctl print gui/$(id -u)/com.repo-metrics.daily     # confirm it is loaded
launchctl kickstart -k gui/$(id -u)/com.repo-metrics.daily   # run it now, do not wait for 6am
```

To remove it:

```sh
launchctl bootout gui/$(id -u)/com.repo-metrics.daily
rm ~/Library/LaunchAgents/com.repo-metrics.daily.plist
```

If nothing seems to happen, read `/srv/repo-metrics/logs/repo-metrics.err.log`.
That is what the `StandardErrorPath` key in the plist is there for. A launchd job
that fails has nowhere else to tell you.

(`launchctl load` and `unload` still work and you will see them in older writeups.
`bootstrap` and `bootout` are the current spelling and give real error messages
when something is wrong, so prefer them.)

## cron, on Linux

Same thing, one line. Absolute paths for the same reason as above: cron gets a
minimal `PATH` and will not find a binary you installed somewhere interesting.
The per-repo command in your config inherits that same `PATH`, so if it starts
with `go`, set a `PATH` at the top of the crontab rather than hoping.

```
PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin

5 6 * * *  /usr/local/bin/repo-metrics collect --config /srv/repo-metrics/repo-metrics.yaml ; /usr/local/bin/repo-metrics report --config /srv/repo-metrics/repo-metrics.yaml --window 7d --out /srv/repo-metrics/report.md
```

`;` between the two, not `&&`. `collect` exits 1 when any single repo failed,
deliberately, because it keeps going so one unreachable repo does not cost you
the other nine. Chained with `&&`, that same exit code hands one bad repo a veto
over the whole report. The plist ships `;` for the same reason and says so.

## 7d or 168h

Both work on `--window`, and they mean the same thing. The flag understands a
`d` suffix for days on top of Go's duration syntax, so `7d`, `168h`, and `1d12h`
all parse.

The duration fields inside the config file are not the same. `window:`,
`timeout:`, and `max_age:` go through Go's `time.ParseDuration`, whose largest
unit is the hour, so `7d` there is a load error rather than a silent fallback
and a week has to be written `168h`.
