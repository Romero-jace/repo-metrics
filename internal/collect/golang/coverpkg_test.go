package golang_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
)

// -coverpkg=./... changes what an untested package looks like in the stream,
// and it changes it in the direction that hides the problem.
//
// Without the flag, a package with no test files is skipped and the toolchain
// prints "[no test files]". With it, the toolchain builds an instrumented test
// binary for every package in the pattern, so the same package returns a plain
// "pass" with no tests in it and no marker anywhere.
//
// Since -coverpkg is the recommended default here, precisely because it keeps
// untested packages in the coverage denominator, keying only on the marker
// reported zero untested packages on essentially every real run. Verified
// against Go 1.26 by running this repo's own suite both ways: cmd/repo-metrics
// emits "skip" plus the marker without the flag, and a bare "pass" with it.
func TestUntestedPackageIsFoundWithAndWithoutCoverpkg(t *testing.T) {
	// What `go test ./... -json` emits: explicit marker.
	withoutCoverpkg := `
{"Action":"output","Package":"m/cmd/tool","Output":"?   \tm/cmd/tool\t[no test files]\n"}
{"Action":"skip","Package":"m/cmd/tool","Elapsed":0}
{"Action":"pass","Package":"m/lib","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"m/lib","Elapsed":0.1}
`
	// What `go test ./... -json -coverpkg=./...` emits for the same tree: the
	// untested package passes, silently.
	withCoverpkg := `
{"Action":"pass","Package":"m/cmd/tool","Elapsed":0.05}
{"Action":"pass","Package":"m/lib","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"m/lib","Elapsed":0.1}
`

	for name, stream := range map[string]string{
		"without coverpkg": withoutCoverpkg,
		"with coverpkg":    withCoverpkg,
	} {
		t.Run(name, func(t *testing.T) {
			s, err := golang.ParseTestJSON(strings.NewReader(stream))
			if err != nil {
				t.Fatalf("ParseTestJSON: %v", err)
			}
			if got := s.PackagesWithoutTests(); got != 1 {
				t.Errorf("PackagesWithoutTests: got %d, want 1. "+
					"An untested package went unreported, which is what makes a "+
					"flattering coverage number look earned.", got)
			}
			for _, p := range s.Packages {
				want := p.Package == "m/cmd/tool"
				if p.Untested() != want {
					t.Errorf("%s: Untested() = %v, want %v", p.Package, p.Untested(), want)
				}
			}
		})
	}
}

// A package that ran tests is never untested, however they turned out. In
// particular a package whose every test skipped itself has tests, and saying
// otherwise would report deliberately-skipped code as unexercised.
func TestPackagesThatRanTestsAreNotUntested(t *testing.T) {
	s, err := golang.ParseTestJSON(strings.NewReader(`
{"Action":"skip","Package":"m/allskipped","Test":"TestOne","Elapsed":0}
{"Action":"skip","Package":"m/allskipped","Elapsed":0}
{"Action":"fail","Package":"m/broken","Test":"TestTwo","Elapsed":0.01}
{"Action":"fail","Package":"m/broken","Elapsed":0.05}
{"Action":"pass","Package":"m/subtests","Test":"TestTable/case","Elapsed":0.01}
{"Action":"pass","Package":"m/subtests","Elapsed":0.05}
`))
	if err != nil {
		t.Fatalf("ParseTestJSON: %v", err)
	}
	if got := s.PackagesWithoutTests(); got != 0 {
		t.Errorf("PackagesWithoutTests: got %d, want 0", got)
	}
}

// A package seen only through output events, with no verdict of its own, has
// not been shown to be untested. Guessing from silence is how the unmeasured
// gets reported as measured.
func TestPackageWithNoVerdictIsNotAssumedUntested(t *testing.T) {
	s, err := golang.ParseTestJSON(strings.NewReader(`
{"Action":"output","Package":"m/partial","Output":"some build chatter\n"}
{"Action":"pass","Package":"m/lib","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"m/lib","Elapsed":0.1}
`))
	if err != nil {
		t.Fatalf("ParseTestJSON: %v", err)
	}
	if got := s.PackagesWithoutTests(); got != 0 {
		t.Errorf("PackagesWithoutTests: got %d, want 0 for a package that never returned a verdict", got)
	}
}
