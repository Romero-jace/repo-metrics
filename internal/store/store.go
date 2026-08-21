// Package store persists collection snapshots in SQLite.
//
// It is deliberately plain database/sql with hand-written queries. There is no
// ORM and no migration framework: the whole schema is one embedded, idempotent
// .sql file applied on every Open, versioned by PRAGMA user_version. See
// schema.go for the version guard.
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

// Open opens (creating if needed) the database at path and brings its schema up
// to date. It fails with ErrSchemaTooNew rather than opening a file written by a
// newer repo-metrics.
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
	if err := applySchema(context.Background(), db, path); err != nil {
		_ = db.Close()
		return nil, err
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

// SnapshotByID returns one of a repo's snapshots by its own id, or nil.
//
// Scoped to the repo rather than looked up globally, so an id copied from
// another repo's series cannot become this repo's baseline. Two repos' numbers
// are not comparable, and subtracting one from the other is arithmetic over
// unrelated code that would render as a confident delta.
//
// Failed snapshots are returned rather than skipped, unlike every selection
// query above. This one answers "the snapshot you named", and a caller who names
// one that stored nothing is owed that sentence rather than "no such snapshot".
func (s *Store) SnapshotByID(ctx context.Context, repoID, id int64) (*Snapshot, error) {
	return s.querySnapshot(ctx, `
		SELECT id, repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms
		FROM snapshots
		WHERE repo_id = ? AND id = ?`,
		repoID, id)
}

// SnapshotsByGitSHAPrefix returns every snapshot of a repo whose git sha starts
// with prefix, newest first.
//
// A list rather than one row, because an ambiguous prefix has to be reported as
// ambiguous. Quietly taking the newest match would compare against a commit the
// caller did not name, which is a wrong answer wearing the shape of a right one.
//
// Matched with substr rather than LIKE so that a prefix carrying % or _ cannot
// become a wildcard. Callers validate the prefix as hex, and a query that is
// only correct because its callers are careful is one refactor from being wrong.
// An empty git_sha, which is what a checkout that is not a git repo records,
// matches no non-empty prefix.
func (s *Store) SnapshotsByGitSHAPrefix(ctx context.Context, repoID int64, prefix string) ([]Snapshot, error) {
	if prefix == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms
		FROM snapshots
		WHERE repo_id = ? AND substr(git_sha, 1, ?) = ?
		ORDER BY collected_at DESC, id DESC`,
		repoID, len(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("store: listing snapshots by sha: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning snapshot: %w", err)
		}
		out = append(out, *snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing snapshots by sha: %w", err)
	}
	return out, nil
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
	snap, err := scanSnapshot(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: querying snapshot: %w", err)
	}
	return snap, nil
}

// rowScanner is the part of *sql.Row and *sql.Rows that scanSnapshot needs.
//
// It exists so the single-row readers and the SnapshotSeries row loop share one
// decoder. Two hand-copied decoders drift: the dirty-int-to-bool, status-string
// and duration-milliseconds conversions would have to be fixed twice, and the
// copy nobody remembered would keep returning plausible-looking wrong values
// rather than failing.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSnapshot decodes one snapshots row and converts its stored encodings back
// into Go types.
//
// It depends on the SELECT listing exactly these ten columns in exactly this
// order, which every snapshot query in this file does. A future query that
// reorders them still compiles and still scans, and returns a snapshot with the
// branch in the SHA, so keep the column list in step with this function.
//
// The error from Scan is returned unwrapped so callers can still recognize
// sql.ErrNoRows and decide for themselves whether an absent row is a failure.
func scanSnapshot(row rowScanner) (*Snapshot, error) {
	var (
		snap      Snapshot
		collected string
		dirty     int
		status    string
		ms        int64
	)
	if err := row.Scan(
		&snap.ID, &snap.RepoID, &collected, &snap.GitSHA, &snap.GitBranch,
		&dirty, &snap.Env, &status, &snap.Error, &ms,
	); err != nil {
		return nil, err
	}

	var err error
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

// SnapshotSeries returns every snapshot for a repo collected between from and
// to, both bounds inclusive, each paired with the metrics that snapshot stored.
//
// Failed snapshots and snapshots that stored no metrics are included on
// purpose. A point at which nothing was measured is itself the finding: it says
// the collection ran and came back with nothing, which is what a broken build
// looks like from here. Dropping such a point renders the outage as a straight
// line between the measurements on either side of it, so a chart shows a gap in
// the data as though the question was never asked during that week.
//
// That makes this deliberately the opposite of LatestSnapshot's status filter,
// and the two are not in conflict. LatestSnapshot is picking a single number to
// compare, so a snapshot with no numbers in it is useless and gets skipped.
// This method is drawing a series, where the run that produced no numbers is a
// point the reader has to see.
//
// The ordering is part of the contract, the same way MetricsFor's is: points
// ascend by collected_at with an id tiebreak, and the metrics within a point are
// ordered by (metric_key, scope). Callers render this directly, so an unstable
// order is a diff in the output for an unchanged database.
//
// An inverted range is an error, not an empty series. "No data" is a real
// answer here, and returning it for what is actually a caller bug would let a
// swapped pair of arguments read as a repo that was never collected.
func (s *Store) SnapshotSeries(ctx context.Context, repoID int64, from, to time.Time) ([]SnapshotMetrics, error) {
	if from.After(to) {
		return nil, fmt.Errorf("store: snapshot series for repo %d: range starts at %s, after it ends at %s",
			repoID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}

	// Two queries stitched in Go rather than one LEFT JOIN, and that is a
	// correctness choice rather than a style one.
	//
	// With the stitch, "this snapshot measured nothing" is the DEFAULT state of
	// a point: the snapshot is simply absent from the metrics map, so the repo
	// whose every run failed and which owns zero metric rows survives by
	// construction, with no predicate anywhere that has to stay correct.
	//
	// A LEFT JOIN gets the same answer only for as long as every metrics
	// predicate stays in the ON clause and every metric column stays scanned
	// through a nullable type. Move one predicate down to WHERE, which reads
	// like a harmless tidy-up, and the outer join collapses into an inner one
	// and that repo disappears from the report entirely, silently and exactly
	// when it most needs to be visible.
	//
	// It is also cheaper: a join fans all ten snapshot columns across every
	// metric row, and a six-month window has thousands of metric rows per
	// snapshot.
	//
	// No transaction wraps the two reads, for two reasons. InsertSnapshot
	// writes a snapshot and its metrics in a single transaction, so if query
	// one sees a snapshot then that transaction has committed and query two,
	// which runs after it, sees that snapshot's metrics too. The only
	// interleaving left is query two returning metrics for a snapshot inserted
	// after query one ran, and the stitch drops those because it walks the
	// snapshot slice. And it matters that we can skip it: Open caps the pool at
	// one connection, so a BeginTx here would hold the process's only
	// connection for the whole read.
	lo, hi := formatTime(from), formatTime(to)

	snapRows, err := s.db.QueryContext(ctx, `
		SELECT id, repo_id, collected_at, git_sha, git_branch, git_dirty, env, status, error, duration_ms
		FROM snapshots
		WHERE repo_id = ? AND collected_at >= ? AND collected_at <= ?
		ORDER BY collected_at, id`, repoID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("store: listing snapshots for repo %d: %w", repoID, err)
	}
	defer func() { _ = snapRows.Close() }()

	var out []SnapshotMetrics
	for snapRows.Next() {
		snap, err := scanSnapshot(snapRows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning snapshot: %w", err)
		}
		out = append(out, SnapshotMetrics{Snapshot: *snap})
	}
	if err := snapRows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing snapshots for repo %d: %w", repoID, err)
	}
	// Released explicitly, right here, because the next statement issues a
	// second query against a pool Open caps at one connection: open Rows hold
	// that connection, and a reader has to be able to see that the first result
	// set is finished before the second query asks for the only connection
	// there is. The deferred Close and the drained Next loop both already
	// handle it, so this is a no-op that states the constraint rather than a
	// guard against it.
	if err := snapRows.Close(); err != nil {
		return nil, fmt.Errorf("store: closing snapshot rows: %w", err)
	}

	metricRows, err := s.db.QueryContext(ctx, `
		SELECT m.snapshot_id, m.metric_key, m.scope, m.value
		FROM metrics m
		JOIN snapshots s ON s.id = m.snapshot_id
		WHERE s.repo_id = ? AND s.collected_at >= ? AND s.collected_at <= ?
		ORDER BY m.snapshot_id, m.metric_key, m.scope`, repoID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("store: listing metrics for repo %d: %w", repoID, err)
	}
	defer func() { _ = metricRows.Close() }()

	bySnapshot := make(map[int64][]Metric)
	for metricRows.Next() {
		var (
			snapshotID int64
			m          Metric
		)
		if err := metricRows.Scan(&snapshotID, &m.Key, &m.Scope, &m.Value); err != nil {
			return nil, fmt.Errorf("store: scanning metric: %w", err)
		}
		bySnapshot[snapshotID] = append(bySnapshot[snapshotID], m)
	}
	if err := metricRows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing metrics for repo %d: %w", repoID, err)
	}

	// Attach by snapshot id while walking the ordered slice. Keying on the id
	// rather than on position is what keeps a snapshot that owns no metrics
	// from shifting every later point's metrics onto the wrong run.
	for i := range out {
		out[i].Metrics = bySnapshot[out[i].Snapshot.ID]
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
