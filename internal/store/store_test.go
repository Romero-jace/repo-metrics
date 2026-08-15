package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustRepo(t *testing.T, st *store.Store, name, path string) int64 {
	t.Helper()
	id, err := st.UpsertRepo(context.Background(), name, path)
	if err != nil {
		t.Fatalf("UpsertRepo(%q): %v", name, err)
	}
	return id
}

func at(y int, m time.Month, d, h, min, sec, nsec int) time.Time {
	return time.Date(y, m, d, h, min, sec, nsec, time.UTC)
}

// Opening twice against the same file must be a no-op the second time. The
// schema is applied on every Open, so anything not guarded by IF NOT EXISTS
// would surface here.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := first.UpsertRepo(context.Background(), "a", "/a"); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	repos, err := second.Repos(context.Background())
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "a" {
		t.Errorf("reopened store lost data: got %+v", repos)
	}
}

// A repo is identified by name. Re-collecting must reuse the row rather than
// accumulating duplicates, and a moved checkout must update the recorded path.
func TestUpsertRepoIsIdempotentAndUpdatesPath(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	first := mustRepo(t, st, "svc", "/old/path")
	second := mustRepo(t, st, "svc", "/new/path")

	if first != second {
		t.Errorf("UpsertRepo returned a new id for the same name: %d then %d", first, second)
	}

	repos, err := st.Repos(ctx)
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(repos))
	}
	if repos[0].Path != "/new/path" {
		t.Errorf("path not updated: got %q, want %q", repos[0].Path, "/new/path")
	}
}

func TestSnapshotAndMetricsRoundTrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	want := store.Snapshot{
		RepoID:      repoID,
		CollectedAt: at(2026, time.August, 15, 12, 30, 0, 0),
		GitSHA:      "abc123",
		GitBranch:   "main",
		GitDirty:    true,
		Env:         "go1.26.5;gowork=off",
		Status:      store.StatusOK,
		Duration:    1500 * time.Millisecond,
	}
	metrics := []store.Metric{
		{Key: "coverage.stmt.covered", Scope: "svc/internal/a", Value: 40},
		{Key: "coverage.stmt.total", Scope: "svc/internal/a", Value: 50},
		{Key: "pkg.without_tests", Scope: "", Value: 3},
	}

	id, err := st.InsertSnapshot(ctx, want, metrics)
	if err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}

	got, err := st.LatestSnapshot(ctx, repoID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("LatestSnapshot returned nil after an insert")
	}
	if got.ID != id {
		t.Errorf("id: got %d, want %d", got.ID, id)
	}
	if !got.CollectedAt.Equal(want.CollectedAt) {
		t.Errorf("CollectedAt: got %s, want %s", got.CollectedAt, want.CollectedAt)
	}
	if got.GitSHA != want.GitSHA || got.GitBranch != want.GitBranch || !got.GitDirty {
		t.Errorf("git metadata round-trip failed: got %+v", got)
	}
	if got.Env != want.Env {
		t.Errorf("Env: got %q, want %q", got.Env, want.Env)
	}
	if got.Status != store.StatusOK {
		t.Errorf("Status: got %q, want %q", got.Status, store.StatusOK)
	}
	if got.Duration != want.Duration {
		t.Errorf("Duration: got %s, want %s", got.Duration, want.Duration)
	}

	gotMetrics, err := st.MetricsFor(ctx, id)
	if err != nil {
		t.Fatalf("MetricsFor: %v", err)
	}
	if len(gotMetrics) != len(metrics) {
		t.Fatalf("metric count: got %d, want %d", len(gotMetrics), len(metrics))
	}
	// MetricsFor sorts by (key, scope), so this ordering is part of the contract.
	wantOrder := []string{"coverage.stmt.covered", "coverage.stmt.total", "pkg.without_tests"}
	for i, key := range wantOrder {
		if gotMetrics[i].Key != key {
			t.Errorf("metric[%d].Key: got %q, want %q", i, gotMetrics[i].Key, key)
		}
	}
}

// A snapshot and its metrics are one unit. If any metric is rejected, the
// snapshot row must not survive, or a later report reads a snapshot whose
// coverage silently sums to less than what was measured.
func TestInsertSnapshotRollsBackOnBadMetric(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	dup := []store.Metric{
		{Key: "coverage.stmt.total", Scope: "pkg/a", Value: 10},
		{Key: "coverage.stmt.total", Scope: "pkg/a", Value: 20},
	}

	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID:      repoID,
		CollectedAt: at(2026, time.August, 15, 12, 0, 0, 0),
		Status:      store.StatusOK,
	}, dup); err == nil {
		t.Fatal("InsertSnapshot accepted a duplicate (key, scope) pair; want an error")
	}

	got, err := st.LatestSnapshot(ctx, repoID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got != nil {
		t.Errorf("failed insert left snapshot %d behind; the write was not atomic", got.ID)
	}
}

// Timestamps are stored as text and ordered by SQL, so the format has to be
// fixed-width. time.RFC3339Nano trims trailing zeros, which makes
// "12:00:00.5Z" sort BEFORE "12:00:00Z" lexicographically ('.' < 'Z') and
// silently returns the wrong snapshot as "latest".
func TestOrderingHoldsAcrossSubSecondPrecision(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	earlier := at(2026, time.August, 15, 12, 0, 0, 0)
	later := at(2026, time.August, 15, 12, 0, 0, 500000000)

	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: earlier, Status: store.StatusOK,
	}, nil); err != nil {
		t.Fatalf("insert earlier: %v", err)
	}
	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: later, Status: store.StatusOK,
	}, nil); err != nil {
		t.Fatalf("insert later: %v", err)
	}

	got, err := st.LatestSnapshot(ctx, repoID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got == nil || !got.CollectedAt.Equal(later) {
		t.Errorf("latest snapshot: got %v, want %s", got, later)
	}
}

