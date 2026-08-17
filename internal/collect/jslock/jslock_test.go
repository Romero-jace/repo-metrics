package jslock_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/jslock"
)

// A package-lock.json version 3, which is what npm 7 and later write. The
// packages map is keyed by install path, the project itself is the empty key,
// and a workspace sibling is a link entry rather than an installed package.
const npmV3 = `{
  "name": "svc",
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "svc", "dependencies": {"left-pad": "^1.0.0"}},
    "node_modules/left-pad": {"version": "1.3.0"},
    "node_modules/right-pad": {"version": "2.0.0", "dev": true},
    "packages/inner": {"name": "@svc/inner", "version": "0.1.0"},
    "node_modules/@svc/inner": {"resolved": "packages/inner", "link": true}
  }
}`

// A version 1 lockfile, which nests instead. The same package can appear at more
// than one depth, so the count is by distinct name.
const npmV1 = `{
  "lockfileVersion": 1,
  "dependencies": {
    "a": {"version": "1.0.0", "dependencies": {
      "shared": {"version": "3.0.0"}
    }},
    "b": {"version": "2.0.0", "dependencies": {
      "shared": {"version": "3.0.0"}
    }}
  }
}`

// A bun.lock. JSON with trailing commas, and a workspace sibling resolves to a
// workspace descriptor rather than to a version.
const bunLock = `{
  "lockfileVersion": 1,
  "workspaces": {
    "": {"name": "root", "devDependencies": {"vitest": "^1.0.0",},},
    "packages/inner": {"name": "@svc/inner",},
  },
  "packages": {
    "vitest": ["vitest@1.6.0", "", {}, "sha512-aaa=="],
    "left-pad": ["left-pad@1.3.0", "", {}, "sha512-bbb=="],
    "@svc/inner": ["@svc/inner@workspace:packages/inner"],
  },
}`

func parseOK(t *testing.T, in string, bun bool) *jslock.Summary {
	t.Helper()
	read := jslock.ParseNPM
	if bun {
		read = jslock.ParseBun
	}
	got, _, err := read(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("Parse returned a nil summary and no error")
	}
	return got
}

// The project itself and its workspace siblings are not dependencies of it.
//
// Without both exclusions the number stops being comparable to the one the Go
// parser reports, which is the build list minus the main module and its
// workspace siblings. A monorepo would count its own source and read as larger
// for being split up.
func TestNPMExcludesTheProjectAndItsWorkspaceSiblings(t *testing.T) {
	got := parseOK(t, npmV3, false)
	// Five entries: the root, two real packages, the workspace member's own entry,
	// and the link pointing at it. Two of those five are dependencies.
	if got.Total != 3 {
		t.Errorf("Total: got %d, want 3 (left-pad, right-pad and the workspace member's own entry; the root and the link are excluded)", got.Total)
	}
}

// A nested version 1 tree counts by distinct name, and says so.
func TestNPMV1CountsDistinctNames(t *testing.T) {
	got, diags, err := jslock.ParseNPM(strings.NewReader(npmV1))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// a, b and shared. Four nodes, three names, because npm 6 wrote the shared
	// dependency under each parent that wanted it.
	if got.Total != 3 {
		t.Errorf("Total: got %d, want 3 distinct names across 4 tree nodes", got.Total)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "distinct name") {
		t.Errorf("the interpretation this count required was not disclosed: %+v", diags)
	}
}

// bun.lock is JSON with trailing commas, and the workspace sibling is told from
// the rest by its descriptor.
func TestBunCountsPackagesExcludingWorkspaces(t *testing.T) {
	got := parseOK(t, bunLock, true)
	if got.Total != 2 {
		t.Errorf("Total: got %d, want 2 (vitest and left-pad; @svc/inner resolves to a workspace)", got.Total)
	}
}

// A comma separating two array elements must survive.
//
// This is a real bug that was written and caught: the stripper cleared its
// pending comma on punctuation but not on a quote, so ["a", "b"] lost its
// separator. That is worse than a parse failure, because the result is still
// syntactically plausible and simply means something else. All three real bun
// lockfiles on hand failed on it, which is why they are cross-checked at all.
func TestSeparatorsInsideArraysSurvive(t *testing.T) {
	got := parseOK(t, `{"lockfileVersion":1,"packages":{
		"a": ["a@1.0.0", "", {}, "sha512-x=="],
		"b": ["b@2.0.0", "", {}, "sha512-y=="],
	}}`, true)
	if got.Total != 2 {
		t.Errorf("Total: got %d, want 2; an array separator was eaten", got.Total)
	}
}

// A comma inside a string is not punctuation. Integrity hashes and resolved URLs
// can contain anything, and a naive replace corrupts one.
func TestCommasInsideStringsAreLeftAlone(t *testing.T) {
	got := parseOK(t, `{"lockfileVersion":1,"packages":{
		"a": ["a@1.0.0", "", {}, "sha512-x,}=="],
		"b": ["b@1.0.0", "", {}, "escaped \" quote, and a brace }"],
	}}`, true)
	if got.Total != 2 {
		t.Errorf("Total: got %d, want 2; a comma inside a string was treated as punctuation", got.Total)
	}
}

// A project with no dependencies measured zero, and that is a finding rather
// than an absence: the file parsed and listed nothing.
func TestAnEmptyLockfileIsAMeasuredZero(t *testing.T) {
	if got := parseOK(t, `{"lockfileVersion": 3, "packages": {"": {"name": "svc"}}}`, false); got.Total != 0 {
		t.Errorf("npm: got %d, want 0", got.Total)
	}
	if got := parseOK(t, `{"lockfileVersion": 1, "packages": {}}`, true); got.Total != 0 {
		t.Errorf("bun: got %d, want 0", got.Total)
	}
}

// Something that is not a lockfile must fail rather than measure zero.
func TestNonLockfilesAreFatal(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bun        bool
	}{
		{"a package.json", `{"name": "svc", "version": "1.0.0"}`, false},
		{"not json", "SF:src/a.ts\nLF:10\n", false},
		{"empty", "", true},
		{"a package.json to the bun parser", `{"name": "svc"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := jslock.ParseNPM
			if tc.bun {
				read = jslock.ParseBun
			}
			got, _, err := read(strings.NewReader(tc.body))
			if err == nil {
				t.Fatalf("Parse accepted a non-lockfile, returning %+v", got)
			}
			if got != nil {
				t.Error("Parse returned both an error and a summary; a caller could publish that count")
			}
		})
	}
}
