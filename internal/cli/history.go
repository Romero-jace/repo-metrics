package cli

import (
	"context"
	"fmt"
	"io"
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

	repos, err := selectRepos(cfg.Repos, *only)
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
