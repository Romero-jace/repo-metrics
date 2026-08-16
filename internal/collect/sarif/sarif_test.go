package sarif_test

import (
	"io"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect/sarif"
)

// realCleanDoc is the complete, unmodified SARIF document a clean golangci-lint
// v2.12.2 run writes. Captured with:
//
//	cd /Users/jace/Dev/repo-metrics && GOWORK=off golangci-lint run \
//	  --enable-only=godot --output.sarif.path clean.sarif \
//	  --output.text.path /dev/null ./internal/run/...
//
// Nothing has been reformatted. Note what is NOT here: the run carries only
// tool and results, the driver carries only a name, and there is no rules
// array at all. A parser that needs rules to resolve a severity would fail on
// the single most common real input.
const realCleanDoc = `{"version":"2.1.0","$schema":"https://schemastore.azurewebsites.net/schemas/json/sarif-2.1.0-rtm.6.json","runs":[{"tool":{"driver":{"name":"golangci-lint"}},"results":[]}]}`

// realDirtyDoc is a genuine golangci-lint v2.12.2 document from a run with
// findings, carrying two rule ids at two different levels. Captured with:
//
//	cd /Users/jace/Dev/repo-metrics && GOWORK=off golangci-lint run \
//	  --config mixed.golangci.yml --output.sarif.path mixed.sarif \
//	  --output.text.path /dev/null ./internal/collect/golang/...
//
// where mixed.golangci.yml enabled lll at line-length 40 so a clean repo still
// produced findings, and mapped godot to warning through a severity rule:
//
//	severity:
//	  default: error
//	  rules:
//	    - linters: [godot]
//	      severity: warning
//
// The full run emitted 50 error and 3 warning results, which is the same shape
// as the dirty run this parser was specified against. The only edit here is
// that 48 of the 53 result objects were deleted; the four that remain are byte
// identical to the emitter's output, including key order, nesting, and levels.
//
// This is the fixture that proves golangci-lint really does emit a level other
// than "error", so the level tally is checked against the emitter rather than
// only against hand-written JSON.
const realDirtyDoc = `{"version":"2.1.0","$schema":"https://schemastore.azurewebsites.net/schemas/json/sarif-2.1.0-rtm.6.json","runs":[{"tool":{"driver":{"name":"golangci-lint"}},"results":[{"ruleId":"lll","level":"error","message":{"text":"The line is 76 characters long, which exceeds the maximum of 40 characters."},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"../../../../../../Users/jace/Dev/repo-metrics/internal/collect/golang/coverprofile.go","index":0},"region":{"startLine":1,"startColumn":1}}}]},{"ruleId":"lll","level":"error","message":{"text":"The line is 46 characters long, which exceeds the maximum of 40 characters."},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"../../../../../../Users/jace/Dev/repo-metrics/internal/collect/golang/coverprofile.go","index":0},"region":{"startLine":2,"startColumn":1}}}]},{"ruleId":"godot","level":"warning","message":{"text":"Comment should end in a period"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"../../../../../../Users/jace/Dev/repo-metrics/internal/collect/golang/coverprofile_property_test.go","index":0},"region":{"startLine":84,"startColumn":1}}}]},{"ruleId":"godot","level":"warning","message":{"text":"Comment should end in a period"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"../../../../../../Users/jace/Dev/repo-metrics/internal/collect/golang/coverprofile_property_test.go","index":0},"region":{"startLine":85,"startColumn":1}}}]}]}]}`

// realCleanStdout is the exact 183 bytes golangci-lint writes to stdout for a
// clean run when the SARIF output is routed there. The document is followed by
// a newline and the human summary, which is the whole reason trailing prose is
// a warning rather than an error.
const realCleanStdout = realCleanDoc + "\n0 issues.\n"

// realDirtyTrailer is the byte sequence that followed the document on the same
// run written to stdout instead of a file. The counts refer to the untrimmed
// 53 result run.
const realDirtyTrailer = "\n53 issues:\n* godot: 3\n* lll: 50\n"

func parseOK(t *testing.T, in string) (*sarif.Summary, []sarif.Diagnostic) {
	t.Helper()
	got, diags, err := sarif.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("Parse returned a nil summary and no error")
	}
	return got, diags
}

