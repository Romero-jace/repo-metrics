package report_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// The headings each section owns, so "did narrowing actually narrow" can be
// asked of the rendered text rather than of the View that produced it.
const (
	headingMovers   = "## What moved"
	headingRepos    = "## Every repo"
	headingChurn    = "## Packages that came and went"
	headingProblems = "## Collection problems"
)

func TestParseSectionAcceptsEveryNameItAdvertises(t *testing.T) {
	for _, name := range report.Sections() {
		sec, err := report.ParseSection(name)
		if err != nil {
			t.Errorf("ParseSection(%q): %v, but it is listed as valid", name, err)
			continue
		}
		if string(sec) != name {
			t.Errorf("ParseSection(%q) returned %q", name, sec)
		}
	}
	if len(report.Sections()) != 4 {
		t.Errorf("Sections(): got %v, want the four the CLI documents", report.Sections())
	}
}

// A typo that quietly renders the whole report answers a question nobody asked,
// with nothing saying so. That is the same shape of lie as an unmeasured zero,
// and it is worse for an agent than for a human because an agent has no way to
// notice the answer is to a different question.
func TestParseSectionRejectsAnUnknownNameAndSaysWhatIsValid(t *testing.T) {
	for _, bad := range []string{"", "mover", "Movers", "all repos", "problem"} {
		sec, err := report.ParseSection(bad)
		if err == nil {
			t.Errorf("ParseSection(%q) returned %q with no error, so a typo silently renders something else", bad, sec)
			continue
		}
		if sec != "" {
			t.Errorf("ParseSection(%q) returned an error and also the section %q", bad, sec)
		}
		if !strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("ParseSection(%q) error does not quote what was rejected: %v", bad, err)
		}
		// The error has to be actionable on its own. A reader who mistyped needs
		// the list, not just the news that they were wrong.
		for _, name := range report.Sections() {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("ParseSection(%q) error does not list the valid section %q: %v", bad, name, err)
			}
		}
	}
}

// Narrowing has to reach the prose, not just the data. A movers-only render that
// still carried the every-repo table's footnotes, or a repos-only render that
// announced nothing cleared the threshold, would be answering the wider question
// in words while claiming to answer the narrow one.
func TestMarkdownSectionsRenderOnlyWhatWasAskedFor(t *testing.T) {
	rep := fullReport()
	for _, tc := range []struct {
		sec     report.Section
		want    []string
		notWant []string
	}{
		{
			sec:     report.SectionAll,
			want:    []string{headingMovers, headingRepos, headingChurn, headingProblems},
			notWant: nil,
		},
		{
			sec:     report.SectionMovers,
			want:    []string{headingMovers},
			notWant: []string{headingRepos, headingChurn, headingProblems},
		},
		{
			sec:     report.SectionRepos,
			want:    []string{headingRepos, headingChurn},
			notWant: []string{headingMovers, headingProblems},
		},
		{
			sec:     report.SectionProblems,
			want:    []string{headingProblems},
			notWant: []string{headingMovers, headingRepos, headingChurn},
		},
	} {
		md := mustMarkdownSection(t, rep, tc.sec)
		for _, want := range tc.want {
			if !strings.Contains(md, want) {
				t.Errorf("--section %s does not render %q:\n%s", tc.sec, want, md)
			}
		}
		for _, notWant := range tc.notWant {
			if strings.Contains(md, notWant) {
				t.Errorf("--section %s renders %q, which belongs to another section:\n%s", tc.sec, notWant, md)
			}
		}
		// The header is not a section and has to survive every narrowing, or a
		// narrowed report has no date and no window on it.
		if !strings.Contains(md, "# Repo metrics,") || !strings.Contains(md, "back.") {
			t.Errorf("--section %s dropped the report header:\n%s", tc.sec, md)
		}
	}
}

