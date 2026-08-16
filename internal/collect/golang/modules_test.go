package golang_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
)

// refNow is the clock every age assertion is measured against. Ages are a
// function of a caller-supplied now precisely so tests never depend on today.
var refNow = time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC)

// mainModule is the object `go list -m all` always emits first. Most fixtures
// only need it to be present.
const mainModule = `{"Path":"example.com/repo","Main":true,"Dir":"/repo"}`

func parseModules(t *testing.T, body string) *golang.ModuleSummary {
	t.Helper()
	s, err := golang.ParseModules(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseModules: %v", err)
	}
	return s
}

// day is a readable unit for pinned-version ages.
const day = 24 * time.Hour

// TestAgainstARealModuleStream runs the parser over output captured verbatim
// from `GOWORK=off go list -m -json all` in this repo on Go 1.26.5.
//
// Hand-built fixtures prove the counting rules do what the code intends. Only a
// genuine stream proves the intent matches what the toolchain actually emits:
// that objects arrive pretty-printed and concatenated rather than as an array,
// that the main module carries no Version and no Time, that a direct dependency
// is distinguished from an indirect one purely by the ABSENCE of an Indirect
// field, and that Time is present on every dependency with no network involved.
func TestAgainstARealModuleStream(t *testing.T) {
	s := parseModules(t, realModuleStream)

	// 27 objects in the stream, one of which is the main module.
	if s.Total != 26 {
		t.Errorf("Total: got %d, want 26 dependencies", s.Total)
	}
	// go.mod requires exactly one module without an // indirect marker,
	// modernc.org/sqlite. Everything else arrived through it.
	if s.Direct != 1 {
		t.Errorf("Direct: got %d, want 1", s.Direct)
	}

	// The capture was taken without -u, which is the offline mode. Those three
	// counts being zero here is not evidence that the dependencies are current,
	// it is evidence that nothing checked. That ambiguity is the reason
	// ModuleSummary.Errored exists.
	if s.UpdateAvailable != 0 || s.DirectUpdateAvailable != 0 {
		t.Errorf("updates: got %d (%d direct), want 0 without -u",
			s.UpdateAvailable, s.DirectUpdateAvailable)
	}
	if s.Deprecated != 0 || s.Retracted != 0 || s.Errored != 0 {
		t.Errorf("got deprecated=%d retracted=%d errored=%d, want 0/0/0",
			s.Deprecated, s.Retracted, s.Errored)
	}

	ages, ok := s.Ages(refNow)
	if !ok {
		t.Fatal("Ages reported no usable timestamps, but every dependency in the capture has one")
	}
	// The invariant that catches a whole class of bookkeeping slips: every
	// dependency either contributed a timestamp or was counted as lacking one.
	if ages.Counted+s.WithoutTime != s.Total {
		t.Errorf("counted %d + without-time %d != total %d",
			ages.Counted, s.WithoutTime, s.Total)
	}
	if s.WithoutTime != 0 {
		t.Errorf("WithoutTime: got %d, want 0 (Time is present offline)", s.WithoutTime)
	}

	// modernc.org/token v1.1.0, the earliest Time in the capture.
	wantOldest := refNow.Sub(time.Date(2022, time.November, 13, 14, 28, 3, 0, time.UTC))
	if ages.Oldest != wantOldest {
		t.Errorf("Oldest: got %s, want %s", ages.Oldest, wantOldest)
	}

	// Recompute the median from the raw text, independently of the parser's own
	// bookkeeping, so this checks that the right modules were included rather
	// than only that the arithmetic ran.
	if want := medianAgeOf(t, timestampsIn(t, realModuleStream)); ages.Median != want {
		t.Errorf("Median: got %s, want %s (independent scan of the raw stream)", ages.Median, want)
	}
	t.Logf("total=%d direct=%d median-age=%s oldest-age=%s", s.Total, s.Direct, ages.Median, ages.Oldest)
}

