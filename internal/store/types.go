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
