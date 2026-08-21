package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// runShow prints everything one repo's newest snapshot recorded.
//
// It answers the question the other three could not. repos says whether a repo
// was collected and what its coverage is, the report says what moved and needs
// two snapshots to say anything, and history charts one signal over time. None
// of them will tell you every signal on the latest run with the counts behind
// the rates, so that was being rebuilt out of Python one-liners against the
// database file.
func runShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("show", stderr)
	configPath := set.String("config", defaultConfigPath, "config file to read")
	dbPath := set.String("database", "", "database to read instead of the one the config names")
	only := set.String("repo", "", "which repo to show, required")
	format := set.String("format", string(report.FormatMarkdown),
		"which format to render: "+report.FormatChoice())
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	renderFormat, err := report.ParseFormat(*format)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	// Config first, so a missing config reports the same way here as for every
	// other subcommand rather than being pre-empted by the required-flag check.
	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	// Required, like history's. Showing every repo's every signal at once is the
	// report, and picking one silently would answer a question nobody asked.
	if *only == "" {
		err := fmt.Errorf("show needs --repo NAME, one of: %s", strings.Join(repoNames(cfg.Repos), ", "))
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	repos, err := selectRepos(cfg.Repos, oneName(*only))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	st, err := openStore(databasePath(cfg, *dbPath), stderr)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	in, err := showInput(ctx, st, repos[0].Name, stderr)
	if err != nil {
		return err
	}

	if err := report.RenderShow(stdout, renderFormat, report.BuildShow(time.Now(), in)); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	return nil
}

// showInput loads one repo's newest snapshot and its metrics.
//
// The same two-step every other read path uses: the newest usable snapshot, and
// on a nil the newest of any status, because "every run failed" and "never
// collected" are opposite instructions to the reader and would otherwise render
// identically. A failed snapshot has no metrics, which is what makes every
// measurement below it null rather than zero.
func showInput(
	ctx context.Context, st *store.Store, name string, stderr io.Writer,
) (report.ShowInput, error) {
	in := report.ShowInput{Name: name}

	known, err := st.Repos(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return in, err
	}
	for _, repo := range known {
		if repo.Name != name {
			continue
		}
		snap, err := st.LatestSnapshot(ctx, repo.ID)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return in, err
		}
		if snap == nil {
			if snap, err = st.LatestSnapshotAny(ctx, repo.ID); err != nil {
				_, _ = fmt.Fprintf(stderr, "%v\n", err)
				return in, err
			}
			in.Snapshot = snap
			return in, nil
		}
		in.Snapshot = snap
		if in.Metrics, err = st.MetricsFor(ctx, snap.ID); err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return in, err
		}
		return in, nil
	}

	// Configured and the database has never heard of it. Not an error: it is the
	// answer, and the empty snapshot is how the view says so.
	return in, nil
}
