// Package junit parses the JUnit XML documents that test runners emit.
//
// It is to test results what the sarif package is to lint findings: one parser
// for every language this tool will ever be pointed at, chosen over each
// runner's native output for exactly that reason. pytest writes it with
// --junitxml, vitest with --reporter=junit, jest through jest-junit, and Go
// through go-junit-report.
//
// There is no official JUnit schema and the emitters genuinely disagree, so this
// reads the document rather than trusting its summary attributes. The two
// disagreements that drove the design, both verified against real output:
//
// Suite names are not comparable across runners. vitest writes one <testsuite>
// per file and names it with the file's path; pytest writes ONE <testsuite>
// named "pytest" for an entire run and puts the module on each <testcase>'s
// classname. Grouping by suite name would give a vitest repo a useful breakdown
// and a pytest repo a single bucket, so cases are grouped by classname and fall
// back to the suite name only when there is none.
//
// Summary attributes are optional and omitted when zero. go-junit-report marks
// every attribute on its root omitempty, so a document produced from empty stdin
// is well formed and carries no numbers at all. Counting the <testcase> elements
// cannot be fooled that way, and it is also what tells a run that collected
// nothing from a run that found nothing, which is the distinction this whole
// tool exists to preserve.
package junit

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Severity classifies a diagnostic. It mirrors the collect package's own rather
// than importing it, so this package stays a parser of a public format rather
// than a piece of this tool.
type Severity string

// The severities in play.
const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Diagnostic explains something the caller should know about a document that
// still parsed.
type Diagnostic struct {
	Severity Severity
	Message  string
}

// Group is one scope's counts: a Python module, a TypeScript test file, a Go
// package, or whatever the emitter put in classname.
type Group struct {
	Name string
	// Passed, Failed and Skipped partition the cases in this group, so their sum
	// is the group's total and no case is counted twice.
	Passed, Failed, Skipped int
	// Duration is the sum of the cases' own times, which is machine work rather
	// than wall clock, the same way `go test`'s per-package elapsed is.
	Duration time.Duration
	// Timed says at least one case in this group carried a parseable time. When
	// it is false Duration is zero because nothing measured it, which is not the
	// same as a group that ran instantly.
	Timed bool
}

// Total is how many cases this group carried.
func (g Group) Total() int { return g.Passed + g.Failed + g.Skipped }

// Summary is one parsed document.
type Summary struct {
	// Groups are the scopes found, sorted by name so two runs of the same
	// document produce the same metric rows in the same order.
	Groups []Group
	// Cases is the total number of <testcase> elements read, across every group.
	//
	// It is the presence question for the whole document. A well formed file
	// carrying zero of them is what pytest writes when it collected nothing, and
	// it is indistinguishable from a genuine measurement of a repo with no tests,
	// so the caller must record nothing rather than a zero.
	Cases int
	// Suites counts the <testsuite> elements, including nested ones. Reported for
	// the caller's diagnostics; the counts above are the measurement.
	Suites int
}

// Parse reads one JUnit XML document.
//
// An error means nothing here is trustworthy. A document that is not XML at all,
// or whose root is neither <testsuites> nor <testsuite>, is that case: something
// other than a test report was pointed at, and reporting zero tests for it would
// be worse than reporting nothing.
func Parse(r io.Reader) (*Summary, []Diagnostic, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxDocument))
	// Some runners emit a Latin-1 or otherwise non-UTF-8 declaration. Without
	// this the decoder refuses the whole document over its header, which would
	// discard a report whose numbers are perfectly readable.
	dec.CharsetReader = passthroughCharset

	root, err := firstElement(dec)
	if err != nil {
		return nil, nil, err
	}

	var suites []xmlSuite
	switch root.Name.Local {
	case "testsuites":
		var doc xmlSuites
		if err := dec.DecodeElement(&doc, root); err != nil {
			return nil, nil, fmt.Errorf("junit: reading the report: %w", err)
		}
		suites = doc.Suites
	case "testsuite":
		var one xmlSuite
		if err := dec.DecodeElement(&one, root); err != nil {
			return nil, nil, fmt.Errorf("junit: reading the report: %w", err)
		}
		suites = []xmlSuite{one}
	default:
		return nil, nil, fmt.Errorf(
			"junit: the root element is <%s>, so this is not a JUnit report; want <testsuites> or <testsuite>",
			root.Name.Local)
	}

	sum := &Summary{}
	groups := make(map[string]*Group)
	var diags []Diagnostic
	for _, s := range suites {
		sum.collect(s, groups, &diags)
	}

	sum.Groups = make([]Group, 0, len(groups))
	for _, g := range groups {
		sum.Groups = append(sum.Groups, *g)
	}
	sort.Slice(sum.Groups, func(i, j int) bool { return sum.Groups[i].Name < sum.Groups[j].Name })

	return sum, diags, nil
}

