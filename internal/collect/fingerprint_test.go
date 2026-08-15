package collect

import (
	"context"
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
func TestEnvFingerprintDegradesToUnknown(t *testing.T) {
	got := envFingerprint(context.Background(), t.TempDir(), []string{"PATH=/nonexistent"})
	if got == "" {
		t.Fatal("no fingerprint recorded when go env could not run")
	}
	if !strings.HasPrefix(got, "go=") {
		t.Errorf("unexpected fingerprint shape: %q", got)
	}
}
