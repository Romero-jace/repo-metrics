package cli_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// againstSeed is a repo with two snapshots that are far apart in time and in
// coverage, seeded through the store so their timestamps and shas are ours to
// choose. Two collections a second apart could not exercise the age rules at
// all.
//
// baselineAgo is how far back the older snapshot sits. The head is always now.
func againstSeed(t *testing.T, baselineAgo time.Duration) (cfgPath string, baseID, headID int64) {
	t.Helper()
	dir := t.TempDir()
	repo := repoDir(t, dir, "service", sampleProfile)
	dbPath := filepath.Join(dir, "metrics.db")
	cfgPath = writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		dbPath, ingestRepoEntry("service", repo, "coverage.out")))

	ctx := context.Background()
	seed := openStore(t, dbPath)
	repoID, err := seed.UpsertRepo(ctx, "service", repo)
	if err != nil {
		t.Fatalf("seeding the repo: %v", err)
	}

	write := func(ago time.Duration, sha string, covered int) int64 {
		snap := store.Snapshot{
			RepoID:      repoID,
			CollectedAt: time.Now().Add(-ago),
			GitSHA:      sha,
			Env:         "go=go1.26.5",
			Status:      store.StatusOK,
		}
		id, err := seed.InsertSnapshot(ctx, snap, []store.Metric{
			{Key: collect.KeyCoveredStmts, Scope: "example.com/demo/a", Value: float64(covered)},
			{Key: collect.KeyTotalStmts, Scope: "example.com/demo/a", Value: 100},
		})
		if err != nil {
			t.Fatalf("seeding a snapshot: %v", err)
		}
		return id
	}
	// Distinct shas, both long enough to abbreviate, sharing no 7 character
	// prefix so an abbreviation in one test cannot match the other snapshot.
	baseID = write(baselineAgo, "abc1234def5678901234567890abcdef12345678", 90)
	headID = write(0, "9876543210fedcba9876543210fedcba98765432", 50)

	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed store: %v", err)
	}
	return cfgPath, baseID, headID
}

// The feature, stated as the job it was asked for: get a delta and the packages
// behind it without waiting a week for history to accumulate.
func TestReportAgainstNamedSnapshotProducesADelta(t *testing.T) {
	cfgPath, baseID, _ := againstSeed(t, 2*time.Hour)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "service",
		"--against", fmt.Sprint(baseID), "--format", "json")
	if err != nil {
		t.Fatalf("report --against: %v (stderr: %s)", err, stderr)
	}
	doc := decodeReport(t, stdout)

	row := jsonRepo(t, *doc.Repos, "service")
	if row.Coverage == nil {
		t.Fatal("coverage is null on a snapshot that measured 50 percent")
	}
	if row.Coverage.Value != 50 {
		t.Errorf("coverage.value = %v, want 50", row.Coverage.Value)
	}
	if row.Coverage.Delta == nil {
		t.Fatal("delta is null, so --against produced no comparison at all: this is the whole feature")
	}
	if *row.Coverage.Delta != -40 {
		t.Errorf("coverage.delta = %v, want -40 against the 90 percent baseline", *row.Coverage.Delta)
	}

	// The envelope says which baseline it used. Without it, this report and a
	// windowed one are the same bytes making different claims.
	if doc.BaselineRef == nil {
		t.Error("baseline_ref is null on a report that named its baseline")
	} else if *doc.BaselineRef != fmt.Sprint(baseID) {
		t.Errorf("baseline_ref = %q, want %v", *doc.BaselineRef, baseID)
	}
}

// The control for the field above: a windowed report must not claim a named
// baseline.
func TestReportWithoutAgainstReportsNoNamedBaseline(t *testing.T) {
	cfgPath, _, _ := againstSeed(t, 30*24*time.Hour)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "service", "--format", "json")
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}
	if ref := decodeReport(t, stdout).BaselineRef; ref != nil {
		t.Errorf("baseline_ref = %q on a report whose baseline came from the window", *ref)
	}
}

