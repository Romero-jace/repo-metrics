// Package store persists collection snapshots in SQLite.
//
// It is deliberately plain database/sql with hand-written queries. There is no
// ORM and no migration framework: the whole schema is one embedded, idempotent
// .sql file applied on every Open.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver, so the binary builds with CGO_ENABLED=0 and
	// cross-compiles. Note the driver name is "sqlite", not "sqlite3".
	_ "modernc.org/sqlite"
)

//go:embed migrations.sql
var migrationSQL string

// timeLayout is a FIXED-WIDTH UTC timestamp layout.
//
// Ordering is done by SQL on the stored string, so the width has to be
// constant. time.RFC3339Nano trims trailing zeros, which makes
// "2026-08-15T12:00:00.5Z" sort before "2026-08-15T12:00:00Z" (because '.' is
// 0x2E and 'Z' is 0x5A) and quietly returns the wrong row as the latest
// snapshot. Always format times through formatTime, which normalizes to UTC.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// Store is a handle on the metrics database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: creating %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}

	// SQLite is a single-writer database. Capping the pool at one connection
	// is what avoids "database is locked" under concurrent collection, and it
	// also keeps the PRAGMAs below applied to the connection we actually use.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: setting pragmas: %w", err)
	}
	if _, err := db.Exec(migrationSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: applying schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle. It exists for tests and ad-hoc queries;
// production code should use the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// UpsertRepo returns the id for name, inserting it if new and refreshing the
// recorded path if the checkout has moved. Name is the identity, not path.
func (s *Store) UpsertRepo(ctx context.Context, name, path string) (int64, error) {
	if name == "" {
		return 0, errors.New("store: empty repo name")
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO repos (name, path, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET path = excluded.path
		RETURNING id`,
		name, path, formatTime(time.Now()),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upserting repo %q: %w", name, err)
	}
	return id, nil
}

// Repos lists tracked repositories, ordered by name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, path, created_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Repo
	for rows.Next() {
		var (
			r       Repo
			created string
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.Path, &created); err != nil {
			return nil, fmt.Errorf("store: scanning repo: %w", err)
		}
		if r.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing repos: %w", err)
	}
	return out, nil
}

// InsertSnapshot writes a snapshot and its metrics as one transaction.
//
// The atomicity is the point: a snapshot whose metrics were only partly written
// reads later as a real measurement that happens to be missing packages, which
// is indistinguishable from a genuine coverage drop.
func (s *Store) InsertSnapshot(ctx context.Context, snap Snapshot, metrics []Metric) (int64, error) {
	if snap.Status == "" {
		return 0, errors.New("store: snapshot has no status")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots
			(repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.RepoID, formatTime(snap.CollectedAt), snap.GitSHA, snap.GitBranch,
		boolToInt(snap.GitDirty), snap.Env, string(snap.Status), snap.Error,
		snap.Duration.Milliseconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("store: inserting snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reading snapshot id: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO metrics (snapshot_id, metric_key, scope, value) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: preparing metric insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, m := range metrics {
		if _, err := stmt.ExecContext(ctx, id, m.Key, m.Scope, m.Value); err != nil {
			return 0, fmt.Errorf("store: inserting metric %s/%s: %w", m.Key, m.Scope, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: committing snapshot: %w", err)
	}
	return id, nil
}

// LatestSnapshot returns the most recent usable snapshot for a repo, or nil if
// there is none. Failed snapshots are skipped because they carry no numbers.
//
// Nil from here means "no numbers", which is not the same as "never ran": a repo
// whose every collection failed also gets nil. Use LatestSnapshotAny when the
// caller has to tell those two apart.
func (s *Store) LatestSnapshot(ctx context.Context, repoID int64) (*Snapshot, error) {
	return s.querySnapshot(ctx, `
		SELECT id, repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms
		FROM snapshots
		WHERE repo_id = ? AND status != ?
		ORDER BY collected_at DESC
		LIMIT 1`,
		repoID, string(StatusFailed))
}

// LatestSnapshotAny returns the most recent snapshot for a repo whatever its
// status, or nil only when the repo has never been collected at all.
//
// This does not replace LatestSnapshot and must not be used to pick a head or a
// baseline for the delta: a failed run stores no metrics, so comparing against
// one reports a total coverage cliff this week and a full recovery next week,
// neither of which happened. It exists so a caller can say "every run failed"
// instead of "never ran", which are opposite instructions to the reader.
func (s *Store) LatestSnapshotAny(ctx context.Context, repoID int64) (*Snapshot, error) {
	// The id tiebreak matters: two runs can land in the same collected_at, and
	// without it SQLite is free to return either, so the same database would
	// report a repo as failed or as ok depending on the query plan.
	return s.querySnapshot(ctx, `
		SELECT id, repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms
		FROM snapshots
		WHERE repo_id = ?
		ORDER BY collected_at DESC, id DESC
		LIMIT 1`,
		repoID)
}

// SnapshotAtOrBefore returns the most recent usable snapshot for a repo taken
// at or before cutoff, or nil if there is none. This is baseline selection: the
// boundary is inclusive, and failed snapshots are skipped.
func (s *Store) SnapshotAtOrBefore(ctx context.Context, repoID int64, cutoff time.Time) (*Snapshot, error) {
	return s.querySnapshot(ctx, `
		SELECT id, repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms
		FROM snapshots
		WHERE repo_id = ? AND status != ? AND collected_at <= ?
		ORDER BY collected_at DESC
		LIMIT 1`,
		repoID, string(StatusFailed), formatTime(cutoff))
}

func (s *Store) querySnapshot(ctx context.Context, query string, args ...any) (*Snapshot, error) {
	var (
		snap      Snapshot
		collected string
		dirty     int
		status    string
		ms        int64
	)
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&snap.ID, &snap.RepoID, &collected, &snap.GitSHA, &snap.GitBranch,
		&dirty, &snap.Env, &status, &snap.Error, &ms,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: querying snapshot: %w", err)
	}
	if snap.CollectedAt, err = parseTime(collected); err != nil {
		return nil, err
	}
	snap.GitDirty = dirty != 0
	snap.Status = Status(status)
	snap.Duration = time.Duration(ms) * time.Millisecond
	return &snap, nil
}

// MetricsFor returns a snapshot's metrics ordered by (key, scope). The ordering
// is part of the contract so that report output is deterministic.
func (s *Store) MetricsFor(ctx context.Context, snapshotID int64) ([]Metric, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT metric_key, scope, value
		FROM metrics
		WHERE snapshot_id = ?
		ORDER BY metric_key, scope`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("store: listing metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Metric
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.Key, &m.Scope, &m.Value); err != nil {
			return nil, fmt.Errorf("store: scanning metric: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing metrics: %w", err)
	}
	return out, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parsing timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
