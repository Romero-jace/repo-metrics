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

// mustSnapshot inserts one snapshot and returns its id, so the series tests can
// build a timeline without ten lines of error handling per point.
func mustSnapshot(t *testing.T, st *store.Store, snap store.Snapshot, metrics []store.Metric) int64 {
	t.Helper()
	id, err := st.InsertSnapshot(context.Background(), snap, metrics)
	if err != nil {
		t.Fatalf("InsertSnapshot at %s: %v", snap.CollectedAt, err)
	}
	return id
}

// The single most important case for SnapshotSeries: a repo whose every run
// failed owns zero metric rows, and it must still come back as one point per
// run with nil Metrics.
//
// This is the shape a LEFT JOIN loses. Move one metrics predicate out of the ON
// clause and into WHERE and the outer join quietly becomes an inner one, the
// repo drops out of the series entirely, and the report shows nothing at all
// for exactly the repo whose build has been broken for a month.
func TestSnapshotSeriesKeepsFailedSnapshotsWithNoMetrics(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "broken", "/broken")

	when := []time.Time{
		at(2026, time.August, 11, 12, 0, 0, 0),
		at(2026, time.August, 12, 12, 0, 0, 0),
		at(2026, time.August, 13, 12, 0, 0, 0),
		at(2026, time.August, 14, 12, 0, 0, 0),
		at(2026, time.August, 15, 12, 0, 0, 0),
	}
	for _, w := range when {
		mustSnapshot(t, st, store.Snapshot{
			RepoID: repoID, CollectedAt: w, Status: store.StatusFailed, Error: "build failed",
		}, nil)
	}

	got, err := st.SnapshotSeries(ctx, repoID, when[0], when[len(when)-1])
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	if len(got) != len(when) {
		t.Fatalf("got %d points, want %d: a repo with no metric rows was dropped from its own series",
			len(got), len(when))
	}
	for i, point := range got {
		if !point.Snapshot.CollectedAt.Equal(when[i]) {
			t.Errorf("point[%d].CollectedAt: got %s, want %s", i, point.Snapshot.CollectedAt, when[i])
		}
		if point.Snapshot.Status != store.StatusFailed {
			t.Errorf("point[%d].Status: got %q, want %q", i, point.Snapshot.Status, store.StatusFailed)
		}
		if point.Snapshot.Error != "build failed" {
			t.Errorf("point[%d].Error: got %q, want the recorded failure text", i, point.Snapshot.Error)
		}
		// Nil, not an empty non-nil slice: callers key "nothing was measured"
		// off this, and a zero-length measurement would plot as a real zero.
		if point.Metrics != nil {
			t.Errorf("point[%d].Metrics: got %+v, want nil for a run that stored nothing", i, point.Metrics)
		}
	}
}

// Both bounds are inclusive. An exclusive edge silently clips the first and last
// point of every window, which reads as a shorter history rather than as a bug.
func TestSnapshotSeriesBoundsAreInclusive(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	before := at(2026, time.August, 1, 12, 0, 0, 0)
	from := at(2026, time.August, 2, 12, 0, 0, 0)
	middle := at(2026, time.August, 3, 12, 0, 0, 0)
	to := at(2026, time.August, 4, 12, 0, 0, 0)
	after := at(2026, time.August, 5, 12, 0, 0, 0)

	for _, w := range []time.Time{before, from, middle, to, after} {
		mustSnapshot(t, st, store.Snapshot{
			RepoID: repoID, CollectedAt: w, Status: store.StatusOK,
		}, nil)
	}

	got, err := st.SnapshotSeries(ctx, repoID, from, to)
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	want := []time.Time{from, middle, to}
	if len(got) != len(want) {
		t.Fatalf("got %d points, want %d (both bounds inclusive, neither neighbor included)", len(got), len(want))
	}
	for i := range want {
		if !got[i].Snapshot.CollectedAt.Equal(want[i]) {
			t.Errorf("point[%d]: got %s, want %s", i, got[i].Snapshot.CollectedAt, want[i])
		}
	}
}

