package jslock_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/jslock"
)

// Cross-checks against real lockfiles, opt-in and skipped by default so the unit
// suite stays hermetic. Point it at a directory holding checkouts:
//
//	REPO_METRICS_TEST_LOCKFILE_DIR=/Users/jace/Dev \
//	  go test ./internal/collect/jslock/ -run TestAgainstRealLockfiles -v
//
// The expected counts are the precedent from coverprofile_real_test.go one
// format over: a parser agreeing with a hand-written fixture proves it agrees
// with the fixture, and real files are where the shapes nobody anticipated live.
// Every number below was computed independently before the parser was run.
func TestAgainstRealLockfiles(t *testing.T) {
	root := os.Getenv("REPO_METRICS_TEST_LOCKFILE_DIR")
	if root == "" {
		t.Skip("set REPO_METRICS_TEST_LOCKFILE_DIR to cross-check against real lockfiles")
	}

	cases := []struct {
		path  string
		bun   bool
		total int
		why   string
	}{
		{"web-app/bun.lock", true, 709, "710 entries, one of them a workspace sibling"},
		{"app-two/bun.lock", true, 392, "397 entries, five workspace siblings"},
		{"app-six/bun.lock", true, 122, "125 entries, three workspace siblings"},
		{"app-three/package-lock.json", false, 599, "v3, 600 entries including the root"},
		{"app-five/package-lock.json", false, 6, "v3, 7 entries including the root"},
		{"app-four/package-lock.json", false, 170, "v1, counted by distinct name in the nested tree"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			f, err := os.Open(filepath.Join(root, tc.path))
			if err != nil {
				t.Skipf("not present: %v", err)
			}
			defer func() { _ = f.Close() }()

			read := jslock.ParseNPM
			if tc.bun {
				read = jslock.ParseBun
			}
			got, _, err := read(f)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.Total != tc.total {
				t.Errorf("Total: got %d, want %d (%s)", got.Total, tc.total, tc.why)
			}
		})
	}
}