// A failed collection has no numbers in it. Treating one as the head or the
// baseline would report a fabricated cliff and then a fabricated recovery.
func TestLatestSnapshotIgnoresFailed(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	good := at(2026, time.August, 14, 12, 0, 0, 0)
	bad := at(2026, time.August, 15, 12, 0, 0, 0)

	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: good, Status: store.StatusOK,
	}, nil); err != nil {
		t.Fatalf("insert good: %v", err)
	}
	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: bad, Status: store.StatusFailed, Error: "boom",
	}, nil); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, err := st.LatestSnapshot(ctx, repoID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got == nil || !got.CollectedAt.Equal(good) {
		t.Errorf("latest usable snapshot: got %v, want the ok one at %s", got, good)
	}
}

// "Every run failed" and "never ran" both come back as nil from LatestSnapshot,
// and they call for opposite actions: go fix your build, versus go run collect.
// LatestSnapshotAny is what tells them apart.
func TestLatestSnapshotAnyKeepsFailedRows(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	broken := mustRepo(t, st, "broken", "/broken")
	never := mustRepo(t, st, "never", "/never")

	bad := at(2026, time.August, 15, 12, 0, 0, 0)
	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: broken, CollectedAt: bad, Status: store.StatusFailed, Error: "boom",
	}, nil); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, err := st.LatestSnapshotAny(ctx, broken)
	if err != nil {
		t.Fatalf("LatestSnapshotAny: %v", err)
	}
	if got == nil {
		t.Fatal("LatestSnapshotAny dropped the failed row, so a broken repo reads as one that never ran")
	}
	if got.Status != store.StatusFailed {
		t.Errorf("Status: got %q, want %q", got.Status, store.StatusFailed)
	}
	if !got.CollectedAt.Equal(bad) {
		t.Errorf("CollectedAt: got %s, want %s", got.CollectedAt, bad)
	}
	if got.Error != "boom" {
		t.Errorf("Error: got %q, want the recorded failure text", got.Error)
	}

	// The other half of the distinction: a repo with no rows at all still
	// reports nothing, so callers cannot mistake it for a failing one.
	none, err := st.LatestSnapshotAny(ctx, never)
	if err != nil {
		t.Fatalf("LatestSnapshotAny: %v", err)
	}
	if none != nil {
		t.Errorf("want nil for a repo that has never been collected, got %+v", none)
	}

	// LatestSnapshot's own contract is unchanged, since delta baseline selection
	// depends on failed rows staying out of it.
	usable, err := st.LatestSnapshot(ctx, broken)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if usable != nil {
		t.Errorf("LatestSnapshot must still skip failed rows, got %+v", usable)
	}
}

// A failed run and an ok run can land in the same collected_at, so the newest
// row has to be picked deterministically or the same database reports a repo as
// broken or as healthy depending on the query plan.
func TestLatestSnapshotAnyBreaksTimestampTiesByID(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	same := at(2026, time.August, 15, 12, 0, 0, 0)
	if _, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: same, Status: store.StatusOK,
	}, nil); err != nil {
		t.Fatalf("insert ok: %v", err)
	}
	last, err := st.InsertSnapshot(ctx, store.Snapshot{
		RepoID: repoID, CollectedAt: same, Status: store.StatusFailed, Error: "boom",
	}, nil)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, err := st.LatestSnapshotAny(ctx, repoID)
	if err != nil {
		t.Fatalf("LatestSnapshotAny: %v", err)
	}
	if got == nil || got.ID != last {
		t.Errorf("got %+v, want the last-written row %d", got, last)
	}
}

// Baseline selection is "most recent usable snapshot at or before the cutoff".
// The boundary is inclusive, and a partial snapshot still carries numbers so it
// counts; a failed one does not.
func TestSnapshotAtOrBefore(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	times := []struct {
		when   time.Time
		status store.Status
	}{
		{at(2026, time.August, 1, 12, 0, 0, 0), store.StatusOK},
		{at(2026, time.August, 8, 12, 0, 0, 0), store.StatusPartial},
		{at(2026, time.August, 9, 12, 0, 0, 0), store.StatusFailed},
		{at(2026, time.August, 15, 12, 0, 0, 0), store.StatusOK},
	}
	for _, tc := range times {
		if _, err := st.InsertSnapshot(ctx, store.Snapshot{
			RepoID: repoID, CollectedAt: tc.when, Status: tc.status,
		}, nil); err != nil {
			t.Fatalf("insert %s: %v", tc.when, err)
		}
	}

	cases := []struct {
		name   string
		cutoff time.Time
		want   *time.Time
	}{
		{"exact match is included", at(2026, time.August, 8, 12, 0, 0, 0), &times[1].when},
		{"skips the failed one", at(2026, time.August, 10, 12, 0, 0, 0), &times[1].when},
		{"nothing early enough", at(2026, time.July, 1, 12, 0, 0, 0), nil},
		{"picks the newest eligible", at(2026, time.August, 15, 12, 0, 0, 0), &times[3].when},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.SnapshotAtOrBefore(ctx, repoID, tc.cutoff)
			if err != nil {
				t.Fatalf("SnapshotAtOrBefore: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Errorf("got snapshot at %s, want none", got.CollectedAt)
				}
				return
			}
			if got == nil {
				t.Fatalf("got no snapshot, want the one at %s", *tc.want)
			}
			if !got.CollectedAt.Equal(*tc.want) {
				t.Errorf("got %s, want %s", got.CollectedAt, *tc.want)
			}
		})
	}
}
