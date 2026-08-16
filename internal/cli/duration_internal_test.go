// This one file is an internal test because Run is the package's only exported
// symbol, and the window rule is worth testing directly rather than only
// through a subcommand. Everything else lives in cli_test.go, externally.
//
// The parser's own table used to live here too. It moved to internal/config
// with the parser, which is where both the flags and the config file now read
// their durations from.
package cli

import (
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{in: "7d", want: 7 * 24 * time.Hour},
		// The usage text advertises `--since 26w`, and parseWindow is what
		// --since goes through.
		{in: "26w", want: 26 * 7 * 24 * time.Hour},
		{in: "36h", want: 36 * time.Hour},
	}
	for _, tc := range cases {
		got, err := parseWindow(tc.in)
		if err != nil {
			t.Errorf("parseWindow(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseWindow(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// A window at or below zero would compare every head snapshot against
	// itself and report a confident set of zeroes, so it has to be rejected.
	for _, in := range []string{"0s", "-7d", "-2w", "banana"} {
		if got, err := parseWindow(in); err == nil {
			t.Errorf("parseWindow(%q) = %v, want an error", in, got)
		}
	}
}