// An abbreviated commit sha resolves, which is the form anyone actually has to
// hand after running git log.
func TestReportAgainstAcceptsAnAbbreviatedSHA(t *testing.T) {
	cfgPath, _, _ := againstSeed(t, 2*time.Hour)

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "service",
		"--against", "abc1234", "--format", "json")
	if err != nil {
		t.Fatalf("report --against abc1234: %v (stderr: %s)", err, stderr)
	}
	row := jsonRepo(t, *decodeReport(t, stdout).Repos, "service")
	if row.Coverage == nil || row.Coverage.Delta == nil {
		t.Fatalf("no delta from a sha prefix: %+v", row.Coverage)
	}
	if *row.Coverage.Delta != -40 {
		t.Errorf("coverage.delta = %v, want -40", *row.Coverage.Delta)
	}
}

// A named baseline is exempt from the staleness rule, and this is the assertion
// that makes --against worth having rather than merely working.
//
// The rule refuses to nominate a repo whose baseline is more than three windows
// old, because a quarter of accumulated drift must not outrank the repos that
// really moved this week. It infers "nobody was watching" from a large gap, and
// that inference is wrong the moment somebody chose the far end deliberately.
// Left on, movers and culprits stay dark for exactly the comparison this flag
// exists to make: a worktree against a release tag from three months back.
//
// 100 days against the default 7 day window is well past the 21 day cutoff, so a
// windowed report of the same two snapshots is checked below to prove the rule
// really does fire on this fixture. Without that half, this test would pass on a
// build where the staleness rule was simply broken.
func TestReportAgainstIsExemptFromTheStalenessRule(t *testing.T) {
	cfgPath, baseID, _ := againstSeed(t, 100*24*time.Hour)

	named, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "service",
		"--against", fmt.Sprint(baseID), "--format", "json")
	if err != nil {
		t.Fatalf("report --against: %v (stderr: %s)", err, stderr)
	}
	if movers := decodeReport(t, named).Movers; movers == nil || len(*movers) != 1 {
		t.Errorf("a named baseline 100 days back nominated no mover, so a 40 point drop went unreported: %v", movers)
	}

	windowed, stderr, err := runCLI(t, "report", "--config", cfgPath, "--repo", "service", "--format", "json")
	if err != nil {
		t.Fatalf("report: %v (stderr: %s)", err, stderr)
	}
	if movers := decodeReport(t, windowed).Movers; movers != nil && len(*movers) != 0 {
		t.Errorf("the same two snapshots nominated a mover through the window, so the staleness rule is not firing on this fixture and the exemption above proves nothing: %v", *movers)
	}
}

// Every way of naming a baseline that cannot mean what it says is refused with
// the reason, rather than resolved by precedence or by a guess.
func TestReportAgainstRefusesTheAmbiguousAndTheImpossible(t *testing.T) {
	cfgPath, _, headID := againstSeed(t, 2*time.Hour)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			"with a window",
			[]string{"--repo", "service", "--against", "1", "--window", "7d"},
			"cannot be used together",
		},
		{
			"without a repo",
			[]string{"--against", "1"},
			"needs --repo",
		},
		{
			"neither a sha nor an id",
			[]string{"--repo", "service", "--against", "origin/dev"},
			"neither a snapshot id nor a commit sha",
		},
		{
			"nothing matches",
			[]string{"--repo", "service", "--against", "99999"},
			"no snapshot of service matches",
		},
		{
			"the head itself",
			[]string{"--repo", "service", "--against", fmt.Sprint(headID)},
			"zero by construction",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"report", "--config", cfgPath}, tc.args...)
			stdout, stderr, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("accepted %v, rendering %q", tc.args, stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not say %q, so the refusal is a dead end: %q", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("a refused report still wrote to stdout, which a pipeline would try to parse: %q", stdout)
			}
		})
	}
}
