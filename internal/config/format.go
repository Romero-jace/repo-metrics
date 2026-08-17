package config

import (
	"fmt"
	"strings"
)

// Format names a parser: how to read one thing a collection step leaves behind.
//
// The set is closed and lives here rather than in the collect package because
// these names are config vocabulary. An operator types them into a YAML file, so
// the list that validates them and the list the help text prints have to be the
// same list.
type Format string

// The formats this tool can read.
const (
	// FormatGoCoverprofile is the file `go test -coverprofile` writes.
	FormatGoCoverprofile Format = "go-coverprofile"
	// FormatGoTestJSON is the event stream `go test -json` writes to stdout.
	FormatGoTestJSON Format = "go-test-json"
	// FormatSARIF is the OASIS static-analysis interchange format, which
	// golangci-lint, eslint, ruff, semgrep, clippy and CodeQL all emit.
	//
	// It was the first format here that is not Go-specific, and the argument for
	// choosing it over each linter's native output is the one FormatJUnitXML and
	// FormatLCOV were later added on: one parser covers every language this tool
	// will ever be pointed at, so a new ecosystem is a new command in somebody's
	// config rather than new code here.
	FormatSARIF Format = "sarif"
	// FormatGoListModules is the object stream `go list -m -json` writes: a run
	// of concatenated JSON objects, not an array.
	FormatGoListModules Format = "go-list-modules"
	// FormatJUnitXML is the test-report format pytest, vitest, jest and
	// go-junit-report all emit.
	//
	// It is to test results what SARIF is to lint findings, and chosen for the
	// same reason: one parser rather than one per runner. What it cannot express
	// is the point of keeping go-test-json alongside it. A JUnit document lists
	// the suites that RAN, so nothing in it can say how many source files carry
	// no tests at all, and a repo measured this way reports that signal as
	// unmeasured rather than as zero.
	FormatJUnitXML Format = "junit-xml"
	// FormatLCOV is the tracefile istanbul, nyc, coverage.py and gcov all write.
	//
	// It is the only widely emitted coverage format that stores counts rather
	// than rates, which is what this tool needs and Cobertura XML cannot supply
	// without reconstructing the counts from percentages.
	//
	// It counts LINES where a Go coverage profile counts statements. The two are
	// stored under different keys and reported as different signals for that
	// reason: they are not the same unit, so their ratios are not comparable and
	// their counts must never be summed.
	FormatLCOV Format = "lcov"
	// FormatNPMLockfile is a package-lock.json, read for the package count.
	FormatNPMLockfile Format = "npm-lockfile"
	// FormatBunLockfile is a bun.lock, read for the same.
	//
	// Both are read as FILES rather than as the output of `npm outdated` or
	// `bun outdated`, and that is the point of them. Those commands resolve
	// through the installed tree, so on a checkout where nothing has been
	// installed they return an empty result and exit 0 — a repo nobody installed
	// and a repo with nothing outdated produce identical output. That is the same
	// trap GOPROXY=off sets for the Go dependency parser, and the answer here is
	// to read a file that either lists the packages or is not a lockfile.
	//
	// What neither can supply is the other two dependency measurements. No
	// JavaScript lockfile records when a version was published, so dependency age
	// is not recoverable from one, and whether a newer version exists needs a
	// registry. Both stay unmeasured.
	FormatBunLockfile Format = "bun-lockfile"
)

// formatPolicy is what the tool needs to know about a format beyond how to parse
// it. Both fields exist to keep a knob out of the config file: they are facts
// about the tools that emit each format, not preferences an operator holds.
type formatPolicy struct {
	// Repeatable says two steps in one repo may both use this format.
	//
	// Most cannot. A repo has one test suite, so two steps emitting test.count
	// for the same package would write over each other on the metrics table's
	// primary key and cost the whole snapshot. SARIF is the exception and a real
	// one: a polyglot repo genuinely runs golangci-lint and eslint side by side,
	// and lint findings are filed under the step's own name, so two of them sum
	// rather than collide.
	Repeatable bool

	// NonZeroExitIsNormal says this format's producers exit non-zero as a way of
	// reporting findings rather than of reporting failure.
	//
	// Every SARIF-emitting linter does: golangci-lint exits 1 when it finds
	// issues, and so do eslint, ruff and clippy. Without this, a lint step would
	// mark its snapshot degraded on every run where the linter did its job, and
	// the status field would stop meaning anything.
	//
	// `go test` is the opposite case and deliberately not covered by it. A
	// non-zero exit there means the suite is red, which is a real fact about the
	// repo worth flagging even though the coverage numbers still stand.
	NonZeroExitIsNormal bool

	// RepoScopedKeys are the metric keys this format's parser writes with an
	// empty scope, meaning one row for the whole repo.
	//
	// Repeatable answers whether two steps may share a FORMAT. This answers the
	// question next to it, which nothing asked until a repo could run two
	// different test formats: whether two steps using DIFFERENT formats would
	// write the same row. The metrics primary key is (snapshot, key, scope), so
	// two repo-scoped rows under one key collide however they got there, and the
	// per-format count cannot see it.
	//
	// Left empty by a format whose parser scopes everything it writes, which is
	// what makes that format safe to run beside anything.
	//
	// The keys are literals here rather than references to the collect package's
	// constants, because collect imports config and the dependency cannot go both
	// ways. TestDeclaredRepoScopedKeysAreWhatTheParsersWrite pins them.
	RepoScopedKeys []string

	// Toolchain names the toolchain whose output this is, or is empty for a
	// format no single toolchain owns.
	//
	// It exists for one caller: the snapshot's toolchain fingerprint, which used
	// to run `go env` against every repo unconditionally. On a repo with no Go in
	// it that probe either answered with the ambient Go version, which can never
	// move when the repo's actual runtime does, or failed and recorded a
	// placeholder that compares equal to every other failure. Both publish "the
	// toolchain did not change" as a fact nobody established.
	//
	// SARIF leaves this empty on purpose. It is the one format here that says
	// nothing about what produced it, so a repo whose only step is a lint run
	// genuinely has no toolchain this tool can name, and saying so is the honest
	// answer rather than guessing Go.
	Toolchain string
}

