package report

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// ShowInput is one repo's newest snapshot and what it stored.
type ShowInput struct {
	Name     string
	Snapshot *store.Snapshot
	Metrics  []store.Metric
}

// ShowView is everything one snapshot recorded, with nothing derived from a
// second one.
//
// It is the query an operator kept rebuilding out of Python one-liners against
// the database: every signal, the counts behind the coverage rates, the commit,
// whether the tree was dirty, and what went wrong. repos answers a narrower
// question and the report answers a comparative one, so neither could be it.
//
// It carries the signal catalog, which repos does not and history carries one
// entry of. The rule the other two follow is that the legend rides along when a
// payload publishes measurements a consumer has to walk without knowing their
// names: the report pays 284 tokens once for fourteen signals rather than
// labeling every row, repos publishes only coverage so its key says which, and
// history charts exactly one so it carries that one. This publishes all fourteen
// and is therefore the report's case rather than either of the others.
type ShowView struct {
	GeneratedAt string `json:"generated_at"`
	Repo        string `json:"repo"`
	// Status is the store's own vocabulary, the same one repos and the report
	// publish. Three payloads describing one repo must not name its state three
	// ways.
	Status string `json:"status"`
	// CollectedAt is null when this repo has never been collected. Not an empty
	// string, which a consumer would have to already know means never.
	CollectedAt *string `json:"collected_at"`
	HasSnapshot bool    `json:"has_snapshot"`
	// GitSHA and GitBranch are null on a snapshot from a directory that is not a
	// git checkout, which collection allows and reports.
	GitSHA    *string `json:"git_sha"`
	GitBranch *string `json:"git_branch"`
	// GitDirty says the numbers below belong to no commit and cannot be
	// reproduced from one.
	GitDirty bool `json:"git_dirty"`
	// Env is the toolchain fingerprint, null when nothing identified it. That
	// null is the finding rather than a gap: a repo whose toolchain nothing named
	// cannot be compared across an upgrade, and an empty string here would read
	// as a toolchain called "".
	Env *string `json:"env"`
	// Degraded is three-state on the same terms as the report's: true means these
	// numbers were taken under protest, false means cleanly, null means nothing
	// recorded it, which is every snapshot written before v0.2.0.
	Degraded *bool `json:"degraded"`
	// Signals is the legend for the measurements below.
	Signals []SignalCatalogEntry `json:"signals"`
	// Measurements is one entry per signal in registry order, allocated and empty
	// rather than nil so a consumer reading its length does not crash on a repo
	// that measured nothing.
	Measurements []ShowMeasurement `json:"measurements"`
	// Error is what went wrong, omitted when nothing did. The only omitted key
	// here, on the same terms as the report row's.
	Error string `json:"error,omitempty"`
}

// ShowMeasurement is one signal's level on one snapshot.
//
// The value is inside a nullable group for the reason every measurement on every
// one of these payloads is: a signal nothing measured has no number, and a
// consumer reading it with a default turns that straight back into a measured
// zero.
type ShowMeasurement struct {
	// Signal is the registry id, so a consumer can join this to the catalog
	// above without depending on the order of either.
	Signal      string     `json:"signal"`
	Measurement *ShowLevel `json:"measurement"`
}

// ShowLevel is a measured value and, for the two signals that are a rate over
// counts, the counts it came from.
//
// Covered and Total are null for the other twelve. A test count has no
// denominator, and lending it the coverage counts stored beside it would be a
// number nobody measured.
type ShowLevel struct {
	Value   float64 `json:"value"`
	Covered *int    `json:"covered"`
	Total   *int    `json:"total"`
}

