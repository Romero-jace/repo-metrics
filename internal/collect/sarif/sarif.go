// Package sarif reads SARIF 2.1.0 static analysis logs and reduces them to
// per-rule and per-severity counts.
//
// SARIF is the one output format golangci-lint, eslint, ruff, semgrep, clippy
// and CodeQL all emit, so parsing it is what keeps this tool from being able to
// measure Go and nothing else.
//
// The package is deliberately pure: it takes an io.Reader and returns a
// summary. It knows nothing about metrics, storage, or repositories, so it can
// be tested against real tool output without a database or a checkout.
package sarif

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The four levels SARIF 2.1.0 defines for a result. A tool may emit something
// else; see Summary.Levels for what happens then.
const (
	LevelError   = "error"
	LevelWarning = "warning"
	LevelNote    = "note"
	LevelNone    = "none"
)

// NoRuleID stands in for a result that carries no ruleId.
//
// An absent ruleId is legal SARIF, so it needs a key rather than a rejection.
// The empty string cannot be that key: elsewhere in this codebase an empty
// scope means the whole repository, so a rule id of "" would render as a
// repo-wide total instead of as one unattributed finding, and a reader would
// have no way to tell the two apart.
const NoRuleID = "(no rule id)"

// UnnamedDriver stands in for a run whose driver carries no name, for the same
// reason NoRuleID exists: a blank entry in a sorted tool list reads as a
// formatting bug rather than as a fact about the input.
const UnnamedDriver = "(unnamed tool)"

// maxRuleIDBytes caps how much of a rule id is used as a map key.
//
// Rule ids come from the analyzed repository's own tool output, which is not
// trusted input. Without a cap, a malformed or hostile log sizes this map from
// bytes on disk.
const maxRuleIDBytes = 200

// trailingScanLimit bounds how far past the SARIF document the parser reads
// looking for a second value or human text. golangci-lint's stdout summary is
// a few dozen bytes; nothing legitimate needs more than this, and reading to
// EOF would pull an arbitrarily large file into memory for a diagnostic.
const trailingScanLimit = 4096

// trailingQuoteLimit bounds how many trailing bytes are quoted back in the
// diagnostic. Enough to recognize the text, not enough to flood a report.
const trailingQuoteLimit = 200

// Severity says whether a diagnostic cost the caller any numbers.
type Severity string

const (
	// SeverityWarn means something was skipped but the counts still stand.
	SeverityWarn Severity = "warn"
	// SeverityError means the counts cannot be trusted.
	SeverityError Severity = "error"
)

// Diagnostic explains something the parser noticed but did not treat as fatal.
//
// It mirrors the collect package's type rather than importing it, because this
// package must not depend on the collector it feeds.
type Diagnostic struct {
	Severity Severity
	Message  string
}

func warnf(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarn, Message: fmt.Sprintf(format, args...)}
}

// Summary is one SARIF log reduced to counts.
//
// Every count here covers ACTIVE results only. Suppressed results are held
// separately in Suppressed, because folding them in would make a repository
// look worse for having triaged its findings, which is the opposite of the
// incentive this tool should create.
type Summary struct {
	// Rules counts active findings per rule id, folded across every run in the
	// log. See NoRuleID for results that carry no rule id.
	Rules map[string]int
	// Levels counts active findings per normalized level. Keys are normally the
	// four SARIF levels, but a tool that emits something else gets its own key
	// rather than being dropped, so the levels always sum to the rule counts.
	Levels map[string]int
	// Suppressed counts results excluded from Rules and Levels because they
	// carried a non-empty suppressions array.
	Suppressed int
	// Drivers lists every tool name seen, sorted and deduplicated. A run with
	// no results still contributes its name, which is what distinguishes "this
	// tool ran and found nothing" from "this tool never ran".
	//
	// Driver names are NOT length capped the way rule ids are. They come from
	// the same untrusted output, but there is one per run rather than one per
	// finding, so the map cannot be grown by repeating them.
	Drivers []string
}

// Active returns the total number of active findings.
//
// It reads Levels rather than Rules because both carry exactly one entry per
// active result, and Levels is the one a caller is least likely to have
// already summed for its own reasons.
func (s *Summary) Active() int {
	var n int
	for _, c := range s.Levels {
		n += c
	}
	return n
}

