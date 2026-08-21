package cli_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// showDoc is the show payload, decoded the safe way.
type showDoc struct {
	Repo         string                `json:"repo"`
	Status       string                `json:"status"`
	CollectedAt  *string               `json:"collected_at"`
	HasSnapshot  bool                  `json:"has_snapshot"`
	Env          *string               `json:"env"`
	Degraded     *bool                 `json:"degraded"`
	Signals      *[]signalDoc          `json:"signals"`
	Measurements *[]showMeasurementDoc `json:"measurements"`
	Error        string                `json:"error"`
}

type showMeasurementDoc struct {
	Signal string `json:"signal"`
	// A pointer for the reason every measurement group in this suite is: a
	// struct of plain floats decodes a signal nothing measured straight back
	// into 0.0, and a suite that reads the wire unsafely cannot catch the bug
	// the wire shape exists to prevent.
	Measurement *showLevelDoc `json:"measurement"`
}

type showLevelDoc struct {
	Value   float64 `json:"value"`
	Covered *int    `json:"covered"`
	Total   *int    `json:"total"`
}

func decodeShow(t *testing.T, out string) showDoc {
	t.Helper()
	var doc showDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unparseable show json: %v\n%s", err, out)
	}
	if doc.Measurements == nil {
		t.Fatal("measurements is null rather than a list, so a consumer reading its length crashes on a repo that measured nothing")
	}
	return doc
}

func levelFor(t *testing.T, doc showDoc, signal string) *showLevelDoc {
	t.Helper()
	for _, m := range *doc.Measurements {
		if m.Signal == signal {
			return m.Measurement
		}
	}
	t.Fatalf("no measurement entry for %q in %+v", signal, *doc.Measurements)
	return nil
}

// show is the query that was being rebuilt out of Python one-liners: every
// signal on the newest snapshot, with the counts behind the rates.
//
// repos says whether a repo was collected and what its coverage is, report needs
// two snapshots to say anything, and history charts one signal over time. None
// of the three answers "what does this repo measure right now".
func TestShowPublishesEverySignalOnTheNewestSnapshot(t *testing.T) {
	dir := t.TempDir()
	repo := repoDir(t, dir, "webapp", "")
	writeFile(t, filepath.Join(repo, "coverage.lcov"), sampleLCOV)
	cfgPath := writeConfig(t, dir, fmt.Sprintf("database: %q\nrepos:\n%s",
		filepath.Join(dir, "metrics.db"), ingestLCOVEntry("webapp", repo, "coverage.lcov")))

	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, "show", "--config", cfgPath, "--repo", "webapp", "--format", "json")
	if err != nil {
		t.Fatalf("show: %v (stderr: %s)", err, stderr)
	}
	doc := decodeShow(t, stdout)

	if doc.Repo != "webapp" || !doc.HasSnapshot {
		t.Fatalf("show described the wrong thing: %+v", doc)
	}
	// Every signal has an entry, measured or not, so a consumer walks a fixed
	// shape rather than discovering which keys happen to be present.
	if len(*doc.Measurements) != 14 {
		t.Errorf("got %d measurement entries, want one per registered signal", len(*doc.Measurements))
	}
	if doc.Signals == nil || len(*doc.Signals) != 14 {
		t.Errorf("the catalog is missing or short, so a consumer walking fourteen measurements has no legend for them: %+v", doc.Signals)
	}

	// The counts behind the rate, which is what setting a floor needs and what
	// no other payload would give without a second query.
	lines := levelFor(t, doc, "coverage_lines")
	if lines == nil {
		t.Fatal("coverage_lines is null on a repo whose tracefile measured 75 percent")
	}
	if lines.Value != 75 || lines.Covered == nil || *lines.Covered != 3 || lines.Total == nil || *lines.Total != 4 {
		t.Errorf("coverage_lines = %+v, want 75 percent of 3/4 lines", lines)
	}

	// And the unit it did not measure stays null rather than borrowing the one
	// it did.
	if stmts := levelFor(t, doc, "coverage"); stmts != nil {
		t.Errorf("coverage = %+v, want null: nothing measured statements in this repo", stmts)
	}
	if tests := levelFor(t, doc, "tests"); tests != nil {
		t.Errorf("tests = %+v, want null: no test report was collected", tests)
	}
}

