package report

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// ReposInput is one configured repo as the store answered for it.
//
// Snapshot is nil only when the repo has never been collected at all. A repo
// whose every run failed carries its failed row here, because the caller looks
// it up the same two-step way reportInputs does: LatestSnapshot skips failed
// rows, so a repo that only ever failed comes back nil from it and would
// otherwise arrive looking exactly like one that has never run. Those two call
// for opposite actions, go fix your build versus go run collect.
type ReposInput struct {
	Name     string
	Snapshot *store.Snapshot
	Metrics  []store.Metric
}

// ReposView is what every configured repo last did.
type ReposView struct {
	GeneratedAt string `json:"generated_at"`
	// Repos is allocated and empty rather than nil, for the same reason the
	// report's sections are: a null marshals into a consumer's length check and
	// crashes it on the empty case.
	Repos []RepoStateView `json:"repos"`
}

// RepoStateView is one repo's line, in four states rather than three.
//
// The fourth is the one that is easy to miss: a run that succeeded and measured
// nothing, which a header-only coverage profile produces. It parses clean, the
// snapshot is stored ok, and its percentage is zero for want of a denominator.
// On the wire that has to be a null group and never a pct of zero.
type RepoStateView struct {
	Name string `json:"name"`
	// Status carries the store's own status, or StatusNotCollected for a repo
	// nobody has collected. That is deliberately the same vocabulary the report
	// publishes: two payloads describing one repo must not name its state two
	// different ways.
	Status string `json:"status"`
	// CollectedAt is null, not the empty string, when this repo has never been
	// collected. Same reasoning as ScopeView.Repo: a consumer testing for an
	// empty string has to already know that empty means never, while null reads
	// as absent on its own.
	CollectedAt *string `json:"collected_at"`
	HasSnapshot bool    `json:"has_snapshot"`
	// Coverage is nil in three of the four states, and only one of them is a
	// failure.
	Coverage *CoverageTotalsView `json:"coverage"`
	// CoverageLines is the same thing in lines rather than statements, under the
	// key the report already publishes it as. Two payloads describing one repo
	// must not name its state two different ways.
	//
	// Without it this command answered "no coverage" for a Python or TypeScript
	// repo that had measured perfectly well, because the only coverage it knew
	// about was a Go profile's. Reported from a real fleet, where it read as a
	// collection that had not worked.
	CoverageLines *CoverageTotalsView `json:"coverage_lines"`
}

// CoverageTotalsView is what one snapshot measured about coverage, with nothing
// derived from comparing it to another.
//
// It is deliberately not CoverageView. That type carries a delta, culprits and
// package churn, all of which are baseline comparisons, and publishing them as
// permanently null here would dilute "null means nothing measured" into "null
// means not applicable". Those are different claims and the whole shape depends
// on them staying different.
// The rate is published as value rather than as pct, which is the name the
// report and history both use for the number a signal measured. One walker
// should read a measurement out of any of the three payloads without knowing
// which one it is holding, and a second name for one concept is what stops that.
// This was a wire break, and it is why the release carrying it is v0.2.0.
type CoverageTotalsView struct {
	Value   float64 `json:"value"`
	Covered int     `json:"covered"`
	Total   int     `json:"total"`
}

// StatusText is what the table prints, which is not always what the wire says.
//
// The wire says "not collected" because that is the vocabulary the report
// already publishes for the same state. The table says "never collected"
// because it reads better in a status column beside a "never" timestamp. These
// are methods so the prose can never reach the wire, and they are computed from
// the same fields the JSON publishes so the two cannot describe different
// states.
func (r RepoStateView) StatusText() string {
	if !r.HasSnapshot {
		return "never collected"
	}
	return r.Status
}

// CollectedText is the timestamp, or the word for not having one.
func (r RepoStateView) CollectedText() string {
	if r.CollectedAt == nil {
		return "never"
	}
	return *r.CollectedAt
}

// CoverageText is the statement-coverage cell.
func (r RepoStateView) CoverageText() string {
	return r.coverageCell(r.Coverage)
}

