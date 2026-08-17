package collect

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// goRepo is a repo whose steps read a Go format, so envFingerprint reaches the
// `go env` probe. Before the probe became conditional every repo did; now the
// formats are what select it, and a fixture that forgets them exercises the
// unidentified branch while looking like it tests the Go one.
func goRepo(dir string) config.Repo {
	return config.Repo{
		Path: dir,
		Signals: []config.Signal{{
			Name:         "coverage",
			Command:      []string{"go", "test", "./..."},
			StdoutFormat: config.FormatGoTestJSON,
		}},
	}
}

// `go env GOWORK` prints one of three things and only one of them is a path:
// the literal "off" when the workspace is disabled, the go.work path when one
// is active, and the empty string when there is none.
//
// Reading "any non-empty value means a workspace is on" therefore recorded
// GOWORK=off as gowork=on, the exact inverse of the truth for the one case the
// fingerprint exists to detect. Measured against Go 1.26:
//
//	GOWORK=off go env GOWORK  ->  off
//	(inside a workspace)      ->  /Users/.../go.work
//
// A wrong fingerprint is worse than none: the report uses it to decide whether
// two snapshots are comparable, so an inverted one silently permits exactly the
// diff it was added to refuse.
func TestEnvFingerprintReadsGoworkOffAsOff(t *testing.T) {
	cases := []struct {
		name   string
		gowork string
		want   string
	}{
		{"workspace explicitly disabled", "off", "gowork=off"},
		{"no workspace at all", "", "gowork=off"},
		{"workspace active", "/Users/someone/Dev/go.work", "gowork=on"},
		{"path with surrounding space", "  /tmp/go.work  ", "gowork=on"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fingerprintFrom("go1.26.5\n" + tc.gowork + "\n")
			if !strings.Contains(got, tc.want) {
				t.Errorf("go env GOWORK printed %q, fingerprint is %q, want it to contain %q",
					tc.gowork, got, tc.want)
			}
		})
	}
}

func TestEnvFingerprintCarriesTheGoVersion(t *testing.T) {
	if got := fingerprintFrom("go1.26.5\noff\n"); !strings.Contains(got, "go=go1.26.5") {
		t.Errorf("fingerprint %q does not carry the reported Go version", got)
	}
}

// A directory where `go env` cannot run at all still has to produce a
// fingerprint, or snapshots become silently incomparable rather than loudly so.
//
// And it has to SAY it failed. Two snapshots that both fall back to the
// placeholder record the same string and therefore compare as the same
// toolchain, so the one boundary this fingerprint exists to flag goes unflagged
// exactly when nothing measured it. Without a diagnostic nothing anywhere says
// so, which is how the sibling git probe has always behaved and this one did not.
func TestEnvFingerprintDegradesToUnknownAndSaysSo(t *testing.T) {
	// A working directory that does not exist, so the process cannot start at
	// all. Setting PATH in the env does NOT work here, and that is worth writing
	// down: run.Options.Env is APPENDED to the parent environment rather than
	// replacing it, so `go` still resolves and the probe succeeds. The version of
	// this test that did that asserted only that the result started with "go=",
	// which a successful run also satisfies, so it never once exercised the
	// failure path it was named for.
	missing := filepath.Join(t.TempDir(), "gone")

	got, diags := envFingerprint(context.Background(), goRepo(missing), nil)
	if got == "" {
		t.Fatal("no fingerprint recorded when go env could not run")
	}
	if !EnvIsUnidentified(got) {
		t.Errorf("fingerprint %q: a probe that could not run must say unidentified rather than guess", got)
	}
	if len(diags) == 0 {
		t.Error("the fingerprint silently fell back to a placeholder, so nothing tells anyone the toolchain was never identified")
	}
	for _, d := range diags {
		if d.Severity != SeverityWarn {
			t.Errorf("severity %q: a missing fingerprint costs a comparison, not the snapshot", d.Severity)
		}
	}

	// The control: a probe that works says nothing, or every snapshot carries a
	// warning about a thing that went fine.
	real, realDiags := envFingerprint(context.Background(), goRepo(t.TempDir()), nil)
	if len(realDiags) != 0 {
		t.Errorf("a successful probe emitted %d diagnostics: %+v", len(realDiags), realDiags)
	}
	if EnvIsUnidentified(real) {
		t.Skip("go env is not runnable in this environment, so the control cannot distinguish itself")
	}
}

