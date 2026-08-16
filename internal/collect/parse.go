package collect

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
	"github.com/Romero-jace/repo-metrics/internal/collect/sarif"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/run"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// source is one thing to read, plus everything a parser might need to know
// about how it came to exist.
//
// The context beyond Path is here because some measurements are not in the file.
// The old shape handed parsers a path and nothing else, which meant a parser
// could not see the repo it was reading, the environment the command ran under,
// or how long it took, and every one of those is a measurement something wants.
type source struct {
	// Path is the file to read, absolute.
	Path string
	// Repo is the repository being measured.
	Repo config.Repo
	// Step is the configured step this source belongs to.
	Step config.Signal
	// Env is the step's merged environment as KEY=VALUE, ready for run.Options.
	Env []string
	// Run is how the command finished, or nil in ingest mode where nobody ran
	// anything.
	Run *run.Result
	// Now is the collection's timestamp, passed in rather than read from the
	// clock so that anything derived from it is a pure function of this struct.
	// A parser re-reading an archived artifact then gets the answer as of the run
	// that produced it rather than as of today.
	Now time.Time
}

// parser turns one source into metrics.
//
// An error means this source produced nothing trustworthy. Diagnostics alongside
// a nil error mean it produced something with a caveat, which is the ordinary
// case for a coverage profile that instrumented no packages.
//
// The context is here because reading a file is not always enough to know what
// the numbers in it mean. The dependency parser has to ask the toolchain whether
// the module proxy was even consulted, since the stream cannot say, and that
// question is a subprocess that has to die with the rest of the run.
type parser func(ctx context.Context, src source) ([]store.Metric, []Diagnostic, error)

// parsers maps each configured format to the code that reads it.
//
// config.formats is the list an operator may name and this is the list the tool
// can actually read. They have to be the same list, or a step validates, runs a
// whole test suite, and records nothing.
// TestEveryConfiguredFormatHasAParser pins them together.
var parsers = map[config.Format]parser{
	config.FormatGoCoverprofile: parseCoverprofile,
	config.FormatGoTestJSON:     parseTestJSON,
	config.FormatSARIF:          parseSARIF,
	config.FormatGoListModules:  parseModules,
}

// parserFor returns the parser for a format. An unknown format is a programming
// error rather than an operator one, because config validation rejects those
// before collection starts.
func parserFor(f config.Format) (parser, error) {
	p, ok := parsers[f]
	if !ok {
		return nil, fmt.Errorf("no parser for format %q", f)
	}
	return p, nil
}

// parseCoverprofile reads a Go coverage profile into per-package statement
// counts.
//
// Percentages are never stored. A repo's rate is sum(covered)/sum(total), which
// is not the mean of its packages' rates, so the counts have to survive as
// counts for the arithmetic downstream to be right.
func parseCoverprofile(_ context.Context, src source) ([]store.Metric, []Diagnostic, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening coverage profile: %w", err)
	}
	defer func() { _ = f.Close() }()

	profile, err := golang.ParseCoverProfile(f)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing coverage profile: %w", err)
	}

	var diags []Diagnostic
	metrics := make([]store.Metric, 0, len(profile.Packages)*2)
	for _, pkg := range profile.Packages {
		metrics = append(metrics,
			store.Metric{Key: KeyCoveredStmts, Scope: pkg.Package, Value: float64(pkg.Covered)},
			store.Metric{Key: KeyTotalStmts, Scope: pkg.Package, Value: float64(pkg.Total)},
		)
	}
	if len(profile.Packages) == 0 {
		// No marker rows, so coverage reads as unmeasured downstream rather than
		// as zero percent. That is the whole point: this profile parsed fine and
		// instrumented nothing, and a percentage computed from a zero denominator
		// would lead the report as a total collapse.
		diags = append(diags, warnf("coverage profile has no instrumented packages"))
	}
	return metrics, diags, nil
}

