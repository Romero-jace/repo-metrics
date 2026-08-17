package junit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect/junit"
)

// realPytestDoc is the complete, unmodified document pytest 9.1.1 writes for a
// suite containing one of everything. Captured with:
//
//	python3 -m venv .pyenv && .pyenv/bin/pip install pytest
//	.pyenv/bin/pytest --junitxml=real.xml
//
// against a module holding a pass, a failing assert, an explicit skip, an xfail,
// a test whose fixture raises, and a three-case parametrize.
//
// Nothing has been reformatted. Note what it establishes, none of which is
// guessable from the format's description: there is exactly ONE <testsuite> and
// it is named "pytest" rather than after any file, so the module lives on each
// case's classname and grouping by suite name would put an entire Python repo in
// one bucket. An xfail is filed as <skipped type="pytest.xfail">. A fixture that
// raises produces <error>, which is a separate attribute from failures. And the
// three parametrize cases are three <testcase> elements, so this counts what
// `go test -json` deliberately does not.
const realPytestDoc = `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="1" failures="1" skipped="2" tests="8" time="0.048" timestamp="2026-08-17T08:35:27.231048-04:00" hostname="Chalupa-Batman.local"><testcase classname="test_sample" name="test_passes" time="0.001" /><testcase classname="test_sample" name="test_fails" time="0.001"><failure message="assert 1 == 2">def test_fails():
&gt;       assert 1 == 2
E       assert 1 == 2

test_sample.py:9: AssertionError</failure></testcase><testcase classname="test_sample" name="test_skipped" time="0.000"><skipped type="pytest.skip" message="not ready">test_sample.py:12: not ready</skipped></testcase><testcase classname="test_sample" name="test_expected_failure" time="0.000"><skipped type="pytest.xfail" message="" /></testcase><testcase classname="test_sample" name="test_errors" time="0.000"><error message="failed on setup with &quot;RuntimeError: fixture blew up&quot;">@pytest.fixture
    def broken():
&gt;       raise RuntimeError("fixture blew up")
E       RuntimeError: fixture blew up

test_sample.py:24: RuntimeError</error></testcase><testcase classname="test_sample" name="test_parametrized[1]" time="0.001" /><testcase classname="test_sample" name="test_parametrized[2]" time="0.000" /><testcase classname="test_sample" name="test_parametrized[3]" time="0.000" /></testsuite></testsuites>`

// realPytestEmptyDoc is what pytest 9.1.1 writes when it collects nothing.
// Captured the same way, run against an empty directory. The process exited 5.
//
// This is the single most important fixture in this package. The document is
// complete, well formed, and carries a full set of zeroes, so it is
// indistinguishable from a genuine measurement of a repo with no tests. Nothing
// inside it says which one happened.
const realPytestEmptyDoc = `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="0" failures="0" skipped="0" tests="0" time="0.004" timestamp="2026-08-17T08:35:13.584935-04:00" hostname="Chalupa-Batman.local" /></testsuites>`

// vitestShapedDoc is a vitest report, built to the attribute set read out of the
// installed reporter rather than captured from a run, so treat it as a model of
// that source and not as evidence about vitest's behavior:
//
//	node_modules/vitest/dist/chunks/index.UpGiHP7g.js:3841-3860 writes the
//	<testsuites> root and one <testsuite> per file, named with the file's path
//	relative to the config root, with errors hardcoded to the literal 0.
//	:3734-3747 writes each <testcase> with classname defaulting to that same
//	relative path.
//
// It exists to pin the second dialect: per-file suites, a classname that is a
// path rather than a module, and an errors attribute that is always zero and
// must never be read as evidence.
const vitestShapedDoc = `<?xml version="1.0" encoding="UTF-8" ?>
<testsuites name="vitest tests" tests="3" failures="1" errors="0" time="1.5">
    <testsuite name="src/lib/a.test.ts" timestamp="2026-08-17T12:00:00.000Z" hostname="host" tests="2" failures="1" errors="0" skipped="0" time="1.0">
        <testcase classname="src/lib/a.test.ts" name="adds" time="0.4"/>
        <testcase classname="src/lib/a.test.ts" name="subtracts" time="0.6">
            <failure message="expected 1 to be 2">AssertionError</failure>
        </testcase>
    </testsuite>
    <testsuite name="src/lib/b.test.ts" timestamp="2026-08-17T12:00:01.000Z" hostname="host" tests="1" failures="0" errors="0" skipped="1" time="0.5">
        <testcase classname="src/lib/b.test.ts" name="pending" time="0.5">
            <skipped/>
        </testcase>
    </testsuite>
</testsuites>`

