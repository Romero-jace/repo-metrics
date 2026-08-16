package golang

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// noTestFilesMarker is what the toolchain prints for a package that has no test
// files at all.
//
// The marker is load-bearing rather than decorative. A no-test-files package
// reports Action "skip" with an empty Test field, but so does a package whose
// every test called t.Skip. Requiring the marker as well is what keeps a
// deliberately-skipped suite from being miscounted as untested code.
const noTestFilesMarker = "[no test files]"

// buildFailedMarkers are what the toolchain prints on the output stream when a
// package's test binary could not be produced.
//
// The structured FailedBuild field on the fail event is the primary signal and
// these are the belt to its braces: the field is comparatively recent, and a
// stream captured from an older toolchain, or replayed from a file somebody
// kept, still carries the text. Reading both costs one string comparison and
// removes a silent dependency on which toolchain wrote the stream.
var buildFailedMarkers = []string{"[build failed]", "[setup failed]"}

// TestSummary is a parsed `go test -json` stream, aggregated per package.
type TestSummary struct {
	// Packages is sorted by import path.
	Packages []PackageTests
	// Malformed counts lines that were not usable JSON events. A killed run
	// leaves a truncated final line, and a failed build can put non-JSON text
	// on stdout, so these are tolerated rather than fatal.
	Malformed int
}

// PackageTests is one package's test results.
type PackageTests struct {
	Package string
	// Passed, Failed, and Skipped count TOP-LEVEL tests only.
	//
	// Subtests are tracked separately because table-driven counts swing with
	// the size of a fixture table, and a headline "tests: 1240 -> 1255" should
	// track test functions rather than table rows.
	Passed  int
	Failed  int
	Skipped int
	// Subtests counts every result whose name contains a slash.
	Subtests int
	Duration time.Duration
	// NoTestFiles reports a package the toolchain skipped for having no test
	// files, as opposed to one whose tests all skipped themselves.
	//
	// Only set when the toolchain printed the marker, which it does NOT do
	// under -coverpkg. See HasResult.
	NoTestFiles bool
	// HasResult reports whether the package produced a package-level verdict.
	//
	// It exists because -coverpkg=./... changes what an untested package looks
	// like. Without it, such a package is skipped and marked "[no test files]".
	// With it, the toolchain builds an instrumented test binary for every
	// package in the pattern, so the same package reports a plain "pass" and
	// the marker never appears. Since -coverpkg is the recommended default
	// here (it is what keeps untested packages in the coverage denominator),
	// the marker alone would report zero untested packages on almost every
	// real run.
	HasResult bool
	// BuildFailed reports a package whose test binary would not compile.
	//
	// The toolchain reports this as a package-level "fail" with no test events
	// in it, which is byte-identical to how it reports a package that built
	// fine and contains no tests. Telling them apart is the entire reason this
	// field exists: without it, breaking a build reads as deleting the tests.
	BuildFailed bool
}

// Untested reports whether this package carries no tests at all.
//
// Two signals, because the toolchain's answer depends on flags nobody should
// have to think about: the explicit marker, or a package that returned a
// verdict without a single test result in it.
//
// A package whose build failed is neither. It may be full of tests, and nobody
// can say, because none of them ran. Counting it here published a fabricated
// measurement rather than a missing one: the report announced a package that
// has no tests, which is a fact nothing established. That is worse than the
// silence this project is usually guarding against, and it shipped.
func (p PackageTests) Untested() bool {
	if p.BuildFailed {
		return false
	}
	return p.NoTestFiles ||
		(p.HasResult && p.Passed+p.Failed+p.Skipped+p.Subtests == 0)
}

// PackagesThatWouldNotBuild counts packages whose test binary failed to compile.
//
// Any non-zero answer makes every other number from this stream a partial count
// rather than a total, which is what the delta layer needs to know before it
// subtracts one week's totals from another's.
func (s *TestSummary) PackagesThatWouldNotBuild() int {
	var n int
	for _, p := range s.Packages {
		if p.BuildFailed {
			n++
		}
	}
	return n
}

