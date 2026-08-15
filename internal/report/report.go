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

var funcs = template.FuncMap{
	// pct renders a coverage percentage.
	"pct": func(f float64) string { return fmt.Sprintf("%.1f%%", f) },

	// pctOf is pct over a figure that might not exist. An added package has no
	// earlier percentage and a removed one has no later percentage, and printing
	// either as 0.0% says the package climbed from nothing or collapsed to it.
	//
	// It is separate from pct for the same reason points is separate from pts:
	// text/template will silently take the address of an addressable float64 to
	// satisfy a *float64 parameter, so one pointer-typed func would accept a
	// plain value by accident and the nil branch would look dead.
	"pctOf": func(f *float64) string {
		if f == nil {
			return "not measured"
		}
		return fmt.Sprintf("%.1f%%", *f)
	},

	// pts renders a change in percentage points, always signed, so a reader
	// never has to work out which direction it went.
	"pts": func(f float64) string { return fmt.Sprintf("%+.1f pts", f) },

	// points is pts over a delta that might not exist. Absent is not zero, and
	// the template must never be able to print a synthetic "+0.0 pts" for a repo
	// that has nothing to compare against.
	//
	// It is deliberately separate from pts rather than one func over any: text/
	// template will silently take the address of an addressable float64 to
	// satisfy a *float64 parameter, so a single pointer-typed func would accept
	// a plain value by accident and the nil branch would look dead.
	"points": func(f *float64) string {
		if f == nil {
			return "no baseline yet"
		}
		return fmt.Sprintf("%+.1f pts", *f)
	},

	"signed": func(n *int) string {
		if n == nil {
			return "no baseline yet"
		}
		return fmt.Sprintf("%+d", *n)
	},

	// repos renders a count of repos with its noun, so a single-repo config
	// cannot produce "out of 1 repos in this config". It goes through the same
	// helper as days for the same reason.
	"repos": func(n int) string { return count(float64(n), "repo") },

	// days renders the reporting window. It steps down to hours and minutes
	// because a sub-day window is a real setting: --window 12h through %.0f
	// prints "0 days", which reads as comparing a repo against right now.
	//
	// The count is rounded before the noun is chosen, so a 1.4 day window says
	// "1 day" rather than "1 days".
	"days": func(d float64) string {
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
	},

	// ne is not a convenience shadow of the builtin: the builtin cannot compare
	// a *int against an int and fails with "incompatible types for comparison",
	// which is what the template needs for an optional test delta. Removing this
	// breaks rendering at run time, not at compile time.
	"ne": func(n *int, v int) bool { return n != nil && *n != v },

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
}

// count renders a rounded quantity with a singular or plural noun. Rounding
// first is what keeps "1 days" out of the report: %.0f on 1.4 rounds for
// display but the grammar would still be decided by the unrounded value.
func count(n float64, unit string) string {
	rounded := math.Round(n)
	if rounded == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%.0f %ss", rounded, unit)
}

var tmpl = template.Must(template.New("report").Funcs(funcs).Parse(markdownTemplate))

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
	enc := json.NewEncoder(w)
	// Not indented, on purpose. JSON here is the machine format and markdown is
	// the human one, so paying for indentation in the machine format buys
	// readability for a reader who has the better format available anyway. It
	// was measured at roughly a fifth of the payload, and a tokenizer handles
	// runs of spaces worse than the byte count suggests. Anyone who does want to
	// read it can pipe it through jq.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(BuildSection(rep, sec, scope)); err != nil {
		return fmt.Errorf("report: rendering json: %w", err)
	}
	return nil
}

// MarkdownString is a convenience for callers that want the text in hand.
func MarkdownString(rep delta.Report, sec Section, scope Scope) (string, error) {
	var b strings.Builder
	if err := Markdown(&b, rep, sec, scope); err != nil {
		return "", err
	}
	return b.String(), nil
}
