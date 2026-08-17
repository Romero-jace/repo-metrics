package lcov_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/lcov"
)

// realCoveragePyTracefile is the complete, unmodified tracefile coverage.py
// writes through pytest-cov. Captured with:
//
//	.pyenv/bin/pip install pytest pytest-cov
//	.pyenv/bin/pytest --cov=. --cov-report=lcov:coverage.lcov
//
// against a module holding a pass, a failing assert, a skip, an xfail, a raising
// fixture and a three-case parametrize.
//
// Nothing has been reformatted. Note what it establishes: the record order is
// SF, then the DA lines, then LF/LH, then the function block. There are NO
// branch records at all, because --cov-branch was not passed. And FN carries
// THREE comma-separated fields here, a start line, an end line and a name, where
// istanbul writes two. A parser that assumed a layout, required branch records,
// or counted fields in the records it ignores would break on this file.
const realCoveragePyTracefile = `SF:test_sample.py
DA:1,1
DA:4,1
DA:5,1
DA:8,1
DA:9,1
DA:12,1
DA:13,1
DA:14,0
DA:17,1
DA:18,1
DA:19,1
DA:22,1
DA:23,1
DA:24,1
DA:27,1
DA:28,0
DA:31,1
DA:32,1
DA:33,1
LF:19
LH:17
FN:4,5,test_passes
FNDA:1,test_passes
FN:8,9,test_fails
FNDA:1,test_fails
FN:13,14,test_skipped
FNDA:0,test_skipped
FN:18,19,test_expected_failure
FNDA:1,test_expected_failure
FN:23,24,broken
FNDA:1,broken
FN:27,28,test_errors
FNDA:0,test_errors
FN:32,33,test_parametrized
FNDA:1,test_parametrized
FNF:7
FNH:5
end_of_record
`

// istanbulShapedTracefile models the other emitter's layout, built from the
// writer's source rather than captured from a run:
//
//	istanbul-reports@3.2.0 lib/lcovonly/index.js:32-69 writes TN, SF, the FN
//	block with FNF/FNH before FNDA, then the DA lines, then LF/LH, then the
//	branch records, then end_of_record.
//
// It exists to pin that nothing here depends on record order, and that branch
// and function records are stepped over rather than interpreted.
const istanbulShapedTracefile = `TN:
SF:src/lib/a.ts
FN:3,add
FNF:2
FNH:1
FNDA:4,add
FN:9,sub
FNDA:0,sub
DA:3,4
DA:4,4
DA:9,0
DA:10,0
LF:4
LH:2
BRDA:3,0,0,4
BRDA:3,0,1,0
BRF:2
BRH:1
end_of_record
TN:
SF:src/lib/b.ts
DA:1,1
LF:1
LH:1
end_of_record
`

func parseOK(t *testing.T, in string) *lcov.Summary {
	t.Helper()
	got, _, err := lcov.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("Parse returned a nil summary and no error")
	}
	return got
}

func onlyFile(t *testing.T, s *lcov.Summary) lcov.File {
	t.Helper()
	if len(s.Files) != 1 {
		t.Fatalf("want one file, got %d: %+v", len(s.Files), s.Files)
	}
	return s.Files[0]
}

// The counts are LINES, read from the LF/LH summary the generator wrote.
func TestCoveragePyCounts(t *testing.T) {
	f := onlyFile(t, parseOK(t, realCoveragePyTracefile))

	if f.Name != "test_sample.py" {
		t.Errorf("Name: got %q", f.Name)
	}
	if f.Total != 19 || f.Covered != 17 {
		t.Errorf("covered/total: got %d/%d, want 17/19", f.Covered, f.Total)
	}
	// Two DA lines carry a zero hit count, which is exactly the 19 minus 17. The
	// summary and the detail agree here, and the next test is what happens when
	// only the detail exists.
	if f.Total-f.Covered != 2 {
		t.Errorf("uncovered lines: got %d, want 2", f.Total-f.Covered)
	}
}

// Record order is not assumed, branch records are stepped over, and the function
// block is ignored whatever its field count.
func TestIstanbulLayoutParses(t *testing.T) {
	got := parseOK(t, istanbulShapedTracefile)

	if len(got.Files) != 2 {
		t.Fatalf("want two files, got %+v", got.Files)
	}
	if got.Files[0].Name != "src/lib/a.ts" || got.Files[1].Name != "src/lib/b.ts" {
		t.Errorf("files are not sorted by name: %q, %q", got.Files[0].Name, got.Files[1].Name)
	}
	if f := got.Files[0]; f.Covered != 2 || f.Total != 4 {
		t.Errorf("a.ts: got %d/%d, want 2/4", f.Covered, f.Total)
	}

	covered, total := got.Totals()
	if covered != 3 || total != 5 {
		t.Errorf("Totals: got %d/%d, want 3/5", covered, total)
	}
	// Summed as counts, so the repo rate is 60 percent rather than the mean of
	// 50 and 100, which would be 75. That difference is the whole reason counts
	// are stored instead of rates.
	if pct := float64(covered) / float64(total) * 100; pct != 60 {
		t.Errorf("repo rate: got %.1f, want 60.0; a mean of the per-file rates would be 75.0", pct)
	}
}

