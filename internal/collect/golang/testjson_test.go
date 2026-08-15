package golang_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
)

func parseJSON(t *testing.T, body string) *golang.TestSummary {
	t.Helper()
	s, err := golang.ParseTestJSON(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseTestJSON: %v", err)
	}
	return s
}

func pkgTests(t *testing.T, s *golang.TestSummary, name string) golang.PackageTests {
	t.Helper()
	for _, p := range s.Packages {
		if p.Package == name {
			return p
		}
	}
	t.Fatalf("package %q not in summary; have %+v", name, s.Packages)
	return golang.PackageTests{}
}

func TestParseCountsResultsPerPackage(t *testing.T) {
	s := parseJSON(t, `
{"Action":"run","Package":"m/a","Test":"TestOne"}
{"Action":"pass","Package":"m/a","Test":"TestOne","Elapsed":0.01}
{"Action":"fail","Package":"m/a","Test":"TestTwo","Elapsed":0.02}
{"Action":"skip","Package":"m/a","Test":"TestThree"}
{"Action":"fail","Package":"m/a","Elapsed":1.5}
{"Action":"pass","Package":"m/b","Test":"TestFour","Elapsed":0.01}
{"Action":"pass","Package":"m/b","Elapsed":0.25}
`)

	a := pkgTests(t, s, "m/a")
	if a.Passed != 1 || a.Failed != 1 || a.Skipped != 1 {
		t.Errorf("m/a: got passed=%d failed=%d skipped=%d, want 1/1/1", a.Passed, a.Failed, a.Skipped)
	}
	if a.Duration != 1500*time.Millisecond {
		t.Errorf("m/a duration: got %s, want 1.5s", a.Duration)
	}

	tests, failed, skipped, elapsed := s.Totals()
	if tests != 4 || failed != 1 || skipped != 1 {
		t.Errorf("Totals: got tests=%d failed=%d skipped=%d, want 4/1/1", tests, failed, skipped)
	}
	if elapsed != 1750*time.Millisecond {
		t.Errorf("Totals elapsed: got %s, want 1.75s", elapsed)
	}
}

// Table-driven subtest counts swing with the size of a fixture table, so a
// headline test count that included them would move for reasons nobody cares
// about. They are counted, just separately.
func TestSubtestsAreCountedSeparately(t *testing.T) {
	s := parseJSON(t, `
{"Action":"pass","Package":"m/a","Test":"TestTable/case_one","Elapsed":0.01}
{"Action":"pass","Package":"m/a","Test":"TestTable/case_two","Elapsed":0.01}
{"Action":"fail","Package":"m/a","Test":"TestTable/case_three","Elapsed":0.01}
{"Action":"fail","Package":"m/a","Test":"TestTable","Elapsed":0.05}
{"Action":"fail","Package":"m/a","Elapsed":0.1}
`)

	a := pkgTests(t, s, "m/a")
	if a.Failed != 1 || a.Passed != 0 {
		t.Errorf("top-level: got passed=%d failed=%d, want 0/1", a.Passed, a.Failed)
	}
	if a.Subtests != 3 {
		t.Errorf("Subtests: got %d, want 3", a.Subtests)
	}

	tests, _, _, _ := s.Totals()
	if tests != 1 {
		t.Errorf("Totals tests: got %d, want 1 (subtests excluded)", tests)
	}
}

// A coverage profile cannot tell you a package has no tests. This is the signal
// that keeps a repo from reporting flattering coverage while whole directories
// go unexercised.
func TestNoTestFilesPackagesAreDetected(t *testing.T) {
	s := parseJSON(t, `
{"Action":"output","Package":"m/empty","Output":"?   \tm/empty\t[no test files]\n"}
{"Action":"skip","Package":"m/empty","Elapsed":0}
{"Action":"pass","Package":"m/real","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"m/real","Elapsed":0.1}
`)

	if got := s.PackagesWithoutTests(); got != 1 {
		t.Errorf("PackagesWithoutTests: got %d, want 1", got)
	}
	if !pkgTests(t, s, "m/empty").NoTestFiles {
		t.Error("m/empty was not flagged as having no test files")
	}
	if pkgTests(t, s, "m/real").NoTestFiles {
		t.Error("m/real was wrongly flagged as having no test files")
	}
}

// The ambiguity worth pinning: a package whose every test called t.Skip also
// reports a package-level "skip" with an empty Test. Only the output marker
// distinguishes it from a package with no test files, and conflating the two
// would report tested-but-skipped code as untested.
func TestAllTestsSkippedIsNotTheSameAsNoTestFiles(t *testing.T) {
	s := parseJSON(t, `
{"Action":"skip","Package":"m/skipped","Test":"TestOne","Elapsed":0}
{"Action":"skip","Package":"m/skipped","Elapsed":0}
`)

	if got := s.PackagesWithoutTests(); got != 0 {
		t.Errorf("PackagesWithoutTests: got %d, want 0", got)
	}
	p := pkgTests(t, s, "m/skipped")
	if p.NoTestFiles {
		t.Error("a package whose tests all skipped was misreported as having no test files")
	}
	if p.Skipped != 1 {
		t.Errorf("Skipped: got %d, want 1", p.Skipped)
	}
}

// A killed run leaves a truncated final line. Losing every result because the
// last line is half-written would turn a timeout into a total blackout.
func TestTruncatedStreamKeepsWhatItParsed(t *testing.T) {
	s := parseJSON(t, `
{"Action":"pass","Package":"m/a","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"m/a","Test":"TestTwo","Elapsed":0.01}
{"Action":"pass","Package":"m/a","Test":"TestThr`)

	a := pkgTests(t, s, "m/a")
	if a.Passed != 2 {
		t.Errorf("Passed: got %d, want 2", a.Passed)
	}
	if s.Malformed != 1 {
		t.Errorf("Malformed: got %d, want 1", s.Malformed)
	}
}

// A failed build can put plain text on stdout. Tolerate it, but count it, so a
// caller can tell a clean run from a noisy one.
func TestNonJSONNoiseIsCountedNotFatal(t *testing.T) {
	s := parseJSON(t, `
# m/a
./broken.go:3:2: undefined: nope
{"Action":"pass","Package":"m/a","Test":"TestOne","Elapsed":0.01}
`)

	if s.Malformed != 2 {
		t.Errorf("Malformed: got %d, want 2", s.Malformed)
	}
	if pkgTests(t, s, "m/a").Passed != 1 {
		t.Error("the one valid event was lost")
	}
}

// Nothing parseable at all means the command did not produce a stream, which is
// a collection failure rather than a repo with zero tests.
func TestEmptyOrUnusableStreamIsAnError(t *testing.T) {
	for _, body := range []string{"", "\n\n", "not json at all\nstill not json\n"} {
		if _, err := golang.ParseTestJSON(strings.NewReader(body)); err == nil {
			t.Errorf("ParseTestJSON(%q) accepted an unusable stream", body)
		}
	}
}

func TestPackagesAreSorted(t *testing.T) {
	s := parseJSON(t, `
{"Action":"pass","Package":"m/zeta","Elapsed":0.1}
{"Action":"pass","Package":"m/alpha","Elapsed":0.1}
{"Action":"pass","Package":"m/mid","Elapsed":0.1}
`)
	var got []string
	for _, p := range s.Packages {
		got = append(got, p.Package)
	}
	want := []string{"m/alpha", "m/mid", "m/zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("packages not sorted: got %v, want %v", got, want)
		}
	}
}