// document is the subset of the SARIF 2.1.0 object model this parser reads.
type document struct {
	Version string `json:"version"`
	// Runs is a pointer so an absent key is distinguishable from an empty
	// array. The difference is the whole point of this package: "runs": []
	// is a tool reporting zero findings, an absent runs key is not a SARIF log
	// at all, and collapsing the second into the first is exactly the silent
	// zero this tool exists to refuse.
	Runs *[]run `json:"runs"`
}

type run struct {
	Tool tool `json:"tool"`
	// Results is a pointer for the same reason Runs is.
	Results *[]result `json:"results"`
}

type tool struct {
	Driver driver `json:"driver"`
}

type driver struct {
	Name string `json:"name"`
	// Rules is absent from real golangci-lint output, which is why severity
	// resolution must not assume it is there.
	Rules []rule `json:"rules"`
}

type rule struct {
	ID                   string       `json:"id"`
	DefaultConfiguration *ruleDefault `json:"defaultConfiguration"`
}

type ruleDefault struct {
	Level string `json:"level"`
}

type result struct {
	RuleID string `json:"ruleId"`
	// RuleIndex is a pointer because 0 is a legal index. Decoded into a plain
	// int, an absent key would be 0 and would silently give every result
	// without a ruleId the first rule's default level.
	RuleIndex *int   `json:"ruleIndex"`
	Level     string `json:"level"`
	Kind      string `json:"kind"`
	// Suppressions needs no pointer: an absent key and an empty array both mean
	// the result is active, so nil and len 0 are the same answer.
	Suppressions []suppression `json:"suppressions"`
}

// suppression is read for its presence only. SARIF gives it a status field, but
// no emitter this parser targets sets one.
type suppression struct {
	Kind string `json:"kind"`
}

// Parse reads exactly one SARIF 2.1.0 log.
//
// Verified against golangci-lint v2.12.2: a clean run emits a document whose
// single run carries only tool and results, whose driver carries only a name,
// and whose results array is empty. There is no rules array to resolve
// severities against, so the defaulting in resolveLevel is load-bearing for
// every tool that does ship one.
func Parse(r io.Reader) (*Summary, []Diagnostic, error) {
	var diags []Diagnostic

	dec := json.NewDecoder(r)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, diags, decodeError(err)
	}

	// The document must BE an object, not merely contain one. Scanning ahead to
	// the first brace would turn a crashed tool's partial output into a clean
	// looking measurement, which is the one bug this whole tool exists to
	// refuse, so leading junk is fatal here rather than skipped.
	if body := bytes.TrimLeft(raw, " \t\r\n"); len(body) == 0 || body[0] != '{' {
		return nil, diags, fmt.Errorf("sarif: the top level JSON value is not an object (starts with %q); a SARIF log is one JSON object", firstByte(body))
	}

	trailingDiag, err := checkTrailing(dec, r)
	if err != nil {
		return nil, diags, err
	}
	if trailingDiag != nil {
		diags = append(diags, *trailingDiag)
	}

	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, diags, fmt.Errorf("sarif: reading the SARIF object: %w", err)
	}
	if err := checkVersion(doc.Version); err != nil {
		return nil, diags, err
	}
	if doc.Runs == nil {
		return nil, diags, errors.New(`sarif: the document has no "runs" property. An empty runs array is a real measurement of zero findings and is accepted, but an absent one means this is not a SARIF log, and counting it as zero would publish something unmeasured as a measurement`)
	}

	summary, foldDiags := fold(*doc.Runs)
	return summary, append(diags, foldDiags...), nil
}

// decodeError classifies why the first value would not decode. The three cases
// have different causes and a caller chasing a broken pipeline needs to know
// which one it hit.
func decodeError(err error) error {
	switch {
	case errors.Is(err, io.EOF):
		return errors.New("sarif: the stream is empty; a tool that died before writing anything leaves exactly this behind, and it is not the same as zero findings")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("sarif: the document is truncated: %w. SARIF is one JSON document rather than a stream of lines, so there is no salvageable prefix to count", err)
	default:
		return fmt.Errorf("sarif: the stream does not begin with a JSON value: %w. Output before the log is rejected rather than skipped, because a tool that printed a crash banner first has not produced a measurement", err)
	}
}

