package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// historyPoint is one collection run in a charted series.
//
// The timestamps are spread out rather than shared, because a section that
// tells a reader to match its lines back to the table by timestamp is only
// readable if the timestamps differ, and a fixture where they all coincide
// would not show that.
//
// A failed run carries no metrics, which is what makes it a gap rather than a
// zero: seeding one with coverage would defeat the point of charting it at all.
func historyPoint(daysAgo int, status store.Status, errText string, covered int) store.SnapshotMetrics {
	point := store.SnapshotMetrics{Snapshot: store.Snapshot{
		ID:          int64(daysAgo),
		RepoID:      10,
		CollectedAt: fixedNow().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Env:         "go1.26.5",
		Status:      status,
		Error:       errText,
	}}
	if status != store.StatusFailed {
		point.Metrics = cov(pkgAlpha, covered, 100)
	}
	return point
}

// mustHistoryMarkdown renders a series the way the CLI's default format does.
func mustHistoryMarkdown(t *testing.T, series []store.SnapshotMetrics) string {
	t.Helper()
	view := report.BuildHistory(
		fixedNow(), fixedNow().Add(-90*24*time.Hour), 90,
		repoHealthy, 4, delta.SignalByID(delta.SigCoverage), series, nil)

	var b strings.Builder
	if err := report.RenderHistory(&b, report.FormatMarkdown, view); err != nil {
		t.Fatalf("rendering the history markdown: %v", err)
	}
	return b.String()
}

// tableRows returns the pipe-delimited rows of the rendered table, minus its
// heading and separator, so an assertion about the table can be told apart from
// one about the prose around it.
func tableRows(md string) []string {
	var out []string
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "| ---") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// A run that did not measure has its reason in the payload and had it thrown
// away on the way to the markdown.
//
// Cells() loads Error onto every cell and the table renders three columns, so
// the text was loaded and dropped: the reader was told a run failed and given
// no way to learn why, while the same series through --format json carried the
// whole explanation.
func TestHistoryMarkdownSaysWhyARunDidNotMeasure(t *testing.T) {
	const (
		crashed = "test command exited 2"
		stale   = "coverage profile was stale"
		noBuild = "one package would not build"
	)

	for _, tc := range []struct {
		name   string
		series []store.SnapshotMetrics
		// want is the error text that has to reach the reader.
		want []string
		// absent is text that must not appear anywhere, which is how the clean
		// case asserts it did not grow an empty heading.
		absent []string
	}{
		{
			name: "one failed run among healthy ones",
			series: []store.SnapshotMetrics{
				historyPoint(60, store.StatusOK, "", 40),
				historyPoint(30, store.StatusFailed, crashed, 0),
				historyPoint(10, store.StatusOK, "", 80),
			},
			want: []string{crashed},
		},
		{
			name: "three failed runs, each with its own reason",
			series: []store.SnapshotMetrics{
				historyPoint(60, store.StatusFailed, crashed, 0),
				historyPoint(30, store.StatusFailed, stale, 0),
				historyPoint(10, store.StatusFailed, noBuild, 0),
			},
			want: []string{crashed, stale, noBuild},
		},
		{
			// A partial run reports a problem and still measures. Its number is
			// real and stays in the table, and the reason it is lower than last
			// week's is the line this section exists to carry.
			name: "a partial run that measured something anyway",
			series: []store.SnapshotMetrics{
				historyPoint(60, store.StatusOK, "", 40),
				historyPoint(10, store.StatusPartial, noBuild, 55),
			},
			want: []string{noBuild},
		},
		{
			name: "a series that only ever collected cleanly",
			series: []store.SnapshotMetrics{
				historyPoint(60, store.StatusOK, "", 40),
				historyPoint(10, store.StatusOK, "", 80),
			},
			absent: []string{"Collection problems"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := mustHistoryMarkdown(t, tc.series)

			for _, want := range tc.want {
				if !strings.Contains(md, want) {
					t.Errorf("the markdown never says %q, so a reader is told a run did not measure and given no way to learn why:\n%s", want, md)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(md, absent) {
					t.Errorf("the markdown carries %q for a series with nothing to explain, so a clean repo grows an empty heading:\n%s", absent, md)
				}
			}

			// Beneath the table, never a fourth column of it. A history table is
			// three columns wide and a sentence of build output in one of them is
			// the noise PointView.GitSHA is kept out of the table to avoid. It is
			// also load-bearing elsewhere: cli's TestHistoryKeepsFailedRunsAsVisibleGaps
			// scans every cell of a failed row from index 2 onward for digits, and
			// error text carrying a number would trip a guard that is right about
			// its intent.
			for _, row := range tableRows(md) {
				for _, want := range tc.want {
					if strings.Contains(row, want) {
						t.Errorf("the error text reached a table row: %q", row)
					}
				}
			}
		})
	}
}

// A repo with one snapshot in the window used to read "1 collections."
//
// The sibling problem was already solved for repos and for days, both of which
// go through count, and this line was the one noun with no helper.
func TestHistoryCountsItsCollectionsWithTheRightNoun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		series []store.SnapshotMetrics
		want   string
	}{
		{
			name:   "one collection in range",
			series: []store.SnapshotMetrics{historyPoint(10, store.StatusOK, "", 80)},
			want:   "1 collection.",
		},
		{
			// The anti-vacuity control. Without it, a helper that answered the
			// singular for every count would pass.
			name: "several collections in range",
			series: []store.SnapshotMetrics{
				historyPoint(60, store.StatusOK, "", 40),
				historyPoint(30, store.StatusOK, "", 60),
				historyPoint(10, store.StatusOK, "", 80),
			},
			want: "3 collections.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := mustHistoryMarkdown(t, tc.series)
			line := summaryLine(t, md)
			if !strings.HasPrefix(line, tc.want) {
				t.Errorf("the summary line reads %q, want it to start %q. A count and its noun disagreeing is the tell that the noun was typed into the template rather than chosen from the number.",
					line, tc.want)
			}
		})
	}
}

// summaryLine finds the sentence under the table that counts the series. It is
// located by its own prose rather than by line number so that adding anything
// above or below it does not silently point this at another line.
func summaryLine(t *testing.T, md string) string {
	t.Helper()
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "A row reading") {
			return line
		}
	}
	t.Fatalf("no summary line in the rendered history:\n%s", md)
	return ""
}
