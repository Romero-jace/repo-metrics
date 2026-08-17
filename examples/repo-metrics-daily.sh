#!/bin/sh
#
# repo-metrics-daily.sh — run a collection on a schedule and fail loudly.
#
# The tool itself has exactly two exit codes, 0 and 1, and a run where some repos
# degraded or went partial exits 0. That is defensible for a command a person
# invokes and reads the output of, and it is not enough for a cron or launchd job,
# where the exit status is the only thing anybody sees. This wrapper turns the
# things a scheduled run needs to notice into distinct non-zero exits, and adds
# the two operational levers the tool deliberately does not carry itself:
#
#   - A BACKUP. The database holds snapshots that cannot be re-collected, because
#     they measured a working tree at a commit that has moved on. Backing it up
#     with `cp` is not safe: WAL is on unconditionally, so a plain copy can miss
#     committed transactions still living in the -wal file. `sqlite3 .backup` uses
#     the online backup API and is safe against a concurrent writer.
#   - A LOCK. The store opens with busy_timeout at 0, so a second process touching
#     the database while a collection writes gets an immediate "database is
#     locked" rather than waiting. Two overlapping collections are the case that
#     matters: a slow run still going when the next one fires.
#
# EXIT CODES, so a mail subject or a launchd log line says which thing broke:
#
#   0  everything collected and stored
#   1  nothing was stored at all — the command could not get as far as measuring
#      anything: a config that will not load, a missing binary, an unopenable
#      database
#   2  it ran and measured, and at least one repo's snapshot came back failed
#   3  the database failed its integrity check
#   4  the newest snapshot is older than RM_MAX_AGE_HOURS — a collection has not
#      been landing, which is the failure the report itself never raises
#   5  another run holds the lock
#
# 1 and 2 are split because collect's own exit code cannot tell them apart, which
# was verified against the binary rather than assumed: ANY repo whose snapshot
# comes back failed makes collect exit 1, even when every other repo collected
# cleanly. So the useful distinction — "nothing works" versus "18 of 20 are fine"
# — has to come from the database, and that is why nothing here acts on collect's
# exit code until after the counts are read.
#
# A partial snapshot is named on stdout and does NOT fail the run. That is
# deliberate and it is a limitation rather than a preference: partial currently
# means "something exited non-zero OR a signal went dark", and a repo with one
# failing test is permanently in that state. Alarming on it would cry wolf nightly.
# When the tool separates those two, this is the line to change.
#
# USAGE
#
#   repo-metrics-daily.sh /path/to/repo-metrics.yaml         # back up, collect, verify, report
#   repo-metrics-daily.sh check /path/to/repo-metrics.yaml   # verify only, writes nothing
#
# THE CHECK MODE IS NOT OPTIONAL POLISH, and it needs its own schedule.
#
# The staleness test below cannot catch a dead scheduler from inside the
# collection job, because the collection job stores a fresh snapshot before it
# gets there — a job that never fires never runs its own checks either. That is
# not a thing to work around, it is a fact about where the check lives. So:
#
#   - Run the collect mode on whatever schedule you want data on.
#   - Run `check` from somewhere ELSE. A different launchd job is better than
#     nothing, but if it shares the scheduler with the collection then a dead
#     launchd takes both and you are back where you started. Better is a different
#     machine, a login shell hook, or the command you run before you read a report.
#   - Genuinely complete coverage needs an external dead-man's switch — a service
#     that alarms when it stops HEARING from you, rather than something here that
#     alarms when it sees a problem. Add a curl to a healthcheck ping URL at the
#     end of the collect mode if you want that; deliberately not wired in here,
#     since it means choosing a third-party service.
#
# Environment, all optional:
#
#   RM_BIN             path to the binary (default: repo-metrics, found on PATH)
#   RM_DB              path to the database (default: read from the config)
#   RM_BACKUP_DIR      where backups go (default: <db dir>/backups)
#   RM_BACKUP_KEEP     how many backups to keep (default: 14)
#   RM_MAX_AGE_HOURS   staleness threshold (default: 48)
#   RM_REPORT_OUT      where to write the markdown report (default: none)
#   RM_WINDOW          report window (default: the config's own)
#
# Run it by hand once before scheduling it. Every failure mode above prints what
# happened and why on stdout, which a scheduler will mail you.

set -eu

MODE=collect
if [ "${1:-}" = "check" ]; then
	MODE=check
	shift
fi

CONFIG="${1:-}"
if [ -z "$CONFIG" ]; then
	echo "usage: $0 [check] /path/to/repo-metrics.yaml" >&2
	exit 64
fi
if [ ! -f "$CONFIG" ]; then
	echo "config not found: $CONFIG" >&2
	exit 64
fi

RM_BIN="${RM_BIN:-repo-metrics}"
RM_BACKUP_KEEP="${RM_BACKUP_KEEP:-14}"
RM_MAX_AGE_HOURS="${RM_MAX_AGE_HOURS:-48}"

