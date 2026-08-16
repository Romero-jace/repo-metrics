package cli

import (
	"fmt"
	"time"

	"github.com/Romero-jace/repo-metrics/internal/config"
)

// parseWindow is config.ParseDuration with the one extra rule a reporting
// window has.
//
// The parser itself is config.ParseDuration rather than a second one kept here.
// There were two, and they disagreed: this package understood "7d" while the
// config file went through time.ParseDuration alone, so "window: 7d" in a file
// was a load error even though 7d is what --window, the README and the starter
// config all teach. It lives in internal/config because this package already
// imports that one and the dependency cannot run the other way.
//
// A zero or negative window would put the baseline cutoff at or after the head
// snapshot, so every repo would be compared against itself and the report would
// confidently say nothing changed.
func parseWindow(s string) (time.Duration, error) {
	d, err := config.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("window must be positive, got %q", s)
	}
	return d, nil
}
