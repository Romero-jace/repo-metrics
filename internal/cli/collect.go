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
		// Announced before the work, because collectOne's line only lands after
		// that repo's signals have all run and been stored. A cold three-repo
		// run was 78 seconds of silence, and one signal may take ten minutes, so
		// on nine repos there was nothing to tell working from hung, and nothing
		// saying which repo it was on.
		//
		// Unconditional: there is no terminal detection in this codebase and
		// this must not be what introduces it, so a cron log gets the same lines
		// a terminal does. The leading word is not the repo name, so this line
		// cannot be mistaken for the completion line's row in the table below.
		printStarting(stdout, repo.Name, i+1, len(repos))
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
	res := collect.Collect(ctx, repo, time.Now())

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

	printProgress(stdout, repo.Name, string(res.Snapshot.Status), progressSummary(res))

	if res.Snapshot.Status == store.StatusFailed {
		return fmt.Errorf("%s: %s", repo.Name, res.Snapshot.Error)
	}
	return nil
}

// progressSummary is the right-hand end of a repo's progress line.
//
// It names the signals that landed and the ones that did not, which a repo
// running one step never needed: "partial" was a complete answer when there was
// only one thing it could be partial about. With several, a status word without
// the list makes someone open the diagnostics to find out which measurement is
// missing.
func progressSummary(res collect.Result) string {
	var parts []string
	// Only when there is a denominator. Coverage of zero statements is not zero
	// percent, and this line is the first place anyone would read it as one.
	if covered, total := coverageTotals(res.Metrics); total > 0 {
		parts = append(parts, fmt.Sprintf("%.1f%% of %d statements",
			float64(covered)/float64(total)*100, total))
	}
	if len(res.Collected) > 0 {
		parts = append(parts, "collected "+strings.Join(res.Collected, ", "))
	}
	if len(res.Failed) > 0 {
		parts = append(parts, "could not collect "+strings.Join(res.Failed, ", "))
	}
	if len(parts) == 0 {
		return "nothing collected"
	}
	return strings.Join(parts, "; ")
}

// printProgress writes one line per repo as the run goes, rather than a table at
// the end, because the interesting case is watching which repo a long run is
// stuck on.
func printProgress(w io.Writer, name, status, summary string) {
	_, _ = fmt.Fprintf(w, "%-28s %-8s %s\n", name, status, summary)
}

// printStarting says which repo is being collected, before it is.
//
// Deliberately not padded into the same columns as printProgress. Padding it
// would put the repo name in the first cell twice, once for the start and once
// for the finish, which reads as a duplicated row rather than as progress.
// Counting the repos is what answers "how much longer".
func printStarting(w io.Writer, name string, index, total int) {
	_, _ = fmt.Fprintf(w, "collecting %s (%d of %d)\n", name, index, total)
}

// repoNames lists what a config knows about, for an error that tells the caller
// what they could have asked for instead of only that they got it wrong.
func repoNames(repos []config.Repo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Name)
	}
	return out
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
