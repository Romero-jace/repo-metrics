package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/Romero-jace/repo-metrics/internal/store"
)

// runRepos answers "did the cron job actually run", so it lists every repo the
// config names, including the ones the database has never heard of.
func runRepos(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("repos", stderr)
	configPath := set.String("config", defaultConfigPath, "config file to read")
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	st, err := openStore(cfg.Database, stderr)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	known, err := st.Repos(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	byName := make(map[string]store.Repo, len(known))
	for _, r := range known {
		byName[r.Name] = r
	}

	// Buffered until Flush, which is fine here: this is a table, not progress.
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "REPO\tLAST COLLECTED\tSTATUS\tCOVERAGE")

	for _, configured := range cfg.Repos {
		// LatestSnapshot skips failed rows because they carry no numbers, so a
		// repo whose only runs failed lands here looking like one that has
		// never run. The wording says "no usable snapshot" rather than "never"
		// so that the table is not quietly lying about which one it is.
		collected, status, coverage := "no usable snapshot", "-", "-"

		if repo, ok := byName[configured.Name]; ok {
			snap, err := st.LatestSnapshot(ctx, repo.ID)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "%s: %v\n", configured.Name, err)
				return err
			}
			if snap != nil {
				collected = snap.CollectedAt.UTC().Format(timeFormat)
				status = string(snap.Status)

				metrics, err := st.MetricsFor(ctx, snap.ID)
				if err != nil {
					_, _ = fmt.Fprintf(stderr, "%s: %v\n", configured.Name, err)
					return err
				}
				coverage = formatCoverage(coverageTotals(metrics))
			}
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", configured.Name, collected, status, coverage)
	}

	if err := w.Flush(); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	return nil
}