func parseErr(t *testing.T, in string) error {
	t.Helper()
	got, _, err := sarif.Parse(strings.NewReader(in))
	if err == nil {
		t.Fatalf("Parse: expected an error, got summary %+v", got)
	}
	if got != nil {
		t.Errorf("Parse returned both an error and a summary %+v; a caller could publish those counts", got)
	}
	return err
}

// wantCounts asserts the whole shape of a summary at once, because a parser
// that gets the rule map right and the level map wrong is still publishing a
// wrong measurement.
func wantCounts(t *testing.T, got *sarif.Summary, rules, levels map[string]int, suppressed int) {
	t.Helper()
	assertMap(t, "Rules", got.Rules, rules)
	assertMap(t, "Levels", got.Levels, levels)
	if got.Suppressed != suppressed {
		t.Errorf("Suppressed: got %d, want %d", got.Suppressed, suppressed)
	}
	// Every active result contributes exactly one rule entry and one level
	// entry, so a disagreement means one of the two tallies dropped a result.
	var ruleTotal int
	for _, n := range got.Rules {
		ruleTotal += n
	}
	if ruleTotal != got.Active() {
		t.Errorf("rule counts sum to %d but Active() is %d; the two tallies disagree", ruleTotal, got.Active())
	}
}

func assertMap(t *testing.T, name string, got, want map[string]int) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s[%q]: got %d, want %d (full map %v)", name, k, got[k], w, got)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s has unexpected key %q with count %d", name, k, g)
		}
	}
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

func diagText(diags []sarif.Diagnostic) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(string(d.Severity))
		b.WriteString(": ")
		b.WriteString(d.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestRealCleanRun is the case the rest of the package is built around: a tool
// ran, found nothing, and that zero is a measurement rather than an absence.
func TestRealCleanRun(t *testing.T) {
	got, diags := parseOK(t, realCleanDoc)

	wantCounts(t, got, map[string]int{}, map[string]int{}, 0)
	if got.Active() != 0 {
		t.Errorf("Active: got %d, want 0", got.Active())
	}
	// The driver name is what separates "golangci-lint ran and found nothing"
	// from "nothing ran". Dropping it for a run with no results would make the
	// two indistinguishable downstream.
	assertStrings(t, "Drivers", got.Drivers, []string{"golangci-lint"})
	if len(diags) != 0 {
		t.Errorf("a clean document produced diagnostics:\n%s", diagText(diags))
	}
}

// TestRealDirtyRun proves the parser against the actual emitter rather than
// only against hand-written JSON, across two rule ids and two levels.
func TestRealDirtyRun(t *testing.T) {
	got, diags := parseOK(t, realDirtyDoc)

	wantCounts(t,
		got,
		map[string]int{"lll": 2, "godot": 2},
		map[string]int{sarif.LevelError: 2, sarif.LevelWarning: 2},
		0,
	)
	if got.Active() != 4 {
		t.Errorf("Active: got %d, want 4", got.Active())
	}
	assertStrings(t, "Drivers", got.Drivers, []string{"golangci-lint"})
	if len(diags) != 0 {
		t.Errorf("a well formed document produced diagnostics:\n%s", diagText(diags))
	}
}

// TestLeadingJunkIsFatal covers a rule with no line to revert: the strictness
// comes from json.Decoder starting at byte zero and from the absence of any
// scan-ahead. Demonstrated to fail by temporarily inserting the lenient
// preprocessor it forbids (skip to the first '{' before decoding), which turned
// every case here green.
func TestLeadingJunkIsFatal(t *testing.T) {
	cases := map[string]string{
		"crash banner":      "panic: runtime error: invalid memory address\n" + realCleanDoc,
		"warning line":      "WARN [runner] the skip-dirs option is deprecated\n" + realCleanDoc,
		"single stray byte": "x" + realCleanDoc,
		"shell prompt":      "$ golangci-lint run\n" + realCleanDoc,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, in)
			if strings.Contains(err.Error(), "truncated") {
				t.Errorf("leading junk was reported as truncation, which points at the wrong cause: %v", err)
			}
		})
	}
}

// TestLeadingWhitespaceIsFine guards the other side of the same boundary, so
// the strictness above cannot be satisfied by rejecting harmless formatting.
func TestLeadingWhitespaceIsFine(t *testing.T) {
	got, _ := parseOK(t, "\n\t  "+realCleanDoc)
	assertStrings(t, "Drivers", got.Drivers, []string{"golangci-lint"})
}