// PackagesFailingToBuild names those packages, sorted, for a diagnostic that
// tells an operator which ones to go and look at.
func (s *TestSummary) PackagesFailingToBuild() []string {
	var out []string
	for _, p := range s.Packages {
		if p.BuildFailed {
			out = append(out, p.Package)
		}
	}
	sort.Strings(out)
	return out
}

// Totals sums the per-package numbers.
func (s *TestSummary) Totals() (tests, failed, skipped int, elapsed time.Duration) {
	for _, p := range s.Packages {
		tests += p.Passed + p.Failed + p.Skipped
		failed += p.Failed
		skipped += p.Skipped
		elapsed += p.Duration
	}
	return tests, failed, skipped, elapsed
}

// PackagesWithoutTests counts packages that carry no test files.
//
// This is the number a coverage profile alone cannot tell you, and leaving it
// out is what lets a repo report a flattering coverage percentage while whole
// directories go unexercised.
func (s *TestSummary) PackagesWithoutTests() int {
	var n int
	for _, p := range s.Packages {
		if p.Untested() {
			n++
		}
	}
	return n
}

// testEvent is one line of the test2json stream. Only the fields we use.
type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
	// FailedBuild is set by the toolchain on the package-level fail event when
	// the test binary would not compile. It carries the id of the package that
	// failed to build, which is not always this one: a package fails when a
	// dependency of its test binary fails.
	//
	// The toolchain has always told us this and the struct simply did not ask,
	// so the answer was discarded by the decoder before anything could read it.
	FailedBuild string `json:"FailedBuild"`
}

// ParseTestJSON reads a `go test -json` event stream.
//
// Verified against Go 1.26: a package with no test files emits Action "skip"
// with an empty Test alongside an output line carrying "[no test files]", while
// a package that has tests emits "pass" or "fail" even when every test is
// filtered out by -run.
func ParseTestJSON(r io.Reader) (*TestSummary, error) {
	sc := bufio.NewScanner(r)
	// Output events carry whole lines of test output, which can be long.
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	summary := &TestSummary{}
	byPkg := make(map[string]*PackageTests)
	var events int

	pkg := func(name string) *PackageTests {
		p, ok := byPkg[name]
		if !ok {
			p = &PackageTests{Package: name}
			byPkg[name] = p
		}
		return p
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev testEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// A truncated last line from a killed run, or build noise. Neither
			// justifies discarding the results we did parse.
			summary.Malformed++
			continue
		}
		events++

		if ev.Package == "" {
			continue
		}
		p := pkg(ev.Package)

		// A build failure is announced on the fail event itself, and separately
		// printed on the output stream. Either is enough, and both arrive before
		// the package-level result is counted below.
		if ev.FailedBuild != "" {
			p.BuildFailed = true
		}

		if ev.Action == "output" {
			if ev.Test == "" && strings.Contains(ev.Output, noTestFilesMarker) {
				p.NoTestFiles = true
			}
			if ev.Test == "" && containsAny(ev.Output, buildFailedMarkers) {
				p.BuildFailed = true
			}
			continue
		}

		switch {
		case ev.Test != "":
			// A subtest name is "TestParent/case".
			if strings.Contains(ev.Test, "/") {
				if isResult(ev.Action) {
					p.Subtests++
				}
				continue
			}
			switch ev.Action {
			case "pass":
				p.Passed++
			case "fail":
				p.Failed++
			case "skip":
				p.Skipped++
			}
		case isResult(ev.Action):
			// Package-level result. Elapsed is the package's wall time.
			p.HasResult = true
			p.Duration = time.Duration(ev.Elapsed * float64(time.Second))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("golang: reading test json: %w", err)
	}
	if events == 0 {
		return nil, errors.New("golang: no usable events in the test json stream")
	}

	summary.Packages = make([]PackageTests, 0, len(byPkg))
	for _, p := range byPkg {
		summary.Packages = append(summary.Packages, *p)
	}
	sort.Slice(summary.Packages, func(i, j int) bool {
		return summary.Packages[i].Package < summary.Packages[j].Package
	})
	return summary, nil
}

func isResult(action string) bool {
	return action == "pass" || action == "fail" || action == "skip"
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
