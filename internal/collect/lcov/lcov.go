// Package lcov parses the LCOV tracefiles that coverage tools emit.
//
// It is the coverage counterpart to the junit package: one parser for every
// language, chosen because LCOV is the only widely emitted coverage format that
// stores COUNTS rather than rates. Istanbul and nyc write it for JavaScript and
// TypeScript, coverage.py writes it for Python, and gcov writes it for C.
// Cobertura XML was considered and rejected for the opposite property: its
// per-file elements carry only line-rate and branch-rate, so the counts this
// tool refuses to compute from percentages would have to be reconstructed from
// them.
//
// What it reads is deliberately narrow. Of the record types LCOV defines, only
// SF, DA, LF, LH and end_of_record are consulted, because lines are the one unit
// every emitter agrees on. Function and branch records are parsed past rather
// than interpreted.
//
// Emitters disagree about layout and the parser assumes none of it. Verified
// against two real tracefiles: istanbul-reports 3.2.0 writes TN, SF, the FN
// block, DA lines, LF/LH, then branches; coverage.py writes SF, DA lines, LF/LH,
// then the FN block, and omits branch records entirely unless asked for them. It
// also writes FN with three comma-separated fields where istanbul writes two.
// Nothing here depends on order, on a record being present, or on how many
// fields the records it ignores happen to carry.
package lcov

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Severity classifies a diagnostic, mirroring the collect package's own rather
// than importing it, so this stays a parser of a public format.
type Severity string

// The severities in play.
const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Diagnostic explains something about a tracefile that still parsed.
type Diagnostic struct {
	Severity Severity
	Message  string
}

// File is one source file's line counts.
type File struct {
	// Name is the path as the tracefile spells it, which every emitter writes
	// relative to the project root.
	Name string
	// Covered and Total are LINES, not statements. That distinction is the
	// reason these are stored under their own metric keys rather than reusing
	// Go's: several statements on one source line collapse to one line here, so
	// the two are not the same unit and their ratios are not comparable.
	Covered, Total int
}

// Summary is one parsed tracefile.
type Summary struct {
	// Files are the records found, sorted by name so two runs over the same
	// tracefile write the same metric rows in the same order.
	Files []File
}

