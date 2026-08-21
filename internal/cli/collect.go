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
	dbPath := set.String("database", "", "database to write instead of the one the config names")
	var only nameList
	set.Var(&only, "repo", "collect just this repo, by name. Repeat for several")
	var steps nameList
	set.Var(&steps, "signal", "collect only this step, by the name its config gives it. Repeat for several")
	// Default one rather than the core count, so the output contract and the
	// order repos are reported in do not change for anyone who did not ask.
	jobs := set.Int("jobs", 1, "how many repos to collect at once")
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	if *jobs < 1 {
		err := fmt.Errorf("--jobs %d is not a number of repos to collect at once", *jobs)
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	repos, err := selectRepos(cfg.Repos, only)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	if repos, err = narrowSignals(repos, steps, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	st, err := openStore(databasePath(cfg, *dbPath), stderr)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Cancellation is checked per repo rather than mid-repo so that whatever is
	// already collected gets written. Everything downstream takes ctx too, so a
	// signal during a long test run still lands inside the subprocess.
	//
	// Each repo is announced before its work, because the completion line only
	// lands after every signal has run and been stored. A cold three-repo run
	// was 78 seconds of silence, and one signal may take ten minutes, so on nine
	// repos there was nothing to tell working from hung. Announcing is
	// unconditional: there is no terminal detection in this codebase and this
	// must not be what introduces it, so a cron log gets the same lines a
	// terminal does.
	failed, err := collectPool(ctx, st, repos, *jobs, stdout, stderr)
	if err != nil {
		return err
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
	parts := coverageSummaries(res.Metrics)
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
// It takes a list rather than one name because collect's --repo repeats: a fleet
// re-measure is usually two or three repos rather than one or all of them, and
// forking a process per repo was what people did instead. report's flag stays
// single-valued and passes a list of one, so both subcommands still go through
// this and cannot drift on what an unknown name does.
//
// Every name is checked before any is accepted, so `--repo api --repo wroker`
// fails naming the typo rather than half-collecting.
func selectRepos(repos []config.Repo, only nameList) ([]config.Repo, error) {
	if len(only) == 0 {
		return repos, nil
	}
	names := make([]string, 0, len(repos))
	byName := make(map[string]config.Repo, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
		byName[r.Name] = r
	}

	var unknown []string
	for _, want := range only {
		if _, ok := byName[want]; !ok {
			unknown = append(unknown, want)
		}
	}
	if len(unknown) > 0 {
		if len(names) == 0 {
			return nil, fmt.Errorf("no repo named %s: the config has no repos at all",
				strings.Join(unknown, ", "))
		}
		return nil, fmt.Errorf("no repo named %s in the config, which has %s",
			strings.Join(unknown, ", "), strings.Join(names, ", "))
	}

	// Config order, not flag order, so two runs naming the same repos in a
	// different order collect them in the same order and their progress output
	// can be compared.
	out := make([]config.Repo, 0, len(only))
	for _, r := range repos {
		if only.has(r.Name) {
			out = append(out, r)
		}
	}
	return out, nil
}
