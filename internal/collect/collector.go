package collect

import (
	"fmt"
	"os"

	"github.com/Romero-jace/repo-metrics/internal/collect/golang"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// Metric keys. Coverage is always a pair of statement counts, never a
// percentage, so that repo coverage can be computed as sum(covered)/sum(total)
// rather than as a meaningless average of per-package rates.
const (
	KeyCoveredStmts   = "coverage.stmt.covered"
	KeyTotalStmts     = "coverage.stmt.total"
	KeyTestCount      = "test.count"
	KeyTestFailed     = "test.failed"
	KeyTestSkipped    = "test.skipped"
	KeyTestDurationMS = "test.duration_ms"
	KeyPkgWithoutTest = "pkg.without_tests"
)

// Severity classifies a diagnostic.
type Severity string

const (
	// SeverityWarn means something was lost but the snapshot still carries
	// usable numbers.
	SeverityWarn Severity = "warn"
	// SeverityError means the snapshot carries nothing trustworthy.
	SeverityError Severity = "error"
)

// Diagnostic explains something that went wrong, without aborting the run.
//
// One unreachable repo must not cost you the other nine, so collection
// degrades into diagnostics rather than returning early.
type Diagnostic struct {
	Severity Severity
	Message  string
}

func warnf(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarn, Message: fmt.Sprintf(format, args...)}
}

func errorf(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityError, Message: fmt.Sprintf(format, args...)}
}

// Artifacts are the files a collection produced or found.
type Artifacts struct {
	// CoverprofilePath is absolute.
	CoverprofilePath string
	// StdoutPath is where the command's stdout was captured, or empty if it
	// was not captured.
	StdoutPath string
}

// Collector turns a repo's artifacts into metrics.
//
// One implementation exists today. The interface is here so that adding, say,
// an lcov collector is a new file rather than a change to storage or reporting,
// which is what "not Go-only forever" has to mean in practice. It is
// deliberately not a plugin registry.
type Collector interface {
	Name() string
	Collect(a Artifacts) ([]store.Metric, []Diagnostic, error)
}

// GoCollector reads Go coverage profiles and test2json streams.
type GoCollector struct{}

// Name implements Collector.
func (GoCollector) Name() string { return "go" }

// Collect implements Collector.
//
// A missing or unparseable coverage profile is an error, because coverage is
// the reason the tool exists. A missing or unparseable stdout stream is only a
// warning, since it costs test counts but leaves coverage intact.
func (GoCollector) Collect(a Artifacts) ([]store.Metric, []Diagnostic, error) {
	var diags []Diagnostic

	f, err := os.Open(a.CoverprofilePath)
	if err != nil {
		return nil, diags, fmt.Errorf("opening coverage profile: %w", err)
	}
	defer func() { _ = f.Close() }()

	profile, err := golang.ParseCoverProfile(f)
	if err != nil {
		return nil, diags, fmt.Errorf("parsing coverage profile: %w", err)
	}

	metrics := make([]store.Metric, 0, len(profile.Packages)*2)
	for _, pkg := range profile.Packages {
		metrics = append(metrics,
			store.Metric{Key: KeyCoveredStmts, Scope: pkg.Package, Value: float64(pkg.Covered)},
			store.Metric{Key: KeyTotalStmts, Scope: pkg.Package, Value: float64(pkg.Total)},
		)
	}
	if len(profile.Packages) == 0 {
		diags = append(diags, warnf("coverage profile has no instrumented packages"))
	}

	if a.StdoutPath == "" {
		return metrics, diags, nil
	}

	tests, err := readTestJSON(a.StdoutPath)
	if err != nil {
		// Coverage survived, so this is a downgrade rather than a failure.
		diags = append(diags, warnf("test counts unavailable: %v", err))
		return metrics, diags, nil
	}

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

	// Repo-level, because it counts packages rather than measuring one.
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

func readTestJSON(path string) (*golang.TestSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening captured stdout: %w", err)
	}
	defer func() { _ = f.Close() }()
	return golang.ParseTestJSON(f)
}
