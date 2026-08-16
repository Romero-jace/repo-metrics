package delta

import "github.com/Romero-jace/repo-metrics/internal/store"

// Side is one snapshot's raw material, indexed once.
//
// Every signal's extractor is a map lookup rather than another scan over the
// metric slice, and the per-package coverage map that the culprit ranking needs
// is built here rather than twice. Deriving it in one place is also what keeps
// the repo's coverage rate from having a second implementation.
type Side struct {
	Snapshot *store.Snapshot

	// Packages and Coverage are coverage's counts, which are its authority.
	// Repo coverage is the sum of covered over the sum of total, never the mean
	// of the per-package rates, so the counts have to survive as counts.
	Packages map[string]Coverage
	Coverage Coverage

	// pkgSum totals every package-scoped row per key. repoVal holds the
	// repo-scoped row per key. present records which (key, scope) pairs existed
	// at all, which is the only thing that answers whether a signal was
	// measured.
	pkgSum  map[string]float64
	repoVal map[string]float64
	present map[markerKey]bool
}

// markerKey is a metric key paired with the scope it was found at, because the
// same key can legitimately appear at both levels and only one of them is a
// given signal's marker.
type markerKey struct {
	key   string
	scope MarkerScope
}

// newSide indexes one snapshot's metrics.
//
// A nil snapshot with no metrics is a valid Side: every lookup answers zero and
// every presence check answers false, which is exactly right for a repo that
// has never been collected.
func newSide(snap *store.Snapshot, metrics []store.Metric) Side {
	s := Side{
		Snapshot: snap,
		Packages: coverageByPackage(metrics),
		pkgSum:   make(map[string]float64),
		repoVal:  make(map[string]float64),
		present:  make(map[markerKey]bool),
	}
	s.Coverage = sumCoverage(s.Packages)

	for _, m := range metrics {
		if m.Scope == "" {
			s.repoVal[m.Key] = m.Value
			s.present[markerKey{m.Key, ScopeRepo}] = true
			continue
		}
		s.pkgSum[m.Key] += m.Value
		s.present[markerKey{m.Key, ScopePackage}] = true
	}
	return s
}

// has reports whether a marker row existed, which is the whole presence
// question. It deliberately ignores the value: a package covering none of its
// statements, or a suite with zero failures, both measured something.
func (s Side) has(key string, scope MarkerScope) bool {
	return s.present[markerKey{key, scope}]
}

// measure reads one signal off this side, answering unmeasured when the
// signal's marker is absent. This is the only place a Measurement is built from
// stored metrics, so the presence rule is applied once per snapshot rather than
// once per signal.
func (s Side) measure(sig Signal) Measurement {
	if !s.has(sig.Marker, sig.MarkerScope) {
		return Unmeasured()
	}
	return Measured(sig.Extract(s))
}

// Measure reads every registered signal off one snapshot's stored metrics.
//
// It is the entry point for anything that wants one snapshot's measurements
// without a comparison, which is what a history series is: a run of levels
// rather than a delta between two of them. Going through here rather than
// summing metric keys directly is what makes history obey the same presence
// rules as the report, and what makes a new signal appear in both at once.
func Measure(metrics []store.Metric) map[SignalID]Measurement {
	return measureAll(newSide(nil, metrics))
}

// CoverageCounts returns one snapshot's statement counts, and whether anything
// measured them.
//
// It exists so that nothing outside this package has to know which metric keys
// coverage is stored under, or to re-implement summing them. The counts are the
// authority, and the second return is what stops a caller dividing by a zero
// denominator and publishing the result as a percentage.
func CoverageCounts(metrics []store.Metric) (Coverage, bool) {
	side := newSide(nil, metrics)
	if !side.has(SignalByID(SigCoverage).Marker, ScopePackage) {
		return Coverage{}, false
	}
	return side.Coverage, true
}

// measureAll reads every registered signal off one side. It ranges the registry
// so a signal that exists but was never wired here is impossible.
func measureAll(s Side) map[SignalID]Measurement {
	out := make(map[SignalID]Measurement, len(signals))
	for _, sig := range signals {
		out[sig.ID] = s.measure(sig)
	}
	return out
}

// sumOver totals a key across every package-scoped row. It is the extractor for
// any signal that counts things spread over packages.
func sumOver(key string) func(Side) float64 {
	return func(s Side) float64 { return s.pkgSum[key] }
}

// repoValue reads a key's single repo-scoped row. It is the extractor for
// anything the collector counts about the repo as a whole.
func repoValue(key string) func(Side) float64 {
	return func(s Side) float64 { return s.repoVal[key] }
}
