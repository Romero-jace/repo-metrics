package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// migrationSteps is empty until there is a version 2, so the loop that runs it
// would otherwise ship untested and only be exercised the day someone needs it.
// This substitutes a fake step to prove the two things the loop must get right:
// a step above the stored version runs, and one at or below it does not.
//
// It is an internal test because migrationSteps is unexported on purpose. The
// extension point is not API.
//
// Both tests here swap the package-level migrationSteps and restore it in
// t.Cleanup. That is safe only because tests in a package run sequentially: do
// not add t.Parallel() to this file without giving the steps an injection point
// instead.
func TestApplySchemaRunsOnlyStepsAboveStoredVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	// Get a real, fully stamped database first.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := st.DB()
	defer func() { _ = st.Close() }()

	var ran []int
	record := func(v int) func(context.Context, *sql.Tx) error {
		return func(context.Context, *sql.Tx) error {
			ran = append(ran, v)
			return nil
		}
	}

	saved := migrationSteps
	t.Cleanup(func() { migrationSteps = saved })
	migrationSteps = []migrationStep{
		{version: 1, apply: record(1)},
		{version: 2, apply: record(2)},
		{version: 3, apply: record(3)},
	}

	ctx := context.Background()
	// The file is stamped at schemaVersion (1), so steps 2 and 3 are pending and
	// step 1 is already applied.
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		t.Fatalf("stamping: %v", err)
	}
	if err := applySchema(ctx, db, path); err != nil {
		t.Fatalf("applySchema: %v", err)
	}
	if len(ran) != 2 || ran[0] != 2 || ran[1] != 3 {
		t.Errorf("steps run from version 1: got %v, want [2 3] in order", ran)
	}

	// From 0 every step is pending, and they still run lowest first: applying a
	// later step before an earlier one is how a migration corrupts a database.
	ran = nil
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 0`); err != nil {
		t.Fatalf("stamping: %v", err)
	}
	if err := applySchema(ctx, db, path); err != nil {
		t.Fatalf("applySchema: %v", err)
	}
	if len(ran) != 3 || ran[0] != 1 || ran[1] != 2 || ran[2] != 3 {
		t.Errorf("steps run from version 0: got %v, want [1 2 3] in order", ran)
	}
}

// A step that fails must take the stamp down with it. A stamped-but-unmigrated
// database is worse than a failed open: every later run sees a current version,
// skips the work, and reads the wrong shape with no error anywhere.
func TestApplySchemaRollsBackTheStampWhenAStepFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := st.DB()
	defer func() { _ = st.Close() }()

	saved := migrationSteps
	t.Cleanup(func() { migrationSteps = saved })
	migrationSteps = []migrationStep{
		{version: 2, apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `THIS IS NOT SQL`)
			return err
		}},
	}

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 0`); err != nil {
		t.Fatalf("stamping: %v", err)
	}
	if err := applySchema(ctx, db, path); err == nil {
		t.Fatal("applySchema succeeded despite a failing step")
	}

	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if v != 0 {
		t.Errorf("user_version is %d after a failed step; the stamp must roll back with the work", v)
	}
}
