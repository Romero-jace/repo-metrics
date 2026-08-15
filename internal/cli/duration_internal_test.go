// This one file is an internal test because Run is the package's only exported
// symbol, and the day-suffix parser is worth testing directly rather than only
// through a subcommand. Everything else lives in cli_test.go, externally.
package cli

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	const day = 24 * time.Hour

	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		// The whole reason this function exists: time.ParseDuration rejects
		// this, and it is the most obvious thing anyone will type.
		{name: "days", in: "7d", want: 7 * day},
		{name: "one day", in: "1d", want: day},
		{name: "fractional days", in: "0.5d", want: 12 * time.Hour},
		{name: "days plus hours", in: "1d12h", want: 36 * time.Hour},
		{name: "negative days", in: "-7d", want: -7 * day},
		{name: "hours delegate to the stdlib", in: "24h", want: 24 * time.Hour},
		{name: "minutes delegate to the stdlib", in: "90m", want: 90 * time.Minute},
		{name: "compound stdlib duration", in: "1h30m", want: 90 * time.Minute},
		{name: "surrounding space", in: "  7d ", want: 7 * day},
		{name: "empty", in: "", wantErr: true},
		{name: "nonsense", in: "banana", wantErr: true},
		{name: "d with no number", in: "d", wantErr: true},
		{name: "letters before the d", in: "abcd", wantErr: true},
		{name: "no unit at all", in: "7", wantErr: true},
		{name: "junk after the day suffix", in: "7dx", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDuration(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDuration(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDuration(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseWindow(t *testing.T) {
	got, err := parseWindow("7d")
	if err != nil {
		t.Fatalf("parseWindow(7d): %v", err)
	}
	if want := 7 * 24 * time.Hour; got != want {
		t.Errorf("parseWindow(7d) = %v, want %v", got, want)
	}

	// A window at or below zero would compare every head snapshot against
	// itself and report a confident set of zeroes, so it has to be rejected.
	for _, in := range []string{"0s", "-7d", "banana"} {
		if got, err := parseWindow(in); err == nil {
			t.Errorf("parseWindow(%q) = %v, want an error", in, got)
		}
	}
}
