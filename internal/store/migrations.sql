-- repo-metrics schema.
--
-- Applied in full on every Open, so every statement is IF NOT EXISTS and
-- re-running is a no-op.
--
-- Three things here are load-bearing:
--
--   * Coverage is stored as STATEMENT COUNTS, never as a percentage. Repo
--     coverage is sum(covered)/sum(total), which is not the mean of the
--     per-package percentages. A coverage_pct column would bake that error
--     in permanently and make correct rollups impossible.
--
--   * metrics is long and narrow: (metric_key, scope, value). scope holds a
--     package import path today; it can hold a test name for flaky rates or a
--     linter name for lint findings later, with no schema change.
--
--   * Timestamps are TEXT in a FIXED-WIDTH UTC layout, because ordering is done
--     by SQL on the string. A variable-width layout like RFC3339Nano sorts
--     "12:00:00.5Z" before "12:00:00Z" ('.' < 'Z') and silently returns the
--     wrong snapshot as latest. See timeLayout in store.go.

CREATE TABLE IF NOT EXISTS repos (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    path       TEXT    NOT NULL,
    created_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshots (
    id           INTEGER PRIMARY KEY,
    repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    collected_at TEXT    NOT NULL,
    git_sha      TEXT    NOT NULL DEFAULT '',
    git_branch   TEXT    NOT NULL DEFAULT '',
    git_dirty    INTEGER NOT NULL DEFAULT 0,
    -- Toolchain fingerprint. Go workspaces can swap a dependency between a
    -- local working tree and a pinned version, which moves coverage for
    -- reasons unrelated to the code, so snapshots taken under different
    -- fingerprints must not be diffed silently.
    env          TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL,  -- ok | partial | failed
    error        TEXT    NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_snapshots_repo_time
    ON snapshots (repo_id, collected_at);

CREATE TABLE IF NOT EXISTS metrics (
    snapshot_id INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    metric_key  TEXT    NOT NULL,
    scope       TEXT    NOT NULL DEFAULT '',  -- package import path; '' is repo-level
    value       REAL    NOT NULL,
    PRIMARY KEY (snapshot_id, metric_key, scope)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_metrics_key_scope ON metrics (metric_key, scope);
