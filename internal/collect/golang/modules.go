package golang

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// ModuleSummary is a parsed `go list -m -u -json all` stream.
//
// Every count here EXCLUDES the main module. A repo is not one of its own
// dependencies, and under a go.work workspace the stream carries one Main entry
// per workspace module, so all of them are excluded rather than just the first.
type ModuleSummary struct {
	// Total is the number of dependency modules in the build list.
	Total int
	// Direct counts dependencies the main module requires itself, which is to
	// say every module the toolchain did NOT mark Indirect.
	Direct int

	// UpdateAvailable counts dependencies with a newer version available.
	//
	// Zero has two possible meanings and this type cannot tell them apart. See
	// the comment on Errored.
	UpdateAvailable int
	// DirectUpdateAvailable is the subset of UpdateAvailable that is direct.
	//
	// This is the number anyone actually acts on: bumping an indirect module is
	// usually a consequence of bumping the direct one that pulls it in, so a
	// headline built on UpdateAvailable alone reports work that is not really
	// there.
	DirectUpdateAvailable int

	// Deprecated counts dependencies whose authors marked them deprecated.
	// Only populated when -u was passed, same as UpdateAvailable.
	Deprecated int
	// Retracted counts dependencies whose pinned version was retracted.
	// Only populated when -u (or -retracted) was passed.
	Retracted int

	// Errored counts dependencies the toolchain could not fully resolve.
	//
	// This is the count that keeps the other counts honest, and it exists
	// because of a measured behavior of the toolchain rather than a theory.
	//
	// Verified on Go 1.26.5 against this repo: with GOPROXY=off,
	// `go list -m -u -json all` exits 0, writes NOTHING to stderr, streams all
	// 27 modules, and sets Update, Deprecated, and Retracted on NONE of them.
	// So "every dependency is current" and "nothing was ever checked" are
	// byte-identical streams, and no parser can tell them apart. This one does
	// not try. Resolving whether the proxy was reachable is the caller's job,
	// and a caller that cannot answer it must refuse to record rather than
	// record three flattering zeroes.
	//
	// The PARTIAL case is the one a parser can catch, and it was measured too:
	// pointed at an unreachable proxy with -e, the toolchain exits 0, writes
	// nothing to stderr, and emits each unresolvable module with Error set and
	// Update absent. Without a per-module error count, a proxy that answers for
	// some modules and fails on others reads as FEWER updates available, which
	// is worse than a hard failure because it looks like good news. Any nonzero
	// Errored means every other count here is a floor, not a total.
	//
	// One caveat the caller has to know: `go list` only reports these per
	// module when invoked with -e. Without it, an unresolvable module is a hard
	// failure with a nonzero exit and an EMPTY stdout, so the caller sees the
	// problem as a failed command instead. Both invocations are safe; passing
	// neither -e nor checking the exit status is not.
	Errored int

	// WithoutTime counts dependencies that carried no publish timestamp, which
	// excludes them from Ages. They are counted rather than dropped because an
	// absent timestamp is not an age of zero, and a directory replacement
	// (`replace x => ./local`) genuinely has no version and no timestamp, so
	// this is a normal state and not a parse failure.
	WithoutTime int

	// pinned holds the publish timestamp of every dependency that had one.
	// Unexported because the aggregate is the product, and handing out a slice
	// invites a caller to average it.
	pinned []time.Time
}

// AgeStats describes how old a repo's pinned dependency versions are.
type AgeStats struct {
	// Median is the age of the typical pin. See Ages for why it is the median.
	Median time.Duration
	// Oldest is the age of the single most stale pin.
	Oldest time.Duration
	// Counted is how many dependencies had a usable timestamp and therefore
	// contributed. Counted plus ModuleSummary.WithoutTime always equals
	// ModuleSummary.Total.
	Counted int
}

// Ages reports how stale the pinned dependency versions are as of now.
//
// The second return is false when NOT ONE dependency carried a timestamp. That
// is not an age of zero, and the whole point of returning it separately is that
// a caller cannot accidentally record the zero value as a measurement.
//
// now is a parameter rather than a time.Now() call inside so this stays a pure
// function of its input: tests are deterministic, and a caller re-parsing an
// archived stream gets the age as of the run that produced it rather than as of
// today.
//
// The aggregate is the MEDIAN, not the mean. A healthy dependency set almost
// always contains a few very old but entirely fine pins, a stable utility that
// has not needed a release in years, and a mean is dragged bodily by them: the
// repo reads as alarming when nothing is wrong, and the number lurches whenever
// one ancient module enters or leaves the graph, so the trend line tracks noise
// instead of maintenance. The median moves only when the bulk of the set moves.
// Oldest rides along because the median alone would hide a single five-year-old
// pin in an otherwise fresh set, and the extreme is the one a person goes and
// fixes.
//
// Ages are not clamped at zero. A pin dated in the future, from clock skew or a
// caller passing a now that predates the stream, yields a negative age and says
// so, rather than being quietly rounded into "brand new".
func (s *ModuleSummary) Ages(now time.Time) (AgeStats, bool) {
	if len(s.pinned) == 0 {
		return AgeStats{}, false
	}

	ages := make([]time.Duration, len(s.pinned))
	for i, t := range s.pinned {
		ages[i] = now.Sub(t)
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })

	stats := AgeStats{
		Oldest:  ages[len(ages)-1],
		Counted: len(ages),
	}
	// Even counts take the mean of the two middle values, the ordinary
	// definition. Integer division truncates by at most a nanosecond.
	if mid := len(ages) / 2; len(ages)%2 == 1 {
		stats.Median = ages[mid]
	} else {
		stats.Median = (ages[mid-1] + ages[mid]) / 2
	}
	return stats, true
}

