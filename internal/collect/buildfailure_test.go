package collect_test

import (
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// brokenBuildStream is what the toolchain emits when one package's test binary
// will not compile and another's runs fine.
//
// The failing package produces a package-level "fail" with no test events in it
// and an Elapsed of zero, which is byte-identical to how a package that built
// fine and contains no tests reports itself. FailedBuild is the only thing in
// the stream that tells them apart, and this struct did not used to read it.
const brokenBuildStream = `{"Action":"run","Package":"example.com/m/alpha","Test":"TestOne"}
{"Action":"pass","Package":"example.com/m/alpha","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"example.com/m/alpha","Elapsed":0.02}
{"Action":"output","Package":"example.com/m/broken","Output":"FAIL\texample.com/m/broken [build failed]\n"}
{"Action":"fail","Package":"example.com/m/broken","Elapsed":0,"FailedBuild":"example.com/m/broken"}
`

func brokenBuildRepo(t *testing.T) collect.Result {
	t.Helper()
	dir := repoDir(t)
	step := coverageStep("cat", writeFile(t, dir, "stream.json", brokenBuildStream))
	step.Artifact = ""
	step.ArtifactFormat = ""
	step.StdoutFormat = config.FormatGoTestJSON
	return collectOnce(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}})
}

// A package that would not build is not a package without tests.
//
// It may be full of them and nobody can say, because none of them ran. Counting
// it as untested published a fabricated measurement rather than a missing one,
// which is the worse half of this project's recurring bug and is what shipped:
// break one build and the report announces a package that has no tests, a fact
// nothing established.
func TestABrokenBuildIsNotAPackageWithoutTests(t *testing.T) {
	res := brokenBuildRepo(t)

	if got := metric(t, res, collect.KeyPkgWithoutTest, ""); got != 0 {
		t.Errorf("packages without tests: got %v, want 0. The package that would not build is not known to have no tests, and the one that built has one.", got)
	}
	if got := metric(t, res, collect.KeyTestBuildFailed, ""); got != 1 {
		t.Errorf("packages that would not build: got %v, want 1", got)
	}
	// The package that did build is still counted. A broken sibling costs its
	// own numbers and nothing else.
	if got := metric(t, res, collect.KeyTestCount, "example.com/m/alpha"); got != 1 {
		t.Errorf("the working package's test count: got %v, want 1", got)
	}
	// And the broken one contributes no row at all, rather than a zero. Nothing
	// knows how many tests it has, and a zero here is the exact lie this key
	// exists to prevent.
	for _, key := range []string{collect.KeyTestCount, collect.KeyTestFailed, collect.KeyTestSkipped} {
		for _, m := range res.Metrics {
			if m.Key == key && m.Scope == "example.com/m/broken" {
				t.Errorf("%s was recorded as %v for a package whose tests never ran", key, m.Value)
			}
		}
	}
}

// The operator has to be told, by name, or the numbers above are just quietly
// smaller than last week's.
func TestABrokenBuildIsReportedAndDegradesTheSnapshot(t *testing.T) {
	res := brokenBuildRepo(t)

	if res.Snapshot.Status != store.StatusPartial {
		t.Errorf("Status: got %q, want partial. The counts are real for what built and short for what did not.\nDiagnostics:\n%s",
			res.Snapshot.Status, diagText(res))
	}
	// Stored, not just printed. Before this the snapshot kept no reason for a
	// warning-degraded run, and the report listed the repo under Collection
	// problems as a bare name.
	if !strings.Contains(res.Snapshot.Error, "example.com/m/broken") {
		t.Errorf("the snapshot does not name the package that would not build, got %q", res.Snapshot.Error)
	}
	if !strings.Contains(res.Snapshot.Error, "partial sum") {
		t.Errorf("the snapshot does not say what the failure cost, got %q", res.Snapshot.Error)
	}
}

// The anti-vacuity control for the pair above: the same shape of stream with
// nothing broken records a measured zero and stays clean.
func TestAWorkingBuildRecordsAMeasuredZero(t *testing.T) {
	dir := repoDir(t)
	stream := `{"Action":"pass","Package":"example.com/m/alpha","Test":"TestOne","Elapsed":0.01}
{"Action":"pass","Package":"example.com/m/alpha","Elapsed":0.02}
`
	step := coverageStep("cat", writeFile(t, dir, "stream.json", stream))
	step.Artifact = ""
	step.ArtifactFormat = ""
	step.StdoutFormat = config.FormatGoTestJSON
	res := collectOnce(t, config.Repo{Name: "svc", Path: dir, Signals: []config.Signal{step}})

	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok.\nDiagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}
	if !hasMetric(res, collect.KeyTestBuildFailed) {
		t.Fatal("no build-failure count was written, so a run where everything compiled is indistinguishable from one nobody checked")
	}
	if got := metric(t, res, collect.KeyTestBuildFailed, ""); got != 0 {
		t.Errorf("packages that would not build: got %v, want a measured 0", got)
	}
	// A clean snapshot stays quiet. The reason field is for a snapshot that cost
	// something, and picking up an ordinary warning here would put text under
	// Collection problems for a repo that has none.
	if res.Snapshot.Error != "" {
		t.Errorf("a clean snapshot recorded a reason: %q", res.Snapshot.Error)
	}
}
