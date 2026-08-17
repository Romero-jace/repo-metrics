package collect_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/collect"
	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// A lockfile fills the dependency COUNT and deliberately nothing else.
//
// The two signals it does not fill are the point of the test. Dependency age
// needs a publish timestamp, which no JavaScript lockfile records, and the
// outdated count needs a registry to have been asked. Approximating either from
// what is here would be the same failure as reading `npm outdated` on a checkout
// where nothing is installed: a confident number nobody measured.
func TestALockfileFillsTheCountAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{"lockfileVersion":3,"packages":{
		"": {"name":"svc"},
		"node_modules/a": {"version":"1.0.0"},
		"node_modules/b": {"version":"2.0.0"}
	}}`)

	r := config.Repo{
		Name:        "svc",
		Path:        dir,
		Fingerprint: []string{"echo", "node-v22"},
		Signals: []config.Signal{{
			Name:           "dependencies",
			Artifact:       "package-lock.json",
			ArtifactFormat: config.FormatNPMLockfile,
			Timeout:        config.Duration(time.Minute),
			MaxAge:         config.Duration(24 * time.Hour),
		}},
	}

	res := collectOnce(t, r)
	if res.Snapshot.Status != store.StatusOK {
		t.Fatalf("Status: got %q, want ok. Diagnostics:\n%s", res.Snapshot.Status, diagText(res))
	}

	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 2 {
		t.Errorf("deps.total: got %v, want 2", got)
	}
	for _, key := range []string{collect.KeyDepsAgeMedianDays, collect.KeyDepsOutdatedDirect} {
		if hasMetric(res, key) {
			t.Errorf("%s was written from a lockfile, which records neither a publish time nor a registry's opinion", key)
		}
	}
}

// A project with no dependencies measures zero, because the file parsed and
// listed none. That is the same rule the Go module count follows, and it is why
// deps.total is its own marker rather than sharing one with the other two.
func TestALockfileWithNoDependenciesMeasuresZero(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bun.lock", `{"lockfileVersion":1,"packages":{}}`)

	res := collectOnce(t, config.Repo{
		Name:        "svc",
		Path:        dir,
		Fingerprint: []string{"echo", "bun-1"},
		Signals: []config.Signal{{
			Name:           "dependencies",
			Artifact:       "bun.lock",
			ArtifactFormat: config.FormatBunLockfile,
			Timeout:        config.Duration(time.Minute),
			MaxAge:         config.Duration(24 * time.Hour),
		}},
	})

	if !hasMetric(res, collect.KeyDepsTotal) {
		t.Fatalf("nothing recorded for a lockfile that parsed and listed no packages:\n%s", diagText(res))
	}
	if got := metric(t, res, collect.KeyDepsTotal, ""); got != 0 {
		t.Errorf("deps.total: got %v, want a measured 0", got)
	}
}

// Two lockfile steps in one repo would write the same repo-level row, and that
// is caught at load rather than rediscovered every night when the second one
// fails.
func TestTwoLockfileStepsAreRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package-lock.json", `{"lockfileVersion":3,"packages":{"":{}}}`)
	writeFile(t, dir, "bun.lock", `{"lockfileVersion":1,"packages":{}}`)
	cfgPath := writeFile(t, dir, "repo-metrics.yaml", `
repos:
  - name: svc
    path: `+dir+`
    fingerprint: ["echo", "x"]
    signals:
      - {name: npm, artifact: package-lock.json, artifact_format: npm-lockfile}
      - {name: bun, artifact: bun.lock, artifact_format: bun-lockfile}
`)

	_, err := config.Load(cfgPath)
	if err == nil {
		t.Fatal("Load accepted two steps that would both record deps.total for the repo")
	}
	if !strings.Contains(err.Error(), collect.KeyDepsTotal) {
		t.Errorf("the error does not name the colliding row: %v", err)
	}
}
