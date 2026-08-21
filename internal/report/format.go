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

// FormatChoice renders the valid names as a choice rather than as a list, for
// help text.
//
// "markdown, json" is a list of two things, and a reader can reasonably take a
// list as something you may pass more than one of. Exactly one is allowed, so
// the connective should say so. Reported from a real fleet: the help text was
// read as though both could be passed at once.
//
// Kept here rather than written out at the four call sites for the reason the
// rest of this file exists: one set of names, one place. A third format changes
// every rendering of this line without anyone remembering to.
func FormatChoice() string {
	names := Formats()
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
