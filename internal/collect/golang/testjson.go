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
	NoTestFiles bool
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
		if p.NoTestFiles {
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

		if ev.Action == "output" {
			if ev.Test == "" && strings.Contains(ev.Output, noTestFilesMarker) {
				p.NoTestFiles = true
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