// TestTrailingHumanTextIsTolerated uses the exact bytes golangci-lint writes to
// stdout. This is the case a decode-based trailing check gets wrong: "0 issues."
// and "50 issues:" both begin with a valid JSON number, so asking whether
// another JSON value parses reports real linter output as two SARIF logs.
func TestTrailingHumanTextIsTolerated(t *testing.T) {
	cases := map[string]struct {
		in    string
		quote string
	}{
		"real clean stdout": {in: realCleanStdout, quote: "0 issues."},
		"real dirty stdout": {in: realDirtyDoc + realDirtyTrailer, quote: "53 issues:"},
		"bare number":       {in: realCleanDoc + "\n523\n", quote: "523"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, diags := parseOK(t, tc.in)
			if got.Drivers == nil {
				t.Fatal("trailing text cost the counts")
			}
			if len(diags) != 1 {
				t.Fatalf("want exactly one diagnostic, got %d:\n%s", len(diags), diagText(diags))
			}
			if diags[0].Severity != sarif.SeverityWarn {
				t.Errorf("severity: got %q, want %q", diags[0].Severity, sarif.SeverityWarn)
			}
			// The diagnostic has to quote the bytes, or a human reading the
			// report cannot tell a linter summary from a corrupted stream.
			if !strings.Contains(diags[0].Message, tc.quote) {
				t.Errorf("diagnostic does not quote the trailing bytes %q: %s", tc.quote, diags[0].Message)
			}
		})
	}
}

// TestTrailingTextIsTruncatedInTheDiagnostic keeps a runaway stdout from
// putting kilobytes of prose into a report.
func TestTrailingTextIsTruncatedInTheDiagnostic(t *testing.T) {
	noise := strings.Repeat("z", 5000)
	_, diags := parseOK(t, realCleanDoc+"\n"+noise)
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic, got %d", len(diags))
	}
	if len(diags[0].Message) > 1000 {
		t.Errorf("diagnostic is %d bytes; trailing bytes are not being truncated", len(diags[0].Message))
	}
	if !strings.Contains(diags[0].Message, "zzz") {
		t.Errorf("diagnostic quotes none of the trailing bytes: %s", diags[0].Message)
	}
}

// TestSecondDocumentIsFatal covers two logs concatenated into one stream, which
// is what shell redirection of two runs to the same file produces.
func TestSecondDocumentIsFatal(t *testing.T) {
	cases := map[string]string{
		"two objects":       realCleanDoc + realCleanDoc,
		"separated by text": realCleanDoc + "\n" + realDirtyDoc,
		"trailing array":    realCleanDoc + "\n[1,2,3]",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, in)
			if !strings.Contains(err.Error(), "two SARIF logs") {
				t.Errorf("error does not name the cause: %v", err)
			}
		})
	}
}

// TestTruncatedDocumentIsFatal covers a killed tool. There is no per-line
// tolerance to fall back on the way there is for a test2json stream: SARIF is
// one document, so a partial one has no countable prefix.
func TestTruncatedDocumentIsFatal(t *testing.T) {
	cases := map[string]string{
		"cut mid document": realDirtyDoc[:len(realDirtyDoc)-40],
		"cut mid string":   realDirtyDoc[:200],
		"open brace only":  `{`,
		"cut after runs":   `{"version":"2.1.0","runs":[`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, in)
			if !strings.Contains(err.Error(), "truncated") {
				t.Errorf("error does not name truncation: %v", err)
			}
		})
	}
}

func TestEmptyStreamIsFatal(t *testing.T) {
	err := parseErr(t, "")
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error does not name the empty stream: %v", err)
	}
	// An empty stream must not be confused with zero findings, which is the
	// exact conflation this package exists to prevent.
	if strings.Contains(err.Error(), "truncated") {
		t.Errorf("an empty stream was reported as truncation: %v", err)
	}
}

func TestNonObjectTopLevelIsFatal(t *testing.T) {
	cases := map[string]string{
		"array":  `[{"version":"2.1.0","runs":[]}]`,
		"string": `"2.1.0"`,
		"number": `523`,
		"null":   `null`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, in)
			if !strings.Contains(err.Error(), "not an object") {
				t.Errorf("error does not name the cause: %v", err)
			}
		})
	}
}

