package collect

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

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

	got, diags := envFingerprint(context.Background(), missing, nil)
	if got == "" {
		t.Fatal("no fingerprint recorded when go env could not run")
	}
	if got != "go=unknown" {
		t.Errorf("fingerprint %q: a probe that could not run must say unknown rather than guess", got)
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
	real, realDiags := envFingerprint(context.Background(), t.TempDir(), nil)
	if len(realDiags) != 0 {
		t.Errorf("a successful probe emitted %d diagnostics: %+v", len(realDiags), realDiags)
	}
	if real == "go=unknown" {
		t.Skip("go env is not runnable in this environment, so the control cannot distinguish itself")
	}
}
