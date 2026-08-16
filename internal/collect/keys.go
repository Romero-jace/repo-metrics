package collect

import "fmt"

// Metric keys.
//
// Coverage is always a pair of statement counts, never a percentage, so that
// repo coverage can be computed as sum(covered)/sum(total) rather than as a
// meaningless average of per-package rates.
//
// Every group here has a MARKER: a key a parser emits unconditionally once it
// has read its input, even when the answer is zero. Presence of the marker is
// the only thing that distinguishes a measured zero from nothing having looked,
// and that distinction is the reason this tool exists. The contract is stated on
// delta.Signal and written down in CONTRIBUTING.md, because no registry entry
// can check it for the collector.
const (
	KeyCoveredStmts = "coverage.stmt.covered"
	// KeyTotalStmts is coverage's marker, at package scope. A profile carrying
	// only its "mode: set" header stores neither key, which is exactly the case
	// the marker exists to catch.
	KeyTotalStmts     = "coverage.stmt.total"
	KeyTestCount      = "test.count"
	KeyTestFailed     = "test.failed"
	KeyTestSkipped    = "test.skipped"
	KeyTestDurationMS = "test.duration_ms"
	// KeyPkgWithoutTest is the marker for every test signal, at repo scope. The
	// five of them come from one parsed stream, so either the parser read the
	// test output or it did not; there is no state where it counted the passes
	// but not the failures.
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

// prefix labels a diagnostic with the step it came from. With one step per repo
// the source was obvious; with several it is not, and a bare "test counts
// unavailable" against a repo running four steps says nothing about which.
func (d Diagnostic) prefix(name string) Diagnostic {
	d.Message = name + ": " + d.Message
	return d
}