// parseTestJSON reads a `go test -json` event stream into per-package test
// counts, plus the repo-level count of packages carrying no tests at all.
func parseTestJSON(_ context.Context, src source) ([]store.Metric, []Diagnostic, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the test event stream: %w", err)
	}
	defer func() { _ = f.Close() }()

	tests, err := golang.ParseTestJSON(f)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing the test event stream: %w", err)
	}

	var diags []Diagnostic
	var metrics []store.Metric
	for _, pkg := range tests.Packages {
		total := pkg.Passed + pkg.Failed + pkg.Skipped
		if total == 0 && pkg.Duration == 0 {
			continue
		}
		metrics = append(metrics,
			store.Metric{Key: KeyTestCount, Scope: pkg.Package, Value: float64(total)},
			store.Metric{Key: KeyTestFailed, Scope: pkg.Package, Value: float64(pkg.Failed)},
			store.Metric{Key: KeyTestSkipped, Scope: pkg.Package, Value: float64(pkg.Skipped)},
			store.Metric{Key: KeyTestDurationMS, Scope: pkg.Package, Value: float64(pkg.Duration.Milliseconds())},
		)
	}

	// The marker, at repo scope, emitted unconditionally because the stream
	// parsed. A repo where every package has tests measured zero, and that is a
	// finding rather than an absence.
	metrics = append(metrics, store.Metric{
		Key:   KeyPkgWithoutTest,
		Value: float64(tests.PackagesWithoutTests()),
	})

	if tests.Malformed > 0 {
		diags = append(diags, warnf(
			"%d unparseable lines in the test output stream, which usually means a truncated or failed run",
			tests.Malformed))
	}
	return metrics, diags, nil
}

// parseSARIF reads a static-analysis log into finding counts.
//
// The counts are scoped by the STEP's name rather than being repo-level, because
// SARIF is the one repeatable format: a polyglot repo runs two linters as two
// steps, and two repo-scoped rows would collide on the metrics primary key.
// Scoped, they sum, and the report reads the total across every linter.
func parseSARIF(_ context.Context, src source) ([]store.Metric, []Diagnostic, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the analysis log: %w", err)
	}
	defer func() { _ = f.Close() }()

	summary, sarifDiags, err := sarif.Parse(f)
	diags := adoptSARIFDiags(sarifDiags)
	if err != nil {
		return nil, diags, fmt.Errorf("parsing the analysis log: %w", err)
	}

	// Emitted unconditionally, including all-zero, because a linter that ran and
	// found nothing is the good news this tool should be able to report. Without
	// the row it is indistinguishable from a repo nobody lints.
	scope := src.Step.Name
	metrics := []store.Metric{
		{Key: KeyLintFindings, Scope: scope, Value: float64(summary.Active())},
		{Key: KeyLintErrors, Scope: scope, Value: float64(summary.Levels[sarif.LevelError])},
		{Key: KeyLintSuppressed, Scope: scope, Value: float64(summary.Suppressed)},
	}

	if len(summary.Drivers) == 0 {
		// A log with no runs at all. It parsed, so this is not a failure, but
		// zero findings from a tool that never identified itself is worth saying
		// out loud rather than charting as an improvement.
		diags = append(diags, warnf(
			"the analysis log names no tool at all, so these zero findings are from a run that may not have happened"))
	}
	return metrics, diags, nil
}

// proxyProbeTimeout bounds the `go env GOPROXY` lookup. It reads configuration
// and touches no network, so anything slower than this means the toolchain is
// wedged and the answer is not coming.
const proxyProbeTimeout = 15 * time.Second

// parseModules reads a `go list -m -json` stream into dependency counts.
//
// Three measurements under three different conditions, which is why each gets
// its own marker rather than sharing one. The module count needs nothing. The
// age aggregate needs at least one dependency to carry a publish timestamp. The
// outdated count needs the module proxy to have actually been consulted, and
// that last one cannot be answered from the stream at all.
func parseModules(ctx context.Context, src source) ([]store.Metric, []Diagnostic, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the module list: %w", err)
	}
	defer func() { _ = f.Close() }()

	summary, err := golang.ParseModules(f)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing the module list: %w", err)
	}

	var diags []Diagnostic
	// The marker for the module count, written unconditionally because the
	// stream parsed. A repo with no dependencies at all measured zero.
	metrics := []store.Metric{{Key: KeyDepsTotal, Value: float64(summary.Total)}}

	if ages, ok := summary.Ages(src.Now); ok {
		metrics = append(metrics, store.Metric{
			Key:   KeyDepsAgeMedianDays,
			Value: ages.Median.Hours() / 24,
		})
	} else if summary.Total > 0 {
		// Not an age of zero. A directory replacement has no version and no
		// timestamp, so this is a normal state rather than a parse failure, and
		// the row is simply not written.
		diags = append(diags, warnf(
			"not one of the %d dependencies carried a publish timestamp, so their age was not measured",
			summary.Total))
	}

	if why := updatesWereChecked(ctx, src, summary); why != "" {
		// Recording nothing rather than recording zero. This is the whole reason
		// the check exists: the flattering number and the unmeasured one are the
		// same number, and only one of them is true.
		diags = append(diags, warnf(
			"available updates were not recorded because %s. Zero outdated dependencies and an unchecked proxy produce identical output, so nothing is recorded rather than a zero that might be a lie", why))
	} else {
		metrics = append(metrics, store.Metric{
			Key:   KeyDepsOutdatedDirect,
			Value: float64(summary.DirectUpdateAvailable),
		})
	}
	return metrics, diags, nil
}

