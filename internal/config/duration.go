package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Duration is a time.Duration that unmarshals from a YAML string like "10m".
type Duration time.Duration

// UnmarshalYAML implements goccy/go-yaml's BytesUnmarshaler.
func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	// ParseDuration rather than time.ParseDuration. This called the standard
	// library directly, so "window: 7d" in a config file was a load error even
	// though 7d is what the --window flag, the help text, the README and this
	// tool's own starter config all teach.
	parsed, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// ParseDuration is time.ParseDuration plus a "w" suffix for weeks and a "d"
// suffix for days.
//
// The standard library tops out at hours, so a bare time.ParseDuration rejects
// "7d" with "unknown unit d in duration". That is the single most obvious thing
// anyone types for a weekly report, it is the default this tool documents in its
// own help text, and history's --since advertises "26w" as well.
//
// It is exported, and lives here rather than in internal/cli, because the config
// file needs the same parser the flags use: internal/cli already imports this
// package and this package must not import that one. Two parsers is what made
// "window: 7d" a load error in the file whose comments recommend 7d.
//
// Weeks and days are handled here; everything else is delegated unchanged.
// Units run largest first, so "1w3d12h" works the way Go's own "1h30m" reads.
// Written the other way round, "3d1w" is rejected rather than guessed at,
// because the text before the "w" is not a number.
func ParseDuration(s string) (time.Duration, error) {
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

	// No unit the standard library knows contains a "w" or a "d" (ns, us, ms,
	// s, m, h), so finding one is unambiguous: it is either the suffix or a
	// typo, and the ParseFloat below tells the two apart. A bare "w" or "d"
	// leaves ParseFloat an empty string, which is the error it should be.
	var total time.Duration
	scaled := false
	if i := strings.IndexByte(rest, 'w'); i >= 0 {
		weeks, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %q is not a number of weeks", s, rest[:i])
		}
		total += time.Duration(weeks * float64(7*24*time.Hour))
		rest = rest[i+1:]
		scaled = true
	}
	if i := strings.IndexByte(rest, 'd'); i >= 0 {
		days, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %q is not a number of days", s, rest[:i])
		}
		total += time.Duration(days * float64(24*time.Hour))
		rest = rest[i+1:]
		scaled = true
	}

	if !scaled {
		// Nothing here to add, so the standard library owns the whole string,
		// sign included, and its message is the better one for a typo.
		d, err := time.ParseDuration(text)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q, want something like 7d or 36h: %w", s, err)
		}
		return d, nil
	}

	// A tail is allowed so "1w3d" and "1d12h" work as well as "7d" does.
	if rest != "" {
		extra, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %q is not a duration: %w", s, rest, err)
		}
		total += extra
	}
	// The sign is applied once, at the end, so "-1w3d" is ten days ago rather
	// than four.
	return sign * total, nil
}
