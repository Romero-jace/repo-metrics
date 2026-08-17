package collect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
	"github.com/Romero-jace/repo-metrics/internal/collect/jslock"
	"github.com/Romero-jace/repo-metrics/internal/collect/junit"
	"github.com/Romero-jace/repo-metrics/internal/collect/lcov"
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
	config.FormatJUnitXML:       parseJUnitXML,
	config.FormatLCOV:           parseLCOV,
	config.FormatNPMLockfile:    parseLockfile(jslock.ParseNPM, "package-lock.json"),
	config.FormatBunLockfile:    parseLockfile(jslock.ParseBun, "bun.lock"),
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

// parseLockfile builds a parser for one JavaScript lockfile format.
//
// A constructor rather than two near-identical functions, because the two differ only
// in which decoder runs and what the file is called in a diagnostic. Everything
// that matters here — which key is written, when it is written, and what is
// deliberately NOT written — is one decision and belongs in one place.
func parseLockfile(
	read func(io.Reader) (*jslock.Summary, []jslock.Diagnostic, error),
	what string,
) parser {
	return func(_ context.Context, src source) ([]store.Metric, []Diagnostic, error) {
		f, err := os.Open(src.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("opening the %s: %w", what, err)
		}
		defer func() { _ = f.Close() }()

		summary, lockDiags, err := read(f)
		diags := adoptLockDiags(lockDiags)
		if err != nil {
			return nil, diags, fmt.Errorf("parsing the %s: %w", what, err)
		}

		// Written unconditionally because the file parsed, zero included. A
		// project with no dependencies at all measured zero, and that is a real
		// answer rather than an absence — the same reasoning as the Go module
		// count, which is the marker for this signal in both cases.
		metrics := []store.Metric{{Key: KeyDepsTotal, Value: float64(summary.Total)}}

		// And nothing else. deps.age_median_days needs a publish timestamp, which
		// no JavaScript lockfile records, and deps.outdated_direct needs a registry
		// to have been asked. Both come back unmeasured rather than approximated
		// from what is here, which is the same refusal the Go parser makes when the
		// module proxy was never consulted.
		return metrics, diags, nil
	}
}

// adoptLockDiags converts the parser's own diagnostics into this package's, the
// same way adoptSARIFDiags does and for the same reason.
func adoptLockDiags(in []jslock.Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		severity := SeverityWarn
		if d.Severity == jslock.SeverityError {
			severity = SeverityError
		}
		out = append(out, Diagnostic{Severity: severity, Message: d.Message})
	}
	return out
}

// parseLCOV reads an LCOV tracefile into per-file LINE counts.
//
// Percentages are never stored, for the same reason parseCoverprofile does not
// store them: a repo's rate is sum(covered)/sum(total), which is not the mean of
// its files' rates.
//
// The keys are the line pair rather than the statement pair, and that is the
// whole point of a separate parser. Reusing Go's keys would let a repo's
// coverage number be a sum of statements and lines, two different denominators,
// producing a percentage that is arithmetically fine and describes nothing.
func parseLCOV(_ context.Context, src source) ([]store.Metric, []Diagnostic, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the coverage profile: %w", err)
	}
	defer func() { _ = f.Close() }()

	profile, lcovDiags, err := lcov.Parse(f)
	diags := adoptLCOVDiags(lcovDiags)
	if err != nil {
		return nil, diags, fmt.Errorf("parsing the coverage profile: %w", err)
	}

	metrics := make([]store.Metric, 0, len(profile.Files)*2)
	var instrumented int
	for _, file := range profile.Files {
		if file.Total == 0 {
			// A record naming a file and carrying no line information. Writing a
			// zero denominator would put a file into the marker's scope set while
			// contributing nothing, so the set changes without the number changing
			// and next week's delta gets refused for no reason.
			continue
		}
		instrumented++
		metrics = append(metrics,
			store.Metric{Key: KeyCoveredLines, Scope: file.Name, Value: float64(file.Covered)},
			store.Metric{Key: KeyTotalLines, Scope: file.Name, Value: float64(file.Total)},
		)
	}

	if instrumented == 0 {
		// No marker rows, so line coverage reads as unmeasured downstream rather
		// than as zero percent. This is not hypothetical: vitest 4.1.10 with
		// @vitest/coverage-v8 4.1.4 wrote a ZERO-BYTE lcov.info for a full
		// 1,964-test run and exited 0. A percentage from a zero denominator would
		// have led the report as a total collapse, and the run that produced it
		// looked entirely healthy.
		diags = append(diags, warnf(
			"the coverage profile instruments no files at all, which is what a coverage run writes when its "+
				"include patterns match nothing or instrumentation never attached. Nothing is recorded, "+
				"because a rate computed from no lines is not a measurement"))
	}
	return metrics, diags, nil
}

