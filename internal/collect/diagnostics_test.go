package collect_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// sarifRunWithoutResults is a log whose single run carries a tool and no
// "results" property at all.
//
// That run contributed nothing, which is not the same as it having found
// nothing, and it is the shape of this project's own bug class: the counts still
// come back as zeroes, so the only thing standing between the operator and a
// silent zero is the parser's diagnostic.
const sarifRunWithoutResults = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"golangci-lint"}}}]}`

// TestSARIFParserDiagnosticsReachTheResult pins the one bridge between the SARIF
// parser's diagnostics and this package's.
//
// The two types are separate on purpose, so that internal/collect/sarif stays a
// parser of a public format rather than a piece of this tool, and the whole body
// of the function that converts between them can be replaced with `return nil`
// without build, vet, lint or any other test noticing. What that silently drops
// is every caveat the parser knows about, on the only path by which they reach
// an operator.
//
// Both messages asserted here are built inside the sarif package. A message this
// package builds for itself, such as the one for a log naming no tool at all,
// would survive a bridge that discarded its input and would prove nothing.
func TestSARIFParserDiagnosticsReachTheResult(t *testing.T) {
	cases := map[string]struct {
		log  string
		want string
	}{
		"a run with no results property": {
			log:  sarifRunWithoutResults,
			want: "contributed nothing",
		},
		// golangci-lint appends its human summary to the same stream when the
		// SARIF goes to stdout, so this is the ordinary case rather than a
		// corrupted one, and the parser says what it skipped.
		"human text after the log": {
			log:  cleanSARIF + "\n0 issues.\n",
			want: "bytes of non-JSON text after the SARIF log",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := repoDir(t)
			r := config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{
				lintStep("lint", writeFile(t, dir, "report.sarif", tc.log)),
			}}

			res := collectOnce(t, r)

			// The anti-vacuity control. Both logs parse, so the step measured
			// something and the diagnostic is a caveat on a real measurement. A
			// failed snapshot would mean this is asserting about some other path.
			if res.Snapshot.Status != store.StatusOK {
				t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
			}
			if !hasMetric(res, collect.KeyLintFindings) {
				t.Fatalf("no findings count was recorded, so this is not the caveat case at all. Metrics: %+v", res.Metrics)
			}

			d, ok := findDiagnostic(res, tc.want)
			if !ok {
				t.Fatalf("the SARIF parser's diagnostic never arrived, so nothing the parser noticed can reach an operator. Want a message containing %q, got:\n%s",
					tc.want, diagText(res))
			}
			// The severity is carried across the two types too, not just the
			// text. A warn arriving as an error would put this message in the
			// snapshot's error field and read as a snapshot carrying nothing
			// trustworthy.
			if d.Severity != collect.SeverityWarn {
				t.Errorf("severity: got %q, want %q", d.Severity, collect.SeverityWarn)
			}
		})
	}
}

// findDiagnostic returns the first diagnostic whose message contains want, so a
// test can assert on the severity of a specific one rather than on the rendered
// text of all of them. Collection always emits several, since a temp directory
// is not a git checkout.
func findDiagnostic(res collect.Result, want string) (collect.Diagnostic, bool) {
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, want) {
			return d, true
		}
	}
	return collect.Diagnostic{}, false
}
