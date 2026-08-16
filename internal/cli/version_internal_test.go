// This one file is an internal test because the states versionLine has to tell
// apart cannot be produced from outside the package. A test binary carries
// whatever stamps the toolchain happened to give it, and there is no way to ask
// it for a tagged module version, a dirty tree, or no build information at all.
// The wiring is tested externally in version_test.go, where it belongs.
package cli

import (
	"runtime/debug"
	"strings"
	"testing"
)

func buildInfo(version string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Path: "github.com/Romero-jace/repo-metrics", Version: version},
		Settings: settings,
	}
}

func setting(key, value string) debug.BuildSetting {
	return debug.BuildSetting{Key: key, Value: value}
}

// The absent and the dirty cases are the point of this table. Anything can
// print a version when it has a clean one; the failures worth guarding are a
// binary that does not know what it is and says something confident anyway, and
// a binary that names a commit it does not actually contain.
func TestVersionLine(t *testing.T) {
	const (
		rev  = "810c3d3ab123def4567890abcdef1234567890ab"
		when = "2026-08-15T22:21:00Z"
	)

	cases := []struct {
		name    string
		info    *debug.BuildInfo
		want    []string
		notWant []string
	}{
		{
			name: "installed from a tag",
			info: buildInfo("v0.1.0"),
			want: []string{"repo-metrics v0.1.0"},
			// A tagged install out of the module cache has no VCS stamps at
			// all, so any talk of a commit here would be invented.
			notWant: []string{"devel", "built from", "unknown", "uncommitted"},
		},
		{
			name: "built in a checkout, where the toolchain derives a version",
			info: buildInfo("v0.0.0-20260815222100-810c3d3ab123",
				setting("vcs.revision", rev),
				setting("vcs.time", when),
				setting("vcs.modified", "false"),
			),
			// The derived version already carries the timestamp and the commit
			// prefix, and it is the form you can paste into go get, so it wins
			// over reformatting the stamps into prose.
			want:    []string{"v0.0.0-20260815222100-810c3d3ab123"},
			notWant: []string{"devel", "unknown", "uncommitted"},
		},
		{
			name: "built from a tag with local edits",
			info: buildInfo("v0.1.0+dirty"),
			// The marker is spelled out, and split off the version so what
			// remains is still something a reader can paste into go get.
			want:    []string{"v0.1.0", "uncommitted changes"},
			notWant: []string{"+dirty"},
		},
		{
			name: "built in a checkout with local edits",
			info: buildInfo("v0.0.0-20260815222100-810c3d3ab123+dirty",
				setting("vcs.modified", "true"),
			),
			want:    []string{"v0.0.0-20260815222100-810c3d3ab123", "uncommitted changes"},
			notWant: []string{"+dirty"},
		},
		{
			name: "a module in a subdirectory of its repository",
			// Confirmed by experiment, which is the only reason this branch
			// exists: the tags do not describe the module, so the toolchain
			// stamps the commit and leaves the version at devel.
			info: buildInfo("(devel)",
				setting("vcs.revision", rev),
				setting("vcs.time", when),
				setting("vcs.modified", "false"),
			),
			want: []string{"devel", "810c3d3ab123", "2026-08-15"},
			// The full forty character hash is noise, and claiming uncommitted
			// changes on a clean tree would send someone looking for a diff
			// that is not there.
			notWant: []string{rev, "uncommitted"},
		},
		{
			name: "a module in a subdirectory, built dirty",
			info: buildInfo("(devel)",
				setting("vcs.revision", rev),
				setting("vcs.time", when),
				setting("vcs.modified", "true"),
			),
			// This is the one that matters most. The hash is real and it still
			// does not name the code that ran, so the line has to say so.
			want: []string{"810c3d3ab123", "uncommitted changes"},
		},
		{
			name: "built with no VCS stamping at all",
			info: buildInfo("(devel)"),
			// A build inside a git worktree lands here, because the toolchain
			// silently stamps nothing when .git is a file. No commit was
			// recorded, so there is nothing to print but the admission, and a
			// bare "devel" would read as an answer.
			want:    []string{"cannot say what it was built from", "buildvcs"},
			notWant: []string{"built from 8", "810c3d3"},
		},
		{
			name: "no build information at all",
			info: nil,
			want: []string{"version unknown", "no build information"},
		},
		{
			name: "a short revision is not truncated into nonsense",
			info: buildInfo("(devel)", setting("vcs.revision", "abc123")),
			want: []string{"abc123"},
		},
		{
			name: "an unparseable timestamp is passed through rather than dropped",
			info: buildInfo("(devel)",
				setting("vcs.revision", rev),
				setting("vcs.time", "last tuesday"),
			),
			want: []string{"810c3d3ab123", "last tuesday"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := versionLine(tc.info)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("versionLine() = %q, which does not mention %q", got, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("versionLine() = %q, which should not mention %q", got, notWant)
				}
			}
			if !strings.HasPrefix(got, "repo-metrics") {
				t.Errorf("versionLine() = %q, which does not start with the program name", got)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("versionLine() = %q, which is meant to be one line", got)
			}
		})
	}
}
