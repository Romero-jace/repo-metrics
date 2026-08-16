package store

import "time"

// Status describes how much of a collection run actually succeeded.
//
// The distinction matters for delta reporting: partial snapshots still carry
// real numbers and can serve as a head or a baseline, while failed ones carry
// none and must be skipped, or the report invents a cliff followed by a
// recovery that never happened.
type Status string

const (
	// StatusOK means every configured signal was collected.
	StatusOK Status = "ok"
	// StatusPartial means some signals were collected and some were not, for
	// example a coverage profile parsed but the test command exited non-zero.
	StatusPartial Status = "partial"
	// StatusFailed means nothing usable came back.
	StatusFailed Status = "failed"
)

// Repo is a tracked repository. Name is the identity; path can move.
type Repo struct {
	ID        int64
	Name      string
	Path      string
	CreatedAt time.Time
}

// Snapshot is one collection run against one repo.
type Snapshot struct {
	ID          int64
	RepoID      int64
	CollectedAt time.Time
	GitSHA      string
	GitBranch   string
	GitDirty    bool
	// Env fingerprints the toolchain the measurement was taken with, so
	// numbers from incomparable environments are not diffed silently.
	Env      string
	Status   Status
	Error    string
	Duration time.Duration
}

// Metric is one measurement within a snapshot.
//
// Scope is a package import path, or empty for a repo-level measurement. Value
// is a float64 because it carries both counts and durations; coverage is always
// stored as covered and total statement counts rather than a percentage.
type Metric struct {
	Key   string
	Scope string
	Value float64
}

// SnapshotMetrics pairs one snapshot with the metrics that snapshot stored.
//
// Snapshot is a value, never a pointer: every point in a series is a run that
// really happened, so there is no "no snapshot here" state to represent. All the
// absence in this type lives in Metrics.
//
// Metrics is nil when the snapshot stored no metric rows at all, and that nil is
// the point of the type rather than an inconvenience to normalize away. A failed
// run produces it, and so does a header-only coverage profile from a test binary
// that never ran. Both mean "nothing was measured here", which is not the same
// as "zero was measured here": a caller that reads nil as an empty measurement
// and sums it up republishes zeroes nobody ever measured, turning a broken
// collection into a coverage cliff on a chart. Check for nil before averaging,
// diffing, or plotting, and render the gap as a gap.
type SnapshotMetrics struct {
	Snapshot Snapshot
	Metrics  []Metric
}