// TestVersionGate rejects everything outside the 2.1 line.
//
// A prefix test on "2." would admit 2.0, which is the one version the check
// exists to stop, so the 2.0 cases are the point of this test rather than
// filler around it.
func TestVersionGate(t *testing.T) {
	cases := map[string]struct {
		version string
		wantErr bool
	}{
		"2.1.0 accepted":    {version: "2.1.0", wantErr: false},
		"2.1 accepted":      {version: "2.1", wantErr: false},
		"2.2.0 accepted":    {version: "2.2.0", wantErr: false},
		"2.10.0 accepted":   {version: "2.10.0", wantErr: false},
		"2.0.0 rejected":    {version: "2.0.0", wantErr: true},
		"2.0 rejected":      {version: "2.0", wantErr: true},
		"bare 2 rejected":   {version: "2", wantErr: true},
		"1.0.0 rejected":    {version: "1.0.0", wantErr: true},
		"3.0.0 rejected":    {version: "3.0.0", wantErr: true},
		"empty rejected":    {version: "", wantErr: true},
		"nonsense rejected": {version: "2.x", wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := `{"version":"` + tc.version + `","runs":[]}`
			if !tc.wantErr {
				parseOK(t, doc)
				return
			}
			err := parseErr(t, doc)
			// Naming what was found is what turns a rejection into something a
			// caller can act on.
			if tc.version != "" && !strings.Contains(err.Error(), tc.version) {
				t.Errorf("error does not name the version it found (%q): %v", tc.version, err)
			}
		})
	}
}

// TestVersionMissingKeyRejected covers an absent version key, which decodes to
// the same empty string as an explicit "" but means something different.
func TestVersionMissingKeyRejected(t *testing.T) {
	err := parseErr(t, `{"runs":[]}`)
	// The message has to say the property is ABSENT. Reporting it as an
	// unsupported version would send a reader looking for a version string
	// that is not there.
	if !strings.Contains(err.Error(), `no "version" property`) {
		t.Errorf("error does not report the property as missing: %v", err)
	}
}

// TestEmptyRunsIsNotAnError is the counterweight to every rejection above.
//
// Demonstrated to fail by temporarily inserting the check it forbids
// (an error when len(runs) == 0), rather than by reverting a line, because the
// behavior comes from the absence of a check.
func TestEmptyRunsIsNotAnError(t *testing.T) {
	got, diags := parseOK(t, `{"version":"2.1.0","runs":[]}`)
	wantCounts(t, got, map[string]int{}, map[string]int{}, 0)
	if len(got.Drivers) != 0 {
		t.Errorf("Drivers: got %v, want empty", got.Drivers)
	}
	if len(diags) != 0 {
		t.Errorf("an empty runs array produced diagnostics:\n%s", diagText(diags))
	}
}

// TestAbsentRunsIsFatal is the other half of that distinction. An empty array
// is a tool reporting zero; an absent key is not a SARIF log, and counting it
// as zero would publish something unmeasured as a measurement.
func TestAbsentRunsIsFatal(t *testing.T) {
	err := parseErr(t, `{"version":"2.1.0"}`)
	if !strings.Contains(err.Error(), "runs") {
		t.Errorf("error does not name the missing property: %v", err)
	}
}

// TestAbsentResultsWarns applies the same reasoning one level down, but as a
// warning: one malformed run should not discard the runs that parsed.
func TestAbsentResultsWarns(t *testing.T) {
	in := `{"version":"2.1.0","runs":[
		{"tool":{"driver":{"name":"eslint"}}},
		{"tool":{"driver":{"name":"ruff"}},"results":[{"ruleId":"E501","level":"error"}]}
	]}`
	got, diags := parseOK(t, in)

	wantCounts(t, got, map[string]int{"E501": 1}, map[string]int{sarif.LevelError: 1}, 0)
	assertStrings(t, "Drivers", got.Drivers, []string{"eslint", "ruff"})
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic, got %d:\n%s", len(diags), diagText(diags))
	}
	if !strings.Contains(diags[0].Message, "eslint") {
		t.Errorf("diagnostic does not name the run that contributed nothing: %s", diags[0].Message)
	}
}