// A repo that runs no Go-format step must not be fingerprinted with `go env`.
//
// This is the whole reason the probe became conditional. On a machine with Go
// installed that call SUCCEEDS from any directory, so a TypeScript repo used to
// record the ambient Go version: a string that cannot move when its actual
// runtime does, and does move when someone upgrades Go on the collector. Both
// halves are wrong and neither is visible.
//
// The assertion is deliberately not "the result is not a Go version". A machine
// without Go would satisfy that while the bug was fully present.
func TestEnvFingerprintRefusesToGuessForANonGoRepo(t *testing.T) {
	repo := config.Repo{
		Path: t.TempDir(),
		Signals: []config.Signal{{
			Name:         "lint",
			Command:      []string{"eslint", "."},
			StdoutFormat: config.FormatSARIF,
		}},
	}

	got, diags := envFingerprint(context.Background(), repo, nil)
	if !EnvIsUnidentified(got) {
		t.Errorf("fingerprint %q for a repo with no Go step; nothing here established a toolchain, so nothing may be recorded as one", got)
	}
	if strings.Contains(got, "go=") {
		t.Errorf("fingerprint %q carries a Go version for a repo that runs no Go toolchain", got)
	}
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic explaining the gap, got %d: %+v", len(diags), diags)
	}
	if !strings.Contains(diags[0].Message, "fingerprint") {
		t.Errorf("diagnostic does not name the setting that fixes it: %s", diags[0].Message)
	}

	// The control: the same repo with a Go step reaches the probe. Without this,
	// a version of envFingerprint that returned unidentified unconditionally
	// would pass everything above.
	if withGo, _ := envFingerprint(context.Background(), goRepo(repo.Path), nil); withGo == got {
		t.Skip("go env is not runnable in this environment, so the control cannot distinguish itself")
	}
}

// A configured probe wins over the derived one, and its output becomes the
// fingerprint.
func TestEnvFingerprintUsesTheConfiguredProbe(t *testing.T) {
	repo := config.Repo{
		Path:        t.TempDir(),
		Fingerprint: []string{"echo", "v22.11.0"},
		// A Go step as well, so this also pins the precedence: an explicit probe
		// is the operator overriding the derivation, not a fallback for when the
		// derivation finds nothing.
		Signals: goRepo("").Signals,
	}

	got, diags := envFingerprint(context.Background(), repo, nil)
	if len(diags) != 0 {
		t.Errorf("a successful probe emitted %d diagnostics: %+v", len(diags), diags)
	}
	if got != "echo=v22.11.0" {
		t.Errorf("fingerprint %q, want the probe name and its output", got)
	}
}

// A probe that exits 0 and prints nothing is a failure, not an empty
// fingerprint.
//
// `command -v` and friends do exactly this when they find nothing, and an empty
// string stored as the fingerprint compares equal to every other empty string,
// which is the defect the unidentified sentinel exists to close. Storing it
// would reintroduce the bug through the one door an operator controls.
func TestEnvFingerprintRejectsASilentProbe(t *testing.T) {
	repo := config.Repo{
		Path:        t.TempDir(),
		Fingerprint: []string{"true"},
	}

	got, diags := envFingerprint(context.Background(), repo, nil)
	if !EnvIsUnidentified(got) {
		t.Errorf("fingerprint %q from a probe that printed nothing", got)
	}
	if len(diags) != 1 {
		t.Fatalf("want one diagnostic, got %d: %+v", len(diags), diags)
	}
	if !strings.Contains(diags[0].Message, "printed nothing") {
		t.Errorf("diagnostic does not say what went wrong: %s", diags[0].Message)
	}
}

// The legacy placeholder still reads as unidentified.
//
// Snapshots carrying "go=unknown" are in databases now. If it stopped counting,
// every one of them would compare as a real toolchain against a newer snapshot
// and the report would announce a change that never happened.
func TestEnvIsUnidentifiedCoversTheLegacyPlaceholder(t *testing.T) {
	for _, env := range []string{"", EnvUnidentified, "go=unknown"} {
		if !EnvIsUnidentified(env) {
			t.Errorf("%q reads as an identified toolchain", env)
		}
	}
	// The control, or the function could return true for everything and the loop
	// above would still pass.
	if EnvIsUnidentified("go=go1.26.5;gowork=off") {
		t.Error("a real fingerprint reads as unidentified, so nothing would ever be comparable")
	}
}
