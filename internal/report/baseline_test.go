package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// at returns a snapshot taken a given distance before the report's now, so a
// test can say how far apart the two sides really are.
func at(id, repoID int64, ago time.Duration) *store.Snapshot {
	s := snap(id, repoID, "go1.26.5", store.StatusOK, "")
	s.CollectedAt = fixedNow().Add(-ago)
	return s
}

// slidRepo is a repo whose coverage fell from 80 to 40 percent between two
// snapshots however far apart the caller puts them.
func slidRepo(name string, headAgo, baseAgo time.Duration) delta.Input {
	return delta.Input{
		Repo:        repoAt(1, name),
		Head:        at(2, 1, headAgo),
		HeadMetrics: cov("m/a", 40, 100),
		Base:        at(1, 1, baseAgo),
		BaseMetrics: cov("m/a", 80, 100),
	}
}

// The report used to call every delta "on the week" whatever the two snapshots
// were actually a week apart or a quarter.
//
// Baseline selection takes the newest snapshot at or before the cutoff and puts
// no floor on how far before, so a repo nobody collected for two months compares
// against a two-month-old snapshot and every number is a two-month change. The
// header said "about 7 days back" and each line said "on the week", and both
// were describing what was asked for rather than what was found.
// A fortnight is the ordinary version of this: one missed collection, still a
// mover, and a delta covering twice what the header asked for.
func TestAMoverLineSaysTheSpanItActuallyCovers(t *testing.T) {
	opts := options()
	opts.Window = 7 * 24 * time.Hour

	rep := delta.Compute([]delta.Input{slidRepo("missed-a-run", 0, 14*24*time.Hour)}, opts, fixedNow())
	md := mustMarkdown(t, rep)

	if !strings.Contains(md, "## What moved") || !strings.Contains(md, "missed-a-run") {
		t.Fatalf("a repo whose coverage halved should still lead the report:\n%s", md)
	}
	if strings.Contains(md, "on the week") {
		t.Error("the report still says a delta is on the week, whatever span it covers")
	}
	if !strings.Contains(md, "over the past 14 days") {
		t.Errorf("the report does not say how far back the baseline really is:\n%s", md)
	}
}

// The nomination half. A repo nobody collected for two months has a real delta
// and it is not this week's news: published, labeled with its true span, and not
// allowed to push aside the repos that actually moved this week.
func TestALapsedRepoDoesNotLeadTheReport(t *testing.T) {
	opts := options()
	opts.Window = 7 * 24 * time.Hour

	rep := delta.Compute([]delta.Input{slidRepo("lapsed", 0, 60*24*time.Hour)}, opts, fixedNow())

	if len(rep.Repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(rep.Repos))
	}
	if rep.Repos[0].IsMover {
		t.Errorf("a two-month-old baseline led the report, MovedBy=%v", rep.Repos[0].MovedBy)
	}
	// The delta is still there. Refusing to publish it would be a worse answer
	// than publishing it plainly: it is the best comparison available.
	md := mustMarkdown(t, rep)
	if !strings.Contains(md, "-40.0 pts") {
		t.Errorf("the delta itself was withheld, not just the nomination:\n%s", md)
	}
}

// The anti-vacuity control: an ordinary week still reads like one.
func TestAnOrdinaryWeekSaysSo(t *testing.T) {
	opts := options()
	opts.Window = 7 * 24 * time.Hour

	rep := delta.Compute([]delta.Input{slidRepo("weekly", 0, 7*24*time.Hour)}, opts, fixedNow())
	md := mustMarkdown(t, rep)

	if !strings.Contains(md, "over the past 7 days") {
		t.Errorf("a genuinely weekly report does not say so:\n%s", md)
	}
}

// The wire half. window_days is what was asked for, and until now nothing
// carried what was found, so a consumer could not tell a week from a quarter.
func TestTheWireCarriesTheBaselineTimestamp(t *testing.T) {
	opts := options()
	opts.Window = 7 * 24 * time.Hour

	rep := delta.Compute([]delta.Input{slidRepo("lapsed", 0, 60*24*time.Hour)}, opts, fixedNow())
	row := jsonRepoRows(t, rep)["lapsed"]
	if row == nil {
		t.Fatal("no row for the repo")
	}

	got, present := row["baseline_collected_at"]
	if !present {
		t.Fatal("the row does not carry baseline_collected_at, so window_days is the only timing on the wire and it is the request rather than the answer")
	}
	// 60 days before 2026-03-02 is 2026-01-01.
	if s, ok := got.(string); !ok || !strings.HasPrefix(s, "2026-01-01") {
		t.Errorf("baseline_collected_at = %v, want the baseline's own timestamp", got)
	}
}

// Null rather than absent or an empty string, so a consumer can tell "no
// baseline" from "a baseline at the zero time".
func TestTheBaselineTimestampIsNullWithoutABaseline(t *testing.T) {
	rep := delta.Compute([]delta.Input{{
		Repo:        repoAt(1, "fresh"),
		Head:        at(2, 1, 0),
		HeadMetrics: cov("m/a", 40, 100),
	}}, options(), fixedNow())

	row := jsonRepoRows(t, rep)["fresh"]
	if row == nil {
		t.Fatal("no row for the repo")
	}
	got, present := row["baseline_collected_at"]
	if !present {
		t.Fatal("the key is absent entirely, so a consumer cannot tell it apart from a field nobody implemented")
	}
	if got != nil {
		t.Errorf("baseline_collected_at = %v, want null for a repo with no baseline", got)
	}
}