// A tracefile carrying DA lines and no LF/LH summary still measures. The format
// permits omitting the summary, and counting the detail lines is exact rather
// than an estimate: every instrumented line has one.
func TestCountsAreDerivedWhenTheSummaryIsAbsent(t *testing.T) {
	f := onlyFile(t, parseOK(t, "SF:x.ts\nDA:1,3\nDA:2,0\nDA:3,1\nend_of_record\n"))
	if f.Covered != 2 || f.Total != 3 {
		t.Errorf("got %d/%d, want 2/3 derived from the DA lines", f.Covered, f.Total)
	}

	// The control: where a summary IS present it wins, because it is the
	// generator's own answer and may account for lines it chose not to detail.
	f2 := onlyFile(t, parseOK(t, "SF:x.ts\nDA:1,1\nLF:10\nLH:6\nend_of_record\n"))
	if f2.Covered != 6 || f2.Total != 10 {
		t.Errorf("got %d/%d, want the LF/LH summary to win over the one DA line", f2.Covered, f2.Total)
	}
}

// A zero-byte tracefile is not a coverage of zero percent.
//
// This is not hypothetical. vitest 4.1.10 with @vitest/coverage-v8 4.1.4 wrote
// exactly this for a full 1,964-test run of a real repository and exited 0, with
// an lcov-report directory beside it containing only its static assets. Nothing
// about the run looked wrong.
//
// It parses rather than erroring, because an empty file is a legitimate thing to
// read; what it must not do is produce a file record, because a record is what
// the caller turns into a marker row.
func TestAnEmptyTracefileMeasuresNothing(t *testing.T) {
	got := parseOK(t, "")
	if len(got.Files) != 0 {
		t.Errorf("files from an empty tracefile: %+v", got.Files)
	}
	covered, total := got.Totals()
	if covered != 0 || total != 0 {
		t.Errorf("Totals: got %d/%d, want 0/0", covered, total)
	}
}

// A record with no line information yields a zero total, which the caller drops
// rather than writing a zero denominator into the scope set.
func TestARecordWithNoLinesHasNoTotal(t *testing.T) {
	f := onlyFile(t, parseOK(t, "SF:x.ts\nFNF:2\nFNH:1\nend_of_record\n"))
	if f.Total != 0 {
		t.Errorf("Total: got %d, want 0; nothing in this record counts lines", f.Total)
	}
}

// An unterminated final record is still counted. A truncated tracefile is what a
// killed coverage run leaves behind, and dropping the last record would silently
// shorten the report by one file.
func TestAnUnterminatedRecordIsStillCounted(t *testing.T) {
	got := parseOK(t, "SF:a.ts\nLF:10\nLH:5\nend_of_record\nSF:b.ts\nLF:4\nLH:4\n")
	if len(got.Files) != 2 {
		t.Fatalf("want two files, got %+v", got.Files)
	}
	if f := got.Files[1]; f.Covered != 4 || f.Total != 4 {
		t.Errorf("the unterminated record: got %d/%d, want 4/4", f.Covered, f.Total)
	}
}

// A duplicate path is not merged, because every way of merging invents a number.
// Summing inflates the denominator and taking per-field maxima pairs a numerator
// and a denominator from different records.
func TestADuplicatePathIsReportedNotMerged(t *testing.T) {
	got, diags, err := lcov.Parse(strings.NewReader("SF:a.ts\nLF:10\nLH:5\nend_of_record\nSF:a.ts\nLF:10\nLH:9\nend_of_record\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := onlyFile(t, got)
	if f.Covered != 5 {
		t.Errorf("Covered: got %d, want the first record's 5", f.Covered)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "a.ts") {
		t.Errorf("want one diagnostic naming the duplicated path, got %+v", diags)
	}
}

// A covered count above the total is a generator disagreeing with itself.
// Clamping keeps a rate above 100 percent off the report.
func TestCoveredIsClampedToTotal(t *testing.T) {
	f := onlyFile(t, parseOK(t, "SF:a.ts\nLF:4\nLH:9\nend_of_record\n"))
	if f.Covered != 4 {
		t.Errorf("Covered: got %d, want it clamped to the total of 4", f.Covered)
	}
}

// Something that is not a tracefile must fail rather than measure zero.
func TestNonTracefilesAreFatal(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"xml", `<?xml version="1.0"?><coverage lines-valid="10" lines-covered="5"/>`},
		{"prose", "Coverage report\n  90% of lines covered\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := lcov.Parse(strings.NewReader(tc.body))
			if err == nil {
				t.Fatalf("Parse accepted a non-tracefile, returning %+v", got)
			}
			if got != nil {
				t.Error("Parse returned both an error and a summary; a caller could publish those counts")
			}
			if !strings.Contains(err.Error(), "not a tracefile") {
				t.Errorf("error %q does not say what is wrong", err)
			}
		})
	}
}