// A repo is not one of its own dependencies. Counting the main module would
// inflate Total and Direct by one on every repo, and under a go.work workspace
// by one per workspace module.
func TestMainModuleIsExcludedFromEveryCount(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/dep","Version":"v1.0.0","Time":"2025-08-15T00:00:00Z"}
`)
	if s.Total != 1 {
		t.Errorf("Total: got %d, want 1", s.Total)
	}
	// The main module carries no Indirect field, so a failure to exclude it
	// shows up here as 2.
	if s.Direct != 1 {
		t.Errorf("Direct: got %d, want 1", s.Direct)
	}
	if ages, ok := s.Ages(refNow); !ok || ages.Counted != 1 {
		t.Errorf("Ages: got counted=%d ok=%v, want 1/true", ages.Counted, ok)
	}
}

// A workspace stream carries one Main entry per workspace module. A sibling
// module in the same workspace is not a dependency either.
func TestEveryMainModuleIsExcludedNotJustTheFirst(t *testing.T) {
	s := parseModules(t, `
{"Path":"example.com/repo","Main":true,"Dir":"/repo"}
{"Path":"example.com/sibling","Main":true,"Dir":"/sibling"}
{"Path":"example.com/dep","Version":"v1.0.0"}
`)
	if s.Total != 1 {
		t.Errorf("Total: got %d, want 1 (both workspace modules excluded)", s.Total)
	}
}

// `go list -m all` always emits the main module. Its absence means the stream
// is not what we think it is, and carrying on would treat the repo as one of
// its own dependencies.
func TestMissingMainModuleIsRejected(t *testing.T) {
	_, err := golang.ParseModules(strings.NewReader(`
{"Path":"example.com/dep","Version":"v1.0.0"}
{"Path":"example.com/other","Version":"v2.0.0"}
`))
	if err == nil {
		t.Fatal("a stream with no main module was accepted")
	}
	if !strings.Contains(err.Error(), "main module") {
		t.Errorf("error does not name the missing main module: %v", err)
	}
}

// Nothing at all means the command produced no output, which is a collection
// failure and not a repo with zero dependencies.
func TestEmptyStreamIsRejected(t *testing.T) {
	for _, body := range []string{"", "\n\n", "   \n\t\n"} {
		_, err := golang.ParseModules(strings.NewReader(body))
		if err == nil {
			t.Errorf("ParseModules(%q) accepted an empty stream", body)
			continue
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("ParseModules(%q): error does not say the stream was empty: %v", body, err)
		}
	}
}

// Deliberately unlike ParseTestJSON, which tolerates a truncated final line. A
// module stream is one enumeration of a single set, so a half-written object
// means the set is incomplete and every count taken from it is wrong LOW,
// including the number of dependencies needing an update. Under-reporting stale
// dependencies is the exact silent wrong answer this tool exists to refuse, so
// there is no partial summary to hand back.
func TestTruncatedFinalObjectIsRejected(t *testing.T) {
	s, err := golang.ParseModules(strings.NewReader(mainModule + `
{"Path":"example.com/a","Version":"v1.0.0"}
{"Path":"example.com/b","Vers`))
	if err == nil {
		t.Fatalf("a truncated stream was accepted and summarized as %+v", s)
	}
	if s != nil {
		t.Errorf("a summary was returned alongside the error: %+v", s)
	}
	// The message has to say the set is short, or a reader will treat it as a
	// cosmetic parse complaint.
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error does not say the module set is incomplete: %v", err)
	}
}

// Trailing non-JSON noise is the same failure: the enumeration cannot be
// trusted, so nothing is reported.
func TestTrailingGarbageIsRejected(t *testing.T) {
	if _, err := golang.ParseModules(strings.NewReader(mainModule + `
{"Path":"example.com/a","Version":"v1.0.0"}
go: some warning that should never be on stdout
`)); err == nil {
		t.Fatal("a stream with trailing non-JSON was accepted")
	}
}

// Direct updates are broken out because they are the ones anyone acts on.
// Bumping an indirect module is usually a consequence of bumping the direct
// module that pulls it in, so a headline built on the total reports work that
// is not really there.
func TestUpdatesAreCountedAndSplitByDirectness(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/direct-stale","Version":"v1.0.0","Update":{"Path":"example.com/direct-stale","Version":"v1.1.0"}}
{"Path":"example.com/direct-fresh","Version":"v1.0.0"}
{"Path":"example.com/indirect-stale","Version":"v1.0.0","Indirect":true,"Update":{"Path":"example.com/indirect-stale","Version":"v2.0.0"}}
{"Path":"example.com/indirect-fresh","Version":"v1.0.0","Indirect":true}
`)
	if s.Total != 4 || s.Direct != 2 {
		t.Errorf("got total=%d direct=%d, want 4/2", s.Total, s.Direct)
	}
	if s.UpdateAvailable != 2 {
		t.Errorf("UpdateAvailable: got %d, want 2", s.UpdateAvailable)
	}
	if s.DirectUpdateAvailable != 1 {
		t.Errorf("DirectUpdateAvailable: got %d, want 1", s.DirectUpdateAvailable)
	}
}

// The count is meant to be upgrades a person could act on, and an Update object
// carrying no version names no upgrade.
func TestUpdateWithoutAVersionIsNotAnUpgrade(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/a","Version":"v1.0.0","Update":{}}
{"Path":"example.com/b","Version":"v1.0.0","Update":{"Path":"example.com/b"}}
`)
	if s.UpdateAvailable != 0 {
		t.Errorf("UpdateAvailable: got %d, want 0", s.UpdateAvailable)
	}
}

