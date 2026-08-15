package report_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/report"
)

// scopeOf renders one section under one scope and hands back the envelope's
// scope object, decoded from the wire rather than read off the View. Reading the
// struct would round-trip a Go value back to itself and prove nothing about what
// a consumer receives.
func scopeOf(t *testing.T, sec report.Section, scope report.Scope) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(mustJSONScoped(t, fullReport(), sec, scope)), &doc); err != nil {
		t.Fatalf("--section %s: decoding the rendered json: %v", sec, err)
	}
	got, ok := doc["scope"].(map[string]any)
	if !ok {
		t.Fatalf("--section %s: scope is %v (%T), want an object. It is never null: every report covers something.", sec, doc["scope"], doc["scope"])
	}
	return got
}

// The count of repos a report covers is a fact about the request, so it has to
// survive narrowing to a section that does not render the repo list at all.
//
// This is the trap the field itself was built to avoid, one level in. Under
// --section movers the view's Repos slice is nil by design, and the mover
// exclusion loop drops repos from Movers besides, so deriving the count from
// either published a number that has nothing to do with how many repos were
// asked about. Both wrong answers are plausible-looking small integers, which is
// why this is asserted per section rather than once.
func TestScopeCountsRepositoriesNotRenderedRows(t *testing.T) {
	rep := fullReport()
	want := float64(len(rep.Repos))

	// Anti-vacuity: if the movers list were the same length as the repo list,
	// the movers case below could pass while reading entirely the wrong slice.
	if len(rep.Movers()) >= len(rep.Repos) {
		t.Fatalf("fixture no longer distinguishes movers (%d) from repos (%d), so this test cannot fail on the bug it exists for", len(rep.Movers()), len(rep.Repos))
	}

	for _, name := range report.Sections() {
		sec := report.Section(name)
		t.Run(name, func(t *testing.T) {
			got := scopeOf(t, sec, report.Scope{Configured: len(rep.Repos)})
			if got["selected"] != want {
				t.Errorf("scope.selected = %v, want %v. It counts the repos the report covers, not the rows this section happened to render, and every section covers the same repos.", got["selected"], want)
			}
			if got["configured"] != want {
				t.Errorf("scope.configured = %v, want %v", got["configured"], want)
			}
		})
	}
}

// An unnarrowed report has to say it is unnarrowed, and it cannot say so with an
// empty string: a consumer testing scope.repo == "" would have to know that empty
// means everything, while null reads as no-filter without being told.
func TestUnnarrowedScopeCarriesNoRepoName(t *testing.T) {
	got := scopeOf(t, report.SectionAll, report.Scope{Configured: len(fullReport().Repos)})
	if got["repo"] != nil {
		t.Errorf("scope.repo = %v, want null on a report that covers everything", got["repo"])
	}
}

// The motivating case. A narrowed report and a fleet-wide one must not be able to
// produce the same bytes, because an empty problems list means something
// completely different in each.
func TestNarrowedScopeNamesTheRepoAndTheDenominator(t *testing.T) {
	got := scopeOf(t, report.SectionProblems, report.Scope{Repo: repoMover, Configured: 9})

	if got["repo"] != repoMover {
		t.Errorf("scope.repo = %v, want %q", got["repo"], repoMover)
	}
	if got["configured"] != float64(9) {
		t.Errorf("scope.configured = %v, want 9. The denominator comes from the config, not from the narrowed report, or a consumer can never tell how much it is not being shown.", got["configured"])
	}
	if got["selected"] == got["configured"] {
		t.Errorf("scope.selected == scope.configured (%v) on a narrowed report, so nothing on the wire says the answer is partial", got["selected"])
	}
}

// Markdown gets the same fact, because a human reading a narrowed report has
// exactly the same way of being misled as a machine parsing one.
func TestNarrowingIsAnnouncedInMarkdownOnlyWhenItHappened(t *testing.T) {
	rep := fullReport()

	narrowed := mustMarkdownScoped(t, rep, report.SectionAll, report.Scope{Repo: repoMover, Configured: 9})
	if !strings.Contains(narrowed, "Only covering the repo `"+repoMover+"`") {
		t.Errorf("a narrowed report does not say so. Got header:\n%s", firstLines(narrowed, 6))
	}
	if !strings.Contains(narrowed, "out of 9 repos in this config") {
		t.Errorf("a narrowed report does not say how much it is leaving out. Got header:\n%s", firstLines(narrowed, 6))
	}

	whole := mustMarkdownScoped(t, rep, report.SectionAll, report.Scope{Configured: len(rep.Repos)})
	if strings.Contains(whole, "Only covering") {
		t.Errorf("an unnarrowed report announces a narrowing that did not happen. Got header:\n%s", firstLines(whole, 6))
	}
}

// A single-repo config is the case where a count and a bare noun disagree.
func TestTheDenominatorIsPluralizedHonestly(t *testing.T) {
	md := mustMarkdownScoped(t, fullReport(), report.SectionAll, report.Scope{Repo: repoMover, Configured: 1})
	if !strings.Contains(md, "out of 1 repo in this config") {
		t.Errorf("got %q, want \"out of 1 repo\". A config with one repo in it reads as broken English otherwise.", firstLines(md, 6))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
