package report

import (
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/delta"
)

// Every rendering switch over a unit has a default branch, and every default
// branch renders a bare number.
//
// That makes a missing case invisible: a signal declared in days whose unit
// nobody added a case for prints "412" in the value column and "days" nowhere,
// which is a measurement with its meaning stripped off. Nothing fails, the
// number looks plausible, and a reader takes a dependency age for a count.
//
// Distinctness is the check rather than exact strings, because it is the
// property that actually matters and it needs no updating when a rendering is
// tuned. A unit falling through to default renders identically to UnitCount, so
// the collision is the failure.
func TestEveryUnitRendersDistinctly(t *testing.T) {
	// One value, chosen so no two units could coincide by arithmetic accident:
	// large enough for the duration renderer to reach minutes, and not a round
	// number of anything.
	const v = 90061

	for _, render := range []struct {
		name string
		fn   func(float64, delta.Unit) string
	}{
		{"formatValue", formatValue},
		{"formatDelta", formatDelta},
	} {
		seen := make(map[string]delta.Unit, len(delta.Units()))
		for _, unit := range delta.Units() {
			got := render.fn(v, unit)
			if other, taken := seen[got]; taken {
				t.Errorf("%s renders unit %d and unit %d identically as %q, so one of them has no case and is falling through to the default branch. A measurement is reaching the reader with its meaning stripped off.",
					render.name, other, unit, got)
			}
			seen[got] = unit
		}
	}

	// The catalog name is the JSON half of the same problem: a consumer reading
	// "count" for a duration has no way to know better, and the catalog is the
	// only place the wire says what a value means.
	names := make(map[string]delta.Unit, len(delta.Units()))
	for _, unit := range delta.Units() {
		name := unitName(unit)
		if name == "" {
			t.Errorf("unit %d has no name in the signal catalog", unit)
		}
		if other, taken := names[name]; taken {
			t.Errorf("unitName reports %q for both unit %d and unit %d, so the catalog cannot tell a consumer them apart",
				name, other, unit)
		}
		names[name] = unit
	}
}

// History carries the unit as a string on the envelope, because that is what a
// consumer can read, and then has to turn it back into an enum to format its
// own numbers. That round trip has to be lossless for every unit.
//
// This is a real regression rather than a hypothetical: history had its own
// hand-written copy of the mapping, and when a fourth unit was added its default
// branch swallowed it. A median dependency age charted as 199.33405541417824,
// with the word "days" nowhere on the row.
func TestUnitNamesRoundTrip(t *testing.T) {
	for _, want := range delta.Units() {
		if got := unitByName(unitName(want)); got != want {
			t.Errorf("unit %d renders as %q, which reads back as unit %d. A number in this unit reaches the reader formatted as something else.",
				want, unitName(want), got)
		}
	}
}

// The stronger version of the same guard, at the surface it actually broke: a
// history table's cell has to be formatted in its own signal's unit, for every
// signal in the registry.
//
// It goes through BuildHistory rather than calling the formatter, so it covers
// the whole path a rendered row takes, including the string round trip that
// broke.
func TestHistoryCellsCarryTheirSignalsUnit(t *testing.T) {
	const value = 90061

	for _, sig := range delta.Signals() {
		view := HistoryView{
			Signal: SignalCatalogEntry{ID: string(sig.ID), Unit: unitName(sig.Unit)},
			Points: []PointView{{
				CollectedAt: "2026-01-01 00:00 UTC",
				Status:      "ok",
				Measurement: &PointMeasurement{Value: value},
			}},
		}
		cells := view.Cells()
		if len(cells) != 1 {
			t.Fatalf("%s: got %d cells, want 1", sig.ID, len(cells))
		}
		if want := formatValue(value, sig.Unit); cells[0].Value != want {
			t.Errorf("%s: history renders %q, but the report renders the same number as %q. The two surfaces disagree about what this measurement means.",
				sig.ID, cells[0].Value, want)
		}
	}
}

// Every registered signal has to declare a unit that the renderers know about.
// The test above proves the units are distinct; this one proves the registry
// only uses those.
func TestEverySignalUsesADeclaredUnit(t *testing.T) {
	declared := make(map[delta.Unit]bool, len(delta.Units()))
	for _, u := range delta.Units() {
		declared[u] = true
	}
	for _, sig := range delta.Signals() {
		if !declared[sig.Unit] {
			t.Errorf("signal %q declares unit %d, which is not in delta.Units(), so nothing checks how it renders",
				sig.ID, sig.Unit)
		}
	}
}