// A series is drawn left to right, so points ascend by time. Two runs can land
// in the same collected_at, and without the id tiebreak SQLite is free to return
// either first, so the same database draws a different chart run to run.
func TestSnapshotSeriesAscendsAndBreaksTimestampTiesByID(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	late := at(2026, time.August, 15, 12, 0, 0, 0)
	early := at(2026, time.August, 10, 12, 0, 0, 0)
	tied := at(2026, time.August, 12, 12, 0, 0, 0)

	// Inserted out of order on purpose: insertion order must not be the order
	// that comes back, or the ORDER BY is not being exercised at all.
	lateID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: late, Status: store.StatusOK,
	}, nil)
	earlyID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: early, Status: store.StatusOK,
	}, nil)
	firstTiedID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: tied, Status: store.StatusOK,
	}, nil)
	secondTiedID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: tied, Status: store.StatusFailed, Error: "boom",
	}, nil)

	got, err := st.SnapshotSeries(ctx, repoID, early, late)
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	wantIDs := []int64{earlyID, firstTiedID, secondTiedID, lateID}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d points, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].Snapshot.ID != want {
			t.Errorf("point[%d].ID: got %d, want %d (ascending by collected_at, id)",
				i, got[i].Snapshot.ID, want)
		}
	}
}

// Sub-second timestamps have to survive both the ordering and the range bounds,
// and they are two different ways to get this wrong.
//
// collected_at is TEXT compared as a string, so a variable-width layout like
// RFC3339Nano sorts "12:00:00.5Z" before "12:00:00Z" ('.' < 'Z'). Stored that
// way it reverses points inside a single second; used to BIND the range bounds
// it drags points from past the end of the window back inside it, because every
// sub-second stamp compares less than a whole-second bound. That second half is
// why formatTime stays unexported: a caller reaching for time.RFC3339 gets a
// query that runs, returns rows, and returns the wrong ones.
func TestSnapshotSeriesOrderingHoldsAcrossSubSecondPrecision(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	// Whole second first, then half past it. Under RFC3339Nano the half-second
	// stamp is the shorter string and sorts first, inverting these two.
	whole := at(2026, time.August, 15, 12, 0, 0, 0)
	half := at(2026, time.August, 15, 12, 0, 0, 500000000)

	wholeID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: whole, Status: store.StatusOK,
	}, nil)
	halfID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: half, Status: store.StatusOK,
	}, nil)

	// Ordering: a window wide enough to hold both, so only the sort is at play.
	wide, err := st.SnapshotSeries(ctx, repoID,
		at(2026, time.August, 15, 11, 0, 0, 0),
		at(2026, time.August, 15, 13, 0, 0, 0))
	if err != nil {
		t.Fatalf("SnapshotSeries wide: %v", err)
	}
	if len(wide) != 2 {
		t.Fatalf("got %d points, want 2", len(wide))
	}
	if wide[0].Snapshot.ID != wholeID || wide[1].Snapshot.ID != halfID {
		t.Errorf("got ids [%d %d], want [%d %d]: the whole second precedes the half second",
			wide[0].Snapshot.ID, wide[1].Snapshot.ID, wholeID, halfID)
	}
	if !wide[0].Snapshot.CollectedAt.Equal(whole) || !wide[1].Snapshot.CollectedAt.Equal(half) {
		t.Errorf("got %s then %s, want %s then %s",
			wide[0].Snapshot.CollectedAt, wide[1].Snapshot.CollectedAt, whole, half)
	}

	// Bounds: the window ends exactly on the whole second, so the half-second
	// point is past the end and must be excluded. Bind that upper bound with a
	// trimming layout and "12:00:00.500000000Z" <= "12:00:00Z" is true, so the
	// point outside the window is handed back as though it were inside.
	clipped, err := st.SnapshotSeries(ctx, repoID, at(2026, time.August, 15, 11, 0, 0, 0), whole)
	if err != nil {
		t.Fatalf("SnapshotSeries clipped: %v", err)
	}
	if len(clipped) != 1 {
		t.Fatalf("got %d points, want only the one at or before %s", len(clipped), whole)
	}
	if clipped[0].Snapshot.ID != wholeID {
		t.Errorf("got id %d, want %d: a point after the upper bound was included",
			clipped[0].Snapshot.ID, wholeID)
	}

	// The mirror case on the lower bound: a window starting half a second in
	// must drop the whole-second point rather than sweep it in.
	trailing, err := st.SnapshotSeries(ctx, repoID, half, at(2026, time.August, 15, 13, 0, 0, 0))
	if err != nil {
		t.Fatalf("SnapshotSeries trailing: %v", err)
	}
	if len(trailing) != 1 || trailing[0].Snapshot.ID != halfID {
		t.Errorf("got %d points starting at %s, want only id %d", len(trailing), half, halfID)
	}
}

