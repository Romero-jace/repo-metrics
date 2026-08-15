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

```
5 6 * * *  /usr/local/bin/repo-metrics collect --config /srv/repo-metrics/repo-metrics.yaml && /usr/local/bin/repo-metrics report --config /srv/repo-metrics/repo-metrics.yaml --window 168h --out /srv/repo-metrics/report.md
```

Note `168h` rather than `7d`. Durations use Go's syntax, which has no day unit.
