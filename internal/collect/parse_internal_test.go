package collect

import (
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// The list an operator may name and the list this package can read have to be
// the same list, in both directions.
//
// A format that validates but has no parser lets a step run a whole test suite
// and record silence, which is the failure this tool exists to refuse. A parser
// with no format is the harmless direction and is still worth catching, because
// it means a parser was written and never wired to anything an operator can ask
// for, which is how four signals sat in the database unread for months.
func TestEveryConfiguredFormatHasAParser(t *testing.T) {
	for _, name := range config.Formats() {
		if _, ok := parsers[config.Format(name)]; !ok {
			t.Errorf("config accepts format %q but no parser reads it, so a signal using it would record nothing", name)
		}
	}
	for format := range parsers {
		if !format.Known() {
			t.Errorf("parser %q is wired up but config rejects the name, so nothing can ask for it", format)
		}
	}
	if len(parsers) != len(config.Formats()) {
		t.Errorf("%d parsers against %d configured formats", len(parsers), len(config.Formats()))
	}
}

// parserFor is the one lookup, and an unknown format there is a programming
// error rather than an operator one: config validation rejects those long
// before collection starts. It still has to fail loudly rather than return a
// nil parser for someone to call.
// The sentinel is deliberately a name no format will ever take. It used to be
// "lcov", and two config tests used "junit-xml" the same way; adding junit-xml
// as a real format made both of those fail for a reason that had nothing to do
// with what they were testing, which costs a debugging session every time.
// Anything plausible enough to implement is the wrong sentinel.
func TestParserForRejectsAnUnknownFormat(t *testing.T) {
	if _, err := parserFor(config.Format("not-a-real-format")); err == nil {
		t.Error("parserFor accepted a format nothing can read")
	}
	if _, err := parserFor(config.FormatGoCoverprofile); err != nil {
		t.Errorf("parserFor rejected a real format: %v", err)
	}
}