// The stitch has to attach each metric to the snapshot it was measured on. Two
// points in one window normally carry the SAME metric keys, since that is what a
// time series of one metric is, so keying the attachment on anything but the
// snapshot id, position included, silently reports one week's coverage against
// another week's date.
func TestSnapshotSeriesAttachesMetricsToTheRightSnapshot(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	first := at(2026, time.August, 1, 12, 0, 0, 0)
	middle := at(2026, time.August, 8, 12, 0, 0, 0)
	last := at(2026, time.August, 15, 12, 0, 0, 0)

	firstID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: first, Status: store.StatusOK,
	}, []store.Metric{
		{Key: "coverage.stmt.covered", Scope: "svc/b", Value: 11},
		{Key: "coverage.stmt.covered", Scope: "svc/a", Value: 10},
		{Key: "coverage.stmt.total", Scope: "svc/a", Value: 100},
	})

	// Deliberately metric-less and in the middle: if metrics were attached by
	// position instead of by id, this gap shifts every later point's numbers
	// onto the wrong run, which is the bug this case exists to catch.
	middleID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: middle, Status: store.StatusFailed, Error: "boom",
	}, nil)

	lastID := mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: last, Status: store.StatusOK,
	}, []store.Metric{
		{Key: "coverage.stmt.covered", Scope: "svc/a", Value: 20},
	})

	got, err := st.SnapshotSeries(ctx, repoID, first, last)
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d points, want 3", len(got))
	}
	if got[0].Snapshot.ID != firstID || got[1].Snapshot.ID != middleID || got[2].Snapshot.ID != lastID {
		t.Fatalf("points out of order: got ids [%d %d %d], want [%d %d %d]",
			got[0].Snapshot.ID, got[1].Snapshot.ID, got[2].Snapshot.ID, firstID, middleID, lastID)
	}

	// Within a point, metrics are ordered by (metric_key, scope), matching
	// MetricsFor. The two svc/b and svc/a rows were inserted in the opposite
	// order, so insertion order would show up here.
	wantFirst := []store.Metric{
		{Key: "coverage.stmt.covered", Scope: "svc/a", Value: 10},
		{Key: "coverage.stmt.covered", Scope: "svc/b", Value: 11},
		{Key: "coverage.stmt.total", Scope: "svc/a", Value: 100},
	}
	if len(got[0].Metrics) != len(wantFirst) {
		t.Fatalf("point[0] metric count: got %d, want %d", len(got[0].Metrics), len(wantFirst))
	}
	for i, want := range wantFirst {
		if got[0].Metrics[i] != want {
			t.Errorf("point[0].Metrics[%d]: got %+v, want %+v", i, got[0].Metrics[i], want)
		}
	}

	if got[1].Metrics != nil {
		t.Errorf("point[1].Metrics: got %+v, want nil for the failed run", got[1].Metrics)
	}

	// The same key appears on both ok points with different values, so a stitch
	// that mixed them up would be caught by the value rather than by the key.
	wantLast := []store.Metric{{Key: "coverage.stmt.covered", Scope: "svc/a", Value: 20}}
	if len(got[2].Metrics) != len(wantLast) {
		t.Fatalf("point[2] metric count: got %d, want %d", len(got[2].Metrics), len(wantLast))
	}
	if got[2].Metrics[0] != wantLast[0] {
		t.Errorf("point[2].Metrics[0]: got %+v, want %+v", got[2].Metrics[0], wantLast[0])
	}
}

