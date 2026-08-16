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
	// KeyTestBuildFailed counts packages whose test binary would not compile. It
	// is written at repo scope whenever the stream parses, zero included, so it
	// behaves like a marker even though it is not one: no signal reports it, and
	// nothing reads it for presence.
	//
	// It exists because a non-zero answer makes every other number from this
	// stream a partial count rather than a total. The delta layer reads it to
	// refuse a comparison, the way it refuses two lint sums taken over different
	// sets of steps. A snapshot written before this key existed has no row, which
	// reads as zero and compares normally, which is the right answer for a
	// snapshot taken when nothing was checking.
	KeyTestBuildFailed = "test.build_failed"

	// The lint keys carry the STEP's name as their scope rather than being
	// repo-level singletons, because SARIF is the one repeatable format: a
	// polyglot repo runs golangci-lint and eslint as two steps, and two
	// repo-scoped rows would collide on the metrics primary key. Scoped, they sum.
	//
	// KeyLintFindings is their marker. A clean lint run emits it at zero, which is
	// the whole distinction between a repo with nothing to fix and a repo nobody
	// linted.
	KeyLintFindings = "lint.findings"
	// KeyLintErrors is the subset at SARIF's error level. A new error-level
	// finding is worth more attention than a new nit, and folding the two together
	// would hide that.
	KeyLintErrors = "lint.findings.error"
	// KeyLintSuppressed counts findings excluded by an inline suppression.
	//
	// It is deliberately NOT folded into the totals: counting suppressed findings
	// against a repo would make it look worse for having triaged them, which is
	// the opposite of the incentive this tool should create. It is tracked
	// separately because a rising suppression count is its own finding.
	KeyLintSuppressed = "lint.suppressed"

	// The dependency keys are repo-level, since a repo has one module graph, and
	// each is its own marker. That is not a shortcut: the three are measurable
	// under DIFFERENT conditions, so one shared marker would claim all three were
	// measured whenever any of them was.
	//
	// KeyDepsTotal is the size of the build list, excluding the main module and
	// any workspace siblings. It needs no network.
	KeyDepsTotal = "deps.total"
	// KeyDepsAgeMedianDays is how old the typical pinned version is.
	//
	// The MEDIAN rather than the mean: a healthy dependency set contains a few
	// very old but entirely fine pins, and a mean is dragged bodily by them, so
	// the repo reads as alarming when nothing is wrong and the trend line tracks
	// whichever ancient module last entered the graph. Absent when not one
	// dependency carried a publish timestamp, which is a real state and not an
	// age of zero.
	KeyDepsAgeMedianDays = "deps.age_median_days"
	// KeyDepsOutdatedDirect counts DIRECT dependencies with a newer version
	// available.
	//
	// Direct only, because that is the number anyone acts on: bumping an indirect
	// module is a consequence of bumping the direct one that pulls it in, so a
	// headline built on the total reports work that is not really there.
	//
	// Written only when the update check demonstrably happened. See
	// updatesWereChecked: with GOPROXY=off the toolchain exits 0, writes nothing
	// to stderr, and reports no updates on any module, so "everything is current"
	// and "nothing was ever checked" are byte-identical streams. Recording the
	// flattering zero from that is the exact bug this tool exists to refuse.
	KeyDepsOutdatedDirect = "deps.outdated_direct"

	// KeySignalDurationMS is how long one step's command took, scoped by the
	// step's name so a repo running four of them can see which one got slower.
	//
	// It is not the same number as snapshots.duration_ms and both are worth
	// having. That column is the whole collection's wall clock, including the
	// three git subprocesses and the toolchain fingerprint, so it moves when the
	// tool itself changes. This is one command, and it is the only thing here
	// that measures a step's cost rather than what the step found.
	//
	// Written only for a step that actually ran something. Ingest mode has no
	// command, so there is no wall time to record, and a zero there would be a
	// duration nobody measured.
	KeySignalDurationMS = "signal.duration_ms"
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
	// Degrades marks a diagnostic that cost the snapshot something. Severity
	// says how bad the news is; this says whether the snapshot is still whole.
	//
	// They are close but not the same, and collapsing them was a bug. A warning
	// that the module proxy was never consulted is not a degradation: the
	// collector deliberately recorded nothing there, which is the designed
	// answer rather than a loss. A warning that a package would not build is a
	// degradation, because numbers are missing from a total that still got
	// published.
	//
	// A degrading diagnostic makes the snapshot partial, and a partial snapshot
	// reports the first one of these as its reason. Before that, a snapshot
	// degraded by a warning stored no reason at all and the report listed the
	// repo under Collection problems with nothing after its name.
	Degrades bool
}

func warnf(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarn, Message: fmt.Sprintf(format, args...)}
}

// degradef is a warning that also costs the snapshot its completeness.
func degradef(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarn, Message: fmt.Sprintf(format, args...), Degrades: true}
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
