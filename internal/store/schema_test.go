package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/store"

	// Needed for rawOpen, which has to read a file that store.Open refuses.
	_ "modernc.org/sqlite"
)

// rawOpen bypasses store.Open. It exists only so a test can inspect a database
// this binary is supposed to refuse.
func rawOpen(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// wantStampedVersion is store.schemaVersion, which is unexported. Bumping the
// constant is supposed to break this, because a bump without a migration step is
// the mistake worth catching.
const wantStampedVersion = 2

// userVersion reads the stamp straight off the file, which is the only thing
// that proves the stamp was actually written rather than inferred.
func userVersion(t *testing.T, st *store.Store) int {
	t.Helper()
	var v int
	if err := st.DB().QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	return v
}

// setUserVersion stamps a version by hand, standing in for a database written by
// some other build of repo-metrics.
func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open before stamping %d: %v", v, err)
	}
	// PRAGMA values cannot be bound parameters, hence the switch on a literal
	// rather than a placeholder.
	switch v {
	case 0:
		_, err = st.DB().ExecContext(context.Background(), `PRAGMA user_version = 0`)
	case 99:
		_, err = st.DB().ExecContext(context.Background(), `PRAGMA user_version = 99`)
	default:
		t.Fatalf("setUserVersion: unhandled version %d", v)
	}
	if err != nil {
		t.Fatalf("stamping user_version = %d: %v", v, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close after stamping: %v", err)
	}
}

// Branch one: a fresh database gets the schema AND the stamp. Without the stamp
// the file is indistinguishable from a pre-versioning one forever after.
func TestOpenFreshDatabaseStampsSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if got := userVersion(t, st); got != wantStampedVersion {
		t.Errorf("user_version after a fresh Open: got %d, want %d", got, wantStampedVersion)
	}

	// The schema is really there, not just the stamp.
	if _, err := st.UpsertRepo(context.Background(), "svc", "/svc"); err != nil {
		t.Errorf("UpsertRepo on a freshly opened store: %v", err)
	}
}

// Branch two: a database stamped below current is upgraded in place and KEEPS
// ITS ROWS. Data preservation is the entire justification for this unit. You can
// re-derive today's coverage; you cannot re-collect last week's, so "delete the
// database and start over" throws away the only irreplaceable thing here.
func TestOpenUpgradesOlderDatabaseAndPreservesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	ctx := context.Background()
	collected := at(2026, time.August, 8, 12, 0, 0, 0)

	// Write history, then rewind the stamp so the next Open sees an old file.
	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	repoID, err := first.UpsertRepo(ctx, "svc", "/svc")
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if _, err := first.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: collected, Status: store.StatusOK,
	}, []store.Metric{
		{Key: "coverage.stmt.covered", Scope: "svc/internal/a", Value: 40},
		{Key: "coverage.stmt.total", Scope: "svc/internal/a", Value: 50},
	}); err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	setUserVersion(t, path, 0)

	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening a version-0 database: %v", err)
	}
	defer func() { _ = second.Close() }()

	// Half one: the upgrade ran and stamped. Without this assertion the test
	// passes against an Open that does no versioning at all, since re-applying
	// idempotent DDL preserves rows all by itself.
	if got := userVersion(t, second); got != wantStampedVersion {
		t.Errorf("user_version after upgrading a version-0 database: got %d, want %d",
			got, wantStampedVersion)
	}

	// Half two: the history survived it.
	repos, err := second.Repos(ctx)
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "svc" {
		t.Fatalf("upgrade lost the repo rows: got %+v", repos)
	}

	snap, err := second.LatestSnapshot(ctx, repos[0].ID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("upgrade dropped the snapshot, which is the history the tool exists to keep")
	}
	if !snap.CollectedAt.Equal(collected) {
		t.Errorf("CollectedAt: got %s, want %s", snap.CollectedAt, collected)
	}

	metrics, err := second.MetricsFor(ctx, snap.ID)
	if err != nil {
		t.Fatalf("MetricsFor: %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("upgrade lost metrics: got %d, want 2 (%+v)", len(metrics), metrics)
	}
	if metrics[0].Value != 40 || metrics[1].Value != 50 {
		t.Errorf("metric values changed across the upgrade: got %+v", metrics)
	}
}

// Branch three: a database stamped above current must be refused. This is the
// one nothing guarded before. CREATE TABLE IF NOT EXISTS evolves nothing, so an
// older binary opening a newer file gets a silent no-op on every statement and
// then reads the columns it expects instead of the ones present.
func TestOpenRefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	setUserVersion(t, path, 99)

	st, err := store.Open(path)
	if err == nil {
		_ = st.Close()
		t.Fatal("Open accepted a database stamped newer than this binary; it would read the wrong columns silently")
	}
	if st != nil {
		t.Error("Open returned a usable store alongside an error")
	}
	if !errors.Is(err, store.ErrSchemaTooNew) {
		t.Errorf("error does not match ErrSchemaTooNew: %v", err)
	}

	// Both versions have to be named, or the message cannot tell anyone which
	// way the mismatch runs or which binary to upgrade. Asserting the typed
	// fields rather than substrings, since "1" appears in almost any message.
	var verErr *store.SchemaVersionError
	if !errors.As(err, &verErr) {
		t.Fatalf("want a *store.SchemaVersionError, got %T: %v", err, err)
	}
	if verErr.Found != 99 {
		t.Errorf("Found: got %d, want 99", verErr.Found)
	}
	if verErr.Supported != wantStampedVersion {
		t.Errorf("Supported: got %d, want %d", verErr.Supported, wantStampedVersion)
	}
	if verErr.Path != path {
		t.Errorf("Path: got %q, want %q", verErr.Path, path)
	}
}

// Refusing must not rewrite anything. If the guard stamped or migrated on its
// way out, the newer binary that owns this file would find it corrupted.
func TestRefusedOpenLeavesTheFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	setUserVersion(t, path, 99)

	if _, err := store.Open(path); err == nil {
		t.Fatal("Open accepted an over-stamped database")
	}

	// Reading the stamp back needs a raw handle, since Open itself refuses now.
	db, err := rawOpen(path)
	if err != nil {
		t.Fatalf("reopening raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	var v int
	if err := db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if v != 99 {
		t.Errorf("refused Open changed user_version to %d; it must not touch the file", v)
	}
}
