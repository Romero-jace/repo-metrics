package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// defaultLookback is how far back history reaches when nobody says.
//
// Deliberately not the config's reporting window. That window is the offset to a
// baseline for a two-point comparison, and seven days of history is not a trend.
const defaultLookback = 90 * 24 * time.Hour

// runHistory answers what one repo's measurement has done over time.
//
// The report subcommand can only ever compare two snapshots. Everything else
// ever collected sits in the database unread, which is what this is for.
func runHistory(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("history", stderr)
	configPath := set.String("config", defaultConfigPath, "config file to read")
	dbPath := set.String("database", "", "database to read instead of the one the config names")
	only := set.String("repo", "", "which repo to chart, required")
	signalFlag := set.String("signal", string(delta.SigCoverage),
		"which measurement to chart, one of: "+strings.Join(delta.SignalNames(), ", "))
	sinceFlag := set.String("since", "", "how far back to look, like 90d (default 90d)")
	format := set.String("format", string(report.FormatMarkdown),
		"which format to render: "+report.FormatChoice())
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	// Config first, so that a missing config reports the same way here as it
	// does for every other subcommand, rather than being pre-empted by the
	// required-flag check below.
	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	// Required rather than defaulting to the whole fleet. A history answer is
	// narrowed by construction: there is no sensible "coverage over six months"
	// for nine repos at once, and picking one silently would answer a question
	// nobody asked.
	if *only == "" {
		err := fmt.Errorf("history needs --repo NAME, one of: %s", strings.Join(repoNames(cfg.Repos), ", "))
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	repos, err := selectRepos(cfg.Repos, oneName(*only))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	sig, err := delta.ParseSignal(*signalFlag)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	renderFormat, err := report.ParseFormat(*format)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	lookback := defaultLookback
	if *sinceFlag != "" {
		if lookback, err = parseWindow(*sinceFlag); err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return err
		}
	}

	st, err := openStore(databasePath(cfg, *dbPath), stderr)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Measured back from now rather than from the newest snapshot, which is the
	// opposite of what the report does. The report reaches back from the head so
	// a repo nobody has collected lately still gets a baseline. Here an empty
	// tail IS the finding: a repo that stopped being collected has to show a
	// series that stops.
	now := time.Now()
	from := now.Add(-lookback)

	series, last, err := historySeries(ctx, st, repos[0].Name, from, now, stderr)
	if err != nil {
		return err
	}

	// Which coverage signal to chart is decided here rather than at the flag,
	// because the answer is in the series and not in the arguments.
	sig = chartedSignal(sig, flagWasGiven(set, "signal"), series, stderr)

	view := report.BuildHistory(now, from, lookback.Hours()/24,
		repos[0].Name, len(cfg.Repos), sig, series, last)

	if err := report.RenderHistory(stdout, renderFormat, view); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	return nil
}

// historySeries loads one repo's points, plus its newest snapshot of any status.
//
// The second value is what tells an empty series that means "nobody has ever run
// collect" apart from one that means "collection stopped before this window".
// Those call for opposite actions and without it they render identically.
func historySeries(
	ctx context.Context, st *store.Store, name string, from, to time.Time, stderr io.Writer,
) ([]store.SnapshotMetrics, *store.Snapshot, error) {
	known, err := st.Repos(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return nil, nil, err
	}

	for _, repo := range known {
		if repo.Name != name {
			continue
		}
		series, err := st.SnapshotSeries(ctx, repo.ID, from, to)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return nil, nil, err
		}
		last, err := st.LatestSnapshotAny(ctx, repo.ID)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return nil, nil, err
		}
		return series, last, nil
	}

	// Configured but the database has never heard of it. That is not an error:
	// it is the answer, and the empty series plus a nil last-collected is how
	// the report says so.
	return nil, nil, nil
}

// flagWasGiven reports whether a flag was actually passed, as opposed to
// carrying its default.
//
// flag.FlagSet.Visit walks only the flags that were set, which is the whole
// difference between "the default" and "the default, typed out". Nothing else in
// this package needs to tell those apart; charting does, because one of them may
// be overridden and the other must not be.
func flagWasGiven(set *flag.FlagSet, name string) bool {
	given := false
	set.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// measuredCoverageSignals reports which coverage units this series recorded, in
// the order the registry lists them.
//
// Any point rather than the newest, because the question is what this repo
// records rather than what its last run managed. A series ending in a failed
// collection still answers it.
func measuredCoverageSignals(series []store.SnapshotMetrics) []delta.SignalID {
	seen := make(map[delta.SignalID]bool, len(delta.CoverageSignals()))
	for _, point := range series {
		measured := delta.Measure(point.Metrics)
		for _, id := range delta.CoverageSignals() {
			if _, ok := measured[id].Value(); ok {
				seen[id] = true
			}
		}
	}
	out := make([]delta.SignalID, 0, len(seen))
	for _, id := range delta.CoverageSignals() {
		if seen[id] {
			out = append(out, id)
		}
	}
	return out
}

// chartedSignal picks the coverage unit to chart, and says so when that differs
// from what was asked for.
//
// history has always defaulted to statement coverage, which is the Go unit. A
// repo measured through an LCOV tracefile therefore answered with a run of null
// measurements on snapshots that had collected perfectly well, and the operator
// had to already know to ask for coverage_lines. Reported from a mixed fleet,
// where it was read as a collection that had not worked: the tool was right and
// unreadable, which for an agent is the same thing as being wrong.
//
// An explicitly named signal is never overridden. Answering a different question
// than the one asked is the failure this whole tool argues against, so an
// explicit --signal coverage on a repo that records lines still charts the empty
// series, and says on stderr what the repo does record.
//
// Both messages go to stderr. --format json is pure JSON on stdout and nothing
// else, and a hint that broke that would be a worse bug than the one it explains.
func chartedSignal(
	want delta.Signal, given bool, series []store.SnapshotMetrics, stderr io.Writer,
) delta.Signal {
	// Only coverage is ambiguous this way. A repo that records no tests is not
	// being invited to chart its lint findings instead.
	if !slices.Contains(delta.CoverageSignals(), want.ID) {
		return want
	}
	measured := measuredCoverageSignals(series)
	// Nothing recorded either unit, so there is no better answer to offer and an
	// empty series is itself the finding.
	if len(measured) == 0 || slices.Contains(measured, want.ID) {
		return want
	}

	other := delta.SignalByID(measured[0])
	if given {
		_, _ = fmt.Fprintf(stderr,
			"%s is not recorded for this repo, so every point below is unmeasured. It records %s\n",
			want.ID, other.ID)
		return want
	}
	_, _ = fmt.Fprintf(stderr,
		"charting %s rather than %s, which this repo has never recorded. Pass --signal to choose\n",
		other.ID, want.ID)
	return other
}
