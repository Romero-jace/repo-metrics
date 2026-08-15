package collect

import (
	"testing"
	"time"
)

func TestIsFresh(t *testing.T) {
	start := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	old := start.Add(-90 * 24 * time.Hour)
	during := start.Add(2 * time.Second)

	cases := []struct {
		name   string
		before fileState
		after  fileState
		want   bool
	}{
		{
			name:   "created by the run",
			before: fileState{},
			after:  fileState{exists: true, mtime: during, size: 100},
			want:   true,
		},
		{
			name:   "rewritten by the run",
			before: fileState{exists: true, mtime: old, size: 100},
			after:  fileState{exists: true, mtime: during, size: 120},
			want:   true,
		},
		{
			// The case that matters: a target that exits 0 without doing
			// anything, over a months-old profile left on disk.
			name:   "untouched stale artifact",
			before: fileState{exists: true, mtime: old, size: 100},
			after:  fileState{exists: true, mtime: old, size: 100},
			want:   false,
		},
		{
			name:   "never existed and still does not",
			before: fileState{},
			after:  fileState{},
			want:   false,
		},
		{
			name:   "deleted by the run",
			before: fileState{exists: true, mtime: old, size: 100},
			after:  fileState{},
			want:   false,
		},
		{
			// Coarse filesystem timestamps: same mtime, different content.
			name:   "same mtime but the size moved",
			before: fileState{exists: true, mtime: during, size: 100},
			after:  fileState{exists: true, mtime: during, size: 101},
			want:   true,
		},
		{
			// Nothing observably changed, but the timestamp already sits
			// inside the run, so it cannot be a leftover.
			name:   "unchanged but timestamped inside the run",
			before: fileState{exists: true, mtime: during, size: 100},
			after:  fileState{exists: true, mtime: during, size: 100},
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFresh(tc.before, tc.after, start); got != tc.want {
				t.Errorf("isFresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsStale(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		state  fileState
		maxAge time.Duration
		want   bool
	}{
		{"fresh enough", fileState{exists: true, mtime: now.Add(-time.Hour)}, 24 * time.Hour, false},
		{"too old", fileState{exists: true, mtime: now.Add(-48 * time.Hour)}, 24 * time.Hour, true},
		{"exactly at the limit", fileState{exists: true, mtime: now.Add(-24 * time.Hour)}, 24 * time.Hour, false},
		{"missing file is not stale, it is absent", fileState{}, 24 * time.Hour, false},
		{"no limit configured", fileState{exists: true, mtime: now.Add(-10000 * time.Hour)}, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStale(tc.state, tc.maxAge, now); got != tc.want {
				t.Errorf("isStale = %v, want %v", got, tc.want)
			}
		})
	}
}
