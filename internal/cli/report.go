package cli

import (
	"context"
	"fmt"
	"io"
	"os"
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
	format := set.String("format", formatMarkdown, "markdown or json")
	proceed, err := parseFlags(set, args, stderr)
	if !proceed || err != nil {
		return err
	}

	// Rejected rather than defaulted: silently rendering markdown for
	// --format=jsom would hand a broken pipeline a file it cannot parse.
	if *format != formatMarkdown && *format != formatJSON {
		err := fmt.Errorf("unknown format %q, want %s or %s", *format, formatMarkdown, formatJSON)
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return err
	}

	cfg, err := loadConfig(*configPath, stderr)
	if err != nil {
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

	inputs, err := reportInputs(ctx, st, cfg, window, stderr)
	if err != nil {
		return err
	}

	rep := delta.Compute(inputs, delta.Options{
		Window:        window,
		MinStatements: cfg.MinStatements,
		MinRepoDelta:  cfg.MinRepoDelta,
		// MaxCulprits stays zero so Compute applies its own default.
	}, time.Now())

	return writeReport(rep, *format, *outPath, stdout, stderr)
}

// reportInputs pairs each configured repo with its newest snapshot and the
// closest one from a window ago.
//
// Every configured repo gets an entry even if it has never been collected. A
// repo that quietly drops out of the report is exactly how a broken cron job
// goes unnoticed for a month, which is the failure this tool exists to catch.
func reportInputs(
	ctx context.Context,
	st *store.Store,
	cfg *config.Config,
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

	inputs := make([]delta.Input, 0, len(cfg.Repos))
	for _, configured := range cfg.Repos {
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

func writeReport(rep delta.Report, format, outPath string, stdout, stderr io.Writer) error {
	render := report.Markdown
	if format == formatJSON {
		render = report.JSON
	}

	if outPath == "" {
		if err := render(stdout, rep); err != nil {
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
	if err := render(f, rep); err != nil {
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
