package collect

import (
	"slices"
	"testing"
)

// envPairs is tested from inside the package because its ordering guarantee is
// not observable from outside: a shell rebuilds its exported environment in
// hash-table order, so `env` run under `sh -c` reports neither the order Go
// handed over nor a stable one. The end-to-end tests in collect_test.go cover
// the values reaching the subprocess; this covers the order they leave in.
func TestEnvPairsIsSorted(t *testing.T) {
	env := map[string]string{
		"GOFLAGS": "-mod=mod",
		"GOWORK":  "off",
		"CGO_":    "0",
		"AAA":     "first",
		"ZZZ":     "last",
	}
	want := []string{"AAA=first", "CGO_=0", "GOFLAGS=-mod=mod", "GOWORK=off", "ZZZ=last"}

	// Go randomizes map iteration order, so an unsorted implementation returns
	// the right answer by luck roughly one call in 120 with five keys. One
	// assertion would therefore be a flaky test rather than a failing one, and
	// repeating it is what makes the verdict decisive. envPairs is pure and
	// allocates five strings, so the loop costs nothing.
	for i := range 200 {
		got := envPairs(env)
		if !slices.Equal(got, want) {
			t.Fatalf("call %d: envPairs returned %q, want %q", i, got, want)
		}
	}
}

// A repo with no env configured must get a nil slice, not an empty one. run
// only replaces cmd.Env when len(Env) > 0, and handing it a non-nil empty
// slice would be a needless second shape for callers to reason about.
func TestEnvPairsEmptyIsNil(t *testing.T) {
	if got := envPairs(nil); got != nil {
		t.Errorf("envPairs(nil): got %q, want nil", got)
	}
	if got := envPairs(map[string]string{}); got != nil {
		t.Errorf("envPairs(empty map): got %q, want nil", got)
	}
}

// An empty value is a legitimate thing to want: `FOO=` unsets meaning without
// removing the variable, and dropping the "=" would set nothing at all.
func TestEnvPairsKeepsEmptyValues(t *testing.T) {
	got := envPairs(map[string]string{"GOFLAGS": ""})
	if !slices.Equal(got, []string{"GOFLAGS="}) {
		t.Errorf("envPairs with an empty value: got %q, want %q", got, []string{"GOFLAGS="})
	}
}
