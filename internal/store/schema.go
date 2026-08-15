package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// schemaVersion is the schema shape this binary understands. It is stamped into
// the database with PRAGMA user_version, SQLite's built-in single integer, so no
// migration framework and no extra dependency is needed.
//
// Bump this whenever migrations.sql changes shape, and append the matching entry
// to migrationSteps below.
const schemaVersion = 1

// ErrSchemaTooNew reports a database written by a newer repo-metrics.
//
// This is the branch that actually bites. CREATE TABLE IF NOT EXISTS evolves
// nothing, so an older binary opening a newer file finds every statement a
// silent no-op and then reads the columns it expects rather than the ones that
// are there. That is the same silent-wrong-answer shape this tool exists to
// refuse, and the cost is not recoverable: today's coverage can be re-derived,
// last week's cannot be re-collected.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this repo-metrics")

// SchemaVersionError names both versions so the message says what to do rather
// than just that something is wrong.
type SchemaVersionError struct {
	Path string
	// Found is the version stamped in the file.
	Found int
	// Supported is the version this binary knows how to read.
	Supported int
}

func (e *SchemaVersionError) Error() string {
	return fmt.Sprintf(
		"store: %s has schema version %d but this repo-metrics only understands version %d: "+
			"a newer repo-metrics wrote this file, so upgrade rather than deleting it "+
			"(collection history cannot be re-collected)",
		e.Path, e.Found, e.Supported)
}

// Unwrap makes both errors.Is(err, ErrSchemaTooNew) and errors.As(err,
// &*SchemaVersionError) work on the same value.
func (e *SchemaVersionError) Unwrap() error { return ErrSchemaTooNew }

// migrationStep is one ordered schema change, applied when the stored version is
// below step.version. Version 1 is the base schema in migrations.sql and is not
// listed here, so this slice is empty until there is a version 2.
//
// Adding one is the whole extension point: bump schemaVersion to 2, then append
//
//	{version: 2, apply: func(ctx context.Context, tx *sql.Tx) error {
//		_, err := tx.ExecContext(ctx, `ALTER TABLE snapshots ADD COLUMN ...`)
//		return err
//	}},
//
// Keep entries in ascending version order; applySchema runs them in slice order.
type migrationStep struct {
	version int
	apply   func(ctx context.Context, tx *sql.Tx) error
}

var migrationSteps []migrationStep

// applySchema brings the database at path up to schemaVersion, or refuses.
//
// A stored version of 0 covers both a brand-new file and one written before this
// guard existed. Those need the same treatment: the base schema is version 1 and
// every statement in it is IF NOT EXISTS, so applying it is correct for a fresh
// file and a no-op for a pre-versioning one that already has the shape.
//
// The DDL, the steps, and the stamp all run in one transaction. Verified against
// modernc.org/sqlite: PRAGMA user_version participates in transactions there, so
// a rollback un-stamps it along with the DDL. That ordering is what stops a crash
// midway from leaving a stamped-but-unmigrated database, which would then be
// skipped by every future upgrade and read with the wrong shape forever.
func applySchema(ctx context.Context, db *sql.DB, path string) error {
	stored, err := readUserVersion(ctx, db)
	if err != nil {
		return err
	}
	if stored > schemaVersion {
		return &SchemaVersionError{Path: path, Found: stored, Supported: schemaVersion}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, migrationSQL); err != nil {
		return fmt.Errorf("store: applying schema: %w", err)
	}
	for _, step := range migrationSteps {
		if step.version <= stored {
			continue
		}
		if err := step.apply(ctx, tx); err != nil {
			return fmt.Errorf("store: applying schema step %d: %w", step.version, err)
		}
	}

	// PRAGMA values cannot be bound parameters in SQLite, so this interpolates.
	// The value is a compile-time int constant, never anything from outside.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("store: stamping schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing schema: %w", err)
	}
	return nil
}

func readUserVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: reading schema version: %w", err)
	}
	return v, nil
}
