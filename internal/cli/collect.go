package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

func runCollect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("collect", stderr)
	configPath := set.String("config", defaultConfigPath, "config file to read")
	only := set.String("repo", "", "collect just this one repo, by name")
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	repos, err := selectRepos(cfg.Repos, *only)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	st, err := openStore(cfg.Database, stderr)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	var failed []string
	for i, repo := range repos {
		// Cancellation is checked between repos rather than mid-repo so that
		// whatever is already collected gets written. Everything downstream of
		// here takes ctx too, so a signal during a long test run still lands.
		if err := ctx.Err(); err != nil {
			_, _ = fmt.Fprintf(stderr, "stopping early, collection was canceled after %d of %d repos\n", i, len(repos))
			return err
		}
		if err := collectOne(ctx, st, repo, stdout, stderr); err != nil {
			failed = append(failed, repo.Name)
		}
	}

	if len(failed) > 0 {
		_, _ = fmt.Fprintf(stderr, "%d of %d repos failed: %s\n",
			len(failed), len(repos), strings.Join(failed, ", "))
		return fmt.Errorf("collection failed for %d of %d repos", len(failed), len(repos))
	}
	return nil
}

// collectOne collects and stores a single repo. An error here means this repo
// failed, never that the run should stop: one unreachable repo must not cost
// you the other nine, which is the whole reason collect.Collect reports failure
// on the snapshot instead of returning an error.
func collectOne(ctx context.Context, st *store.Store, repo config.Repo, stdout, stderr io.Writer) error {
	res := collect.Collect(ctx, repo, collect.GoCollector{}, time.Now())

	for _, d := range res.Diagnostics {
		_, _ = fmt.Fprintf(stderr, "%s: %s: %s\n", repo.Name, d.Severity, d.Message)
	}

	// The snapshot carries a foreign key to repos, and pragma foreign_keys is
	// on, so the upsert has to come first or the insert fails on a zero id.
	repoID, err := st.UpsertRepo(ctx, repo.Name, repo.Path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
		printProgress(stdout, repo.Name, string(store.StatusFailed), "not stored")
		return err
	}
	res.Snapshot.RepoID = repoID

	if _, err := st.InsertSnapshot(ctx, res.Snapshot, res.Metrics); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
		printProgress(stdout, repo.Name, string(store.StatusFailed), "not stored")
		return err
	}

	covered, total := coverageTotals(res.Metrics)
	summary := formatCoverage(covered, total)
	if total > 0 {
		summary = fmt.Sprintf("%s of %d statements", summary, total)
	}
	printProgress(stdout, repo.Name, string(res.Snapshot.Status), summary)

	if res.Snapshot.Status == store.StatusFailed {
		return fmt.Errorf("%s: %s", repo.Name, res.Snapshot.Error)
	}
	return nil
}

// printProgress writes one line per repo as the run goes, rather than a table at
// the end, because the interesting case is watching which repo a long run is
// stuck on.
func printProgress(w io.Writer, name, status, summary string) {
	_, _ = fmt.Fprintf(w, "%-28s %-8s %s\n", name, status, summary)
}

// selectRepos narrows a config's repo list to one name, or hands back all of
// them when no name was given. Both collect and report go through it, so the
// two subcommands cannot drift on what --repo means or on what an unknown name
// does.
//
// An unknown name is an error rather than an empty selection. Quietly doing
// nothing for a typo is the silent wrong answer this tool exists to refuse, and
// it is worse for an agent than for a person: an empty report reads as an
// answer, so the agent concludes nothing regressed. The message lists what is
// configured, because "no repo named x" on its own does not tell you whether
// you misspelled the repo or pointed at the wrong config.
func selectRepos(repos []config.Repo, only string) ([]config.Repo, error) {
	if only == "" {
		return repos, nil
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		if r.Name == only {
			return []config.Repo{r}, nil
		}
		names = append(names, r.Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no repo named %q: the config has no repos at all", only)
	}
	return nil, fmt.Errorf("no repo named %q in the config, which has %s", only, strings.Join(names, ", "))
}