// LineCoverageText is the line-coverage cell, in lines over files.
func (r RepoStateView) LineCoverageText() string {
	return r.coverageCell(r.CoverageLines)
}

// coverageCell keeps the three distinct phrases the table has always used for
// its three coverage-less states, which are three different findings: nobody ran
// it, the run failed, and the run worked without measuring this.
//
// The third phrase used to be "no coverage", which read as a complaint. With two
// columns it would be wrong as well: a Go repo has no line coverage and an LCOV
// repo has no statement coverage, and both of those are the honest answer rather
// than a problem. "not measured" is the word history's table already uses for
// exactly this state, and the README calls the difference between it and "not
// collected" load-bearing, so the two tables now say it the same way.
func (r RepoStateView) coverageCell(c *CoverageTotalsView) string {
	if c != nil {
		return fmt.Sprintf("%.1f%%", c.Value)
	}
	switch {
	case !r.HasSnapshot:
		return "-"
	case r.Status == string(store.StatusFailed):
		return "not collected"
	default:
		return "not measured"
	}
}

// BuildRepos turns the store's answers into the render-ready view.
func BuildRepos(generatedAt time.Time, inputs []ReposInput) ReposView {
	view := ReposView{
		GeneratedAt: generatedAt.UTC().Format(timeFormat),
		Repos:       make([]RepoStateView, 0, len(inputs)),
	}

	for _, in := range inputs {
		out := RepoStateView{Name: in.Name, Status: StatusNotCollected}
		if in.Snapshot != nil {
			at := in.Snapshot.CollectedAt.UTC().Format(timeFormat)
			out.HasSnapshot = true
			out.Status = string(in.Snapshot.Status)
			out.CollectedAt = &at
		}

		// Measured through delta rather than by summing metric keys here, so the
		// presence rule is the one rule instead of a fourth copy of it, and so
		// nothing outside that package needs to know which keys coverage lives
		// under. A header-only profile stores no package rows, the marker is
		// absent, and the group stays nil instead of publishing a percentage
		// computed from a zero denominator.
		out.Coverage = coverageTotals(in.Metrics, delta.SigCoverage)
		out.CoverageLines = coverageTotals(in.Metrics, delta.SigCoverageLines)
		view.Repos = append(view.Repos, out)
	}
	return view
}

// coverageTotals reads one coverage signal off a snapshot's metrics, or nil.
//
// Measured through delta rather than by summing metric keys here, so the
// presence rule is the one rule instead of a fourth copy of it, and so nothing
// outside that package needs to know which keys each unit lives under. A
// header-only profile stores no scoped rows, the marker is absent, and the group
// stays nil instead of publishing a percentage computed from a zero denominator.
func coverageTotals(metrics []store.Metric, id delta.SignalID) *CoverageTotalsView {
	counts, measured := delta.CoverageCountsFor(metrics, id)
	if !measured {
		return nil
	}
	return &CoverageTotalsView{
		Value:   counts.Pct(),
		Covered: counts.Covered,
		Total:   counts.Total,
	}
}

// RenderRepos writes the repo listing in the requested format.
//
// The markdown rendering is the aligned table this command has always printed
// rather than a pipe table, and that is a knowing wart: one format name means
// two different human renderings across subcommands. The alternative buys
// naming purity and costs a user-visible change to output nobody asked to
// change.
func RenderRepos(w io.Writer, f Format, v ReposView) error {
	if f == FormatJSON {
		return renderJSON(w, v, "repos")
	}

	// Buffered until Flush, which is fine here: this is a table, not progress.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "REPO\tLAST COLLECTED\tSTATUS\tCOVERAGE\tLINE COVERAGE"); err != nil {
		return fmt.Errorf("report: rendering repos: %w", err)
	}
	for _, r := range v.Repos {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.CollectedText(), r.StatusText(),
			r.CoverageText(), r.LineCoverageText()); err != nil {
			return fmt.Errorf("report: rendering repos: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("report: rendering repos: %w", err)
	}
	return nil
}