// maxDocument bounds what will be read, so a runaway or truncated file cannot
// exhaust memory. A JUnit report for a very large suite is a few megabytes.
const maxDocument = 256 << 20

// passthroughCharset accepts any declared encoding and reads the bytes as they
// are. Everything this parser reads is attribute values holding names and
// numbers, which are ASCII in every emitter, so a mislabeled header should not
// cost the whole report.
func passthroughCharset(_ string, input io.Reader) (io.Reader, error) { return input, nil }

// firstElement advances to the document's root element.
func firstElement(dec *xml.Decoder) (*xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("junit: the report is empty, which is what a run that died before writing leaves behind")
		}
		if err != nil {
			return nil, fmt.Errorf("junit: reading the report: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			return &start, nil
		}
	}
}

// collect walks one suite and everything nested under it.
//
// Nesting is handled because the shape is legal and some emitters use it to
// model a directory tree. A parser that only read the top level would silently
// report a fraction of the suite, which looks exactly like a suite that shrank.
func (s *Summary) collect(suite xmlSuite, groups map[string]*Group, diags *[]Diagnostic) {
	s.Suites++
	for _, c := range suite.Cases {
		s.Cases++

		name := strings.TrimSpace(c.Classname)
		if name == "" {
			// jest's default classname template produces a describe-block title
			// rather than a path, and go-junit-report omits it in some shapes. The
			// suite's own name is the next best scope; an unnamed case in an
			// unnamed suite is filed under a literal so it is visibly unattributed
			// rather than silently merged into another group.
			name = strings.TrimSpace(suite.Name)
		}
		if name == "" {
			name = "(unnamed)"
		}

		g := groups[name]
		if g == nil {
			g = &Group{Name: name}
			groups[name] = g
		}

		switch {
		case len(c.Failures) > 0 || len(c.Errors) > 0:
			// Errors fold into failures rather than getting their own count.
			// vitest hardcodes the errors attribute to zero and never emits the
			// element, so a separate tally would read as "no errors" for one runner
			// and mean it for another. Folding them keeps the number comparable.
			g.Failed++
		case len(c.Skipped) > 0:
			// pytest files an expected failure as <skipped type="pytest.xfail">, so
			// xfail lands here. That is the closest true statement available: the
			// case did not run to a verdict.
			g.Skipped++
		default:
			g.Passed++
		}

		if d, ok := parseSeconds(c.Time); ok {
			g.Duration += d
			g.Timed = true
		}
	}

	for _, nested := range suite.Suites {
		s.collect(nested, groups, diags)
	}
}

// parseSeconds reads a time attribute, which every emitter writes as seconds
// with a decimal point.
//
// An absent or unreadable value answers false rather than zero. A duration of
// zero is a real thing a fast test does, and using it for "nobody wrote one"
// would put an unmeasured value into a sum that gets published.
func parseSeconds(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// The document shape. Only the attributes this parser reads are declared:
// the summary attributes (tests, failures, errors, skipped) are deliberately
// absent, because they are optional in every emitter and omitted when zero, so
// reading them would make an empty report and a passing one identical.
type xmlSuites struct {
	Suites []xmlSuite `xml:"testsuite"`
}

type xmlSuite struct {
	Name   string     `xml:"name,attr"`
	Cases  []xmlCase  `xml:"testcase"`
	Suites []xmlSuite `xml:"testsuite"`
}

type xmlCase struct {
	Classname string `xml:"classname,attr"`
	Time      string `xml:"time,attr"`
	// Slices rather than pointers so that a case carrying two failure elements,
	// which a retrying runner emits, is still one failed case.
	Failures []struct{} `xml:"failure"`
	Errors   []struct{} `xml:"error"`
	Skipped  []struct{} `xml:"skipped"`
}
