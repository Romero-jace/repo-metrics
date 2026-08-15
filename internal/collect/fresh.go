package collect

import (
	"os"
	"time"
)

// fileState is what we know about an artifact at a point in time.
type fileState struct {
	exists bool
	mtime  time.Time
	size   int64
}

func statFile(path string) fileState {
	info, err := os.Stat(path)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, mtime: info.ModTime(), size: info.Size()}
}

// isFresh reports whether a command that started at startedAt actually wrote
// the artifact.
//
// This exists because "the command exited 0" is not evidence that anything was
// produced. A .PHONY make target with no rule prints "Nothing to be done" and
// exits 0, and if a months-old profile happens to sit at the configured path,
// a collector that trusts the exit code will report that stale file as today's
// number indefinitely, with no error logged anywhere. That is the exact
// silent-wrong-answer failure this tool exists to catch, so it must not be the
// tool's own first bug.
//
// The check compares the artifact before and after rather than only testing
// mtime against a clock. Comparing to the run is immune to filesystem
// timestamp granularity and to any clock skew between the process and the
// filesystem, which a bare "mtime >= startedAt" is not.
func isFresh(before, after fileState, startedAt time.Time) bool {
	switch {
	case !after.exists:
		return false
	case !before.exists:
		// The run created it.
		return true
	case !after.mtime.Equal(before.mtime):
		return true
	case after.size != before.size:
		// Same coarse mtime, different content. Rare, but a one-second-
		// granularity filesystem plus a fast rewrite gets here.
		return true
	default:
		// Nothing observably changed. Only believe it if the timestamp itself
		// already sits inside the run.
		return !after.mtime.Before(startedAt)
	}
}

// isStale reports whether an artifact nobody claims to have just written is too
// old to present as a current measurement. It applies in ingest mode, where
// something else (CI, usually) produces the file on its own schedule.
func isStale(state fileState, maxAge time.Duration, now time.Time) bool {
	if !state.exists || maxAge <= 0 {
		return false
	}
	return now.Sub(state.mtime) > maxAge
}
