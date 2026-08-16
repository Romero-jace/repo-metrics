// This file is the whole version story, and there is deliberately nothing in it
// to maintain: no version constant, no -ldflags -X, no Makefile variable that
// goes stale the first week somebody forgets to bump it. The Go toolchain
// already stamps every binary it builds, so this reads that stamp back rather
// than keeping a second copy of the truth that can disagree with the first.
package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// dirtySuffix is what the toolchain appends to a derived version when the tree
// had uncommitted changes at build time.
const dirtySuffix = "+dirty"

// runVersion prints to stdout rather than stderr, because it is an answer
// somebody asked for and will pipe somewhere. Usage goes to stderr for the
// opposite reason.
func runVersion(args []string, stdout, stderr io.Writer) error {
	// It takes nothing, and a leftover is rejected rather than ignored for the
	// same reason parseFlags rejects one: printing the version and exiting 0
	// tells someone who typed a flag that their flag was honored.
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "version takes no arguments, got %q\n", args[0])
		return fmt.Errorf("unexpected argument %q", args[0])
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	_, _ = fmt.Fprintln(stdout, versionLine(info))

	// Read from the runtime rather than from the build info, so this line still
	// prints when there is no build info at all. It is here for bug reports:
	// "which Go, which platform" is the first question anyone asks.
	_, _ = fmt.Fprintf(stdout, "built with %s for %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

// versionLine names the code this binary was built from, or admits that it
// cannot.
//
// Since Go 1.24 the toolchain derives the main module's version from version
// control, so a binary built in a checkout carries a real version rather than a
// bare "devel". That makes the module version the primary answer and the raw
// VCS stamps the fallback, which is the opposite of how this used to be written
// against older toolchains.
//
// All three paths below were confirmed reachable before being written, because
// a branch nobody can enter is worse than no branch:
//
//   - a module version and no VCS stamps at all is a go install from the module
//     cache, which has no repository to read
//   - VCS stamps and no module version is a module living in a subdirectory of
//     its repository, where the tags do not describe the module
//   - neither is a build with -buildvcs=false, a source tarball with no git
//     history, or a build from inside a git worktree, where the toolchain
//     silently stamps nothing because .git is a file rather than a directory
//
// That last case is the one worth being loud about. Printing a bare "devel"
// there reads as an answer when it is an absence, which is the same mistake
// this tool spends the rest of its code refusing to make about measurements.
//
// The dirty marker is not decoration either. A version or a commit from a
// modified tree names code that never ran, and someone checking that revision
// out would be reading a diff the binary does not contain, so it gets spelled
// out in words rather than left as a suffix people skim past.
func versionLine(info *debug.BuildInfo) string {
	if info == nil {
		return "repo-metrics: version unknown, this binary carries no build information"
	}

	if version, dirty := splitDirty(info.Main.Version); version != "" && version != "(devel)" {
		return "repo-metrics " + version + dirtyNote(dirty)
	}

	var revision, stamped string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			stamped = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	if revision == "" {
		return "repo-metrics: devel build with no commit recorded, so this binary cannot say what it was built from " +
			"(-buildvcs=false, or built outside a checkout)"
	}

	line := "repo-metrics devel, built from " + shortRevision(revision)
	if when := buildDate(stamped); when != "" {
		line += " on " + when
	}
	return line + dirtyNote(dirty)
}

// splitDirty separates the toolchain's dirty marker from the version it is
// stuck to, so the version stays something a reader can paste into go get.
func splitDirty(version string) (clean string, dirty bool) {
	if after, found := strings.CutSuffix(version, dirtySuffix); found {
		return after, true
	}
	return version, false
}

func dirtyNote(dirty bool) string {
	if !dirty {
		return ""
	}
	return ", plus uncommitted changes"
}

// shortRevision trims to the twelve characters the toolchain itself uses in the
// pseudo-versions it writes into go.mod, so the two ways of naming one commit
// are comparable by eye.
func shortRevision(rev string) string {
	const n = 12
	if len(rev) <= n {
		return rev
	}
	return rev[:n]
}

// buildDate reduces the commit timestamp to a date. An unparseable stamp is
// passed through rather than dropped: it is still evidence, and inventing a
// date from a shape we did not expect would be worse than an ugly line.
func buildDate(stamp string) string {
	if stamp == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	return t.Format("2006-01-02")
}
