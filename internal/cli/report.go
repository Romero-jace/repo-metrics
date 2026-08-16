package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/delta"
	"github.com/Romero-jace/repo-metrics/internal/report"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

func runReport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("report", stderr)
	configPath := set.String("config", defaultConfigPath, "config file to read")
	windowFlag := set.String("window", "", "how far back the baseline sits, like 7d (default: the config's window)")
	outPath := set.String("out", "", "write the report here instead of to stdout")
	format := set.String("format", string(report.FormatMarkdown),
		"which format to render: "+strings.Join(report.Formats(), ", "))
	only := set.String("repo", "", "report on just this one repo, by name")
	sectionFlag := set.String("section", string(report.SectionAll),
		"which part of the report to render: "+strings.Join(report.Sections(), ", "))
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	// Rejected rather than defaulted: silently rendering markdown for
	// --format=jsom would hand a broken pipeline a file it cannot parse.
	//
	// Validated by the report package's own parser rather than by a list kept
	// here, for the same reason sections are: two validators over one set of
	// names is one of them going stale, and --format now spans three
	// subcommands rather than one.
	renderFormat, err := report.ParseFormat(*format)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	// Checked here beside --format, and by the report package's own parser
	// rather than by a second list kept here. report.ParseSection already
	// refuses an unknown name and names the valid ones, and two validators over
	// one set of names is one of them going stale.
	sec, err := report.ParseSection(*sectionFlag)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
		return err
	}

	// Narrowed before the database is opened, so a --repo run never loads
	// snapshots and metrics for repos it is about to throw away, and a typo
	// fails before anything has been read at all.
	repos, err := selectRepos(cfg.Repos, *only)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	window := time.Duration(cfg.Window)
	if *windowFlag != "" {
		if window, err = parseWindow(*windowFlag); err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return err
		}
	}

	st, err := openStore(cfg.Database, stderr)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	inputs, err := reportInputs(ctx, st, repos, window, stderr)
	if err != nil {
		return err
	}

	rep := delta.Compute(inputs, delta.Options{
		Window:        window,
		MinStatements: cfg.MinStatements,
		MinRepoDelta:  cfg.MinRepoDelta,
		// MaxCulprits stays zero so Compute applies its own default.
	}, time.Now())

	// cfg.Repos is the honest denominator: selectRepos returns a new slice and
	// never touches the one it was given, so the full configured count is still
	// here after narrowing. The report says what it covers rather than leaving a
	// caller to infer it from a list that may not even be rendered.
	scope := report.Scope{Repo: *only, Configured: len(cfg.Repos)}

	return writeReport(rep, renderFormat, sec, scope, *outPath, stdout, stderr)
}

// reportInputs pairs each repo it is given with its newest snapshot and the
// closest one from a window ago.
//
// It takes the repo list rather than the whole config because --repo narrows
// here: an unwanted repo is never looked up, so nothing is fetched or compared
// only to be dropped afterwards. Filtering after delta.Compute would do the
// work anyway and would also mean the narrowing lived somewhere different from
// collect's, which is two places for one rule.
//
// Every repo it is given gets an entry even if it has never been collected. A
// repo that quietly drops out of the report is exactly how a broken cron job
// goes unnoticed for a month, which is the failure this tool exists to catch.
//
// A repo whose every run failed is the sharper version of that. LatestSnapshot
// skips failed rows, so it comes back nil and such a repo would otherwise reach
// the renderer with no head at all, indistinguishable from one that has never
// been collected: no status, no time, no error text, nothing to act on. It is
// the repo the reader most needs to see, and it would arrive looking like the
// least interesting one. The failed row is attached as the head instead, which
// is what carries the status, the time of the last attempt, and what broke.
func reportInputs(
	ctx context.Context,
	st *store.Store,
	repos []config.Repo,
	window time.Duration,
	stderr io.Writer,
) ([]delta.Input, error) {
	known, err := st.Repos(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return nil, err
	}
	byName := make(map[string]store.Repo, len(known))
	for _, r := range known {
		byName[r.Name] = r
	}

	inputs := make([]delta.Input, 0, len(repos))
	for _, configured := range repos {
		repo, ok := byName[configured.Name]
		if !ok {
			inputs = append(inputs, delta.Input{
				Repo: store.Repo{Name: configured.Name, Path: configured.Path},
			})
			continue
		}

		in := delta.Input{Repo: repo}
		head, err := st.LatestSnapshot(ctx, repo.ID)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
			return nil, err
		}
		if head == nil {
			// No usable snapshot. Either the repo has never been collected, in
			// which case there is genuinely nothing to say about it, or every
			// run failed, in which case the failed row is the finding.
			//
			// No baseline is fetched for a failed head on purpose. A failed run
			// stored no metrics, so a delta against a real baseline is the
			// baseline measured against zero: the repo would clear the mover
			// threshold on a fabricated cliff and lead the report as the week's
			// biggest drop, which is really just a crashed test command.
			last, err := st.LatestSnapshotAny(ctx, repo.ID)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
				return nil, err
			}
			in.Head = last
			inputs = append(inputs, in)
			continue
		}
		in.Head = head
		if in.HeadMetrics, err = st.MetricsFor(ctx, head.ID); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
			return nil, err
		}

		// The baseline is measured back from the head snapshot, not from now.
		// Measuring from now would make a repo that has not been collected in
		// ten days compare against nothing at all.
		base, err := st.SnapshotAtOrBefore(ctx, repo.ID, head.CollectedAt.Add(-window))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
			return nil, err
		}
		// A positive window puts the cutoff strictly before the head, so this
		// guard should never fire. It is here because a head that is its own
		// baseline reports every delta as a confident zero.
		if base != nil && base.ID != head.ID {
			in.Base = base
			if in.BaseMetrics, err = st.MetricsFor(ctx, base.ID); err != nil {
				_, _ = fmt.Fprintf(stderr, "%s: %v\n", repo.Name, err)
				return nil, err
			}
		}
		inputs = append(inputs, in)
	}
	return inputs, nil
}

// writeReport renders the report to stdout or to a file. sec and scope go to
// whichever renderer is chosen rather than being applied afterwards, so markdown
// and JSON cannot disagree about what a section contains or about which repos the
// answer covers.
func writeReport(rep delta.Report, format report.Format, sec report.Section, scope report.Scope, outPath string, stdout, stderr io.Writer) error {
	if outPath == "" {
		if err := report.RenderReport(stdout, format, rep, sec, scope); err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return err
		}
		return nil
	}

	f, err := os.Create(outPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "could not create %s: %v\n", outPath, err)
		return err
	}
	if err := report.RenderReport(f, format, rep, sec, scope); err != nil {
		_ = f.Close()
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}
	// Checked rather than deferred and discarded: a short write surfaces at
	// close, and swallowing it would announce a report that is truncated.
	if err := f.Close(); err != nil {
		_, _ = fmt.Fprintf(stderr, "could not finish writing %s: %v\n", outPath, err)
		return err
	}

	_, _ = fmt.Fprintf(stdout, "wrote %s\n", outPath)
	return nil
}
