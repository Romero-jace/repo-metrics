package collect_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

func lcovStep(t *testing.T, dir, body string) config.Repo {
	t.Helper()
	writeFile(t, dir, "lcov.info", body)
	return config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{{
			Name:           "coverage",
			Artifact:       "lcov.info",
			ArtifactFormat: config.FormatLCOV,
			MaxAge:         config.Duration(24 * 60 * 60 * 1e9),
		}},
	}
}

// Line counts land under their own keys, scoped by source file, and never under
// the statement keys.
//
// That separation is the measurement decision this format forced. A Go profile
// counts statements and a tracefile counts lines; several statements on one
// source line collapse to one line, so a repo percentage summing both
// denominators would be arithmetically fine and describe nothing. Sharing the
// keys would have made that the default behavior of every polyglot repo.
func TestLCOVWritesLineKeysNotStatementKeys(t *testing.T) {
	dir := t.TempDir()
	r := lcovStep(t, dir, "SF:src/a.ts\nLF:10\nLH:6\nend_of_record\nSF:src/b.ts\nLF:5\nLH:5\nend_of_record\n")

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	if got := metric(t, res, collect.KeyTotalLines, "src/a.ts"); got != 10 {
		t.Errorf("a.ts total lines: got %v, want 10", got)
	}
	if got := metric(t, res, collect.KeyCoveredLines, "src/a.ts"); got != 6 {
		t.Errorf("a.ts covered lines: got %v, want 6", got)
	}
	if got := metric(t, res, collect.KeyTotalLines, "src/b.ts"); got != 5 {
		t.Errorf("b.ts total lines: got %v, want 5", got)
	}

	for _, key := range []string{collect.KeyCoveredStmts, collect.KeyTotalStmts} {
		if hasMetric(res, key) {
			t.Errorf("%s was written from a tracefile, which counts lines; the two units would then sum into one meaningless rate", key)
		}
	}
}

// A zero-byte tracefile records NOTHING, not zero percent.
//
// The fixture is empty because that is what a real run produced: vitest 4.1.10
// with @vitest/coverage-v8 4.1.4 wrote a zero-byte lcov.info for a full
// 1,964-test run of web-app and exited 0. The artifact was fresh, the
// command succeeded, and the coverage step had done nothing.
//
// Recording a zero here would have published a total collapse in coverage as the
// largest single movement the report could show, off a run that looked healthy
// from every other angle.
func TestAnEmptyTracefileRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	res := collectOnce(t, lcovStep(t, dir, ""))

	for _, key := range []string{collect.KeyCoveredLines, collect.KeyTotalLines} {
		if hasMetric(res, key) {
			t.Errorf("%s was recorded from an empty tracefile, so a coverage step that produced nothing reads as a measurement", key)
		}
	}
	if !strings.Contains(diagText(res), "instruments no files") {
		t.Errorf("nothing explained the empty tracefile:\n%s", diagText(res))
	}

	// The control: a tracefile with records in it does write them, or the
	// assertions above would pass on a parser that never records anything.
	full := collectOnce(t, lcovStep(t, t.TempDir(), "SF:src/a.ts\nLF:10\nLH:6\nend_of_record\n"))
	if !hasMetric(full, collect.KeyTotalLines) {
		t.Fatal("a populated tracefile recorded nothing either, so this test proves nothing")
	}
}

// A Go coverage step and an LCOV step in one repo is the polyglot case, and
// their scopes are both file-or-package names with no step prefix. They coexist
// because they write DIFFERENT keys, which is the whole reason the units were
// separated rather than shared.
func TestAGoProfileAndATracefileCoexist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "coverage.out", "mode: set\nexample.com/m/a/x.go:1.1,2.2 3 1\n")
	writeFile(t, dir, "lcov.info", "SF:src/a.ts\nLF:10\nLH:6\nend_of_record\n")

	day := config.Duration(24 * 60 * 60 * 1e9)
	r := config.Repo{
		Name: "svc",
		Path: dir,
		Signals: []config.Signal{
			{Name: "go-cov", Artifact: "coverage.out", ArtifactFormat: config.FormatGoCoverprofile, MaxAge: day},
			{Name: "ts-cov", Artifact: "lcov.info", ArtifactFormat: config.FormatLCOV, MaxAge: day},
		},
	}

	res := collectOnce(t, r)
	if len(res.Failed) != 0 {
		t.Fatalf("steps failed, which is what a metric-key collision looks like: %v\n%s", res.Failed, diagText(res))
	}
	if !hasMetric(res, collect.KeyTotalStmts) {
		t.Error("the Go profile's statement counts are missing")
	}
	if !hasMetric(res, collect.KeyTotalLines) {
		t.Error("the tracefile's line counts are missing")
	}
}
