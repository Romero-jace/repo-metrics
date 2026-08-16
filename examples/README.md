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

5 6 * * *  /usr/local/bin/repo-metrics collect --config /srv/repo-metrics/repo-metrics.yaml ; /usr/local/bin/repo-metrics report --config /srv/repo-metrics/repo-metrics.yaml --window 7d --out /srv/repo-metrics/report.md
```

`;` between the two, not `&&`. `collect` exits 1 when any single repo failed,
deliberately, because it keeps going so one unreachable repo does not cost you
the other nine. Chained with `&&`, that same exit code hands one bad repo a veto
over the whole report. The plist ships `;` for the same reason and says so.

## 7d or 168h

Both work, and they mean the same thing. Every duration this tool reads goes
through one parser: Go's duration syntax plus a `w` suffix for weeks and a `d`
suffix for days, so `7d`, `168h`, `1w` and `1d12h` all parse.

That is true of the flags and of the config file alike. `window:`, `timeout:`
and `max_age:` in the file take exactly what `--window` and `--since` take.
Larger units come first, so `1w3d12h` reads the way `1h30m` does, while `3d1w`
is an error rather than a guess at which reading was meant.