// Parse reads one LCOV tracefile.
//
// An error means the input is not a tracefile at all, which is a different thing
// from a tracefile covering nothing. The second case returns an empty summary
// and a diagnostic, and it is a real one: a filtered or misconfigured coverage
// run writes a ZERO-BYTE lcov.info and exits 0. Verified against
// vitest 4.1.10 with @vitest/coverage-v8 4.1.4, which produced exactly that for
// a full 1,964-test run. Nothing in the file says anything went wrong, because
// there is no file content to say it with.
func Parse(r io.Reader) (*Summary, []Diagnostic, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	var (
		diags     []Diagnostic
		files     []File
		seen      = map[string]bool{}
		cur       *record
		anyRecord bool
		line      int
		// xmlTag names the document when the input turns out to be XML, so the
		// rejection below can say which XML rather than only that this is not a
		// tracefile. Looked for only until the first real record, so a genuine
		// tracefile pays for it once at the top of the file and never again.
		xmlTag string
	)

	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if !anyRecord && xmlTag == "" {
			xmlTag = firstXMLTag(text)
		}

		key, value, found := strings.Cut(text, ":")
		if !found {
			// end_of_record is the one directive with no colon. Anything else
			// without one is not a record; noted so that a file made entirely of
			// them is rejected below rather than reported as covering nothing.
			if text == endOfRecord {
				anyRecord = true
				if cur != nil {
					files = append(files, cur.finish())
					cur = nil
				}
			}
			continue
		}
		anyRecord = anyRecord || knownRecord(key)

		switch key {
		case "SF":
			// A tracefile is supposed to close each record, but a truncated one
			// does not, so a new SF also ends the previous record. Dropping it
			// instead would silently shorten the report by one file.
			if cur != nil {
				files = append(files, cur.finish())
			}
			name := strings.TrimSpace(value)
			if name == "" {
				diags = append(diags, warnf("line %d: a source-file record with no path, so its counts cannot be attributed", line))
				cur = nil
				continue
			}
			if seen[name] {
				// Kept as first-wins rather than merged. Summing would inflate the
				// denominator and picking per-field maxima would pair numerators and
				// denominators from different records, and neither is a number
				// anybody measured.
				diags = append(diags, warnf("%s appears more than once; the first record is used and the rest ignored", name))
				cur = nil
				continue
			}
			seen[name] = true
			cur = &record{name: name}

		case "DA":
			if cur == nil {
				continue
			}
			// DA is line,count with an optional third checksum field. Counted here
			// so that a tracefile omitting the LF/LH summary still measures, which
			// the format permits and some emitters do.
			rest := value
			if i := strings.Index(rest, ","); i >= 0 {
				hits := rest[i+1:]
				if j := strings.Index(hits, ","); j >= 0 {
					hits = hits[:j]
				}
				n, err := strconv.Atoi(strings.TrimSpace(hits))
				if err != nil {
					continue
				}
				cur.daTotal++
				if n > 0 {
					cur.daCovered++
				}
			}

		case "LF":
			if cur != nil {
				cur.lf, cur.hasLF = atoiOK(value)
			}
		case "LH":
			if cur != nil {
				cur.lh, cur.hasLH = atoiOK(value)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("lcov: reading the profile: %w", err)
	}
	if cur != nil {
		files = append(files, cur.finish())
	}

	// Bytes that contain not one LCOV record. A coverage report in some other
	// format was pointed at this parser, and reporting zero covered lines for it
	// would be measuring the wrong file.
	if !anyRecord && line > 0 {
		return nil, nil, notATracefile(line, xmlTag)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return &Summary{Files: files}, diags, nil
}

// notATracefile explains bytes carrying no LCOV record, and names the mistake
// when it can recognize it.
//
// Handing this parser a coverage tool's XML is the common way to land here, and
// it is one flag word away from correct: pytest-cov writes Cobertura for
// --cov-report=xml and LCOV only for --cov-report=lcov. "This is not a
// tracefile" is true and leaves an operator to work the rest out from a
// filename, which is a step this tool can take for them.
func notATracefile(lines int, xmlTag string) error {
	switch xmlTag {
	case "coverage":
		return fmt.Errorf("lcov: no LCOV records in %d lines. This looks like Cobertura XML, "+
			"which is what --cov-report=xml writes. Ask the coverage tool for LCOV instead, "+
			"as in --cov-report=lcov for coverage.py or --coverage.reporter=lcov for vitest", lines)
	case "testsuite", "testsuites":
		return fmt.Errorf("lcov: no LCOV records in %d lines. This looks like JUnit XML, which "+
			"carries test results rather than coverage. Read it with the junit-xml format and "+
			"point lcov at the coverage tracefile", lines)
	case "":
		return fmt.Errorf("lcov: no LCOV records in %d lines, so this is not a tracefile", lines)
	default:
		return fmt.Errorf("lcov: no LCOV records in %d lines, so this is not a tracefile. "+
			"It is XML rooted at <%s>, and LCOV is line oriented text", lines, xmlTag)
	}
}

// firstXMLTag returns the name of the first XML element opened in text, or "".
//
// It scans past processing instructions and doctypes rather than stopping at
// them, and it does so within a line as well as across lines, because neither
// identifies the document and a minified report puts the whole prolog and the
// root element on one line. A Cobertura file and a JUnit file open with the same
// <?xml, and telling them apart is the whole point of looking.
//
// A tracefile line reaches the first byte check and leaves, and this is only
// consulted at all when the input turned out to carry no records, so a stray
// angle bracket in a function name can affect nothing but the wording of a
// rejection that was already happening.
func firstXMLTag(text string) string {
	for {
		open := strings.IndexByte(text, '<')
		if open < 0 {
			return ""
		}
		rest := text[open+1:]
		if rest == "" {
			return ""
		}
		if c := rest[0]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			// Bounded because the name reaches a user-facing message, and the
			// line scanner admits a megabyte.
			const maxTag = 40
			if len(rest) > maxTag {
				rest = rest[:maxTag]
			}
			end := strings.IndexAny(rest, " \t/>")
			if end < 0 {
				return ""
			}
			return strings.ToLower(rest[:end])
		}
		text = rest
	}
}

// Totals sums the files into one covered-over-total pair. The sum is taken over
// counts rather than over per-file rates, because a repo's coverage is not the
// mean of its files' coverage.
func (s *Summary) Totals() (covered, total int) {
	for _, f := range s.Files {
		covered += f.Covered
		total += f.Total
	}
	return covered, total
}

const (
	endOfRecord = "end_of_record"
	// maxLine bounds one record. LCOV records are short; a longer line means the
	// input is not a tracefile.
	maxLine = 1 << 20
)

// knownRecord reports whether a prefix is one LCOV defines. The ones this parser
// ignores are listed too, since their presence still proves the input is a
// tracefile.
func knownRecord(key string) bool {
	switch key {
	case "TN", "SF", "FN", "FNDA", "FNF", "FNH",
		"BRDA", "BRF", "BRH", "DA", "LF", "LH",
		"MCDC", "MCF", "MCH", "VER", "FNL", "FNA", "BRL":
		return true
	}
	return false
}

// record accumulates one SF block.
type record struct {
	name               string
	lf, lh             int
	hasLF, hasLH       bool
	daTotal, daCovered int
}

// finish resolves a record's counts.
//
// The LF/LH summary wins where the emitter wrote one, since that is the
// generator's own answer. Where it did not, the DA lines are counted instead,
// which is exact rather than an estimate: every instrumented line has one.
//
// A record with neither still yields a File, with both counts zero. That is not
// a coverage of zero percent being invented: the caller writes rows for a file
// only when its total is above zero, so a record carrying no line information
// contributes nothing to the sums and to nothing published.
func (r *record) finish() File {
	f := File{Name: r.name}
	switch {
	case r.hasLF:
		f.Total = r.lf
		if r.hasLH {
			f.Covered = r.lh
		}
	default:
		f.Total = r.daTotal
		f.Covered = r.daCovered
	}
	if f.Covered > f.Total {
		// A generator disagreeing with itself. Clamping keeps a rate above 100
		// percent off the report, and the diagnostic is the caller's job since it
		// knows which artifact this came from.
		f.Covered = f.Total
	}
	return f
}

func atoiOK(v string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func warnf(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarn, Message: fmt.Sprintf(format, args...)}
}