// A replaced module is built from the replacement, so the replacement's Update
// is the real one and the original's describes code this build never compiles.
//
// The fixture is arranged so a total alone cannot tell the two readings apart:
// either way exactly one module has an update. Only the direct count moves, and
// reading the outer record instead of the replacement scores it as zero.
func TestReplaceRedirectsUpdateToTheReplacement(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/direct","Version":"v1.0.0","Replace":{"Path":"example.com/fork","Version":"v2.0.0","Update":{"Path":"example.com/fork","Version":"v2.1.0"}}}
{"Path":"example.com/indirect","Version":"v1.0.0","Indirect":true,"Update":{"Path":"example.com/indirect","Version":"v1.1.0"},"Replace":{"Path":"example.com/fork2","Version":"v3.0.0"}}
`)
	if s.UpdateAvailable != 1 {
		t.Errorf("UpdateAvailable: got %d, want 1", s.UpdateAvailable)
	}
	if s.DirectUpdateAvailable != 1 {
		t.Errorf("DirectUpdateAvailable: got %d, want 1; 0 means the original's Update was read instead of the replacement's",
			s.DirectUpdateAvailable)
	}
}

// Same reasoning for age: the replacement's publish date is the date of the
// code that actually gets compiled.
func TestReplaceRedirectsAgeToTheReplacement(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/a","Version":"v1.0.0","Time":"2016-08-15T00:00:00Z","Replace":{"Path":"example.com/fork","Version":"v2.0.0","Time":"2026-08-05T00:00:00Z"}}
`)
	ages, ok := s.Ages(refNow)
	if !ok {
		t.Fatal("Ages reported no usable timestamps")
	}
	if ages.Oldest != 10*day {
		t.Errorf("Oldest: got %s, want %s; a decade means the replaced module's own Time was used", ages.Oldest, 10*day)
	}
}

// A directory replacement has no version and therefore no publish date. That is
// a normal state, not a parse failure, and not an age of zero.
func TestDirectoryReplacementHasNoAge(t *testing.T) {
	// Captured verbatim from `go list -m -e -json all` on a module with
	// `replace github.com/dustin/go-humanize => ./local`.
	s := parseModules(t, mainModule+`
{
	"Path": "github.com/dustin/go-humanize",
	"Version": "v1.0.1",
	"Replace": {
		"Path": "./local",
		"Dir": "/probe/local",
		"GoMod": "/probe/local/go.mod",
		"GoVersion": "1.16"
	},
	"Dir": "/probe/local",
	"GoMod": "/probe/local/go.mod",
	"GoVersion": "1.16"
}
`)
	if s.WithoutTime != 1 {
		t.Errorf("WithoutTime: got %d, want 1", s.WithoutTime)
	}
	if ages, ok := s.Ages(refNow); ok {
		t.Errorf("Ages returned %+v for a module with no timestamp anywhere", ages)
	}
}

func TestDeprecatedAndRetractedAreCounted(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/gone","Version":"v1.0.0","Deprecated":"use example.com/new instead"}
{"Path":"example.com/pulled","Version":"v1.0.0","Retracted":["contains a data race"]}
{"Path":"example.com/both","Version":"v1.0.0","Deprecated":"unmaintained","Retracted":["broken build"]}
{"Path":"example.com/fine","Version":"v1.0.0"}
`)
	if s.Deprecated != 2 {
		t.Errorf("Deprecated: got %d, want 2", s.Deprecated)
	}
	if s.Retracted != 2 {
		t.Errorf("Retracted: got %d, want 2", s.Retracted)
	}
}

// A deprecation or retraction reported on the replacement still describes the
// code being built, so it counts. Each module contributes at most one either
// way, so reading both records cannot double count.
func TestDeprecatedAndRetractedOnTheReplacementAreCounted(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/a","Version":"v1.0.0","Replace":{"Path":"example.com/fork","Version":"v2.0.0","Deprecated":"the fork is abandoned"}}
{"Path":"example.com/b","Version":"v1.0.0","Deprecated":"original is deprecated","Replace":{"Path":"example.com/fork2","Version":"v2.0.0","Deprecated":"so is the fork"}}
{"Path":"example.com/c","Version":"v1.0.0","Replace":{"Path":"example.com/fork3","Version":"v2.0.0","Retracted":["pulled"]}}
`)
	if s.Deprecated != 2 {
		t.Errorf("Deprecated: got %d, want 2 (one on the replacement, one on both records)", s.Deprecated)
	}
	if s.Retracted != 1 {
		t.Errorf("Retracted: got %d, want 1", s.Retracted)
	}
}