// BuildShow turns one snapshot into the render-ready view.
func BuildShow(generatedAt time.Time, in ShowInput) ShowView {
	view := ShowView{
		GeneratedAt:  generatedAt.UTC().Format(timeFormat),
		Repo:         in.Name,
		Status:       StatusNotCollected,
		Signals:      signalCatalog(),
		Measurements: make([]ShowMeasurement, 0, len(delta.Signals())),
	}

	if in.Snapshot != nil {
		at := in.Snapshot.CollectedAt.UTC().Format(timeFormat)
		view.HasSnapshot = true
		view.Status = string(in.Snapshot.Status)
		view.CollectedAt = &at
		view.GitDirty = in.Snapshot.GitDirty
		view.Degraded = in.Snapshot.Degraded
		view.Error = in.Snapshot.Error
		if sha := in.Snapshot.GitSHA; sha != "" {
			view.GitSHA = &sha
		}
		if branch := in.Snapshot.GitBranch; branch != "" {
			view.GitBranch = &branch
		}
		if env := in.Snapshot.Env; env != "" {
			view.Env = &env
		}
	}

	// Measured through delta rather than by reading metric keys here, so this
	// payload obeys the same presence rules as the other three and a signal
	// added to the registry appears in all of them at once.
	measured := delta.Measure(in.Metrics)
	for _, sig := range delta.Signals() {
		entry := ShowMeasurement{Signal: string(sig.ID)}
		if value, ok := measured[sig.ID].Value(); ok {
			level := &ShowLevel{Value: value}
			if counts, isRate := delta.CoverageCountsFor(in.Metrics, sig.ID); isRate {
				covered, total := counts.Covered, counts.Total
				level.Covered, level.Total = &covered, &total
			}
			entry.Measurement = level
		}
		view.Measurements = append(view.Measurements, entry)
	}
	return view
}

// RenderShow writes one snapshot in the requested format.
func RenderShow(w io.Writer, f Format, v ShowView) error {
	if f == FormatJSON {
		return renderJSON(w, v, "show")
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, line := range [][2]string{
		{"REPO", v.Repo},
		{"STATUS", v.Status},
		{"COLLECTED", orDash(v.CollectedAt)},
		{"COMMIT", orDash(v.GitSHA)},
		{"BRANCH", orDash(v.GitBranch)},
		{"TOOLCHAIN", orDash(v.Env)},
		{"DIRTY", fmt.Sprint(v.GitDirty)},
		{"DEGRADED", degradedText(v.Degraded)},
	} {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", line[0], line[1]); err != nil {
			return fmt.Errorf("report: rendering show: %w", err)
		}
	}
	if _, err := fmt.Fprintln(tw, "\nSIGNAL\tVALUE"); err != nil {
		return fmt.Errorf("report: rendering show: %w", err)
	}

	for _, m := range v.Measurements {
		// Read off the registry rather than off the catalog this payload just
		// rendered, because the unit is a delta.Unit there and a display string
		// here, and formatting needs the former.
		sig := delta.SignalByID(delta.SignalID(m.Signal))
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", sig.Label, showValueText(m, sig)); err != nil {
			return fmt.Errorf("report: rendering show: %w", err)
		}
	}
	if v.Error != "" {
		if _, err := fmt.Fprintf(tw, "\nERROR\t%s\n", v.Error); err != nil {
			return fmt.Errorf("report: rendering show: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("report: rendering show: %w", err)
	}
	return nil
}

// showValueText formats one level in its own unit, or says nothing measured it.
//
// The words are the ones history already uses, because they are the same two
// findings: a run that failed outright collected nothing, and a run that
// succeeded without producing this signal did not measure it.
func showValueText(m ShowMeasurement, sig delta.Signal) string {
	if m.Measurement == nil {
		return "not measured"
	}
	out := formatValue(m.Measurement.Value, sig.Unit)
	if m.Measurement.Covered != nil && m.Measurement.Total != nil {
		out += fmt.Sprintf(" of %d %s", *m.Measurement.Total, delta.CoverageCountNoun(sig.ID))
	}
	return out
}

// degradedText renders the three-state flag in words, since "false" and "null"
// side by side in a column say nothing about which is which.
func degradedText(v *bool) string {
	switch {
	case v == nil:
		return "not recorded"
	case *v:
		return "yes, these numbers were taken under protest"
	default:
		return "no"
	}
}

// orDash renders a nullable string for a column.
func orDash(v *string) string {
	if v == nil {
		return "-"
	}
	return *v
}
