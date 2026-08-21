package jslock_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/jslock"
)

// TestAgainstRealLockfiles cross-checks the parsers against lockfiles that
// actually exist on the machine running the test. It is opt-in and skipped by
// default so the unit suite stays hermetic.
//
// It is the precedent from coverprofile_real_test.go one format over: a parser
// agreeing with a hand-written fixture only proves it agrees with the fixture,
// and real files are where the shapes nobody anticipated live. A monorepo with
// five workspace siblings, a v1 lockfile with the same package nested at four
// depths, a resolved URL with a comma in it: none of those get written into a
// fixture by someone who has not already been surprised by them.
//
// The cases live outside this repository, in a JSON manifest whose path is
// named by REPO_METRICS_TEST_LOCKFILE_CASES:
//
//	REPO_METRICS_TEST_LOCKFILE_CASES=/path/to/lockfile-cases.json \
//	  go test -count=1 ./internal/collect/jslock/ -run TestAgainstRealLockfiles -v
//
// They live outside because the checkouts one person happens to have on disk
// are that person's business and not this repository's, and because a table
// baked into the source can only ever describe one machine. The manifest is an
// array of objects carrying the four fields the case table used to hold:
//
//	[
//	  {"path": "some-app/bun.lock", "bun": true, "total": 709,
//	   "why": "710 entries, one of them a workspace sibling"},
//	  {"path": "another-app/package-lock.json", "bun": false, "total": 170,
//	   "why": "v1, counted by distinct name in the nested tree"}
//	]
//
// "path" is resolved against the directory holding the manifest when it is
// relative and used as-is when it is absolute, so a manifest that sits beside
// the checkouts it describes can name them by bare name. "bun" selects the
// parser: true for ParseBun and a bun.lock, false for ParseNPM and a
// package-lock.json. "total" is the expected package count and "why" is the
// sentence printed when the comparison fails, which is where the reasoning
// behind the number belongs.
//
// Every expected number must be computed independently, by reading the lockfile
// or counting it with something that is not this parser, before the parser is
// run against it. A number copied out of a failing run's output turns the whole
// test into a tautology: it would then assert only that the parser still agrees
// with itself.
//
// -count=1 matters, for the reason the coverprofile cross-check next door
// records: it too reads its input from a path named in the environment, and the
// test cache was observed there serving a pass from an earlier run against a
// different artifact, reporting the old numbers without ever opening the new
// file. This test has the same shape and so inherits the same hazard.
func TestAgainstRealLockfiles(t *testing.T) {
	manifest := os.Getenv("REPO_METRICS_TEST_LOCKFILE_CASES")
	if manifest == "" {
		t.Skip("set REPO_METRICS_TEST_LOCKFILE_CASES to a JSON manifest of lockfiles to " +
			"cross-check against; the doc comment on this test has the format")
	}

	cases := loadLockfileCases(t, manifest)

	// A relative path is resolved against the manifest's own directory rather
	// than the working directory, which for a Go test is the package directory
	// and never anywhere useful.
	root := filepath.Dir(manifest)

	for _, tc := range cases {
		t.Run(tc.Path, func(t *testing.T) {
			path := tc.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}

			f, err := os.Open(path)
			if err != nil {
				// A missing file fails rather than skips, which is the one
				// behavior change from the version of this test that carried
				// its cases in the source. Back then the table was baked in and
				// described one person's machine, so a file it named being
				// absent was expected and skipping was the honest response.
				// Now the operator wrote the table themselves and pointed this
				// test at it deliberately, so a file named in it that is not on
				// disk is their error, and it has to be loud: a run that
				// quietly skips every case is a green run that measured
				// nothing, which is exactly the failure this whole tool exists
				// to argue against.
				t.Fatalf("%q is named in %s but could not be opened at %s: %v",
					tc.Path, manifest, path, err)
			}
			defer func() { _ = f.Close() }()

			read := jslock.ParseNPM
			if tc.Bun {
				read = jslock.ParseBun
			}
			got, _, err := read(f)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			// Logged on every case, pass or fail, so a -v run shows which files
			// were actually opened and what they were compared against. The
			// reason for the number is deliberately left to the failure message
			// below: printing it here too would put the same sentence in the
			// output twice on a mismatch, which reads as two findings.
			t.Logf("%s: got %d, want %d", path, got.Total, tc.Total)
			if got.Total != tc.Total {
				t.Errorf("Total: got %d, want %d (%s)", got.Total, tc.Total, tc.Why)
			}
		})
	}
}

// lockfileCase is one entry in the manifest TestAgainstRealLockfiles reads. The
// fields are the ones the in-source case table used to hold, so a manifest is a
// straight transcription of what that table said.
type lockfileCase struct {
	Path  string `json:"path"`
	Bun   bool   `json:"bun"`
	Total int    `json:"total"`
	Why   string `json:"why"`
}

// loadLockfileCases reads the manifest, and fails rather than skips on anything
// wrong with it.
//
// The distinction the caller draws is between a manifest that was not asked for
// and a manifest that was asked for and is broken. An unset variable means
// nobody wanted this cross-check and skipping is correct. A variable that is set
// means somebody did want it, so a typo in the path, a stray comma, or a field
// name that does not exist has to stop the run. The alternative is that an
// operator who mistyped a path gets a pass and believes their parser was checked
// against real files.
func loadLockfileCases(t *testing.T, manifest string) []lockfileCase {
	t.Helper()

	f, err := os.Open(manifest)
	if err != nil {
		t.Fatalf("opening the lockfile case manifest: %v", err)
	}
	defer func() { _ = f.Close() }()

	var cases []lockfileCase
	dec := json.NewDecoder(f)
	// An unknown field is an operator error of exactly the same kind as a
	// mistyped path, and encoding/json will not report it otherwise: "paths"
	// instead of "path" decodes without complaint and leaves the case pointing
	// at nothing. The empty-path check below would still catch that one, but it
	// can only say the case has no path, which sends the operator looking at the
	// value they typed correctly rather than at the key they did not.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		t.Fatalf("reading the lockfile case manifest %s: %v", manifest, err)
	}

	// An empty manifest is the placeholder trap in another costume: every case
	// passes because there are no cases, and the run is green having compared
	// nothing.
	if len(cases) == 0 {
		t.Fatalf("the lockfile case manifest %s lists no cases", manifest)
	}
	for i, tc := range cases {
		if tc.Path == "" {
			t.Fatalf("case %d in %s has no path", i, manifest)
		}
	}
	return cases
}
