package collect_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
)

// config.formatPolicy.RepoScopedKeys carries string literals because collect
// imports config and the dependency cannot go the other way. This is the seam
// where the two halves can be compared, since this package sees both.
//
// A literal that drifts from the constant is not a cosmetic problem. The list is
// what stops two steps writing the same repo-level row, so a stale entry means
// validation accepts a config whose second test step then fails on every
// collection, and a missing one means the guard silently protects nothing.
func TestDeclaredRepoScopedKeysAreWhatTheParsersWrite(t *testing.T) {
	want := map[config.Format][]string{
		// Scoped by package throughout, so it collides with nothing.
		config.FormatGoCoverprofile: nil,
		// Scoped by source file. Nothing repo-level, but also not Repeatable:
		// the scope names a file in the repo rather than the step that read it,
		// so two tracefiles covering overlapping files would write the same scope.
		config.FormatLCOV: nil,
		// Scoped by the step's name throughout, which is what earns it Repeatable.
		config.FormatSARIF: nil,
		// Every row is prefixed with the step's name, for the same reason. It is
		// also what lets a repo run this beside go-test-json: that format owns two
		// repo-level rows, and this one owns none to collide with them.
		config.FormatJUnitXML: nil,
		config.FormatGoTestJSON: {
			collect.KeyPkgWithoutTest,
			collect.KeyTestBuildFailed,
		},
		config.FormatGoListModules: {
			collect.KeyDepsTotal,
			collect.KeyDepsAgeMedianDays,
			collect.KeyDepsOutdatedDirect,
		},
		// The lockfile formats write the count and nothing else. No JavaScript
		// lockfile records a publish timestamp, so the age is not recoverable, and
		// the outdated count needs a registry to have been asked. Declaring keys
		// they do not write would make the collision guard forbid pairings that are
		// actually fine.
		config.FormatNPMLockfile: {collect.KeyDepsTotal},
		config.FormatBunLockfile: {collect.KeyDepsTotal},
	}

	// Every format, not every entry above: a format added without a line here is
	// a format whose repo-scoped keys nobody checked.
	for _, name := range config.Formats() {
		f := config.Format(name)
		expected, listed := want[f]
		if !listed {
			t.Errorf("format %q has no entry here, so nothing checks whether its repo-scoped keys are declared correctly", name)
			continue
		}
		got := f.RepoScopedKeys()
		if !slices.Equal(got, expected) {
			t.Errorf("format %q declares repo-scoped keys %v, want %v", name, got, expected)
		}
	}

	// KeyTestSuites must NOT appear anywhere above. It is the marker every test
	// parser writes, so if it were repo-scoped no repo could ever run two test
	// suites, and the guard would forbid the exact case it exists to permit.
	for _, name := range config.Formats() {
		if slices.Contains(config.Format(name).RepoScopedKeys(), collect.KeyTestSuites) {
			t.Errorf("format %q declares %s as repo-scoped; it is scoped by step so that two test suites can coexist",
				name, collect.KeyTestSuites)
		}
	}
}

// The keys are strings in two places, so a typo in either is invisible to the
// compiler. This catches the shape of one: a declared key that no constant in
// this package spells.
func TestEveryDeclaredRepoScopedKeyIsARealMetricKey(t *testing.T) {
	known := map[string]bool{
		collect.KeyCoveredStmts: true, collect.KeyTotalStmts: true,
		collect.KeyTestCount: true, collect.KeyTestFailed: true,
		collect.KeyTestSkipped: true, collect.KeyTestDurationMS: true,
		collect.KeyTestSuites: true, collect.KeyPkgWithoutTest: true,
		collect.KeyTestBuildFailed: true,
		collect.KeyLintFindings:    true, collect.KeyLintErrors: true,
		collect.KeyLintSuppressed: true,
		collect.KeyDepsTotal:      true, collect.KeyDepsAgeMedianDays: true,
		collect.KeyDepsOutdatedDirect: true,
		collect.KeySignalDurationMS:   true,
	}

	for _, name := range config.Formats() {
		for _, key := range config.Format(name).RepoScopedKeys() {
			if !known[key] {
				t.Errorf("format %q declares repo-scoped key %q, which no metric key constant spells. "+
					"A key nothing writes protects nothing", name, key)
			}
		}
	}

	// The control: the check above passes trivially if `known` is permissive.
	if known["metric.that.does.not.exist"] {
		t.Error("the known-key set answers yes to anything, so the check above proves nothing")
	}
	if !strings.Contains(collect.KeyPkgWithoutTest, ".") {
		t.Error("metric keys are dotted names; this fixture is reading something else")
	}
}
