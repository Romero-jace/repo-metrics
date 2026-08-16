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
	// It is the first format here that is not Go-specific, which is the point of
	// choosing it over each linter's native output. One parser covers every
	// language this tool will ever be pointed at.
	FormatSARIF Format = "sarif"
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
}

// formats is the one table. Everything else derives from it, so a format cannot
// be accepted by validation without also having a policy.
//
// A name only belongs here once the collect package can actually read it.
// Validating a format the tool cannot parse would let a step run a whole test
// suite and record silence, which is the failure this tool is built to refuse.
// TestEveryConfiguredFormatHasAParser pins the two lists together.
var formats = map[Format]formatPolicy{
	FormatGoCoverprofile: {},
	FormatGoTestJSON:     {},
	FormatSARIF:          {Repeatable: true, NonZeroExitIsNormal: true},
}

// formatOrder fixes the order for help text and error messages, because ranging
// a map would shuffle them between runs.
var formatOrder = []Format{
	FormatGoCoverprofile,
	FormatGoTestJSON,
	FormatSARIF,
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
