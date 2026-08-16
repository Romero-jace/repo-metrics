package report_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// This tool publishes three payloads and only one of them was walked.
//
// degraded_test.go censuses the report's repo rows and the envelope they sit
// in. The repos listing and the history series had nothing, so a number added to
// either shipped with no test pressure at all, which is the same hole the
// envelope census was written to close and the same reason: nobody had ever
// pointed a walk at it. Both were reachable. Adding a float64 to RepoStateView
// or to PointView passed the build, the vet, the whole suite and the linter.
//
// The walk itself is reused from degraded_test.go rather than copied, so the
// rules (numbers live in nullable groups, groups are demonstrated nullable,
// unclassified keys fail closed) have one implementation across all four
// censuses.

// reposWireFields is the census for the repos listing.
//
// Hand-written for the reason repoWireFields is: deriving it from the struct
// would make it agree automatically, and automatic agreement is what must not
// happen. A new field lands on the wire, fails here, and the author has to come
// and say what it is.
//
// repos is kindContext and NOT kindRepoRows, which looks like the wrong call
// beside the envelope census and is not. That kind exists so the envelope's walk
// can stop at a list whose elements a SECOND table walks in full. There is no
// second table here, so stopping would leave every field of a repo row
// unclassified and unwalked, which is precisely the hole this test exists to
// close.
var reposWireFields = map[string]fieldKind{
	"generated_at": kindContext,
	"repos":        kindContext,

	"repos[].name":   kindContext,
	"repos[].status": kindContext,
	// Null rather than an empty string for a repo nobody has collected, so it is
	// context that happens to be nullable rather than a group: its null says no
	// run has happened, not that a measurement was withheld.
	"repos[].collected_at": kindContext,
	"repos[].has_snapshot": kindContext,

	// The one group in this payload, and it is null in three of the listing's
	// four states: never collected, every run failed, and the run that succeeded
	// while instrumenting nothing. That third one is why a flat pct here would be
	// the recurring bug: a header-only profile parses clean, stores no package
	// rows, and its percentage is zero for want of a denominator.
	"repos[].coverage":         kindGroup,
	"repos[].coverage.pct":     kindMeasurement,
	"repos[].coverage.covered": kindMeasurement,
	"repos[].coverage.total":   kindMeasurement,
}

// historyWireFields is the census for one repo's series.
//
// The envelope carries the request as well as the answer, so kindInput appears
// here and does not in the repos payload: since_days is what the caller asked
// for and the scope counts are what the config named. Nothing measured any of
// them and nothing can fail to.
var historyWireFields = map[string]fieldKind{
	"generated_at": kindContext,
	// since is the resolved lower bound and since_days is the requested one, so
	// they are answer and request. The first is a timestamp, which is not a
	// number on the wire; the second is.
	"since":      kindContext,
	"since_days": kindInput,

	"scope":            kindContext,
	"scope.repo":       kindContext,
	"scope.selected":   kindInput,
	"scope.configured": kindInput,

	// The legend for the measurement below. It is one entry rather than the
	// report's list because a history answer charts one signal.
	"signal":           kindContext,
	"signal.id":        kindContext,
	"signal.label":     kindContext,
	"signal.unit":      kindContext,
	"signal.direction": kindContext,

	// Null when this repo has never been collected at all, which is what tells
	// an empty series that means "collection never started" from one that means
	// "collection stopped". Not a measurement either way.
	"last_collected": kindContext,

	"points": kindContext,

	"points[].collected_at": kindContext,
	"points[].status":       kindContext,
	"points[].git_sha":      kindContext,
	"points[].env":          kindContext,
	// omitempty, so only a run that carried an error reaches this path at all.
	// The fixture below includes a failed point for that reason as well as for
	// the null side of the group.
	"points[].error": kindContext,

	// A point that measured nothing is the finding rather than a gap to
	// interpolate. Drawn as a zero it turns a broken collection into a cliff,
	// which is this project's recurring bug rendered as a chart.
	"points[].measurement":       kindGroup,
	"points[].measurement.value": kindMeasurement,
}

