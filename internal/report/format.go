package report

import (
	"fmt"
	"strings"
)

// Format names one rendering of a payload.
//
// It mirrors Section deliberately. Formats used to be two bare constants in
// package cli, an inline comparison chain, a hand-typed flag usage string and a
// hand-typed line of help text, which is four places for one set of names to
// drift apart. Sections have never had that problem because ParseSection and the
// help text both read one list, and this is that list for formats.
type Format string

const (
	// FormatMarkdown is the human rendering and the default everywhere.
	FormatMarkdown Format = "markdown"
	// FormatJSON is the machine rendering.
	FormatJSON Format = "json"
)

// formats is the one list. ParseFormat validates against it and its error
// message is built from it, so a new format cannot be accepted by the parser
// while going unmentioned in the error, or the other way round.
var formats = []Format{FormatMarkdown, FormatJSON}

// ParseFormat turns a command-line value into a Format.
//
// An unknown name is an error rather than a silent fallback to markdown. Quietly
// rendering markdown for --format=jsom hands a broken pipeline a document it
// cannot parse, and the pipeline reports no error because the tool reported
// none.
func ParseFormat(s string) (Format, error) {
	for _, f := range formats {
		if Format(s) == f {
			return f, nil
		}
	}
	return "", fmt.Errorf("report: unknown format %q, valid formats are %s", s, formatList())
}

// formatList renders the valid names for an error message or help text.
func formatList() string {
	names := make([]string, 0, len(formats))
	for _, f := range formats {
		names = append(names, string(f))
	}
	return strings.Join(names, ", ")
}

// Formats lists the valid format names, in the order help text should show them.
// It exists so the CLI's help cannot drift from what ParseFormat accepts.
func Formats() []string { return strings.Split(formatList(), ", ") }