func parseOK(t *testing.T, in string) *junit.Summary {
	t.Helper()
	got, _, err := junit.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("Parse returned a nil summary and no error")
	}
	return got
}

func parseErr(t *testing.T, in string) error {
	t.Helper()
	got, _, err := junit.Parse(strings.NewReader(in))
	if err == nil {
		t.Fatalf("Parse: expected an error, got summary %+v", got)
	}
	if got != nil {
		t.Errorf("Parse returned both an error and a summary %+v; a caller could publish those counts", got)
	}
	return err
}

func onlyGroup(t *testing.T, s *junit.Summary) junit.Group {
	t.Helper()
	if len(s.Groups) != 1 {
		t.Fatalf("want one group, got %d: %+v", len(s.Groups), s.Groups)
	}
	return s.Groups[0]
}

// The counts come from reading the cases, not from the suite's summary
// attributes, and the two deliberately disagree here.
//
// pytest reports failures="1" errors="1" as separate numbers; this parser folds
// them, because vitest hardcodes errors to zero and never emits the element, so
// a separate tally would mean two different things depending on who wrote the
// file. Four passes, two failed, two skipped, and the skipped pair includes the
// xfail.
func TestPytestCountsComeFromTheCases(t *testing.T) {
	got := parseOK(t, realPytestDoc)

	if got.Cases != 8 {
		t.Errorf("Cases: got %d, want 8", got.Cases)
	}
	if got.Suites != 1 {
		t.Errorf("Suites: got %d, want 1", got.Suites)
	}

	g := onlyGroup(t, got)
	if g.Name != "test_sample" {
		t.Errorf("group name %q; pytest names its only suite \"pytest\", so the module has to come from classname", g.Name)
	}
	if g.Passed != 4 || g.Failed != 2 || g.Skipped != 2 {
		t.Errorf("passed/failed/skipped: got %d/%d/%d, want 4/2/2", g.Passed, g.Failed, g.Skipped)
	}
	if g.Total() != got.Cases {
		t.Errorf("the group totals %d but %d cases were read, so a case was counted twice or dropped", g.Total(), got.Cases)
	}
	// A test whose fixture raised produced <error>, not <failure>. Reading only
	// <failure> would report one failed test where there are two, and the suite
	// would look healthier than it is.
	if g.Failed != 2 {
		t.Error("an <error> case was not counted as failed, so a broken fixture reads as a passing suite")
	}
	if !g.Timed {
		t.Fatal("no case carried a readable time, but three of them do")
	}
	if want := 3 * time.Millisecond; g.Duration != want {
		t.Errorf("Duration: got %s, want %s", g.Duration, want)
	}
}

// A well formed report with no cases in it is NOT a measurement of zero tests.
//
// This is the whole reason the parser counts elements rather than reading the
// tests attribute. Verified against pytest 9.1.1: a run that collects nothing
// exits 5 and still writes this, complete with a full set of zeroes. A parser
// that trusted the attributes would publish "this repo has zero tests" for a
// misconfigured test path, and it would look like the answer.
func TestAReportWithNoCasesIsNotAZero(t *testing.T) {
	got := parseOK(t, realPytestEmptyDoc)

	if got.Cases != 0 {
		t.Errorf("Cases: got %d, want 0", got.Cases)
	}
	if len(got.Groups) != 0 {
		t.Errorf("groups from a document with no cases: %+v", got.Groups)
	}
	// It still parsed, so this is not an error. The caller decides what an empty
	// report means, and it has the exit code this parser does not.
	if got.Suites != 1 {
		t.Errorf("Suites: got %d, want 1; the suite element is there, it is just empty", got.Suites)
	}
}

// The second dialect: per-file suites and a classname that is a path.
func TestVitestGroupsByFile(t *testing.T) {
	got := parseOK(t, vitestShapedDoc)

	if got.Cases != 3 || got.Suites != 2 {
		t.Errorf("Cases/Suites: got %d/%d, want 3/2", got.Cases, got.Suites)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("want two groups, got %+v", got.Groups)
	}
	// Sorted, so the rows a collection writes do not shuffle between runs.
	if got.Groups[0].Name != "src/lib/a.test.ts" || got.Groups[1].Name != "src/lib/b.test.ts" {
		t.Errorf("groups are not sorted by name: %q, %q", got.Groups[0].Name, got.Groups[1].Name)
	}
	if g := got.Groups[0]; g.Passed != 1 || g.Failed != 1 || g.Skipped != 0 {
		t.Errorf("a.test.ts: got %d/%d/%d, want 1/1/0", g.Passed, g.Failed, g.Skipped)
	}
	// A bare <skipped/> with no attributes still marks the case skipped. vitest
	// writes exactly that, so a parser keying on the type attribute would count
	// every vitest skip as a pass.
	if g := got.Groups[1]; g.Skipped != 1 || g.Passed != 0 {
		t.Errorf("b.test.ts: got passed=%d skipped=%d, want 0/1", g.Passed, g.Skipped)
	}
}

