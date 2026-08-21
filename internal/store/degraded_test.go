package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/store"
)

// boolPtr is the three-state value this column exists to carry.
func boolPtr(v bool) *bool { return &v }

// All three states of degraded survive a write and a read.
//
// The one that matters is nil. A snapshot that never recorded whether its run
// was degraded has to come back saying so, because the alternative is claiming
// its numbers were taken cleanly, which is a measurement nobody made.
func TestDegradedRoundTripsInAllThreeStates(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	repoID, err := st.UpsertRepo(ctx, "api", "/repos/api")
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}

	for _, tc := range []struct {
		name string
		want *bool
	}{
		{"under protest", boolPtr(true)},
		{"clean", boolPtr(false)},
		{"never recorded", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := st.InsertSnapshot(ctx, store.Snapshot{
				RepoID:      repoID,
				CollectedAt: time.Now(),
				Status:      store.StatusOK,
				Degraded:    tc.want,
			}, nil)
			if err != nil {
				t.Fatalf("InsertSnapshot: %v", err)
			}
			got, err := st.SnapshotByID(ctx, repoID, id)
			if err != nil {
				t.Fatalf("SnapshotByID: %v", err)
			}
			if got == nil {
				t.Fatal("the snapshot just written cannot be read back")
			}
			switch {
			case tc.want == nil && got.Degraded != nil:
				t.Errorf("Degraded = %v, want nil: an unrecorded flag must not read as a value", *got.Degraded)
			case tc.want != nil && got.Degraded == nil:
				t.Errorf("Degraded = nil, want %v", *tc.want)
			case tc.want != nil && *got.Degraded != *tc.want:
				t.Errorf("Degraded = %v, want %v", *got.Degraded, *tc.want)
			}
		})
	}
}

// A snapshot written before the column existed reads as unrecorded, not clean.
//
// This is the whole argument for making the column nullable rather than NOT NULL
// DEFAULT 0, so it is asserted against a database that genuinely lacks the
// column rather than against one where it was simply left unset. The column is
// dropped and the stamp rolled back to 1, which is exactly the file a v0.1.0
// binary leaves behind.
//
// Without the nullable column this test cannot fail: every pre-migration row
// would read false and false is a plausible answer, which is what makes the bug
// this guards against invisible in the first place.
func TestASnapshotFromBeforeTheColumnReadsAsUnrecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	ctx := context.Background()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	repoID, err := st.UpsertRepo(ctx, "api", "/repos/api")
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	id, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID:      repoID,
		CollectedAt: time.Now(),
		Status:      store.StatusPartial,
		Degraded:    boolPtr(true),
	}, nil)
	if err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}

	// Turn it back into the file a v0.1.0 binary would have written: no column,
	// and a stamp of 1.
	if _, err := st.DB().ExecContext(ctx, `ALTER TABLE snapshots DROP COLUMN degraded`); err != nil {
		t.Fatalf("dropping the column to simulate a v1 database: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		t.Fatalf("restamping: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening a version 1 database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got, err := reopened.SnapshotByID(ctx, repoID, id)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got == nil {
		t.Fatal("the upgrade lost the row, which is worse than anything this column could say")
	}
	if got.Degraded != nil {
		t.Errorf("Degraded = %v on a row written before the column existed. Nothing recorded it, and a value here is a claim about a run nobody checked", *got.Degraded)
	}
	// The row still has to be readable in every other respect, or the migration
	// bought honesty about one field by losing the rest.
	if got.Status != store.StatusPartial {
		t.Errorf("Status = %q after the upgrade, want partial: the columns shifted", got.Status)
	}
}

// Applying the schema twice over a database that already has the column is a
// no-op rather than a failed open.
//
// ALTER TABLE has no IF NOT EXISTS, so the step checks for itself. A stored
// version of 0 covers a brand-new file and one written before the guard existed
// alike, and if that reaches an ADD COLUMN unguarded the open dies with
// "duplicate column name". A failed open is a hard stop for whoever is running
// this, and the reason the schema is applied on every open is that it is safe to.
func TestReapplyingTheSchemaOverAnUpgradedDatabaseIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	ctx := context.Background()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `PRAGMA user_version = 0`); err != nil {
		t.Fatalf("restamping to 0: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening a database that already carries the column: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// openTempStore is a store on a fresh file, closed when the test ends.
func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