// The count that keeps the others honest. Both objects here were captured
// verbatim from Go 1.26.5, and each is a case where the toolchain exits 0 and
// writes nothing to stderr while quietly failing to resolve a module.
//
// The second one is why the replacement is checked at all: a version replace
// whose target could not be resolved puts Error on the REPLACEMENT and leaves
// the outer record with no Error and no Update, so a parser reading only the
// outer record scores that module as resolved and perfectly current.
func TestErroredCountsFailuresOnBothTheModuleAndItsReplacement(t *testing.T) {
	s := parseModules(t, mainModule+`
{
	"Path": "github.com/dustin/go-humanize",
	"Version": "v1.0.1",
	"Time": "2023-01-10T06:44:38Z",
	"Error": {
		"Err": "loading module retractions for github.com/dustin/go-humanize@v1.0.1: Get \"http://127.0.0.1:1/github.com/dustin/go-humanize/@v/list\": dial tcp 127.0.0.1:1: connect: connection refused"
	}
}
{
	"Path": "github.com/google/uuid",
	"Version": "v1.6.0",
	"Replace": {
		"Path": "github.com/google/uuid",
		"Version": "v1.5.0",
		"Error": {
			"Err": "module lookup disabled by GOPROXY=off"
		}
	}
}
{"Path":"example.com/fine","Version":"v1.0.0","Time":"2025-08-15T00:00:00Z"}
`)
	if s.Errored != 2 {
		t.Errorf("Errored: got %d, want 2; 1 means the failure on the replacement was missed", s.Errored)
	}
	// Neither module reported an update, which is exactly how a partial proxy
	// outage reads as good news unless Errored is consulted.
	if s.UpdateAvailable != 0 {
		t.Errorf("UpdateAvailable: got %d, want 0", s.UpdateAvailable)
	}
	// An error and a missing timestamp are independent: the first module here
	// failed its update lookup but still carries the publish date of its pin.
	if s.WithoutTime != 1 {
		t.Errorf("WithoutTime: got %d, want 1", s.WithoutTime)
	}
	if ages, ok := s.Ages(refNow); !ok || ages.Counted != 2 {
		t.Errorf("Ages: got counted=%d ok=%v, want 2/true", ages.Counted, ok)
	}
}

// The heart of this codebase, in one assertion. A dependency with no timestamp
// is unmeasured, not brand new. Folding it in as an age of zero would drag the
// median toward "everything is fresh", which is a measurement of nothing
// published as a flattering number.
func TestMissingTimestampIsExcludedFromTheAgeAggregate(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/timed","Version":"v1.0.0","Time":"2025-08-15T00:00:00Z"}
{"Path":"example.com/untimed","Version":"v1.0.0"}
`)
	if s.Total != 2 {
		t.Fatalf("Total: got %d, want 2", s.Total)
	}
	if s.WithoutTime != 1 {
		t.Errorf("WithoutTime: got %d, want 1", s.WithoutTime)
	}

	ages, ok := s.Ages(refNow)
	if !ok {
		t.Fatal("Ages reported no usable timestamps, but one dependency has one")
	}
	if ages.Counted != 1 {
		t.Errorf("Counted: got %d, want 1", ages.Counted)
	}
	// 2025-08-15 to 2026-08-15 is 365 days. Treating the untimed module as age
	// zero would halve this to 182.5 days.
	if ages.Median != 365*day {
		t.Errorf("Median: got %s, want %s; %s means the untimed module was folded in as age zero",
			ages.Median, 365*day, 182*day+12*time.Hour)
	}
}

// An explicit zero timestamp, which a re-serialized or hand-written stream can
// carry, means the same thing an absent one does. Reading it literally would
// report a dependency pinned two thousand years ago.
func TestExplicitlyZeroTimestampIsNotAnAge(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/a","Version":"v1.0.0","Time":"0001-01-01T00:00:00Z"}
`)
	if s.WithoutTime != 1 {
		t.Errorf("WithoutTime: got %d, want 1", s.WithoutTime)
	}
	if ages, ok := s.Ages(refNow); ok {
		t.Errorf("Ages accepted a zero timestamp and reported %+v", ages)
	}
}