// A bare <testsuite> root is legal and some emitters write it. Reading only
// <testsuites> would reject the document outright.
func TestABareTestsuiteRootIsAccepted(t *testing.T) {
	got := parseOK(t, `<testsuite name="pkg" tests="1"><testcase classname="pkg" name="works" time="0.5"/></testsuite>`)

	g := onlyGroup(t, got)
	if g.Name != "pkg" || g.Passed != 1 {
		t.Errorf("got %+v, want one passing case in group pkg", g)
	}
}

// Nested suites are legal, and some emitters use them to model a directory tree.
// Reading only the top level would report a fraction of the suite, which looks
// exactly like a suite that shrank.
func TestNestedSuitesAreCounted(t *testing.T) {
	got := parseOK(t, `<testsuites>
		<testsuite name="outer">
			<testcase classname="outer" name="a" time="0.1"/>
			<testsuite name="inner">
				<testcase classname="inner" name="b" time="0.1"/>
			</testsuite>
		</testsuite>
	</testsuites>`)

	if got.Cases != 2 {
		t.Errorf("Cases: got %d, want 2; a nested suite's cases were dropped", got.Cases)
	}
	if got.Suites != 2 {
		t.Errorf("Suites: got %d, want 2", got.Suites)
	}
	if len(got.Groups) != 2 {
		t.Errorf("want two groups, got %+v", got.Groups)
	}
}

// jest's default classname template produces a describe-block title rather than
// a path, and go-junit-report omits it in some shapes. The suite's own name is
// the next best scope, and a case with neither is filed somewhere visible rather
// than merged into whatever group happens to be first.
func TestClassnameFallsBackToTheSuiteName(t *testing.T) {
	got := parseOK(t, `<testsuites>
		<testsuite name="from-the-suite"><testcase name="a" time="0.1"/></testsuite>
		<testsuite><testcase name="b" time="0.1"/></testsuite>
	</testsuites>`)

	var names []string
	for _, g := range got.Groups {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "(unnamed),from-the-suite" {
		t.Errorf("groups %v, want the suite name used as a fallback and an unnamed case filed visibly", names)
	}
}

// A time nobody wrote is not a duration of zero.
//
// A fast test genuinely takes no measurable time, so zero is a real value here.
// Using it for an absent attribute would put an unmeasured number into a sum that
// gets published and compared against last week's real one.
func TestAnAbsentTimeIsNotZero(t *testing.T) {
	got := parseOK(t, `<testsuite name="pkg"><testcase classname="pkg" name="a"/></testsuite>`)
	if g := onlyGroup(t, got); g.Timed {
		t.Errorf("a case with no time attribute reported a measured duration of %s", g.Duration)
	}

	// The control: an explicit zero IS measured, or the check above would also
	// pass on a parser that discarded every duration.
	zero := parseOK(t, `<testsuite name="pkg"><testcase classname="pkg" name="a" time="0.000"/></testsuite>`)
	if g := onlyGroup(t, zero); !g.Timed {
		t.Error("an explicit time=\"0.000\" was treated as absent, so a genuinely instant suite reports no duration at all")
	}
}

// Something that is not a test report at all must fail rather than measure zero.
func TestNonReportsAreFatal(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		// A truncated document, which is what a killed runner leaves behind. It
		// gets far enough to have a root element, so this exercises the decode
		// path rather than the no-root one below.
		{"truncated", `<testsuites><testsuite name="a"><testcase name="b"/>`, "reading the report"},
		// Plain text with no element at all. Distinct from the truncated case: the
		// decoder never finds a root, which is also what a zero-byte file gives.
		{"not xml at all", "this is a log file, not a report\n", "empty"},
		{"empty", "", "empty"},
		// A coverage report pointed at the wrong format field. Reporting zero
		// tests for it would be a measurement of the wrong file.
		{"wrong root", `<coverage lines-valid="10" lines-covered="5"></coverage>`, "not a JUnit report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := parseErr(t, tc.body); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
