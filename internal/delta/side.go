package delta

import (
	"sort"
	"strings"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// Side is one snapshot's raw material, indexed once.
//
// Every signal's extractor is a map lookup rather than another scan over the
// metric slice, and the per-package coverage map that the culprit ranking needs
// is built here rather than twice. Deriving it in one place is also what keeps
// the repo's coverage rate from having a second implementation.
type Side struct {
	Snapshot *store.Snapshot

	// Packages and Coverage are statement coverage's counts, which are its
	// authority. Repo coverage is the sum of covered over the sum of total, never
	// the mean of the per-package rates, so the counts have to survive as counts.
	Packages map[string]Coverage
	Coverage Coverage

	// Files and CoverageLines are the same two things for line coverage, kept
	// deliberately apart rather than folded into the pair above.
	//
	// They are a separate pair because statements and lines are separate units:
	// merging the maps would produce a repo rate over two denominators that
	// counts different things, which is the arithmetic the split metric keys
	// exist to make impossible. The scope is a source file path here where it is
	// an import path above, so even the map keys are drawn from different
	// vocabularies and a collision between them would be meaningless rather than
	// merely wrong.
	//
	// A repo can legitimately fill one, the other, or both — a Go service
	// emitting an LCOV tracefile alongside its profile is not a misconfiguration.
	Files         map[string]Coverage
	CoverageLines Coverage

	// pkgSum totals every scoped row per key. repoVal holds the repo-scoped row
	// per key. present records which (key, scope) pairs existed at all, which is
	// the only thing that answers whether a signal was measured.
	pkgSum  map[string]float64
	repoVal map[string]float64
	present map[markerKey]bool

	// scopeSets records which scopes contributed to each key, so a sum can say
	// what it summed over. Only read for signals that asked, via
	// Signal.ScopeSetMustMatch, because for most of them the set changing is a
	// real finding rather than a change of apparatus.
	scopeSets map[string]map[string]bool
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
		Snapshot:  snap,
		Packages:  coverageByScope(metrics, collect.KeyCoveredStmts, collect.KeyTotalStmts),
		Files:     coverageByScope(metrics, collect.KeyCoveredLines, collect.KeyTotalLines),
		pkgSum:    make(map[string]float64),
		repoVal:   make(map[string]float64),
		present:   make(map[markerKey]bool),
		scopeSets: make(map[string]map[string]bool),
	}
	s.Coverage = sumCoverage(s.Packages)
	s.CoverageLines = sumCoverage(s.Files)

	for _, m := range metrics {
		if m.Scope == "" {
			s.repoVal[m.Key] = m.Value
			s.present[markerKey{m.Key, ScopeRepo}] = true
			continue
		}
		s.pkgSum[m.Key] += m.Value
		s.present[markerKey{m.Key, ScopeDetail}] = true
		if s.scopeSets[m.Key] == nil {
			s.scopeSets[m.Key] = make(map[string]bool)
		}
		s.scopeSets[m.Key][m.Scope] = true
	}
	return s
}

// scopeKey fingerprints which scopes contributed rows for a key, sorted so the
// same set always renders the same string. A NUL joiner rather than a comma,
// because a scope is operator-supplied text and a comma inside one would make
// two different sets fingerprint alike.
func (s Side) scopeKey(key string) string {
	set := s.scopeSets[key]
	if len(set) == 0 {
		return ""
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "\x00")
}

// has reports whether a marker row existed, which is the whole presence
// question. It deliberately ignores the value: a package covering none of its
// statements, or a suite with zero failures, both measured something.
func (s Side) has(key string, scope MarkerScope) bool {
	return s.present[markerKey{key, scope}]
}

// firstMarker returns the first of a signal's markers this side carries.
//
// First rather than any, because the answer feeds the scope fingerprint and that
// has to be a function of the stored rows alone. Ranging a map or picking the
// "best" match would make two identical snapshots fingerprint differently
// between runs, which would refuse deltas at random.
func (s Side) firstMarker(sig Signal) (Marker, bool) {
	for _, m := range sig.Markers {
		if s.has(m.Key, m.Scope) {
			return m, true
		}
	}
	return Marker{}, false
}

// Measured reports whether this side carries proof that anything looked at a
// signal. It is the presence half of measure, for callers that want the question
// without building a Measurement.
func (s Side) Measured(sig Signal) bool {
	_, ok := s.firstMarker(sig)
	return ok
}

// measure reads one signal off this side, answering unmeasured when the
// signal's marker is absent. This is the only place a Measurement is built from
// stored metrics, so the presence rule is applied once per snapshot rather than
// once per signal.
func (s Side) measure(sig Signal) Measurement {
	marker, ok := s.firstMarker(sig)
	if !ok {
		return Unmeasured()
	}
	// Whether this side's number is a total or a floor. The value is published
	// either way; what it loses is the right to be subtracted from another one.
	incomplete := sig.PartialWhen != "" && s.repoVal[sig.PartialWhen] > 0

	if sig.ScopeSetMustMatch {
		// The value carries a fingerprint of what it summed over, so Compare can
		// refuse two sums that covered different sets. See Signal.ScopeSetMustMatch
		// for why this is per signal rather than automatic.
		// Fingerprinted over the marker that actually matched, not over the
		// signal's first. Two sides that matched different markers were measured
		// by different apparatus, so their fingerprints differ and Compare refuses
		// the delta rather than subtracting one toolchain's total from another's.
		return measuredOver(sig.Extract(s), s.scopeKey(marker.Key)).partially(incomplete)
	}
	return Measured(sig.Extract(s)).partially(incomplete)
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
	if !side.Measured(SignalByID(SigCoverage)) {
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

// ratioOver reads two scoped sums as a percentage.
//
// The division happens here, over the summed counts, rather than anywhere the
// counts could be averaged. A repo's rate is sum(covered)/sum(total), which is
// not the mean of its files' rates, and keeping the arithmetic in one place is
// what stops a second implementation getting that wrong.
//
// A zero denominator answers zero, and nothing publishes it: the signal's marker
// is the denominator key, so a snapshot with no instrumented files has no marker
// row and reads as unmeasured before this is ever called.
func ratioOver(covered, total string) func(Side) float64 {
	return func(s Side) float64 {
		t := s.pkgSum[total]
		if t <= 0 {
			return 0
		}
		return s.pkgSum[covered] / t * 100
	}
}

// repoValue reads a key's single repo-scoped row. It is the extractor for
// anything the collector counts about the repo as a whole.
func repoValue(key string) func(Side) float64 {
	return func(s Side) float64 { return s.repoVal[key] }
}