// TestSeverityDefaulting walks SARIF 2.1.0 section 3.27.10 in order.
//
// Steps two and three never fire for golangci-lint, which sets level on every
// result and ships no rules array. They exist for ruff, semgrep and CodeQL,
// which is why they are tested explicitly rather than assumed dead.
func TestSeverityDefaulting(t *testing.T) {
	// S3 exists but configures no level, which is not the same as configuring
	// a level of "".
	const rules = `"rules":[` +
		`{"id":"S1","defaultConfiguration":{"level":"note"}},` +
		`{"id":"S2","defaultConfiguration":{"level":"error"}},` +
		`{"id":"S3"}]`

	cases := map[string]struct {
		result string
		want   string
	}{
		// Step 1: an explicit level wins over everything, including a rule
		// whose default says otherwise.
		"explicit level beats the rule default": {
			result: `{"ruleId":"S1","level":"error"}`,
			want:   sarif.LevelError,
		},
		// Step 2: a result that is not a failure has no severity to report.
		"kind pass becomes none": {
			result: `{"ruleId":"S2","kind":"pass"}`,
			want:   sarif.LevelNone,
		},
		"kind informational becomes none": {
			result: `{"ruleId":"S2","kind":"informational"}`,
			want:   sarif.LevelNone,
		},
		// kind "fail" is explicitly NOT step 2; it falls through to the rule.
		"kind fail falls through to the rule default": {
			result: `{"ruleId":"S1","kind":"fail"}`,
			want:   sarif.LevelNote,
		},
		// Step 3, by rule id.
		"rule default by id": {
			result: `{"ruleId":"S1"}`,
			want:   sarif.LevelNote,
		},
		// Step 3, by index into the driver's rules array.
		"rule default by index": {
			result: `{"ruleIndex":1}`,
			want:   sarif.LevelError,
		},
		"rule index zero is a real index": {
			result: `{"ruleIndex":0}`,
			want:   sarif.LevelNote,
		},
		// Step 4: nothing said anything, so SARIF says warning.
		"unknown rule falls back to warning": {
			result: `{"ruleId":"nope"}`,
			want:   sarif.LevelWarning,
		},
		"no rule reference at all falls back to warning": {
			result: `{}`,
			want:   sarif.LevelWarning,
		},
		// A rule that exists but configures no level is not a level of "".
		"rule without a default configuration falls back to warning": {
			result: `{"ruleId":"S3"}`,
			want:   sarif.LevelWarning,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t",` + rules + `}},"results":[` + tc.result + `]}]}`
			got, _ := parseOK(t, in)
			if got.Levels[tc.want] != 1 {
				t.Errorf("level: want one %q, got levels %v", tc.want, got.Levels)
			}
		})
	}
}

// TestRuleIndexOutOfRangeDoesNotPanic covers the case folding several runs
// creates: one run's index is meaningless against another run's shorter rules
// array, and an unguarded lookup panics.
func TestRuleIndexOutOfRangeDoesNotPanic(t *testing.T) {
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t","rules":[{"id":"A","defaultConfiguration":{"level":"note"}}]}},"results":[
		{"ruleId":"B","ruleIndex":7},
		{"ruleId":"C","ruleIndex":-1}
	]}]}`
	got, diags := parseOK(t, in)

	wantCounts(t, got,
		map[string]int{"B": 1, "C": 1},
		map[string]int{sarif.LevelWarning: 2},
		0,
	)
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic about the dangling index, got %d:\n%s", len(diags), diagText(diags))
	}
	if !strings.Contains(diags[0].Message, "ruleIndex") {
		t.Errorf("diagnostic does not name ruleIndex: %s", diags[0].Message)
	}
}

// TestSuppressedResultsAreCountedButNotTallied. Counting triaged findings in
// the headline number makes a repository look worse for having done the triage.
func TestSuppressedResultsAreCountedButNotTallied(t *testing.T) {
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"semgrep"}},"results":[
		{"ruleId":"active","level":"error"},
		{"ruleId":"triaged","level":"error","suppressions":[{"kind":"external"}]},
		{"ruleId":"triaged","level":"error","suppressions":[{"kind":"inSource"}]},
		{"ruleId":"empty-array","level":"error","suppressions":[]}
	]}]}`
	got, diags := parseOK(t, in)

	// "triaged" must not appear in Rules at all. An entry with a count of zero
	// would be indistinguishable from a rule that ran and found nothing.
	wantCounts(t, got,
		map[string]int{"active": 1, "empty-array": 1},
		map[string]int{sarif.LevelError: 2},
		2,
	)
	if _, ok := got.Rules["triaged"]; ok {
		t.Error("a fully suppressed rule still has an entry in Rules")
	}
	if len(diags) != 0 {
		t.Errorf("suppressions produced diagnostics:\n%s", diagText(diags))
	}
}