// checkTrailing looks at what follows the document.
//
// Returning a warning rather than an error for trailing prose is not leniency,
// it is a verified fact about golangci-lint: with the SARIF output routed to
// stdout it appends its human summary to the same stream, so the bytes after
// the closing brace of a real dirty run are "\n50 issues:\n* lll: 50\n". eslint
// and ruff also emit to stdout, so refusing this would refuse the common case.
//
// The discriminator is structural JSON versus prose, NOT "does another JSON
// value parse". A JSON number parses out of "50 issues:" perfectly well, since
// the scanner ends the number at the space, so a decode-based test reports the
// single most common real input as two concatenated logs.
func checkTrailing(dec *json.Decoder, r io.Reader) (*Diagnostic, error) {
	rest, err := readTrailing(dec, r)
	if err != nil {
		return nil, fmt.Errorf("sarif: reading past the end of the document: %w", err)
	}
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 {
		return nil, nil
	}
	if rest[0] == '{' || rest[0] == '[' {
		return nil, fmt.Errorf("sarif: two SARIF logs in one stream: a second JSON value begins after the first document, starting %q. Merging them here would guess at whether they describe the same commit", quoteTrailing(rest))
	}
	d := warnf("ignored %d bytes of non-JSON text after the SARIF log, which is what golangci-lint appends when the log goes to stdout; first %d bytes: %q",
		len(rest), min(len(rest), trailingQuoteLimit), quoteTrailing(rest))
	return &d, nil
}

// readTrailing drains what is left, bounded. dec.Buffered holds whatever the
// decoder read past the value it returned, so both it and the reader behind it
// have to be consulted.
func readTrailing(dec *json.Decoder, r io.Reader) ([]byte, error) {
	buf := make([]byte, trailingScanLimit)
	n, err := io.ReadFull(io.MultiReader(dec.Buffered(), r), buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:n], nil
}

// checkVersion accepts the SARIF 2.1 line only.
//
// A prefix test on "2." is not enough: it admits 2.0, which is the very version
// the check exists to stop. The 2.0 drafts differ in the result and rule shapes
// this parser reads, so counts taken from one are not comparable with counts
// taken from the other.
func checkVersion(v string) error {
	if v == "" {
		return errors.New(`sarif: the document carries no "version" property, so there is no way to tell which SARIF shape these keys follow`)
	}
	major, rest, hasMinor := strings.Cut(v, ".")
	if major != "2" || !hasMinor {
		return fmt.Errorf("sarif: unsupported SARIF version %q; this parser reads the 2.1 line", v)
	}
	minorText, _, _ := strings.Cut(rest, ".")
	minor, err := strconv.Atoi(minorText)
	if err != nil {
		return fmt.Errorf("sarif: unreadable SARIF version %q; this parser reads the 2.1 line", v)
	}
	if minor < 1 {
		return fmt.Errorf("sarif: unsupported SARIF version %q; SARIF 2.0 is an earlier draft whose result and rule shapes differ from 2.1, so its counts are not comparable", v)
	}
	return nil
}

// fold reduces every run to one set of counts.
//
// The fold is across ALL runs on purpose. A log holding a golangci-lint run and
// an eslint run is one measurement of one repository, and emitting the same
// rule id twice because it appeared in two runs would double count it in any
// caller that sums the map.
func fold(runs []run) (*Summary, []Diagnostic) {
	var diags []Diagnostic
	summary := &Summary{
		Rules:  make(map[string]int),
		Levels: make(map[string]int),
	}

	drivers := make(map[string]bool)
	// ruleDrivers records which tools contributed to each folded rule id, so a
	// collision between two tools that happen to share a rule id is reported
	// rather than silently averaged into one number.
	ruleDrivers := make(map[string]map[string]bool)
	unknownLevels := make(map[string]bool)
	var danglingIndex int

	for i, rn := range runs {
		name := rn.Tool.Driver.Name
		if name == "" {
			name = UnnamedDriver
		}
		drivers[name] = true

		if rn.Results == nil {
			diags = append(diags, warnf(`run %d (%s) has no "results" property; it contributed nothing, which is not the same as it having found nothing`, i, name))
			continue
		}

		for _, res := range *rn.Results {
			if len(res.Suppressions) > 0 {
				summary.Suppressed++
				continue
			}

			id := normalizeRuleID(res.RuleID)
			level, dangling := resolveLevel(res, rn.Tool.Driver.Rules)
			if dangling {
				danglingIndex++
			}

			summary.Rules[id]++
			summary.Levels[level]++
			if !knownLevel(level) {
				unknownLevels[level] = true
			}
			if ruleDrivers[id] == nil {
				ruleDrivers[id] = make(map[string]bool)
			}
			ruleDrivers[id][name] = true
		}
	}

	summary.Drivers = sortedKeys(drivers)

	// Map iteration order is randomized, so every diagnostic derived from one
	// is sorted before it is emitted. Otherwise a test asserting on diagnostics
	// passes locally and fails at random in CI.
	for _, id := range sortedKeys(ruleDrivers) {
		if len(ruleDrivers[id]) < 2 {
			continue
		}
		diags = append(diags, warnf("rule id %q was reported by more than one tool (%s) and the counts are folded into one entry; two different checks are being read as one number",
			id, strings.Join(sortedKeys(ruleDrivers[id]), ", ")))
	}
	for _, level := range sortedKeys(unknownLevels) {
		diags = append(diags, warnf("level %q is not one of the four SARIF 2.1.0 levels (%s, %s, %s, %s); it is counted under its own name rather than dropped, so the levels still sum to the rule counts",
			level, LevelError, LevelWarning, LevelNote, LevelNone))
	}
	if danglingIndex > 0 {
		diags = append(diags, warnf("%d result(s) carried a ruleIndex pointing outside the driver's rules array; their severity fell back to %q instead of the rule's configured level",
			danglingIndex, LevelWarning))
	}

	return summary, diags
}

