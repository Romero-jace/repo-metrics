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