// TestRunsFoldIntoOneRuleMap. A merged multi-tool log is one measurement of one
// repository, so the same rule id appearing in two runs is one entry.
func TestRunsFoldIntoOneRuleMap(t *testing.T) {
	in := `{"version":"2.1.0","runs":[
		{"tool":{"driver":{"name":"golangci-lint"}},"results":[
			{"ruleId":"errcheck","level":"error"},
			{"ruleId":"errcheck","level":"error"}
		]},
		{"tool":{"driver":{"name":"golangci-lint"}},"results":[
			{"ruleId":"errcheck","level":"warning"},
			{"ruleId":"misspell","level":"warning"}
		]}
	]}`
	got, diags := parseOK(t, in)

	wantCounts(t, got,
		map[string]int{"errcheck": 3, "misspell": 1},
		map[string]int{sarif.LevelError: 2, sarif.LevelWarning: 2},
		0,
	)
	// One driver name, seen twice, is one entry.
	assertStrings(t, "Drivers", got.Drivers, []string{"golangci-lint"})
	if len(diags) != 0 {
		t.Errorf("folding runs from the same tool produced diagnostics:\n%s", diagText(diags))
	}
}

// TestSharedRuleIDAcrossToolsIsReported. The fold is still correct, but two
// different checks are now one number, and a reader has to be told.
func TestSharedRuleIDAcrossToolsIsReported(t *testing.T) {
	in := `{"version":"2.1.0","runs":[
		{"tool":{"driver":{"name":"eslint"}},"results":[{"ruleId":"no-unused-vars","level":"error"}]},
		{"tool":{"driver":{"name":"clippy"}},"results":[{"ruleId":"no-unused-vars","level":"warning"}]},
		{"tool":{"driver":{"name":"ruff"}},"results":[{"ruleId":"F401","level":"warning"}]}
	]}`
	got, diags := parseOK(t, in)

	wantCounts(t, got,
		map[string]int{"no-unused-vars": 2, "F401": 1},
		map[string]int{sarif.LevelError: 1, sarif.LevelWarning: 2},
		0,
	)
	assertStrings(t, "Drivers", got.Drivers, []string{"clippy", "eslint", "ruff"})

	if len(diags) != 1 {
		t.Fatalf("want one diagnostic, got %d:\n%s", len(diags), diagText(diags))
	}
	msg := diags[0].Message
	for _, want := range []string{"no-unused-vars", "clippy", "eslint"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic does not name %q: %s", want, msg)
		}
	}
	// The rule id that only one tool reported must not be dragged in.
	if strings.Contains(msg, "F401") {
		t.Errorf("diagnostic names an uncontested rule id: %s", msg)
	}
}

// TestEmptyRuleIDTakesTheSentinel. An absent ruleId is legal SARIF, and the
// empty string cannot stand in for it: an empty scope means repository-wide
// elsewhere in this codebase, so "" would render as a repo total.
func TestEmptyRuleIDTakesTheSentinel(t *testing.T) {
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[
		{"level":"error"},
		{"ruleId":"","level":"error"},
		{"ruleId":"real","level":"note"}
	]}]}`
	got, _ := parseOK(t, in)

	wantCounts(t, got,
		map[string]int{sarif.NoRuleID: 2, "real": 1},
		map[string]int{sarif.LevelError: 2, sarif.LevelNote: 1},
		0,
	)
	if _, ok := got.Rules[""]; ok {
		t.Error(`Rules has an entry keyed by the empty string, which reads as a repository-wide total`)
	}
	if sarif.NoRuleID == "" {
		t.Fatal("the sentinel is the empty string, which defeats its purpose")
	}
}

// TestRuleIDIsCapped. Rule ids come from the analyzed repository's own tool
// output, so their length is not trusted input.
func TestRuleIDIsCapped(t *testing.T) {
	long := strings.Repeat("a", 500)
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[{"ruleId":"` + long + `","level":"error"}]}]}`
	got, _ := parseOK(t, in)

	if len(got.Rules) != 1 {
		t.Fatalf("Rules: got %v, want one entry", got.Rules)
	}
	for id := range got.Rules {
		if len(id) > 200 {
			t.Errorf("rule id is %d bytes; the 200 byte cap did not apply", len(id))
		}
		if len(id) != 200 {
			t.Errorf("rule id is %d bytes; want the full 200 byte budget used", len(id))
		}
	}
}

