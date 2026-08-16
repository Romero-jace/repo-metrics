package golang_test

import (
	"testing"
)

// The two signals are tested apart on purpose. A real stream carries both, so a
// fixture with both in it passes whether or not either half works, and the
// structured field is the one that was being silently discarded.
func TestABuildFailureIsRecognizedFromEitherSignal(t *testing.T) {
	cases := map[string]string{
		// What a current toolchain puts on the fail event. No output line at
		// all, so this is the structured field or nothing.
		"the FailedBuild field alone": `{"Action":"fail","Package":"m/broken","Elapsed":0,"FailedBuild":"m/broken"}
`,
		// What a stream captured from an older toolchain carries, and what
		// survives being replayed from a file somebody kept.
		"the output marker alone": `{"Action":"output","Package":"m/broken","Output":"FAIL\tm/broken [build failed]\n"}
{"Action":"fail","Package":"m/broken","Elapsed":0}
`,
		// The variant the toolchain uses when TestMain or a package-level
		// fixture is what failed rather than the compile itself.
		"a setup failure": `{"Action":"output","Package":"m/broken","Output":"FAIL\tm/broken [setup failed]\n"}
{"Action":"fail","Package":"m/broken","Elapsed":0}
`,
	}

	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			s := parseJSON(t, stream)
			p := pkgTests(t, s, "m/broken")

			if !p.BuildFailed {
				t.Fatal("the package is not marked as having failed to build, so it is about to be counted as a package with no tests")
			}
			if p.Untested() {
				t.Error("a package whose tests never ran is reported as having none, which is a measurement nobody took")
			}
			if got := s.PackagesWithoutTests(); got != 0 {
				t.Errorf("packages without tests: got %d, want 0", got)
			}
			if got := s.PackagesThatWouldNotBuild(); got != 1 {
				t.Errorf("packages that would not build: got %d, want 1", got)
			}
			if got := s.PackagesFailingToBuild(); len(got) != 1 || got[0] != "m/broken" {
				t.Errorf("names of packages that would not build: got %v, want [m/broken]", got)
			}
		})
	}
}

// The distinction the whole change turns on. These two packages produce nearly
// identical events: a package-level verdict with no test results in it. One
// genuinely has no tests and one has tests nobody could run.
func TestAPackageWithNoTestsIsNotConfusedWithOneThatWouldNotBuild(t *testing.T) {
	s := parseJSON(t, `{"Action":"output","Package":"m/empty","Output":"?   \tm/empty\t[no test files]\n"}
{"Action":"skip","Package":"m/empty","Elapsed":0}
{"Action":"output","Package":"m/broken","Output":"FAIL\tm/broken [build failed]\n"}
{"Action":"fail","Package":"m/broken","Elapsed":0,"FailedBuild":"m/broken"}
`)

	if empty := pkgTests(t, s, "m/empty"); !empty.Untested() || empty.BuildFailed {
		t.Errorf("m/empty: Untested=%v BuildFailed=%v, want true and false", empty.Untested(), empty.BuildFailed)
	}
	if broken := pkgTests(t, s, "m/broken"); broken.Untested() || !broken.BuildFailed {
		t.Errorf("m/broken: Untested=%v BuildFailed=%v, want false and true", broken.Untested(), broken.BuildFailed)
	}
	if got := s.PackagesWithoutTests(); got != 1 {
		t.Errorf("packages without tests: got %d, want 1, the empty one only", got)
	}
	if got := s.PackagesThatWouldNotBuild(); got != 1 {
		t.Errorf("packages that would not build: got %d, want 1, the broken one only", got)
	}
}

// A package whose build failed because a DEPENDENCY of its test binary failed
// names that other package in FailedBuild. It is still this package that did not
// run, so it is still this package that must not be counted as untested.
func TestAPackageIsMarkedEvenWhenAnotherOneIsNamed(t *testing.T) {
	s := parseJSON(t, `{"Action":"fail","Package":"m/consumer","Elapsed":0,"FailedBuild":"m/dependency"}
`)

	p := pkgTests(t, s, "m/consumer")
	if !p.BuildFailed {
		t.Error("the package that failed to build is the one reporting the event, whatever the field names")
	}
	if p.Untested() {
		t.Error("a package that could not link its dependency is reported as having no tests")
	}
}