// resolveLevel applies SARIF 2.1.0 section 3.27.10 in order.
//
// The second and third steps never fire for golangci-lint, which sets level on
// every result and ships no rules array. They are what make the parser correct
// for ruff, semgrep and CodeQL, which lean on the rule's defaultConfiguration
// and on kind, so leaving them out would quietly report those tools' findings
// at the wrong severity rather than failing.
//
// dangling reports a ruleIndex that pointed outside the rules array. Folding
// several runs is exactly where that happens, because one run's index is
// meaningless against another run's shorter rules array.
func resolveLevel(res result, rules []rule) (level string, dangling bool) {
	if res.Level != "" {
		return res.Level, false
	}
	// A result that is not a failure has no severity to report. Without this,
	// an informational or passing result would be counted as a warning.
	if res.Kind != "" && res.Kind != "fail" {
		return LevelNone, false
	}
	r, found, dangling := findRule(res, rules)
	if found && r.DefaultConfiguration != nil && r.DefaultConfiguration.Level != "" {
		return r.DefaultConfiguration.Level, dangling
	}
	return LevelWarning, dangling
}

// findRule resolves the rule a result refers to, by id first and then by index.
func findRule(res result, rules []rule) (found rule, ok bool, dangling bool) {
	if res.RuleID != "" {
		for _, r := range rules {
			if r.ID == res.RuleID {
				return r, true, false
			}
		}
	}
	if res.RuleIndex == nil {
		return rule{}, false, false
	}
	// Bounds checked rather than assumed: an index from another run's rules
	// array, or from a malformed log, would panic here.
	if *res.RuleIndex < 0 || *res.RuleIndex >= len(rules) {
		return rule{}, false, true
	}
	return rules[*res.RuleIndex], true, false
}

func knownLevel(level string) bool {
	switch level {
	case LevelError, LevelWarning, LevelNote, LevelNone:
		return true
	default:
		return false
	}
}

// normalizeRuleID maps a result's ruleId onto the key used in Summary.Rules.
func normalizeRuleID(id string) string {
	if id == "" {
		return NoRuleID
	}
	// Two rule ids sharing a 200 byte prefix fold into one key. No real linter
	// emits ids remotely this long, and an uncapped key would size the map from
	// the analyzed repository's own output.
	return truncateRunes(id, maxRuleIDBytes)
}

// truncateRunes cuts s to at most limit bytes without splitting a rune, so the
// result stays printable in a report.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	out := s[:limit]
	for len(out) > 0 {
		if r, size := utf8.DecodeLastRuneInString(out); r == utf8.RuneError && size <= 1 {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	return out
}

// quoteTrailing shortens trailing bytes for a diagnostic. The caller formats
// the result with %q, which is what keeps a colorized tool summary from putting
// raw escape sequences into a message a human reads.
func quoteTrailing(b []byte) string {
	return truncateRunes(string(b), trailingQuoteLimit)
}

// firstByte describes the byte a non-object document starts with, or reports
// that there was nothing there at all.
func firstByte(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return string(b[:1])
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