// Refusing to answer is the whole product. A caller that got a zero here would
// record "dependencies are 0 seconds old" for a repo nothing was measured on.
func TestAgesRefusesWhenNothingCarriesATimestamp(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/a","Version":"v1.0.0"}
{"Path":"example.com/b","Version":"v1.0.0"}
`)
	ages, ok := s.Ages(refNow)
	if ok {
		t.Fatalf("Ages claimed a result with no timestamps at all: %+v", ages)
	}
	if ages != (golang.AgeStats{}) {
		t.Errorf("Ages returned %+v alongside ok=false", ages)
	}
	if s.WithoutTime != 2 {
		t.Errorf("WithoutTime: got %d, want 2", s.WithoutTime)
	}
}

// Median arithmetic, pinned for both parities so the even case cannot silently
// become "the upper middle value".
func TestMedianAndOldestAges(t *testing.T) {
	tests := []struct {
		name       string
		agesInDays []int
		wantMedian time.Duration
		wantOldest time.Duration
	}{
		{"single", []int{30}, 30 * day, 30 * day},
		{"odd", []int{10, 30, 20}, 20 * day, 30 * day},
		{"even", []int{40, 10, 30, 20}, 25 * day, 40 * day},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString(mainModule)
			for i, d := range tt.agesInDays {
				stamp := refNow.Add(-time.Duration(d) * day).Format(time.RFC3339)
				b.WriteString("\n{\"Path\":\"example.com/m")
				b.WriteString(string(rune('a' + i)))
				b.WriteString("\",\"Version\":\"v1.0.0\",\"Time\":\"")
				b.WriteString(stamp)
				b.WriteString("\"}")
			}

			ages, ok := parseModules(t, b.String()).Ages(refNow)
			if !ok {
				t.Fatal("Ages reported no usable timestamps")
			}
			if ages.Median != tt.wantMedian {
				t.Errorf("Median: got %s, want %s", ages.Median, tt.wantMedian)
			}
			if ages.Oldest != tt.wantOldest {
				t.Errorf("Oldest: got %s, want %s", ages.Oldest, tt.wantOldest)
			}
			if ages.Counted != len(tt.agesInDays) {
				t.Errorf("Counted: got %d, want %d", ages.Counted, len(tt.agesInDays))
			}
		})
	}
}

// A pin dated after now, from clock skew or from replaying an archived stream
// against an earlier reference time, is reported as negative rather than
// rounded up into "brand new".
func TestFutureTimestampsAreNotClampedToZero(t *testing.T) {
	s := parseModules(t, mainModule+`
{"Path":"example.com/a","Version":"v1.0.0","Time":"2026-08-25T00:00:00Z"}
`)
	ages, ok := s.Ages(refNow)
	if !ok {
		t.Fatal("Ages reported no usable timestamps")
	}
	if ages.Median != -10*day {
		t.Errorf("Median: got %s, want %s", ages.Median, -10*day)
	}
}

// timestampsIn pulls every Time value out of a raw stream by text, so the real
// stream test can check the parser against something that shares none of its
// logic.
func timestampsIn(t *testing.T, stream string) []time.Time {
	t.Helper()
	var out []time.Time
	for _, line := range strings.Split(stream, "\n") {
		_, rest, ok := strings.Cut(strings.TrimSpace(line), `"Time": "`)
		if !ok {
			continue
		}
		value, _, ok := strings.Cut(rest, `"`)
		if !ok {
			t.Fatalf("unterminated Time value in %q", line)
		}
		stamp, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("parsing %q: %v", value, err)
		}
		out = append(out, stamp)
	}
	if len(out) == 0 {
		t.Fatal("no Time values found in the stream, so this check would prove nothing")
	}
	return out
}

// medianAgeOf is the expected-value side of the real stream test.
func medianAgeOf(t *testing.T, stamps []time.Time) time.Duration {
	t.Helper()
	ages := make([]time.Duration, len(stamps))
	for i, s := range stamps {
		ages[i] = refNow.Sub(s)
	}
	for i := 1; i < len(ages); i++ {
		for j := i; j > 0 && ages[j] < ages[j-1]; j-- {
			ages[j], ages[j-1] = ages[j-1], ages[j]
		}
	}
	mid := len(ages) / 2
	if len(ages)%2 == 1 {
		return ages[mid]
	}
	return (ages[mid-1] + ages[mid]) / 2
}