// A repo the database has never heard of is an answer, not an error.
func TestShowDistinguishesANeverCollectedRepo(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha")

	stdout, stderr, err := runCLI(t, "show", "--config", cfgPath, "--repo", "alpha", "--format", "json")
	if err != nil {
		t.Fatalf("show: %v (stderr: %s)", err, stderr)
	}
	doc := decodeShow(t, stdout)

	if doc.HasSnapshot {
		t.Error("has_snapshot is true for a repo nobody has collected")
	}
	if doc.CollectedAt != nil {
		t.Errorf("collected_at = %q, want null rather than an empty string a consumer has to know means never", *doc.CollectedAt)
	}
	if doc.Degraded != nil {
		t.Errorf("degraded = %v on a run that never happened", *doc.Degraded)
	}
	for _, m := range *doc.Measurements {
		if m.Measurement != nil {
			t.Errorf("%s measured %v on a repo that was never collected", m.Signal, m.Measurement.Value)
		}
	}
}

// show needs a repo, and a typo names what exists.
func TestShowRequiresARepoAndRejectsAnUnknownOne(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha", "bravo")

	for _, tc := range []struct{ name, arg, want string }{
		{"missing", "", "needs --repo"},
		{"unknown", "wroker", "no repo named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"show", "--config", cfgPath}
			if tc.arg != "" {
				args = append(args, "--repo", tc.arg)
			}
			stdout, stderr, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("accepted it, rendering %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not say %q: %q", tc.want, stderr)
			}
		})
	}
}

// report exits 0 whatever it found, which is documented and is still how a
// scheduled job goes on looking healthy after collection stops working.
//
// The report is written either way. Withholding the answer because it is bad
// news would make the flag cost information rather than carry it, and it is the
// answer somebody ran the command for.
func TestReportFailOnProblems(t *testing.T) {
	cfgPath := staleAndBrokenFleet(t)
	_, _, _ = runCLI(t, "collect", "--config", cfgPath)

	// The default is unchanged, which is the control: a flag that altered exit
	// codes by existing would break every wrapper already parsing this command.
	if _, _, err := runCLI(t, "report", "--config", cfgPath); err != nil {
		t.Errorf("report exited non-zero without --fail-on, which changes the contract for every existing caller: %v", err)
	}

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--fail-on", "problems")
	if err == nil {
		t.Fatal("--fail-on problems exited 0 with two repos that did not collect cleanly")
	}
	if stdout == "" {
		t.Error("the report was withheld, but it is the thing the caller ran this for")
	}
	if !strings.Contains(stderr, "did not collect cleanly") {
		t.Errorf("stderr does not say what was found, and exit 1 from this binary always explains itself: %q", stderr)
	}
	for _, name := range []string{"stale", "broken"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("stderr does not name %s: %q", name, stderr)
		}
	}
}

// A clean fleet passes the same gate, or the flag says nothing about the data.
func TestReportFailOnPassesOnACleanFleet(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha", "bravo")
	if _, stderr, err := runCLI(t, "collect", "--config", cfgPath); err != nil {
		t.Fatalf("collect: %v (stderr: %s)", err, stderr)
	}

	if _, stderr, err := runCLI(t, "report", "--config", cfgPath, "--fail-on", "problems"); err != nil {
		t.Errorf("--fail-on problems failed on a fleet that collected cleanly: %v (stderr: %s)", err, stderr)
	}
}

// An unknown value is refused rather than read as none.
//
// Silently accepting --fail-on problmes and exiting 0 would hand a pipeline the
// reassurance it had asked to have withheld, which is worse than not offering
// the flag.
func TestReportRejectsAnUnknownFailOn(t *testing.T) {
	cfgPath, _ := fleet(t, "alpha")

	stdout, stderr, err := runCLI(t, "report", "--config", cfgPath, "--fail-on", "problmes")
	if err == nil {
		t.Fatalf("accepted an unknown --fail-on, rendering %q", stdout)
	}
	if !strings.Contains(stderr, "problems") {
		t.Errorf("stderr does not list the valid values: %q", stderr)
	}
}