# The database path comes from the config unless it was given explicitly. The
# config's top level is flat and unknown keys are rejected at load, so the key is
# spelled exactly `database` and appears once at column zero — which is what makes
# reading it with sed safe rather than the usual mistake of parsing YAML by hand.
# Anything more nested than this should be passed in RM_DB instead.
if [ -z "${RM_DB:-}" ]; then
	RM_DB=$(sed -n 's/^database:[[:space:]]*//p' "$CONFIG" | head -1 | tr -d '"'"'")
fi
if [ -z "$RM_DB" ]; then
	echo "could not read a database path from $CONFIG, and RM_DB is not set" >&2
	exit 64
fi

DB_DIR=$(dirname "$RM_DB")
RM_BACKUP_DIR="${RM_BACKUP_DIR:-$DB_DIR/backups}"
LOCK_DIR="$DB_DIR/.repo-metrics-daily.lock"

say() { echo "[repo-metrics-daily] $*"; }

# ---------------------------------------------------------------------------
# Lock. mkdir rather than flock, because flock is util-linux and is NOT present
# on macOS — which is the platform the launchd example beside this file targets.
# mkdir is atomic on every POSIX filesystem and needs nothing installed.
#
# The PID inside is what makes a lock left behind by a killed run recoverable
# rather than permanent. A stale directory with a dead PID is removed once; a
# live PID means a real concurrent run and this exits 5.
# ---------------------------------------------------------------------------
acquire_lock() {
	if mkdir "$LOCK_DIR" 2>/dev/null; then
		echo $$ > "$LOCK_DIR/pid"
		return 0
	fi
	holder=$(cat "$LOCK_DIR/pid" 2>/dev/null || echo "")
	if [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; then
		say "another run is in progress (pid $holder); doing nothing"
		exit 5
	fi
	say "removing a stale lock left by pid ${holder:-unknown}"
	rm -rf "$LOCK_DIR"
	if mkdir "$LOCK_DIR" 2>/dev/null; then
		echo $$ > "$LOCK_DIR/pid"
		return 0
	fi
	say "could not take the lock at $LOCK_DIR"
	exit 5
}
# ---------------------------------------------------------------------------
# The two checks both modes run. Defined before either path so there is exactly
# one implementation of each and check mode cannot drift from collect mode.
# ---------------------------------------------------------------------------

# check_integrity distinguishes a corrupted database from a healthy one, which
# nothing else does: the tool reads a corrupt file as "status: not collected" with
# an empty problems list and exit 0, byte-indistinguishable from a fresh install.
check_integrity() {
	integrity=$(sqlite3 "$RM_DB" "PRAGMA integrity_check;" 2>&1 || echo "check failed to run")
	if [ "$integrity" != "ok" ]; then
		say "DATABASE INTEGRITY CHECK FAILED:"
		echo "$integrity"
		say "the newest backup is in $RM_BACKUP_DIR"
		exit 3
	fi
}

# check_freshness is the assertion the report itself will not make.
#
# Head selection has no age floor and the problems list keys on the newest
# snapshot's STATUS rather than its age, so a database nobody has added to for a
# month still produces a report with a fresh generated_at, a full table of real
# numbers, "Nothing failed to collect", and exit 0. Every figure in it is true and
# a month old, and nothing in the output says so.
#
# In collect mode this catches a run that completed and stored nothing. Catching a
# run that never happened is what check mode is for, and why it has to be
# scheduled separately — see the note at the top of this file.
check_freshness() {
	age_hours=$(sqlite3 "$RM_DB" \
		"select cast((julianday('now') - julianday(max(collected_at))) * 24 as int) from snapshots;" 2>/dev/null || echo "")
	if [ -z "$age_hours" ]; then
		say "there are no snapshots in $RM_DB at all"
		exit 4
	fi
	if [ "$age_hours" -gt "$RM_MAX_AGE_HOURS" ]; then
		say "STALE: the newest snapshot is ${age_hours}h old, past the ${RM_MAX_AGE_HOURS}h threshold."
		say "A report built from this will look current and describe the past. Check the schedule."
		exit 4
	fi
	say "freshest snapshot is ${age_hours}h old, within the ${RM_MAX_AGE_HOURS}h threshold"
}

# ---------------------------------------------------------------------------
# Check mode stops here. It takes no lock and writes nothing, so it is safe to run
# at any time, including while a collection is in progress.
# ---------------------------------------------------------------------------
if [ "$MODE" = check ]; then
	if [ ! -f "$RM_DB" ]; then
		say "no database at $RM_DB"
		exit 4
	fi
	check_integrity
	check_freshness
	say "ok"
	exit 0
fi

acquire_lock
trap 'rm -rf "$LOCK_DIR"' EXIT INT TERM

# ---------------------------------------------------------------------------
# Backup, before anything writes. Skipped on the very first run, when there is
# nothing to back up yet.
# ---------------------------------------------------------------------------
if [ -f "$RM_DB" ]; then
	mkdir -p "$RM_BACKUP_DIR"
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	backup="$RM_BACKUP_DIR/metrics-$stamp.db"
	if sqlite3 "$RM_DB" ".backup '$backup'"; then
		say "backed up to $backup"
	else
		# Not fatal. A failed backup must not stop the collection that would
		# have produced today's data, and the integrity check below is what
		# actually protects the file.
		say "WARNING: backup failed; continuing"
	fi

	# Retention. Newest first, drop everything past the keep count. The `|| true`
	# covers the case where there is nothing to delete, since set -e would
	# otherwise treat an empty xargs as failure on some systems.
	# shellcheck disable=SC2012
	ls -t "$RM_BACKUP_DIR"/metrics-*.db 2>/dev/null \
		| tail -n "+$((RM_BACKUP_KEEP + 1))" \
		| while IFS= read -r old; do rm -f "$old"; done || true
fi

# The highest snapshot id before this run, so the checks afterwards can look at
# what THIS run stored rather than at the whole history. Empty on a fresh
# database, hence the coalesce to 0.
before_max=0
if [ -f "$RM_DB" ]; then
	before_max=$(sqlite3 "$RM_DB" "select coalesce(max(id), 0) from snapshots;" 2>/dev/null || echo 0)
fi

# ---------------------------------------------------------------------------
# Collect.
#
# A non-zero exit here is not enough to tell apart the two things that produce it,
# so the exit code is not acted on until the database has been asked what landed.
# Verified against the binary rather than assumed: ANY repo whose snapshot comes
# back failed makes collect exit 1, even when every other repo collected fine. So
# exit 1 alone cannot distinguish "the command could not run at all" from "18 of 20
# repos are fine and two failed", and those want different reactions.
#
# What exits 0 and should not is a DEGRADED or PARTIAL run: collectOne only errors
# on a failed snapshot, so a repo that lost three signals but kept the rest is
# invisible to the exit code. That is the gap the counts below close.
# ---------------------------------------------------------------------------
collect_rc=0
"$RM_BIN" collect --config "$CONFIG" || collect_rc=$?

if [ ! -f "$RM_DB" ]; then
	say "collect exited $collect_rc and there is no database at $RM_DB"
	exit 1
fi

check_integrity

# ---------------------------------------------------------------------------
# What this run actually stored, which is the thing the exit code cannot say.
# ---------------------------------------------------------------------------
failed=$(sqlite3 "$RM_DB" "select count(*) from snapshots where id > $before_max and status = 'failed';")
partial=$(sqlite3 "$RM_DB" "select count(*) from snapshots where id > $before_max and status = 'partial';")
stored=$(sqlite3 "$RM_DB" "select count(*) from snapshots where id > $before_max;")

say "stored $stored snapshot(s): $failed failed, $partial partial"

# Nothing landed at all. The command did not get as far as measuring anything —
# a config that would not load, a missing binary, a database it cannot open.
if [ "$stored" -eq 0 ]; then
	say "collect exited $collect_rc and stored nothing"
	exit 1
fi

# Detail lines for anything that did not come back clean, so the mail a scheduler
# sends names the repo and the reason rather than only a number.
if [ "$failed" -gt 0 ] || [ "$partial" -gt 0 ]; then
	sqlite3 "$RM_DB" \
		"select '  ' || s.status || ': ' || r.name || ' - ' || coalesce(nullif(s.error, ''), 'no detail') \
		 from snapshots s join repos r on r.id = s.repo_id \
		 where s.id > $before_max and s.status <> 'ok' order by s.status, r.name;"
fi

# Against the wall clock rather than against this run, so a collect that exited 0
# and stored only snapshots for repos that all failed does not pass quietly.
check_freshness

# The exit code is decided here and applied at the very end, so the report still
# runs for the repos that did work. Deciding it before the report is the point:
# the launchd example this file replaces joined collect and report with a
# semicolon, which made the job's status report's, and report's is always 0.
final=0
if [ "$failed" -gt 0 ]; then
	final=2
elif [ "$collect_rc" -ne 0 ]; then
	# Collect complained, nothing came back failed, and the database is fresh and
	# intact. Worth surfacing rather than swallowing, since it means the exit code
	# and the stored state disagree and this script has the wrong model of one of
	# them.
	say "NOTE: collect exited $collect_rc but no snapshot came back failed"
	final=2
fi

# A partial run does NOT fail. Deliberate, and a limitation rather than a
# preference: partial currently means "something exited non-zero OR a signal went
# dark", so a repo with one failing test is permanently in that state and alarming
# on it would cry wolf nightly. When the tool separates those two, this is the
# line to change.

# ---------------------------------------------------------------------------
# Report, last.
# ---------------------------------------------------------------------------
if [ -n "${RM_REPORT_OUT:-}" ]; then
	if [ -n "${RM_WINDOW:-}" ]; then
		"$RM_BIN" report --config "$CONFIG" --window "$RM_WINDOW" --out "$RM_REPORT_OUT" || say "WARNING: report failed"
	else
		"$RM_BIN" report --config "$CONFIG" --out "$RM_REPORT_OUT" || say "WARNING: report failed"
	fi
	[ -f "$RM_REPORT_OUT" ] && say "report written to $RM_REPORT_OUT"
fi

if [ "$final" -eq 0 ]; then
	say "ok"
fi
exit "$final"