// ToolchainGo is the value Toolchain carries for Go's own formats. It is a
// constant rather than a literal because the collector matches on it.
const ToolchainGo = "go"

// formats is the one table. Everything else derives from it, so a format cannot
// be accepted by validation without also having a policy.
//
// A name only belongs here once the collect package can actually read it.
// Validating a format the tool cannot parse would let a step run a whole test
// suite and record silence, which is the failure this tool is built to refuse.
// TestEveryConfiguredFormatHasAParser pins the two lists together.
var formats = map[Format]formatPolicy{
	// Everything a coverage profile writes is scoped by package, so it collides
	// with nothing and needs no entry below.
	FormatGoCoverprofile: {Toolchain: ToolchainGo},
	FormatGoTestJSON: {
		Toolchain: ToolchainGo,
		// Both are repo-level by nature rather than by oversight: one counts the
		// packages that carry no tests, the other the packages that would not
		// build, and neither is a property of any single step. That is exactly why
		// a second test format in the same repo cannot also write them.
		RepoScopedKeys: []string{"pkg.without_tests", "test.build_failed"},
	},
	FormatSARIF: {Repeatable: true, NonZeroExitIsNormal: true},
	// Not repeatable: a repo has one module graph. And a non-zero exit is a real
	// failure here rather than a way of reporting findings, which is load
	// bearing: an unreachable proxy makes `go list -m -u` exit non-zero with an
	// EMPTY stdout, and treating that as normal would turn it into a parse of
	// nothing.
	FormatGoListModules: {
		Toolchain:      ToolchainGo,
		RepoScopedKeys: []string{"deps.total", "deps.age_median_days", "deps.outdated_direct"},
	},
	// Repeatable, and the second format to earn it. A polyglot repo genuinely
	// runs pytest beside vitest, and the parser files every row under a scope
	// that starts with the step's own name, so two of them sum rather than
	// collide. No toolchain: the document says nothing about what produced it,
	// which is the whole reason one parser covers four runners.
	//
	// A red suite is a real fact about the repo, so a non-zero exit is NOT
	// treated as normal here, exactly as it is not for `go test`.
	FormatJUnitXML: {Repeatable: true},
	// Not repeatable, and this one is a genuine limit rather than an oversight.
	// Every row is scoped by the source file the tracefile names, which is a
	// property of the repo rather than of the step, so two steps covering
	// overlapping files would write the same scope twice. A repo producing two
	// tracefiles should merge them before this reads one.
	FormatLCOV: {},
	// Not repeatable, and no toolchain: a lockfile is written by a package
	// manager rather than by the runtime whose version the fingerprint wants. Both
	// own the same repo-level row, so declaring one of each in a repo is rejected
	// at load rather than discovered when the second step fails every night.
	FormatNPMLockfile: {RepoScopedKeys: []string{"deps.total"}},
	FormatBunLockfile: {RepoScopedKeys: []string{"deps.total"}},
}

// formatOrder fixes the order for help text and error messages, because ranging
// a map would shuffle them between runs.
var formatOrder = []Format{
	FormatGoCoverprofile,
	FormatGoTestJSON,
	FormatSARIF,
	FormatGoListModules,
	FormatJUnitXML,
	FormatLCOV,
	FormatNPMLockfile,
	FormatBunLockfile,
}

// Formats lists every readable format, in a stable order.
func Formats() []string {
	out := make([]string, 0, len(formatOrder))
	for _, f := range formatOrder {
		out = append(out, string(f))
	}
	return out
}

// Repeatable reports whether two steps in one repo may both use this format.
func (f Format) Repeatable() bool { return formats[f].Repeatable }

// NonZeroExitIsNormal reports whether a non-zero exit from this format's
// producer means findings rather than failure.
func (f Format) NonZeroExitIsNormal() bool { return formats[f].NonZeroExitIsNormal }

// RepoScopedKeys lists the metric keys this format writes at repo scope. Two
// steps whose lists intersect would write the same row twice.
func (f Format) RepoScopedKeys() []string { return formats[f].RepoScopedKeys }

// Toolchain names the toolchain this format's producer belongs to, or is empty
// for a format no single toolchain owns. An unknown format answers empty, which
// is the same answer it deserves: nothing can be inferred from a name the tool
// does not recognize.
func (f Format) Toolchain() string { return formats[f].Toolchain }

// Known reports whether this is a format the tool can read.
func (f Format) Known() bool {
	_, ok := formats[f]
	return ok
}

// checkFormat validates one format name, naming the alternatives. An unknown
// format that fell through to "parse nothing" would run a whole test suite and
// record silence.
func checkFormat(label, field string, f Format) error {
	if f.Known() {
		return nil
	}
	return fmt.Errorf("%s: unknown %s %q, want one of: %s",
		label, field, f, strings.Join(Formats(), ", "))
}
