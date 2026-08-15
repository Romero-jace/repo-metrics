// Package report renders a computed delta report as markdown or JSON.
//
// Both formats are rendered from one View, so they cannot disagree about a
// number. The markdown is the product; the JSON exists so something downstream
// (an MCP client, a static-site build) can consume the same figures without
// re-deriving them.
package report

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/Romero-jace/repo-metrics/internal/delta"
)

//go:embed report.md.tmpl
var markdownTemplate string

var funcs = template.FuncMap{
	// pct renders a coverage percentage.
	"pct": func(f float64) string { return fmt.Sprintf("%.1f%%", f) },

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

	"days": func(d float64) string {
		if d == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%.0f days", d)
	},

	"ne": func(n *int, v int) bool { return n != nil && *n != v },

	"anyPackageChurn": func(repos []RepoView) bool {
		for _, r := range repos {
			if len(r.AddedPackages) > 0 || len(r.RemovedPackages) > 0 {
				return true
			}
		}
		return false
	},
}

var tmpl = template.Must(template.New("report").Funcs(funcs).Parse(markdownTemplate))

// Markdown renders the report for humans.
func Markdown(w io.Writer, rep delta.Report) error {
	if err := tmpl.Execute(w, Build(rep)); err != nil {
		return fmt.Errorf("report: rendering markdown: %w", err)
	}
	return nil
}

// JSON renders the same numbers for machines.
func JSON(w io.Writer, rep delta.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Build(rep)); err != nil {
		return fmt.Errorf("report: rendering json: %w", err)
	}
	return nil
}

// MarkdownString is a convenience for callers that want the text in hand.
func MarkdownString(rep delta.Report) (string, error) {
	var b strings.Builder
	if err := Markdown(&b, rep); err != nil {
		return "", err
	}
	return b.String(), nil
}
