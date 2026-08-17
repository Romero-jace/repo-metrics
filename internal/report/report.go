// Package report renders a computed delta report as markdown or JSON.
//
// Both formats are rendered from one View, so they cannot disagree about a
// number. The markdown is the product; the JSON exists so something downstream
// (a script, an agent asking what regressed, a static-site build) can consume the
// same figures without re-deriving them.
package report

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"text/template"

	"github.com/Romero-jace/repo-metrics/internal/delta"
)

//go:embed report.md.tmpl
var markdownTemplate string

//go:embed history.md.tmpl
var historyTemplate string

// funcs is what the template can call.
//
// It is deliberately small. Everything that used to format a typed pointer for
// the template (pct, points, signed, and an ne that shadowed the builtin because
// the builtin cannot compare a *int to an int) is gone: SignalCell formats in
// Go now, so the template never holds a pointer and never needs to know a unit.
// What is left is the culprit list, which is coverage-specific, and the prose
// helpers.
var funcs = template.FuncMap{
	// pts renders a change in percentage points, always signed, so a reader
	// never has to work out which direction it went.
	"pts": func(f float64) string { return fmt.Sprintf("%+.1f pts", f) },

	// pctOf is a percentage over a figure that might not exist. An added package
	// has no earlier percentage and a removed one has no later percentage, and
	// printing either as 0.0% says the package climbed from nothing or collapsed
	// to it.
	"pctOf": func(f *float64) string {
		if f == nil {
			return "not measured"
		}
		return fmt.Sprintf("%.1f%%", *f)
	},

	// anyPackageChurn goes through the same nil-reading accessors the churn
	// section itself uses, so a repo that measured no coverage can never make the
	// heading appear over rows that then print nothing.
	"anyPackageChurn": func(repos []RepoView) bool {
		for _, r := range repos {
			if len(r.AddedPackages()) > 0 || len(r.RemovedPackages()) > 0 {
				return true
			}
		}
		return false
	},

	// repos renders a count of repos with its noun, so a single-repo config
	// cannot produce "out of 1 repos in this config". It goes through the same
	// helper as days for the same reason.
	"repos": func(n int) string { return count(float64(n), "repo") },

	// collections renders a count of collection runs with its noun, so a repo
	// with one snapshot in the window cannot produce "1 collections". Same
	// helper as repos and days: three nouns, one rule about when to pluralize
	// them.
	"collections": func(n int) string { return count(float64(n), "collection") },

	// days renders the reporting window. It steps down to hours and minutes
	// because a sub-day window is a real setting: --window 12h through %.0f
	// prints "0 days", which reads as comparing a repo against right now.
	//
	// The count is rounded before the noun is chosen, so a 1.4 day window says
	// "1 day" rather than "1 days".
	"days": humanDays,

	// anyEnvWarned gates the footnote that explains the table's toolchain
	// marker. It goes through the same predicate the rows do so the footnote can
	// never explain a marker that is not there, or be missing under one that is.
	"anyEnvWarned": func(repos []RepoView) bool {
		for _, r := range repos {
			if r.EnvWarned() {
				return true
			}
		}
		return false
	},

	// anyEnvUnknownWarned gates the footnote for rows whose toolchain was never
	// identified at all, which is a different statement from one that changed.
	"anyEnvUnknownWarned": func(repos []RepoView) bool {
		for _, r := range repos {
			if r.EnvUnknownWarned() {
				return true
			}
		}
		return false
	},

	// anyDirtyWarned does the same for the uncommitted changes marker. Two
	// near-identical closures rather than one taking a predicate, because a
	// template FuncMap cannot be handed a method value and the indirection would
	// cost more than the duplication saves.
	"anyDirtyWarned": func(repos []RepoView) bool {
		for _, r := range repos {
			if r.DirtyWarned() {
				return true
			}
		}
		return false
	},
}

// count renders a rounded quantity with a singular or plural noun. Rounding
// first is what keeps "1 days" out of the report: %.0f on 1.4 rounds for
// display but the grammar would still be decided by the unrounded value.
// humanDays renders a number of days at whatever unit keeps it readable. It is
// a plain function as well as a template helper because buildRepo renders the
// baseline span with it, and two ladders that disagree about when a day becomes
// hours would be two ways of saying the same thing.
func humanDays(d float64) string {
	switch {
	case d >= 1:
		return count(d, "day")
	case d*24 >= 1:
		return count(d*24, "hour")
	case d*24*60 >= 1:
		return count(d*24*60, "minute")
	default:
		return "less than a minute"
	}
}

func count(n float64, unit string) string {
	rounded := math.Round(n)
	if rounded == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%.0f %ss", rounded, unit)
}

var tmpl = template.Must(template.New("report").Funcs(funcs).Parse(markdownTemplate))

var historyTmpl = template.Must(template.New("history").Funcs(funcs).Parse(historyTemplate))

// renderJSON is the one place this package decides how JSON reaches the wire.
//
// Not indented, on purpose. JSON here is the machine format and markdown is the
// human one, so paying for indentation in the machine format buys readability
// for a reader who has the better format available anyway. It was measured at
// roughly a fifth of the payload, and a tokenizer handles runs of spaces worse
// than the byte count suggests. Anyone who does want to read it can pipe it
// through jq.
//
// A second payload encoding its own would be free to drift on both the
// indentation and the HTML escaping, and the difference would surface only in a
// consumer's parser.
func renderJSON(w io.Writer, v any, what string) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("report: rendering %s json: %w", what, err)
	}
	return nil
}

// Markdown renders the report for humans, narrowed to sec and to scope.
func Markdown(w io.Writer, rep delta.Report, sec Section, scope Scope) error {
	if err := tmpl.Execute(w, BuildSection(rep, sec, scope)); err != nil {
		return fmt.Errorf("report: rendering markdown: %w", err)
	}
	return nil
}

// JSON renders the same numbers for machines, narrowed the same way. Both
// formats go through BuildSection, so neither can include a section the other
// left out or disagree about what the report covers.
func JSON(w io.Writer, rep delta.Report, sec Section, scope Scope) error {
	return renderJSON(w, BuildSection(rep, sec, scope), "report")
}

// RenderReport writes the report in the requested format.
//
// It takes the report's inputs rather than a built view, because sec and scope
// are narrowing inputs and applying them twice, or in one renderer and not the
// other, is exactly the drift BuildSection exists to prevent.
func RenderReport(w io.Writer, f Format, rep delta.Report, sec Section, scope Scope) error {
	if f == FormatJSON {
		return JSON(w, rep, sec, scope)
	}
	return Markdown(w, rep, sec, scope)
}

// MarkdownString is a convenience for callers that want the text in hand.
func MarkdownString(rep delta.Report, sec Section, scope Scope) (string, error) {
	var b strings.Builder
	if err := Markdown(&b, rep, sec, scope); err != nil {
		return "", err
	}
	return b.String(), nil
}