// reposFixture covers all four states the repos listing distinguishes, which is
// also what gives the coverage group its null side three times over and its
// filled side once. A group no fixture renders both ways is not demonstrated
// nullable, and the walk says so.
func reposFixture() []report.ReposInput {
	return []report.ReposInput{
		// Configured, and the database has never heard of it.
		{Name: repoNever},
		// Collected, and every run failed.
		{
			Name:     repoFailedOnly,
			Snapshot: snap(21, 2, "go1.26.5", store.StatusFailed, "test command exited 2"),
		},
		// The fourth state, and the one that is easy to miss: the run succeeded
		// and measured no coverage, because the profile carried only its header.
		{
			Name:     repoEmptyProfile,
			Snapshot: snap(61, 6, "go1.26.5", store.StatusOK, ""),
			Metrics:  testStream(0, testCount(pkgAlpha, 5)),
		},
		// The measured control. Without it the census would never see the group
		// filled, and the three nulls above would prove nothing.
		{
			Name:     repoHealthy,
			Snapshot: snap(81, 8, "go1.26.5", store.StatusOK, ""),
			Metrics:  metrics(cov(pkgAlpha, 80, 100), testStream(1, testCount(pkgAlpha, 9))),
		},
	}
}

// mustReposJSON renders the repos payload the way the repos subcommand does and
// decodes it as map[string]any.
//
// Through BuildRepos and RenderRepos rather than by marshaling a struct the test
// built, because the point of a census is to walk what actually reaches the
// wire. Decoding into the view types instead would round-trip a Go nil back into
// a Go nil and prove nothing about the bytes a consumer reads.
func mustReposJSON(t *testing.T, inputs []report.ReposInput) map[string]any {
	t.Helper()
	var b strings.Builder
	if err := report.RenderRepos(&b, report.FormatJSON, report.BuildRepos(fixedNow(), inputs)); err != nil {
		t.Fatalf("rendering the repos payload as json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("decoding the rendered repos json: %v\n%s", err, b.String())
	}
	return doc
}

// TestEveryReposFieldIsDeclared is the repos listing's field census.
//
// Same four rules as the report's: every key has to be classified, no number may
// sit outside a nullable group, every group has to be seen both null and filled,
// and the walk recurses so the rule cannot be dodged one level down.
func TestEveryReposFieldIsDeclared(t *testing.T) {
	census := newWireCensus(reposWireFields)
	census.walk("", mustReposJSON(t, reposFixture()), false)

	for _, problem := range census.problems {
		t.Error(problem)
	}

	for path := range reposWireFields {
		if !census.paths[path] {
			t.Errorf("reposWireFields classifies %q but no rendered row carries it, so the classification is rot. Either the field was removed or the fixture no longer reaches the state that emits it.", path)
		}
	}

	for path, kind := range reposWireFields {
		if kind != kindGroup {
			continue
		}
		if !census.nulled[path] {
			t.Errorf("%q is declared a group but no state renders it as null, so nothing here proves it is nullable. A repo that has never been collected, one whose run failed, and one whose profile instrumented nothing all have to reach this payload as an absent group rather than a pct of zero.", path)
		}
		if !census.filled[path] {
			t.Errorf("%q is declared a group but no fixture ever fills it in, so the measured case is untested and the null above proves nothing.", path)
		}
	}

	// Pin the set, not only the rules. The assertions above would still pass if a
	// future edit reclassified a number as context to quiet a failure.
	var measurements []string
	for path, kind := range reposWireFields {
		if kind == kindMeasurement {
			measurements = append(measurements, path)
		}
	}
	want := []string{"repos[].coverage.covered", "repos[].coverage.pct", "repos[].coverage.total"}
	if got := strings.Join(sortedNames(measurements), ","); got != strings.Join(want, ",") {
		t.Errorf("measurement paths: got %v, want %v. A number moved buckets, which is a decision worth making on purpose rather than to quiet a test.", sortedNames(measurements), want)
	}

	// The mirror of TestNoRepoFieldEscapesThroughKindInput, for the payload that
	// is nothing but repo rows. Every figure here is something a collection found
	// and everything a collection finds can fail to be found, so the envelope's
	// escape hatch has no business in this payload at all.
	for path, kind := range reposWireFields {
		if kind == kindInput {
			t.Errorf("%q is classified an input, but this payload carries only what a collection found. An input has no unmeasured state, and every figure here does.", path)
		}
	}
}

// historyFixture is one repo's series with both point states in it: a run that
// measured the charted signal, and a failed run that did not.
//
// The failed point is doing two jobs. It gives the measurement group its null
// side, and it is the only point that carries error text, which is omitempty and
// would otherwise never reach the walk.
func historyFixture() []store.SnapshotMetrics {
	return []store.SnapshotMetrics{
		{
			Snapshot: *snap(101, 10, "go1.26.5", store.StatusOK, ""),
			Metrics:  metrics(cov(pkgAlpha, 60, 100), testStream(0, testCount(pkgAlpha, 4))),
		},
		{
			Snapshot: *snap(102, 10, "go1.26.5", store.StatusFailed, "coverage profile was stale"),
		},
	}
}

// mustHistoryJSON renders a history view through the real path and decodes it.
func mustHistoryJSON(t *testing.T, v report.HistoryView) map[string]any {
	t.Helper()
	var b strings.Builder
	if err := report.RenderHistory(&b, report.FormatJSON, v); err != nil {
		t.Fatalf("rendering the history payload as json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("decoding the rendered history json: %v\n%s", err, b.String())
	}
	return doc
}

// TestEveryHistoryFieldIsDeclared is the history series' field census.
//
// It walks two renders. One is a repo with a series, which is where every point
// path comes from. The other is a repo the database has never heard of, whose
// last_collected is null and whose points list is empty, because that is the
// answer a history call gives most often and a census that never saw it would
// not have walked the shape a first-time user gets.
func TestEveryHistoryFieldIsDeclared(t *testing.T) {
	since := fixedNow().Add(-90 * 24 * time.Hour)
	last := snap(102, 10, "go1.26.5", store.StatusFailed, "coverage profile was stale")
	sig := delta.SignalByID(delta.SigCoverage)

	census := newWireCensus(historyWireFields)
	census.walk("", mustHistoryJSON(t, report.BuildHistory(
		fixedNow(), since, 90, repoHealthy, 4, sig, historyFixture(), last)), false)
	census.walk("", mustHistoryJSON(t, report.BuildHistory(
		fixedNow(), since, 90, repoNever, 4, sig, nil, nil)), false)

	for _, problem := range census.problems {
		t.Error(problem)
	}

	for path := range historyWireFields {
		if !census.paths[path] {
			t.Errorf("historyWireFields classifies %q but no render carries it, so the classification is rot. Either the field was removed or the fixture no longer reaches the state that emits it.", path)
		}
	}

	for path, kind := range historyWireFields {
		if kind != kindGroup {
			continue
		}
		if !census.nulled[path] {
			t.Errorf("%q is declared a group but no point renders it as null, so nothing here proves it is nullable. A run that measured nothing has to reach a chart as a gap rather than as a zero.", path)
		}
		if !census.filled[path] {
			t.Errorf("%q is declared a group but no point ever fills it in, so the measured case is untested and the null above proves nothing.", path)
		}
	}

	var measurements []string
	for path, kind := range historyWireFields {
		if kind == kindMeasurement {
			measurements = append(measurements, path)
		}
	}
	if got, want := strings.Join(sortedNames(measurements), ","), "points[].measurement.value"; got != want {
		t.Errorf("measurement paths: got %v, want [%v]. A number moved buckets, which is a decision worth making on purpose rather than to quiet a test.", sortedNames(measurements), want)
	}

	// The input set is pinned the way the report envelope's is, and for the same
	// reason: reclassifying a derived number as an input is the only
	// reclassification that lets a number out of a nullable group, so it is the
	// one way this guard gets quietly relaxed.
	var inputs []string
	for path, kind := range historyWireFields {
		if kind == kindInput {
			inputs = append(inputs, path)
		}
	}
	want := []string{"scope.configured", "scope.selected", "since_days"}
	if got := strings.Join(sortedNames(inputs), ","); got != strings.Join(want, ",") {
		t.Errorf("input paths: got %v, want %v. A number became an input, which means it left the only rule that keeps an unmeasured figure off the wire. That is a decision to make on purpose.", sortedNames(inputs), want)
	}
}