// adoptLCOVDiags converts the parser's own diagnostics into this package's, the
// same way adoptSARIFDiags does and for the same reason.
func adoptLCOVDiags(in []lcov.Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		severity := SeverityWarn
		if d.Severity == lcov.SeverityError {
			severity = SeverityError
		}
		out = append(out, Diagnostic{Severity: severity, Message: d.Message})
	}
	return out
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

	// The toolchain-neutral marker, at STEP scope, emitted unconditionally
	// because the stream parsed. Every test parser writes this one, so the four
	// signals that only need "did anything read a test result" do not have to ask
	// a Go-specific question to find out.
	metrics = append(metrics, store.Metric{
		Key:   KeyTestSuites,
		Scope: src.Step.Name,
		Value: float64(len(tests.Packages)),
	})

	// The Go-specific marker, at repo scope, also unconditional. A repo where
	// every package has tests measured zero, and that is a finding rather than an
	// absence.
	//
	// It stays because it is the only proof of the one measurement no other
	// toolchain's output can supply: how many source packages carry no tests at
	// all. `go test -json` can say, because the toolchain prints a marker line for
	// a package with no test files; a JUnit document lists the suites that ran and
	// cannot know what did not.
	metrics = append(metrics, store.Metric{
		Key:   KeyPkgWithoutTest,
		Value: float64(tests.PackagesWithoutTests()),
	})

	// Also unconditional, for the same reason and a different consumer: nothing
	// reports this number, the delta layer reads it to decide whether the totals
	// above are a total or a floor.
	broken := tests.PackagesFailingToBuild()
	metrics = append(metrics, store.Metric{
		Key:   KeyTestBuildFailed,
		Value: float64(len(broken)),
	})

	if len(broken) > 0 {
		// Degrading rather than a plain warning. The counts above are real for
		// every package that did build, so they are worth keeping, and they are
		// not the totals they look like.
		diags = append(diags, degradef(
			"%s would not build, so its tests never ran and this repo's test counts are "+
				"a partial sum rather than a total. Nothing here says how many tests it has: %s",
			pluralPackages(len(broken)), strings.Join(broken, ", ")))
	}

	if tests.Malformed > 0 {
		diags = append(diags, warnf(
			"%d unparseable lines in the test output stream, which usually means a truncated or failed run",
			tests.Malformed))
	}
	return metrics, diags, nil
}

// parseJUnitXML reads a test report from any runner that emits JUnit XML.
//
// Every row it writes is scoped, and every scope starts with the step's name.
// That is what earns the format Repeatable: a repo running pytest and vitest as
// two steps files them under two prefixes, so their counts sum rather than
// overwrite each other on the metrics primary key. It also keeps this format's
// rows away from go-test-json's repo-level ones, so a repo can run both.
//
// Compare what this does NOT write. There is no pkg.without_tests here and there
// cannot be: the document lists the suites that ran, and no amount of reading it
// reveals a source file nobody wrote a test for. That signal stays unmeasured,
// which is the true answer.
func parseJUnitXML(_ context.Context, src source) ([]store.Metric, []Diagnostic, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the test report: %w", err)
	}
	defer func() { _ = f.Close() }()

	report, junitDiags, err := junit.Parse(f)
	diags := adoptJUnitDiags(junitDiags)
	if err != nil {
		return nil, diags, fmt.Errorf("parsing the test report: %w", err)
	}

	// A well formed report carrying no cases at all. Verified against pytest
	// 9.1.1: a run that collects nothing exits 5 and still writes a complete
	// document with tests="0" failures="0" errors="0" and no testcase elements,
	// which is byte-identical in shape to a genuine measurement of a repo with no
	// tests. Nothing in the file tells them apart.
	//
	// So no marker is written and no counts are, which reads downstream as
	// unmeasured. Recording the zero would be the exact bug this tool exists to
	// refuse, and it would arrive looking like the best news a repo ever had.
	if report.Cases == 0 {
		return nil, append(diags, warnf(
			"the test report names %s and not one test case, which is what a runner writes when it collected nothing "+
				"rather than when a repo has no tests. Nothing is recorded, because those two produce the same file%s",
			pluralSuites(report.Suites), exitCodeClause(src))), nil
	}

	metrics := make([]store.Metric, 0, len(report.Groups)*4+1)
	for _, g := range report.Groups {
		scope := src.Step.Name + "/" + g.Name
		metrics = append(metrics,
			store.Metric{Key: KeyTestCount, Scope: scope, Value: float64(g.Total())},
			store.Metric{Key: KeyTestFailed, Scope: scope, Value: float64(g.Failed)},
			store.Metric{Key: KeyTestSkipped, Scope: scope, Value: float64(g.Skipped)},
		)
		if g.Timed {
			// Written only where something carried a time. A group whose emitter
			// omits the attribute has a duration nobody measured, and a zero there
			// would be subtracted from last week's real total as an improvement.
			metrics = append(metrics, store.Metric{
				Key: KeyTestDurationMS, Scope: scope, Value: float64(g.Duration.Milliseconds()),
			})
		}
	}

	// The marker, at the step's scope, written because cases were genuinely read.
	metrics = append(metrics, store.Metric{
		Key:   KeyTestSuites,
		Scope: src.Step.Name,
		Value: float64(len(report.Groups)),
	})
	return metrics, diags, nil
}

