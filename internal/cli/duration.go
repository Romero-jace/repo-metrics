package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseDuration is time.ParseDuration plus a "d" suffix for days.
//
// The standard library tops out at hours, so a bare time.ParseDuration rejects
// "7d" with "unknown unit d in duration". That is the single most obvious thing
// anyone types for a weekly report, and it is the default this tool documents in
// its own help text, so shipping it as a runtime error would be a bad joke.
// Days are handled here; everything else is delegated unchanged.
func parseDuration(s string) (time.Duration, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, fmt.Errorf("empty duration, want something like 7d or 36h")
	}

	rest := text
	sign := time.Duration(1)
	switch rest[0] {
	case '+':
		rest = rest[1:]
	case '-':
		sign = -1
		rest = rest[1:]
	}

	// No unit the standard library knows contains a "d" (ns, us, ms, s, m, h),
	// so finding one is unambiguous: it is either the day suffix or a typo, and
	// the ParseFloat below tells the two apart.
	i := strings.IndexByte(rest, 'd')
	if i < 0 {
		d, err := time.ParseDuration(text)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q, want something like 7d or 36h: %w", s, err)
		}
		return d, nil
	}

	days, err := strconv.ParseFloat(rest[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %q is not a number of days", s, rest[:i])
	}
	total := time.Duration(days * float64(24*time.Hour))

	// A tail is allowed so "1d12h" works as well as "7d" does.
	if tail := rest[i+1:]; tail != "" {
		extra, err := time.ParseDuration(tail)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %q is not a duration: %w", s, tail, err)
		}
		total += extra
	}
	return sign * total, nil
}

// parseWindow is parseDuration with the one extra rule a reporting window has.
//
// A zero or negative window would put the baseline cutoff at or after the head
// snapshot, so every repo would be compared against itself and the report would
// confidently say nothing changed.
func parseWindow(s string) (time.Duration, error) {
	d, err := parseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("window must be positive, got %q", s)
	}
	return d, nil
}