// The narrow render has to be the same render, not a second one that happens to
// look similar. Byte equality of the shared span is what rules out a separate
// walk of the report drifting from the full one.
func TestANarrowedSectionIsByteIdenticalToItsPartOfTheWhole(t *testing.T) {
	rep := fullReport()
	full := mustMarkdownSection(t, rep, report.SectionAll)
	movers := mustMarkdownSection(t, rep, report.SectionMovers)

	fullMovers := sectionAfter(full, headingMovers)
	narrowMovers := sectionAfter(movers, headingMovers)
	if strings.TrimSpace(fullMovers) == "" {
		t.Fatalf("fixture is wrong: the full report has no movers section to compare against:\n%s", full)
	}
	if strings.TrimSpace(narrowMovers) != strings.TrimSpace(fullMovers) {
		t.Errorf("the movers section differs between --section all and --section movers.\nall:\n%q\nmovers:\n%q", fullMovers, narrowMovers)
	}
}

// An unrequested section renders as null rather than as an empty list, because
// those are different answers. An empty movers list says nothing moved this
// week; an empty one on a repos-only render would say the same thing about a
// question the caller never asked. That is absence read as a measurement, one
// level up from the coverage numbers.
func TestJSONSectionsNullTheListsNobodyAskedFor(t *testing.T) {
	rep := fullReport()
	for _, tc := range []struct {
		sec     report.Section
		present []string
		absent  []string
	}{
		{report.SectionAll, []string{"movers", "repos", "problems"}, nil},
		{report.SectionMovers, []string{"movers"}, []string{"repos", "problems"}},
		{report.SectionRepos, []string{"repos"}, []string{"movers", "problems"}},
		{report.SectionProblems, []string{"problems"}, []string{"movers", "repos"}},
	} {
		var doc map[string]any
		if err := json.Unmarshal([]byte(mustJSONSection(t, rep, tc.sec)), &doc); err != nil {
			t.Fatalf("--section %s: decoding the rendered json: %v", tc.sec, err)
		}
		if got := doc["section"]; got != string(tc.sec) {
			t.Errorf("--section %s: the json says section=%v, so a consumer cannot tell why the other lists are null", tc.sec, got)
		}
		for _, field := range tc.present {
			if _, ok := doc[field].([]any); !ok {
				t.Errorf("--section %s: %s is %v (%T), want a list", tc.sec, field, doc[field], doc[field])
			}
		}
		for _, field := range tc.absent {
			v, ok := doc[field]
			if !ok {
				t.Errorf("--section %s: %s is missing from the json rather than null, so a consumer defaulting it reads an empty list as a finding", tc.sec, field)
			}
			if v != nil {
				t.Errorf("--section %s: %s is %v, want null because it was not asked for", tc.sec, field, v)
			}
		}
	}
}

// The safety property stated directly, by construction rather than by listing
// today's fields: a repo that measured nothing must carry no number anywhere in
// its rendered object. Not a zero, not a null number, nothing a default-taking
// accessor could reach at all.
//
// The census next door proves every number is inside a nullable group. This
// proves the groups really do go away, over the whole rendered object and at
// every depth.
func TestAnUnmeasuredRepoCarriesNoNumberAtAll(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  string
		in   delta.Input
	}{
		{repoNever, "configured but never collected", delta.Input{Repo: repoAt(1, repoNever)}},
		{
			repoFailedOnly,
			"the run came back with nothing",
			delta.Input{
				Repo: repoAt(2, repoFailedOnly),
				Head: snap(21, 2, "go1.26.5", store.StatusFailed, "test command exited 2"),
			},
		},
	} {
		rep := delta.Compute([]delta.Input{tc.in}, options(), fixedNow())
		row := jsonRepoRows(t, rep)[tc.name]
		if row == nil {
			t.Fatalf("%s is missing from the json entirely", tc.name)
		}
		if found := numberPaths("", row); len(found) != 0 {
			t.Errorf("%s (%s) publishes numbers at %v. Every one of them is a value nobody measured, and a consumer charting it cannot tell.",
				tc.name, tc.why, found)
		}
		// The control: something still has to be on the wire, or this test would
		// pass on a renderer that dropped the repo altogether.
		if row["name"] != tc.name || row["status"] == nil {
			t.Errorf("%s: the row has to keep saying what it is and what happened, got %v", tc.name, row)
		}
	}
}