// Metrics belonging to another repo's snapshots must not leak into this repo's
// series. The metrics query joins back to snapshots for exactly this reason.
func TestSnapshotSeriesIgnoresOtherRepos(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	mine := mustRepo(t, st, "mine", "/mine")
	theirs := mustRepo(t, st, "theirs", "/theirs")

	when := at(2026, time.August, 15, 12, 0, 0, 0)
	mustSnapshot(t, st, store.Snapshot{
		RepoID: mine, CollectedAt: when, Status: store.StatusOK,
	}, []store.Metric{{Key: "coverage.stmt.covered", Scope: "", Value: 1}})
	mustSnapshot(t, st, store.Snapshot{
		RepoID: theirs, CollectedAt: when, Status: store.StatusOK,
	}, []store.Metric{{Key: "coverage.stmt.covered", Scope: "", Value: 999}})

	got, err := st.SnapshotSeries(ctx, mine, when, when)
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if len(got[0].Metrics) != 1 || got[0].Metrics[0].Value != 1 {
		t.Errorf("metrics: got %+v, want only this repo's value 1", got[0].Metrics)
	}
}

// An inverted range is a caller bug, and answering it with an empty series would
// let a swapped pair of arguments read as a repo that was never collected. This
// tool exists to refuse that kind of silent wrong answer.
func TestSnapshotSeriesRejectsInvertedRange(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	early := at(2026, time.August, 1, 12, 0, 0, 0)
	late := at(2026, time.August, 15, 12, 0, 0, 0)
	mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: at(2026, time.August, 8, 12, 0, 0, 0), Status: store.StatusOK,
	}, nil)

	got, err := st.SnapshotSeries(ctx, repoID, late, early)
	if err == nil {
		t.Fatalf("SnapshotSeries accepted from after to and returned %d points; want an error", len(got))
	}
	if got != nil {
		t.Errorf("got %+v alongside the error, want nil", got)
	}

	// An equal pair is not inverted: it is a single-instant window, and the
	// inclusive bounds make it a legitimate one-point query.
	same, err := st.SnapshotSeries(ctx, repoID, early, early)
	if err != nil {
		t.Fatalf("SnapshotSeries with from == to: %v", err)
	}
	if len(same) != 0 {
		t.Errorf("got %d points for an instant with no snapshot, want 0", len(same))
	}
}

// A window that matches nothing is a legitimate answer, not an error: the repo
// simply was not collected then.
func TestSnapshotSeriesEmptyRange(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	repoID := mustRepo(t, st, "svc", "/svc")

	mustSnapshot(t, st, store.Snapshot{
		RepoID: repoID, CollectedAt: at(2026, time.August, 15, 12, 0, 0, 0), Status: store.StatusOK,
	}, []store.Metric{{Key: "coverage.stmt.covered", Scope: "", Value: 5}})

	got, err := st.SnapshotSeries(ctx, repoID,
		at(2026, time.July, 1, 12, 0, 0, 0),
		at(2026, time.July, 31, 12, 0, 0, 0))
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d points, want none for a window before the first collection", len(got))
	}

	// A repo that has never been collected at all behaves the same way.
	never := mustRepo(t, st, "never", "/never")
	none, err := st.SnapshotSeries(ctx, never,
		at(2026, time.August, 1, 12, 0, 0, 0),
		at(2026, time.August, 31, 12, 0, 0, 0))
	if err != nil {
		t.Fatalf("SnapshotSeries: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d points for an uncollected repo, want none", len(none))
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
