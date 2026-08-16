package report_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// Repo names for the dirty-tree fixture. They share no substring with the
// marker text or with each other, so a row lookup by name cannot match the
// wrong row or the footnote.
const (
	repoDirty      = "smudgedrepo"
	repoClean      = "tidyrepo"
	repoDirtyFails = "smudgedbrokenrepo"
)

// dirtySnap is the shared snap fixture with the tree state set.
//
// It is a wrapper rather than a new field on snap itself, deliberately. snap
// feeds degradedRows and every assertion built on it, and adding a dirty tree
// there would change what a dozen unrelated tests are exercising while leaving
// this one no clean control to compare against.
func dirtySnap(id, repoID int64, status store.Status, errText string, dirty bool) *store.Snapshot {
	s := snap(id, repoID, "go1.26.5", status, errText)
	s.GitDirty = dirty
	return s
}

func dirtyFixture() delta.Report {
	head := metrics(cov(pkgAlpha, 80, 100), testStream(0, testCount(pkgAlpha, 9)))
	base := metrics(cov(pkgAlpha, 75, 100), testStream(0, testCount(pkgAlpha, 9)))

	return delta.Compute([]delta.Input{
		{
			Repo:        repoAt(1, repoDirty),
			Head:        dirtySnap(11, 1, store.StatusOK, "", true),
			HeadMetrics: head,
			Base:        dirtySnap(10, 1, store.StatusOK, "", false),
			BaseMetrics: base,
		},
		{
			// The control. Without a clean row beside the dirty one, a field
			// hardwired to true would pass every assertion below.
			Repo:        repoAt(2, repoClean),
			Head:        dirtySnap(21, 2, store.StatusOK, "", false),
			HeadMetrics: head,
			Base:        dirtySnap(20, 2, store.StatusOK, "", false),
			BaseMetrics: base,
		},
		{
			// A failed run over a dirty tree. The fact is true and belongs on the
			// wire; the marker does not belong in the table, because the row
			// published no numbers for it to qualify.
			Repo: repoAt(3, repoDirtyFails),
			Head: dirtySnap(31, 3, store.StatusFailed, "test command exited 2", true),
		},
		{
			// Never collected. There is no tree state to report and the key still
			// has to be on the wire, because a consumer that never sees a key
			// defaults it and cannot tell false from absent.
			Repo: repoAt(4, repoUnseen),
		},
	}, options(), fixedNow())
}

// git_dirty is written on every snapshot and, until it reached RepoView, was
// read by no output surface at all: a measurement taken over a tree full of
// uncommitted work reached every consumer indistinguishable from one taken over
// a clean checkout.
func TestADirtyTreeReachesTheWire(t *testing.T) {
	rows := jsonRepoRows(t, dirtyFixture())

	for _, tc := range []struct {
		repo string
		want bool
		why  string
	}{
		{repoDirty, true, "the snapshot was taken over a tree with uncommitted changes, so its numbers belong to no commit"},
		{repoClean, false, "the snapshot was taken over a clean tree"},
		{repoDirtyFails, true, "the run failed, but it still failed over a dirty tree and the fact is not a measurement"},
		{repoUnseen, false, "no snapshot at all, so there is no tree state and false is the honest answer"},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			row := rows[tc.repo]
			if row == nil {
				t.Fatalf("%s is missing from the json entirely", tc.repo)
			}
			raw, present := row["git_dirty"]
			if !present {
				t.Fatalf("%s: git_dirty is missing from the row rather than false. A consumer that never sees the key defaults it, and cannot tell a clean tree from an unreported one.", tc.repo)
			}
			got, isBool := raw.(bool)
			if !isBool {
				t.Fatalf("%s: git_dirty rendered as %T (%v), want a bool", tc.repo, raw, raw)
			}
			if got != tc.want {
				t.Errorf("%s: git_dirty = %v, want %v (%s)", tc.repo, got, tc.want, tc.why)
			}
		})
	}
}