// module is one object in the `go list -m -json` stream. Only the fields we use.
//
// Update and Replace are the same shape as the module they hang off, which is
// what lets the replacement be read with the same helpers as the original.
type module struct {
	Path       string       `json:"Path"`
	Version    string       `json:"Version"`
	Time       *time.Time   `json:"Time"`
	Update     *module      `json:"Update"`
	Replace    *module      `json:"Replace"`
	Indirect   bool         `json:"Indirect"`
	Main       bool         `json:"Main"`
	Deprecated string       `json:"Deprecated"`
	Retracted  []string     `json:"Retracted"`
	Error      *moduleError `json:"Error"`
}

// moduleError is the toolchain's per-module failure, shaped {"Err":"..."}.
type moduleError struct {
	Err string `json:"Err"`
}

// ParseModules reads a `go list -m -json all` stream, with or without -u.
//
// The stream is a run of concatenated pretty-printed JSON objects, NOT a JSON
// array, so it is decoded one object at a time until io.EOF.
//
// Without -u the toolchain still reports Path, Version, Indirect and Time, so
// the module counts and the age aggregate work with no network at all. Update,
// Deprecated and Retracted are the -u-only fields, and they are also the ones
// that come back empty from an unreachable proxy without any error at all. See
// ModuleSummary.Errored.
func ParseModules(r io.Reader) (*ModuleSummary, error) {
	dec := json.NewDecoder(r)
	summary := &ModuleSummary{}
	var objects, mains int

	for {
		var m module
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A malformed or truncated object is fatal, deliberately unlike
			// ParseTestJSON, which tolerates a bad line and counts it. A test
			// stream is a sequence of independent events, so losing the last
			// one costs you that event. A module stream is one enumeration of a
			// single set: one object that failed to decode means the set is
			// incomplete, and EVERY count derived from it is then wrong low,
			// including the count of modules needing an update. Reporting a
			// smaller number of stale dependencies than the repo really has is
			// the exact failure this tool exists to refuse, so a partial stream
			// gets no summary at all.
			return nil, fmt.Errorf("golang: module stream ended badly after %d complete objects, so the module set is incomplete: %w", objects, err)
		}
		objects++

		if m.Main {
			// The repo itself, or a sibling workspace module. Neither is a
			// dependency of anything being measured.
			mains++
			continue
		}
		summary.count(m)
	}

	if objects == 0 {
		return nil, errors.New("golang: empty module stream: the go list command produced no objects at all")
	}
	if mains == 0 {
		// `go list -m all` always emits the main module first. Its absence
		// means this is not the stream we think it is, and silently treating
		// the repo as one of its own dependencies would inflate every count by
		// one and add the repo's own commit date to the age aggregate.
		return nil, errors.New("golang: no main module in the module stream: go list always emits one, so this stream is not a module list")
	}
	return summary, nil
}

// count folds one dependency module into the summary.
func (s *ModuleSummary) count(m module) {
	s.Total++
	if !m.Indirect {
		s.Direct++
	}

	// built is the record describing the code that actually gets compiled. When
	// a replace directive is in force, the replacement's version is what lands
	// in the binary, so its update and its publish date are the true ones. The
	// original's are describing a module this build never uses.
	built := &m
	if m.Replace != nil {
		built = m.Replace
	}

	if hasUpdate(built) {
		s.UpdateAvailable++
		// Indirect lives on the outer record only. A replacement is a target,
		// not a position in the module graph.
		if !m.Indirect {
			s.DirectUpdateAvailable++
		}
	}

	// Deprecation and retraction are read from EITHER record. Where the
	// toolchain reports them for a replaced module is unverified here, and the
	// union cannot double count because each module contributes at most one, so
	// it errs toward flagging rather than toward missing.
	if m.Deprecated != "" || (m.Replace != nil && m.Replace.Deprecated != "") {
		s.Deprecated++
	}
	if len(m.Retracted) > 0 || (m.Replace != nil && len(m.Replace.Retracted) > 0) {
		s.Retracted++
	}

	// The replacement half of this is not defensive: it was measured. A version
	// replace whose target could not be resolved puts the error on the
	// REPLACEMENT and leaves the outer record with no Error and no Update at
	// all, so counting only m.Error reports that module as resolved and current
	// when nothing about it was resolved.
	if m.Error != nil || (m.Replace != nil && m.Replace.Error != nil) {
		s.Errored++
	}

	// A nil Time is an absent timestamp. An explicitly zero one, which a
	// hand-written or re-serialized stream can carry, means the same thing and
	// must not be read as the year 1 either.
	if built.Time != nil && !built.Time.IsZero() {
		s.pinned = append(s.pinned, *built.Time)
		return
	}
	s.WithoutTime++
}

// hasUpdate reports whether this record names a newer version to move to.
//
// Guarded on the version string rather than on the pointer alone, because the
// count is meant to be upgrades a person could act on and an Update object
// carrying no version names no upgrade.
func hasUpdate(m *module) bool {
	return m.Update != nil && m.Update.Version != ""
}
