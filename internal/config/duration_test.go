package config_test

import (
	"testing"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// This table moved here from internal/cli when the two duration parsers became
// one. It lived beside the flag parser, which is why the config file's own
// parser could quietly be time.ParseDuration and reject the "7d" every document
// in this repo teaches.
func TestParseDuration(t *testing.T) {
	const (
		day  = 24 * time.Hour
		week = 7 * day
	)

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
		// The usage text advertises `history --since 26w` and the parser used to
		// reject it, because no week suffix existed anywhere.
		{name: "weeks", in: "26w", want: 26 * week},
		{name: "one week", in: "1w", want: week},
		{name: "fractional weeks", in: "0.5w", want: 84 * time.Hour},
		// Largest unit first, the way Go's own "1h30m" reads.
		{name: "weeks plus days", in: "1w3d", want: week + 3*day},
		{name: "weeks days and hours", in: "1w3d12h", want: week + 3*day + 12*time.Hour},
		{name: "weeks plus a stdlib tail", in: "2w6h", want: 2*week + 6*time.Hour},
		// One sign, applied to the whole expression, so this is ten days back
		// rather than four.
		{name: "negative weeks", in: "-2w", want: -2 * week},
		{name: "negative weeks and days", in: "-1w3d", want: -(week + 3*day)},
		{name: "leading plus", in: "+2w", want: 2 * week},
		{name: "hours delegate to the stdlib", in: "24h", want: 24 * time.Hour},
		{name: "minutes delegate to the stdlib", in: "90m", want: 90 * time.Minute},
		{name: "compound stdlib duration", in: "1h30m", want: 90 * time.Minute},
		{name: "surrounding space", in: "  7d ", want: 7 * day},
		{name: "empty", in: "", wantErr: true},
		{name: "nonsense", in: "banana", wantErr: true},
		{name: "d with no number", in: "d", wantErr: true},
		// A bare w is the same mistake one unit up, and has to fail the same
		// way rather than counting as zero weeks.
		{name: "w with no number", in: "w", wantErr: true},
		{name: "letters before the d", in: "abcd", wantErr: true},
		{name: "letters before the w", in: "abcw", wantErr: true},
		{name: "no unit at all", in: "7", wantErr: true},
		{name: "junk after the day suffix", in: "7dx", wantErr: true},
		{name: "junk after the week suffix", in: "7wx", wantErr: true},
		// Smallest unit first is rejected rather than guessed at. Accepting it
		// would mean deciding on the operator's behalf which of two readings a
		// misordered duration had.
		{name: "units in the wrong order", in: "3d1w", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := config.ParseDuration(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The defect that made one parser worth having: every duration field in the
// config file went through time.ParseDuration, so the day and week suffixes the
// flags accept were load errors in the file.
func TestLoadAcceptsDayAndWeekSuffixes(t *testing.T) {
	cfg, err := config.Load(write(t, `
window: 7d
repos:
  - name: svc
    path: $REPO
    signals:
      - name: coverage
        artifact: coverage.out
        artifact_format: go-coverprofile
        timeout: 1d
        max_age: 2w
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := time.Duration(cfg.Window), 7*24*time.Hour; got != want {
		t.Errorf("window = %s, want %s", got, want)
	}
	s := onlySignal(t, cfg)
	if got, want := time.Duration(s.Timeout), 24*time.Hour; got != want {
		t.Errorf("timeout = %s, want %s", got, want)
	}
	if got, want := time.Duration(s.MaxAge), 14*24*time.Hour; got != want {
		t.Errorf("max_age = %s, want %s", got, want)
	}
}