// The markdown half. The JSON is what a script reads and the markdown is the
// product, so a fact that changes what every number on a row means has to be on
// the row a person actually reads.
func TestADirtyTreeIsMarkedInTheTable(t *testing.T) {
	const marker = "uncommitted changes"

	md := mustMarkdown(t, dirtyFixture())

	if row := repoRow(t, md, repoDirty); !strings.Contains(row, marker) {
		t.Errorf("the dirty repo's row carries no %q marker, so its numbers read as though they came from a commit: %s", marker, row)
	}
	if row := repoRow(t, md, repoClean); strings.Contains(row, marker) {
		t.Errorf("a repo collected over a clean tree is marked %q: %s", marker, row)
	}
	// A failed row published no numbers, so a note about how they were measured
	// would explain nothing. Same exclusion EnvWarned makes.
	if row := repoRow(t, md, repoDirtyFails); strings.Contains(row, marker) {
		t.Errorf("a failed run is marked %q, but it published no numbers for the warning to be about: %s", marker, row)
	}

	// The footnote goes through the same predicate the rows do, so it can never
	// explain a marker that is not in the table.
	if !strings.Contains(md, "A row marked uncommitted changes") {
		t.Errorf("the table marks a row %q and nothing explains what it means:\n%s", marker, md)
	}

	clean := mustMarkdown(t, fullReport())
	if strings.Contains(clean, marker) {
		t.Errorf("a report in which no repo was collected over a dirty tree still mentions %q:\n%s", marker, clean)
	}
}

// The two markers are independent claims and a row can carry both, so neither
// may swallow the other.
func TestTheDirtyAndToolchainMarkersDoNotDisplaceEachOther(t *testing.T) {
	head := metrics(cov(pkgAlpha, 80, 100), testStream(0, testCount(pkgAlpha, 9)))
	base := metrics(cov(pkgAlpha, 75, 100), testStream(0, testCount(pkgAlpha, 9)))

	dirtyOnNewToolchain := snap(11, 1, "go1.26.5", store.StatusOK, "")
	dirtyOnNewToolchain.GitDirty = true

	rep := delta.Compute([]delta.Input{{
		Repo:        repoAt(1, repoDirty),
		Head:        dirtyOnNewToolchain,
		HeadMetrics: head,
		Base:        snap(10, 1, "go1.25.1", store.StatusOK, ""),
		BaseMetrics: base,
	}}, options(), fixedNow())

	row := repoRow(t, mustMarkdown(t, rep), repoDirty)
	for _, marker := range []string{"toolchain changed", "uncommitted changes"} {
		if !strings.Contains(row, marker) {
			t.Errorf("the row is missing the %q marker, so one warning displaced the other: %s", marker, row)
		}
	}
}

// DirtyWarned answers about the head snapshot, EnvWarned about the comparison,
// and the difference shows up on a repo's first ever run: there is no baseline,
// so nothing can be said about a move, and the numbers still belong to no
// commit.
func TestADirtyFirstRunIsStillMarked(t *testing.T) {
	first := snap(11, 1, "go1.26.5", store.StatusOK, "")
	first.GitDirty = true

	rep := delta.Compute([]delta.Input{{
		Repo:        repoAt(1, repoDirty),
		Head:        first,
		HeadMetrics: metrics(cov(pkgAlpha, 80, 100), testStream(0, testCount(pkgAlpha, 9))),
	}}, options(), fixedNow())

	view := report.Build(rep)
	got := findRepoView(t, view.Repos, repoDirty)
	if !got.DirtyWarned() {
		t.Error("a first ever run over a dirty tree is not marked, so the warning was gated on having a baseline. A baseline is what the toolchain warning needs, because that one is about the move; this one is about the snapshot.")
	}
	if got.EnvWarned() {
		t.Error("a repo with no baseline is marked as having changed toolchain, which is a claim about a comparison that does not exist")
	}
}