// A culprit's two percentages are the one place a fabricated zero survived the
// nesting, and it survived because the markdown had already been taught to hide
// it. A removed package went over the wire at "to_pct": 0 and an added one at
// "from_pct": 0, so a consumer charting the pair read a deleted package as a
// collapse to zero coverage and a brand-new one as a climb from it. That is the
// same divergence residual_test.go was written about, one level down: the
// markdown refusing to print a figure while the JSON published it.
func TestACulpritQuotesNoPercentageForASideThatDidNotExist(t *testing.T) {
	rep := fullReport()
	view := findRepoView(t, report.Build(rep).Movers, repoMover)
	culprits := view.Culprits()
	if len(culprits) == 0 {
		t.Fatalf("fixture is wrong: %s should name culprits", repoMover)
	}

	// Every state has to be exercised, or the absent-side assertions below
	// would pass on a fixture that only ever produced changed packages.
	seen := map[string]bool{}
	for _, c := range culprits {
		seen[c.State] = true
		switch c.State {
		case "added":
			if c.FromPct != nil {
				t.Errorf("%s appears only in the newer snapshot, so its earlier coverage is %v rather than absent, which reads as a climb from nothing", c.Package, *c.FromPct)
			}
			if c.ToPct == nil {
				t.Errorf("%s was measured in the newer snapshot, so its coverage must still be published", c.Package)
			}
		case "removed":
			if c.ToPct != nil {
				t.Errorf("%s is gone, so quoting a later coverage of %v reads as a collapse to that figure rather than a deletion", c.Package, *c.ToPct)
			}
			if c.FromPct == nil {
				t.Errorf("%s was measured in the baseline, so its coverage must still be published", c.Package)
			}
		default:
			if c.FromPct == nil || c.ToPct == nil {
				t.Errorf("%s exists in both snapshots, so both figures were measured and must both be published: from=%v to=%v", c.Package, c.FromPct, c.ToPct)
			}
		}
	}
	for _, state := range []string{"added", "removed", "changed"} {
		if !seen[state] {
			t.Fatalf("fixture is wrong: no culprit is in the %q state, so this test proves nothing about it", state)
		}
	}

	// And on the wire, where a consumer actually reads it.
	for _, c := range wireGroup(t, jsonRepoRows(t, rep)[repoMover], repoMover, "coverage")["culprits"].([]any) {
		row, ok := c.(map[string]any)
		if !ok {
			t.Fatalf("culprits entry is %T, want an object", c)
		}
		absent := map[string]string{"added": "from_pct", "removed": "to_pct"}[row["state"].(string)]
		if absent == "" {
			continue
		}
		v, present := row[absent]
		if !present {
			t.Errorf("%v: %s is missing rather than null, so a consumer defaulting it reads a zero", row["package"], absent)
		}
		if v != nil {
			t.Errorf("%v is %v, so its %s must be null, got %v", row["package"], row["state"], absent, v)
		}
	}
}

// numberPaths returns the path of every JSON number under v. It walks lists and
// nested objects too, because a number one level further down is exactly as
// defaultable as one at the top.
func numberPaths(path string, v any) []string {
	switch val := v.(type) {
	case map[string]any:
		var out []string
		for key, child := range val {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			out = append(out, numberPaths(childPath, child)...)
		}
		return sortedNames(out)
	case []any:
		var out []string
		for _, item := range val {
			out = append(out, numberPaths(path+"[]", item)...)
		}
		return out
	case float64:
		return []string{path}
	default:
		return nil
	}
}