// realModuleStream is `GOWORK=off go list -m -json all` run in this repo on Go
// 1.26.5, captured byte for byte. Absolute paths and checksums are left as they
// came out, because the point of the fixture is that it was not tidied up.
const realModuleStream = `{
	"Path": "github.com/Romero-jace/repo-metrics",
	"Main": true,
	"Dir": "/Users/jace/Dev/repo-metrics",
	"GoMod": "/Users/jace/Dev/repo-metrics/go.mod",
	"GoVersion": "1.26.5"
}
{
	"Path": "github.com/dustin/go-humanize",
	"Version": "v1.0.1",
	"Time": "2023-01-10T06:44:38Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/dustin/go-humanize@v1.0.1",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/dustin/go-humanize/@v/v1.0.1.mod",
	"GoVersion": "1.16",
	"Sum": "h1:GzkhY7T5VNhEkwH0PVJgjz+fX1rhBrR7pRT3mDkpeCY=",
	"GoModSum": "h1:Mu1zIs6XwVuF/gI1OepvI0qD18qycQx+mFykh5fBlto="
}
{
	"Path": "github.com/goccy/go-yaml",
	"Version": "v1.19.2",
	"Time": "2026-01-08T01:12:13Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/goccy/go-yaml@v1.19.2",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/goccy/go-yaml/@v/v1.19.2.mod",
	"GoVersion": "1.21.0",
	"Sum": "h1:PmFC1S6h8ljIz6gMRBopkjP1TVT7xuwrButHID66PoM=",
	"GoModSum": "h1:XBurs7gK8ATbW4ZPGKgcbrY1Br56PdM69F7LkFRi1kA="
}
{
	"Path": "github.com/google/pprof",
	"Version": "v0.0.0-20260802141513-ef3492d7dac3",
	"Time": "2026-08-02T14:15:13Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/google/pprof@v0.0.0-20260802141513-ef3492d7dac3",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/google/pprof/@v/v0.0.0-20260802141513-ef3492d7dac3.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:LMLX+LgTNWpfvCBdFebv6EsYotImrt/Ppc5cXIriCSo=",
	"GoModSum": "h1:jl5iWTm0/hd5PjEYEOuwAJ57L/CibdZfrqZ5XA5GrCk="
}
{
	"Path": "github.com/google/uuid",
	"Version": "v1.6.0",
	"Time": "2024-01-23T18:54:04Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/google/uuid@v1.6.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/google/uuid/@v/v1.6.0.mod",
	"Sum": "h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=",
	"GoModSum": "h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo="
}
{
	"Path": "github.com/hashicorp/golang-lru/v2",
	"Version": "v2.0.7",
	"Time": "2023-09-21T18:26:40Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/hashicorp/golang-lru/v2@v2.0.7",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/hashicorp/golang-lru/v2/@v/v2.0.7.mod",
	"GoVersion": "1.18",
	"Sum": "h1:a+bsQ5rvGLjzHuww6tVxozPZFVghXaHOwFs4luLUK2k=",
	"GoModSum": "h1:QeFd9opnmA6QUJc5vARoKUSoFhyfM2/ZepoAG6RGpeM="
}
{
	"Path": "github.com/mattn/go-isatty",
	"Version": "v0.0.24",
	"Time": "2026-07-23T16:45:22Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/mattn/go-isatty@v0.0.24",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/mattn/go-isatty/@v/v0.0.24.mod",
	"GoVersion": "1.20",
	"Sum": "h1:tGZZoVgT/KiqK1c8ocVLeDS8BSWMRd47J3Lbz7vsReI=",
	"GoModSum": "h1:nMCL3Zebbrt45jsMDgnfIwz6ydEQApk5oEI3HqDio6A="
}
{
	"Path": "github.com/ncruces/go-strftime",
	"Version": "v1.0.0",
	"Time": "2025-10-08T11:45:18Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/ncruces/go-strftime@v1.0.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/ncruces/go-strftime/@v/v1.0.0.mod",
	"GoVersion": "1.17",
	"Sum": "h1:HMFp8mLCTPp341M/ZnA4qaf7ZlsbTc+miZjCLOFAw7w=",
	"GoModSum": "h1:Fwc5htZGVVkseilnfgOVb9mKy6w1naJmn9CehxcKcls="
}
{
	"Path": "github.com/remyoudompheng/bigfft",
	"Version": "v0.0.0-20230129092748-24d4a6f8daec",
	"Time": "2023-01-29T09:27:48Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/github.com/remyoudompheng/bigfft/@v/v0.0.0-20230129092748-24d4a6f8daec.mod",
	"GoVersion": "1.12",
	"Sum": "h1:W09IVJc94icq4NjY3clb7Lk8O1qJ8BdBEF8z0ibU0rE=",
	"GoModSum": "h1:qqbHyh8v60DhA7CoWK5oRCqLrMHRGoxYCSS9EjAz6Eo="
}
{
	"Path": "golang.org/x/mod",
	"Version": "v0.37.0",
	"Time": "2026-06-08T15:10:58Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/golang.org/x/mod@v0.37.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/golang.org/x/mod/@v/v0.37.0.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:vF1DjpVEshcIqoEaauuHebaLk1O1forxjxBaVn884JQ=",
	"GoModSum": "h1:m8S8VeM9r4dzDwjrKO0a1sZP3YjeMamRRlD+fmR2Q/0="
}
{
	"Path": "golang.org/x/sync",
	"Version": "v0.21.0",
	"Time": "2026-06-04T16:57:53Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/golang.org/x/sync@v0.21.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/golang.org/x/sync/@v/v0.21.0.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:HLII4xRRTtCRkxYp4HNFF0Js/Og6q2i++KXbg0gHCwM=",
	"GoModSum": "h1:9xrNwdLfx4jkKbNva9FpL6vEN7evnE43NNNJQ2LF3+0="
}
{
	"Path": "golang.org/x/sys",
	"Version": "v0.47.0",
	"Time": "2026-06-30T17:07:31Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/golang.org/x/sys@v0.47.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/golang.org/x/sys/@v/v0.47.0.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:o7XGOvZQCADBQQ4Y7VNq2dRWQR7JmOUW8Kxx4ZsNgWs=",
	"GoModSum": "h1:4GL1E5IUh+htKOUEOaiffhrAeqysfVGipDYzABqnCmw="
}
{
	"Path": "golang.org/x/tools",
	"Version": "v0.47.0",
	"Time": "2026-06-25T17:02:32Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/golang.org/x/tools@v0.47.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/golang.org/x/tools/@v/v0.47.0.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:7Kn5x/d1svx/PzryTsqeoZN4TZwqeH5pGWjefhLi/1Q=",
	"GoModSum": "h1:dFHnyTvFWY212G+h7ZY4Vsp/K3U4/7W9TyVaAul8uCA="
}
{
	"Path": "modernc.org/cc/v4",
	"Version": "v4.29.1",
	"Time": "2026-07-08T11:45:37Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/cc/v4@v4.29.1",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/cc/v4/@v/v4.29.1.mod",
	"GoVersion": "1.25",
	"Sum": "h1:MKgdCV3WykTSPqpVrnxdEDS0HEd2FHpKZDzxzU5LyeI=",
	"GoModSum": "h1:OnovgIhbbMXMu1aISnJ0wvVD1KnW+cAUJkIrAWh+kVI="
}
{
	"Path": "modernc.org/ccgo/v4",
	"Version": "v4.34.6",
	"Time": "2026-06-26T14:54:35Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/ccgo/v4@v4.34.6",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/ccgo/v4/@v/v4.34.6.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:sBgfIwyN0TQ9C5hwIeuqyeAKyMWnbvj2fvpF4L11uzU=",
	"GoModSum": "h1:SZ8YcN9NG7XVsQYdm6jYBvi8PQP1qi+kqB6OhjqI3Fk="
}
{
	"Path": "modernc.org/fileutil",
	"Version": "v1.4.0",
	"Time": "2026-02-18T10:24:55Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/fileutil@v1.4.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/fileutil/@v/v1.4.0.mod",
	"GoVersion": "1.24",
	"Sum": "h1:j6ZzNTftVS054gi281TyLjHPp6CPHr2KCxEXjEbD6SM=",
	"GoModSum": "h1:EqdKFDxiByqxLk8ozOxObDSfcVOv/54xDs/DUHdvCUU="
}
{
	"Path": "modernc.org/gc/v2",
	"Version": "v2.6.5",
	"Time": "2025-03-12T16:49:23Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/gc/v2@v2.6.5",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/gc/v2/@v/v2.6.5.mod",
	"GoVersion": "1.21",
	"Sum": "h1:nyqdV8q46KvTpZlsw66kWqwXRHdjIlJOhG6kxiV/9xI=",
	"GoModSum": "h1:YgIahr1ypgfe7chRuJi2gD7DBQiKSLMPgBQe9oIiito="
}
{
	"Path": "modernc.org/gc/v3",
	"Version": "v3.1.4",
	"Time": "2026-05-18T09:50:34Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/gc/v3@v3.1.4",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/gc/v3/@v/v3.1.4.mod",
	"GoVersion": "1.23.0",
	"Sum": "h1:2g65LGVSmFQrXeITAw97x7hCRvZFcyE1uDP+7Vng7JI=",
	"GoModSum": "h1:HFK/6AGESC7Ex+EZJhJ2Gni6cTaYpSMmU/cT9RmlfYY="
}
{
	"Path": "modernc.org/goabi0",
	"Version": "v0.2.0",
	"Time": "2025-07-02T15:46:44Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/goabi0@v0.2.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/goabi0/@v/v0.2.0.mod",
	"GoVersion": "1.23.0",
	"Sum": "h1:HvEowk7LxcPd0eq6mVOAEMai46V+i7Jrj13t4AzuNks=",
	"GoModSum": "h1:CEFRnnJhKvWT1c1JTI3Avm+tgOWbkOu5oPA8eH8LnMI="
}
{
	"Path": "modernc.org/libc",
	"Version": "v1.74.4",
	"Time": "2026-07-27T16:37:42Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/libc@v1.74.4",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/libc/@v/v1.74.4.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:fX1Omw4o2/1C2iRkkIsrQTasJQldLhRmuPreXLoWs9k=",
	"GoModSum": "h1:eeQAS9W3sZeKYMFubydxJpII9ybHWshk+7or7bLG9co="
}
{
	"Path": "modernc.org/mathutil",
	"Version": "v1.7.1",
	"Time": "2024-12-26T12:13:25Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/mathutil@v1.7.1",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/mathutil/@v/v1.7.1.mod",
	"GoVersion": "1.21",
	"Sum": "h1:GCZVGXdaN8gTqB1Mf/usp1Y/hSqgI2vAGGP4jZMCxOU=",
	"GoModSum": "h1:4p5IwJITfppl0G4sUEDtCr4DthTaT47/N3aT6MhfgJg="
}
{
	"Path": "modernc.org/memory",
	"Version": "v1.11.0",
	"Time": "2025-05-17T20:55:10Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/memory@v1.11.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/memory/@v/v1.11.0.mod",
	"GoVersion": "1.23.0",
	"Sum": "h1:o4QC8aMQzmcwCK3t3Ux/ZHmwFPzE6hf2Y5LbkRs+hbI=",
	"GoModSum": "h1:/JP4VbVC+K5sU2wZi9bHoq2MAkCnrt2r98UGeSK7Mjw="
}
{
	"Path": "modernc.org/opt",
	"Version": "v0.2.0",
	"Time": "2026-03-24T16:14:02Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/opt@v0.2.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/opt/@v/v0.2.0.mod",
	"GoVersion": "1.21",
	"Sum": "h1:tGyef5ApycA7FSEOMraay9SaTk5zmbx7Tu+cJs4QKZg=",
	"GoModSum": "h1:03fq9lsNfvkYSfxrfUhZCWPk1lm4cq4N+Bh//bEtgns="
}
{
	"Path": "modernc.org/sortutil",
	"Version": "v1.2.1",
	"Time": "2024-12-28T00:09:37Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/sortutil@v1.2.1",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/sortutil/@v/v1.2.1.mod",
	"GoVersion": "1.21",
	"Sum": "h1:+xyoGf15mM3NMlPDnFqrteY07klSFxLElE2PVuWIJ7w=",
	"GoModSum": "h1:7ZI3a3REbai7gzCLcotuw9AC4VZVpYMjDzETGsSMqJE="
}
{
	"Path": "modernc.org/sqlite",
	"Version": "v1.56.0",
	"Time": "2026-08-03T14:31:47Z",
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/sqlite@v1.56.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/sqlite/@v/v1.56.0.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:/D8e2RfFqoy/Zc6PuC76U28zFwmI/sYx1Kjm4yEn9e0=",
	"GoModSum": "h1:yCJ2cmAaIkHQ25oXWrF8H4O1lIfPYPR26yCEDj2P3pQ="
}
{
	"Path": "modernc.org/strutil",
	"Version": "v1.2.1",
	"Time": "2024-12-27T20:23:31Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/strutil@v1.2.1",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/strutil/@v/v1.2.1.mod",
	"GoVersion": "1.21",
	"Sum": "h1:UneZBkQA+DX2Rp35KcM69cSsNES9ly8mQWD71HKlOA0=",
	"GoModSum": "h1:EHkiggD70koQxjVdSBM3JKM7k6L0FbGE5eymy9i3B9A="
}
{
	"Path": "modernc.org/token",
	"Version": "v1.1.0",
	"Time": "2022-11-13T14:28:03Z",
	"Indirect": true,
	"Dir": "/Users/jace/go/pkg/mod/modernc.org/token@v1.1.0",
	"GoMod": "/Users/jace/go/pkg/mod/cache/download/modernc.org/token/@v/v1.1.0.mod",
	"Sum": "h1:Xl7Ap9dKaEs5kLoOQeQmPWevfnk/DM5qcLcYlA8ys6Y=",
	"GoModSum": "h1:UGzOrNV1mAFSEB63lOFHIpNRUVMvYTc6yu1SMY/XTDM="
}
`
