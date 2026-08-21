package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// runRepos answers "did the cron job actually run", so it lists every repo the
// config names, including the ones the database has never heard of.
func runRepos(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("repos", stderr)
	configPath := set.String("config", defaultConfigPath, "config file to read")
	dbPath := set.String("database", "", "database to read instead of the one the config names")
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

	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	st, err := openStore(databasePath(cfg, *dbPath), stderr)
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

	// The whole view is built before a single byte is written, so a store error
	// half way through cannot leave a truncated table or a truncated JSON
	// document behind. It used to write rows into a tabwriter as it went.
	inputs := make([]report.ReposInput, 0, len(cfg.Repos))
	for _, configured := range cfg.Repos {
		in, err := repoState(ctx, st, byName, configured.Name, stderr)
		if err != nil {
			return err
		}
		inputs = append(inputs, in)
	}

	if err := report.RenderRepos(stdout, renderFormat, report.BuildRepos(time.Now(), inputs)); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	return nil
}

// repoState looks up one repo's newest usable snapshot, falling back to its
// newest failed one.
//
// The two-step lookup is the same one reportInputs does and exists for the same
// reason: LatestSnapshot skips failed rows, so a repo whose every run failed
// comes back nil and would arrive looking exactly like a repo nobody has ever
// collected. Those call for opposite actions, go fix your build versus go run
// collect, so the failed row is fetched and shown with its last attempt.
func repoState(
	ctx context.Context, st *store.Store, byName map[string]store.Repo, name string, stderr io.Writer,
) (report.ReposInput, error) {
	in := report.ReposInput{Name: name}

	repo, ok := byName[name]
	if !ok {
		return in, nil
	}

	snap, err := st.LatestSnapshot(ctx, repo.ID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return in, err
	}
	if snap == nil {
		last, err := st.LatestSnapshotAny(ctx, repo.ID)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return in, err
		}
		in.Snapshot = last
		return in, nil
	}

	in.Snapshot = snap
	if in.Metrics, err = st.MetricsFor(ctx, snap.ID); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return in, err
	}
	return in, nil
}
