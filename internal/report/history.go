package report

import (
	"fmt"
	"io"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// timeFormat is how every timestamp this package publishes is rendered. It was
// three inline copies of the same layout string before history added a fourth.
const timeFormat = "2006-01-02 15:04 UTC"

// HistoryView is one repo's measurements over time, for one signal.
//
// One signal rather than all of them, and that is a payload decision with a
// safety consequence. Six nullable groups on every point of a ninety-day series
// is mostly punctuation, and the JSON key would have to vary by signal to name
// them, which the field census cannot pin: a path that changes with the request
// is a path no hand-written table can classify. So the measurement always lands
// at the same key and the envelope says which signal it is.
type HistoryView struct {
	GeneratedAt string `json:"generated_at"`
	// Since is the lower bound the range resolved to, and SinceDays is what was
	// asked for. They are result and request, the same relation Scope.Selected
	// has to Scope.Repo, and both are on the wire for the same reason.
	Since     string  `json:"since"`
	SinceDays float64 `json:"since_days"`
	// Scope says which repo this covers. A history answer is narrowed by
	// construction, so Repo is never null here, and saying so anyway keeps one
	// vocabulary across every payload this tool emits.
	Scope ScopeView `json:"scope"`
	// Signal is the legend for every measurement below: which one it is, what
	// unit, and which direction is good news. On the envelope rather than on
	// each point, because it is the same for all of them.
	Signal SignalCatalogEntry `json:"signal"`
	// LastCollected is the newest snapshot of any status, in the range or out of
	// it, and null when this repo has never been collected at all.
	//
	// It is what tells an empty series that means "nobody has run collect" apart
	// from one that means "collection stopped three months ago". Those call for
	// opposite actions, and without this they are the same empty list.
	LastCollected *string `json:"last_collected"`
	// Points is allocated and empty, never nil. A nil marshals to null and a
	// consumer doing points.length crashes on exactly the case that is most
	// common for a repo nobody has collected yet.
	Points []PointView `json:"points"`
}

// PointView is one collection run.
type PointView struct {
	CollectedAt string `json:"collected_at"`
	Status      string `json:"status"`
	// GitSHA is here and not in the markdown, because it is what turns a cliff
	// in the series into a commit to go and read, and it is noise in a table a
	// person scans.
	GitSHA string `json:"git_sha"`
	Env    string `json:"env"`
	Error  string `json:"error,omitempty"`
	// Measurement is nil when this run measured nothing for the signal being
	// charted: a failed collection, or a repo that simply does not produce it.
	// That null is the finding rather than a gap to interpolate: drawing it as a
	// zero turns a broken collection into a cliff, and dropping the point
	// entirely renders it as though the question was never asked.
	Measurement *PointMeasurement `json:"measurement"`
}

// PointMeasurement is one level. It carries no delta, because in a series the
// comparison is the series: a delta here would be a second, redundant answer to
// the question the shape of the line already answers.
type PointMeasurement struct {
	Value float64 `json:"value"`
}

// BuildHistory turns a series of snapshots into the render-ready view.
//
// Measuring each point through delta.Measure rather than by reading metric keys
// here is what makes history obey the same presence rules as the report. A
// signal added to the registry shows up in both, and neither can decide on its
// own that a missing marker means zero.
func BuildHistory(
	generatedAt, since time.Time,
	sinceDays float64,
	repo string,
	configured int,
	sig delta.Signal,
	series []store.SnapshotMetrics,
	lastCollected *store.Snapshot,
) HistoryView {
	name := repo
	view := HistoryView{
		GeneratedAt: generatedAt.UTC().Format(timeFormat),
		Since:       since.UTC().Format(timeFormat),
		SinceDays:   sinceDays,
		Scope:       ScopeView{Repo: &name, Selected: 1, Configured: configured},
		Signal: SignalCatalogEntry{
			ID:        string(sig.ID),
			Label:     sig.Label,
			Unit:      unitName(sig.Unit),
			Direction: directionName(sig.Direction),
		},
		Points: make([]PointView, 0, len(series)),
	}

	if lastCollected != nil {
		at := lastCollected.CollectedAt.UTC().Format(timeFormat)
		view.LastCollected = &at
	}

	for _, point := range series {
		out := PointView{
			CollectedAt: point.Snapshot.CollectedAt.UTC().Format(timeFormat),
			Status:      string(point.Snapshot.Status),
			GitSHA:      point.Snapshot.GitSHA,
			Env:         point.Snapshot.Env,
			Error:       point.Snapshot.Error,
		}
		if value, measured := delta.Measure(point.Metrics)[sig.ID].Value(); measured {
			out.Measurement = &PointMeasurement{Value: value}
		}
		view.Points = append(view.Points, out)
	}
	return view
}

// Cells renders every point for the markdown table, formatted in the signal's
// own unit. It is a method for the same reason RepoView.Signals is: the
// template holds no pointer and knows no unit, and the words for an unmeasured
// point are written once.
func (h HistoryView) Cells() []PointCell {
	out := make([]PointCell, 0, len(h.Points))
	for _, p := range h.Points {
		cell := PointCell{
			CollectedAt: p.CollectedAt,
			Status:      p.Status,
			Error:       p.Error,
			Value:       "not measured",
		}
		if p.Status == string(store.StatusFailed) {
			cell.Value = "not collected"
		}
		if p.Measurement != nil {
			cell.Value = formatValue(p.Measurement.Value, h.unit())
		}
		out = append(out, cell)
	}
	return out
}

// PointCell is one rendered row of the history table.
type PointCell struct {
	CollectedAt string
	Status      string
	Error       string
	// Value never carries a digit unless something measured it.
	Value string
}

// unit maps the envelope's unit name back to the type the formatter wants. The
// envelope carries a string because that is what a consumer can read; the
// formatter wants the enum.
//
// It goes through unitByName rather than a switch of its own. This was a second,
// hand-written copy of that mapping, and it drifted the moment a fourth unit was
// added: its default swallowed days, so a median dependency age charted as
// 199.33405541417824 with no unit anywhere on the row.
func (h HistoryView) unit() delta.Unit { return unitByName(h.Signal.Unit) }

// Empty reports whether the range turned up nothing, so the template can say
// which kind of nothing it is.
func (h HistoryView) Empty() bool { return len(h.Points) == 0 }

// EverCollected reports whether this repo has ever been collected at all,
// whatever the range holds. An empty series with a collection behind it means
// collection stopped; an empty series with none means it never started.
func (h HistoryView) EverCollected() bool { return h.LastCollected != nil }

// LastCollectedAt reads through the nil for the template.
func (h HistoryView) LastCollectedAt() string {
	if h.LastCollected == nil {
		return ""
	}
	return *h.LastCollected
}

// RenderHistory writes a history view in the requested format.
//
// It takes an already-built view rather than the raw series, unlike
// RenderReport, and the asymmetry is deliberate: the report's section and scope
// are narrowing inputs that must be applied exactly once, so the report builds
// inside. History has no narrowing left to get wrong by the time it is built.
func RenderHistory(w io.Writer, f Format, h HistoryView) error {
	if f == FormatJSON {
		return renderJSON(w, h, "history")
	}
	if err := historyTmpl.Execute(w, h); err != nil {
		return fmt.Errorf("report: rendering history markdown: %w", err)
	}
	return nil
}