// updatesWereChecked returns why the update counts cannot be trusted, or an
// empty string when they can.
//
// Three conditions, each of them measured behavior of the Go toolchain rather
// than a theory:
//
// The command has to pass -u, because without it Update is never populated and
// every module reads as current. This is read off the argv rather than from a
// config field on purpose: a config field can disagree with the command, and
// then the tool records four confident zeroes. The argv cannot disagree with
// itself. Getting the sniff wrong costs an unrecorded measurement, never a
// fabricated one.
//
// GOPROXY must not be off. Verified on Go 1.26.5: with GOPROXY=off,
// `go list -m -u -json all` exits 0, writes NOTHING to stderr, streams every
// module, and sets Update on none of them. So "every dependency is current" and
// "nothing was ever checked" are byte-identical streams and no parser can tell
// them apart. Asking the toolchain what its proxy is set to is the only way to
// know, and it has to be asked under the step's own environment, since the step
// may set GOPROXY itself.
//
// And no module may have failed to resolve. With -e, an unreachable proxy makes
// the toolchain emit each unresolvable module with Error set and Update absent,
// still exiting 0. A proxy that answers for some modules and fails on others then
// reads as FEWER updates available, which is worse than a hard failure because it
// looks like good news.
func updatesWereChecked(ctx context.Context, src source, summary *golang.ModuleSummary) string {
	if !hasUpdateFlag(src.Step.Command) {
		return "the command does not pass -u, so the toolchain never looked for newer versions"
	}
	if proxy := goEnv(ctx, src, "GOPROXY"); proxy == "off" {
		return "GOPROXY is off, so the toolchain answered from the local cache without consulting anything"
	}
	if summary.Errored > 0 {
		return fmt.Sprintf(
			"%d of %d dependencies could not be resolved, which makes every count here a floor rather than a total",
			summary.Errored, summary.Total)
	}
	return ""
}

// hasUpdateFlag reports whether an argv asks the toolchain to look for updates.
func hasUpdateFlag(argv []string) bool {
	for _, arg := range argv {
		if arg == "-u" || arg == "--u" || strings.HasPrefix(arg, "-u=") || strings.HasPrefix(arg, "--u=") {
			return true
		}
	}
	return false
}

// goEnv asks the toolchain for one setting, under the step's own environment.
//
// An unreadable answer comes back empty, which no caller treats as a reason to
// record anything: the checks above only act on a value they positively
// recognize, so a wedged toolchain costs an unrecorded measurement rather than
// producing a wrong one.
func goEnv(ctx context.Context, src source, name string) string {
	var buf bytes.Buffer
	res, err := run.Command(ctx, run.Options{
		Dir:     src.Repo.Path,
		Args:    []string{"go", "env", name},
		Env:     src.Env,
		Timeout: proxyProbeTimeout,
		Stdout:  &buf,
	})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

// adoptSARIFDiags converts the parser's own diagnostics into this package's.
//
// The two types are deliberately separate: internal/collect/sarif has no
// dependency on collect, which is what lets it stay a parser of a public format
// rather than a piece of this tool.
func adoptSARIFDiags(in []sarif.Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		severity := SeverityWarn
		if d.Severity == sarif.SeverityError {
			severity = SeverityError
		}
		out = append(out, Diagnostic{Severity: severity, Message: d.Message})
	}
	return out
}