// TestRuleIDCapKeepsRunesWhole. Cutting at a byte offset can split a multibyte
// rune, which turns a map key into unprintable garbage in a report.
func TestRuleIDCapKeepsRunesWhole(t *testing.T) {
	// Each rune is three bytes, so a 200 byte cut lands mid rune.
	long := strings.Repeat("\u4e16", 300)
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[{"ruleId":"` + long + `","level":"error"}]}]}`
	got, _ := parseOK(t, in)

	for id := range got.Rules {
		if len(id) > 200 {
			t.Errorf("rule id is %d bytes, over the cap", len(id))
		}
		if !strings.HasPrefix(long, id) {
			t.Errorf("truncated rule id is not a prefix of the original: %q", id)
		}
		if strings.ContainsRune(id, '\ufffd') {
			t.Errorf("truncation split a rune: %q", id)
		}
	}
}

// TestUnnamedDriverTakesTheSentinel keeps a blank entry out of a sorted list,
// where it would read as a formatting bug rather than as a fact.
func TestUnnamedDriverTakesTheSentinel(t *testing.T) {
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{}},"results":[]}]}`
	got, _ := parseOK(t, in)
	assertStrings(t, "Drivers", got.Drivers, []string{sarif.UnnamedDriver})
}

// TestUnknownLevelIsCountedAndReported. Dropping an off-spec level would make
// the level counts silently disagree with the rule counts.
func TestUnknownLevelIsCountedAndReported(t *testing.T) {
	in := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[
		{"ruleId":"a","level":"critical"},
		{"ruleId":"b","level":"error"}
	]}]}`
	got, diags := parseOK(t, in)

	wantCounts(t, got,
		map[string]int{"a": 1, "b": 1},
		map[string]int{"critical": 1, sarif.LevelError: 1},
		0,
	)
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic, got %d:\n%s", len(diags), diagText(diags))
	}
	if !strings.Contains(diags[0].Message, "critical") {
		t.Errorf("diagnostic does not name the level: %s", diags[0].Message)
	}
}

// TestDiagnosticOrderIsStable. Both the shared-rule-id and unknown-level
// diagnostics are derived from maps, and Go randomizes map iteration order, so
// without an explicit sort this passes once and fails at random later.
func TestDiagnosticOrderIsStable(t *testing.T) {
	in := `{"version":"2.1.0","runs":[
		{"tool":{"driver":{"name":"aaa"}},"results":[
			{"ruleId":"zzz","level":"sev-z"},
			{"ruleId":"mmm","level":"sev-m"},
			{"ruleId":"aaa","level":"sev-a"}
		]},
		{"tool":{"driver":{"name":"bbb"}},"results":[
			{"ruleId":"zzz","level":"error"},
			{"ruleId":"mmm","level":"error"},
			{"ruleId":"aaa","level":"error"}
		]}
	]}`

	_, first := parseOK(t, in)
	if len(first) < 4 {
		t.Fatalf("expected several diagnostics to order, got %d:\n%s", len(first), diagText(first))
	}
	want := diagText(first)
	for i := range 30 {
		_, again := parseOK(t, in)
		if got := diagText(again); got != want {
			t.Fatalf("diagnostic order changed on iteration %d:\ngot:\n%s\nwant:\n%s", i, got, want)
		}
	}
}

// slowReader hands back one byte per Read, the way a pipe from a live process
// can.
type slowReader struct {
	s string
	i int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.s[r.i]
	r.i++
	return 1, nil
}

// TestParseReadsAChunkedStream exercises the seam between the decoder's own
// buffer and the reader behind it. The trailing scan has to consult both, and
// a reader that returns one byte at a time is what separates the two.
func TestParseReadsAChunkedStream(t *testing.T) {
	got, diags, err := sarif.Parse(&slowReader{s: realCleanStdout})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertStrings(t, "Drivers", got.Drivers, []string{"golangci-lint"})
	if len(diags) != 1 {
		t.Fatalf("want the trailing text diagnostic, got %d:\n%s", len(diags), diagText(diags))
	}
	if !strings.Contains(diags[0].Message, "0 issues.") {
		t.Errorf("trailing bytes were lost across the buffer seam: %s", diags[0].Message)
	}
}