// exitCodeClause names the runner's exit status when there was a run to have
// one. In ingest mode nobody ran anything, so there is nothing to report and the
// sentence has to still read correctly without it.
func exitCodeClause(src source) string {
	if src.Run == nil {
		return ", and no command was run here whose exit status could say which"
	}
	return fmt.Sprintf(", and the command exited %d", src.Run.ExitCode)
}

func pluralSuites(n int) string {
	if n == 1 {
		return "1 suite"
	}
	return fmt.Sprintf("%d suites", n)
}

// adoptJUnitDiags converts the parser's own diagnostics into this package's, the
// same way adoptSARIFDiags does and for the same reason.
func adoptJUnitDiags(in []junit.Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		severity := SeverityWarn
		if d.Severity == junit.SeverityError {
			severity = SeverityError
		}
		out = append(out, Diagnostic{Severity: severity, Message: d.Message})
	}
	return out
}

func pluralPackages(n int) string {
	if n == 1 {
		return "1 package"
	}
	return fmt.Sprintf("%d packages", n)
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

	// The two finding counts are emitted unconditionally, including all-zero,
	// because a linter that ran and found nothing is the good news this tool
	// should be able to report. Without the row it is indistinguishable from a
	// repo nobody lints.
	scope := src.Step.Name
	metrics := []store.Metric{
		{Key: KeyLintFindings, Scope: scope, Value: float64(summary.Active())},
		{Key: KeyLintErrors, Scope: scope, Value: float64(summary.Levels[sarif.LevelError])},
	}

	// The suppression count is NOT, and this is the one place in this parser where
	// zero is not a measurement.
	//
	// A SARIF suppressions array is the only thing a document can carry to say a
	// finding was reported-but-silenced, and almost no linter writes one:
	// golangci-lint drops a //nolint finding before it ever reaches the log, and
	// so do ruff for # noqa, rubocop for # rubocop:disable, detekt for @Suppress,
	// SwiftLint for // swiftlint:disable and clippy for #[allow]. Only ESLint via
	// @microsoft/eslint-formatter-sarif and Roslyn emit the array at all.
	//
	// So writing a zero here published "this repo suppresses nothing" for a repo
	// with a hundred //nolint comments, from a document that simply cannot express
	// the question. Nothing measured it, and the row is left out instead.
	//
	// What that costs, stated plainly: a repo that genuinely deletes its last
	// suppression reads as unmeasured rather than as zero, so the improvement
	// cannot be reported. That is not a compromise so much as the honest answer —
	// a document with no suppressions array is the same bytes whether the repo
	// cleaned up or the linter never had anything to say, and the whole discipline
	// here is that those two do not get one number between them.
	if summary.Suppressed > 0 {
		metrics = append(metrics, store.Metric{Key: KeyLintSuppressed, Scope: scope, Value: float64(summary.Suppressed)})
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
