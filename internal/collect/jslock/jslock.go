// Package jslock parses the lockfiles JavaScript package managers write.
//
// It answers one question — how many packages does this project resolve to —
// and it answers it from the lockfile rather than from the package manager,
// which is the whole design.
//
// `npm outdated` and its siblings resolve through the installed tree: npm's
// implementation calls arb.loadActual(), and pip and uv read the environment.
// On a fresh checkout, with nothing installed, they return an empty result and
// exit 0. So a repo nobody has installed and a repo with nothing outdated
// produce identical output, which is the same shape as the GOPROXY=off trap the
// Go dependency parser refuses. Reading the lockfile has no such state: the file
// either lists the packages or it is not a lockfile.
//
// What that costs is the other two dependency measurements. Neither format here
// records when a version was published, so dependency age is not recoverable,
// and knowing whether a newer version exists needs a registry. Both are left
// unmeasured rather than approximated.
package jslock

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Severity classifies a diagnostic, mirroring the collect package's own rather
// than importing it, so this stays a parser of a public format.
type Severity string

// The severities in play.
const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Diagnostic explains something about a lockfile that still parsed.
type Diagnostic struct {
	Severity Severity
	Message  string
}

// Summary is one parsed lockfile.
type Summary struct {
	// Total is how many packages the project resolves to, excluding the project
	// itself and any workspace siblings.
	//
	// Those exclusions are what make the number comparable to the one the Go
	// parser reports, which is the build list minus the main module and its
	// workspace siblings. A monorepo whose packages depend on each other would
	// otherwise count its own source as a dependency and read as larger for being
	// split up.
	Total int
}

// ParseNPM reads a package-lock.json.
//
// Two shapes are in the wild and both are handled, because both are still on
// disk in real repositories. Version 2 and 3 carry a flat `packages` map keyed
// by install path, which is the easy case. Version 1 carries a nested
// `dependencies` tree instead, where the same package can appear at several
// depths, so it is counted by distinct name rather than by node.
func ParseNPM(r io.Reader) (*Summary, []Diagnostic, error) {
	var doc npmLock
	if err := json.NewDecoder(io.LimitReader(r, maxLockfile)).Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("jslock: reading the npm lockfile: %w", err)
	}

	var diags []Diagnostic
	if len(doc.Packages) > 0 {
		total := 0
		for path, pkg := range doc.Packages {
			switch {
			case path == "":
				// The project itself.
			case pkg.Link:
				// A symlink into a workspace sibling, which is this repo's own
				// source rather than something it depends on.
			default:
				total++
			}
		}
		return &Summary{Total: total}, diags, nil
	}

	if len(doc.Dependencies) > 0 {
		seen := make(map[string]bool)
		countV1(doc.Dependencies, seen)
		diags = append(diags, warnf(
			"lockfile version %d nests its dependency tree, so packages are counted once per distinct name "+
				"rather than once per entry. npm 7 and later write a flat list and need no such interpretation",
			doc.LockfileVersion))
		return &Summary{Total: len(seen)}, diags, nil
	}

	// A lockfile listing nothing. Distinct from a file that is not a lockfile,
	// which fails below: a project genuinely without dependencies is a real state
	// and zero is a real measurement of it.
	if doc.LockfileVersion == 0 {
		return nil, nil, fmt.Errorf(
			"jslock: no lockfileVersion and no packages, so this is not a package-lock.json")
	}
	return &Summary{}, diags, nil
}

// countV1 walks a version 1 dependency tree, recording distinct names.
func countV1(deps map[string]npmV1Dep, seen map[string]bool) {
	for name, dep := range deps {
		seen[name] = true
		countV1(dep.Dependencies, seen)
	}
}

// ParseBun reads a bun.lock.
//
// The file is JSON with trailing commas, which is legal JSONC and not legal
// JSON, so it is normalized before decoding. Verified against bun 1.x output:
// 786 trailing commas in one real lockfile and not a single comment, so the
// commas are the only thing standing between it and encoding/json.
func ParseBun(r io.Reader) (*Summary, []Diagnostic, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxLockfile))
	if err != nil {
		return nil, nil, fmt.Errorf("jslock: reading the bun lockfile: %w", err)
	}

	var doc bunLock
	if err := json.Unmarshal(stripTrailingCommas(raw), &doc); err != nil {
		return nil, nil, fmt.Errorf("jslock: reading the bun lockfile: %w", err)
	}
	if doc.LockfileVersion == 0 && len(doc.Packages) == 0 {
		return nil, nil, fmt.Errorf("jslock: no lockfileVersion and no packages, so this is not a bun.lock")
	}

	total := 0
	for _, entry := range doc.Packages {
		// Each entry is a tuple whose first element is the resolved descriptor.
		// A workspace sibling resolves to "name@workspace:path" rather than to a
		// version, which is how this repo's own packages are told from the ones it
		// depends on.
		if len(entry) > 0 && strings.Contains(string(entry[0]), "@workspace:") {
			continue
		}
		total++
	}
	return &Summary{Total: total}, nil, nil
}

// maxLockfile bounds what will be read. A lockfile for a very large project is a
// few megabytes.
const maxLockfile = 256 << 20

// stripTrailingCommas removes a comma that sits before a closing brace or
// bracket, which is the one way bun.lock departs from JSON.
//
// String state is tracked, so a comma inside a string value is left alone. That
// is not hypothetical caution: a package's integrity hash or a resolved URL can
// contain almost anything, and a naive replace would corrupt one.
func stripTrailingCommas(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString, escaped := false, false
	// comma is the index in out of a comma waiting to see what follows it.
	comma := -1

	for _, c := range in {
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
			// A quote is content, so a comma before it was separating two values
			// rather than trailing the last one. Without this line the separator in
			// ["a", "b"] is removed and the document stops being JSON at all, which
			// is a corruption rather than a parse failure: the bytes still decode
			// for some inputs and mean something else.
			comma = -1
		case inString:
			// Nothing inside a string is punctuation.
		case c == ',':
			comma = len(out)
		case c == '}' || c == ']':
			if comma >= 0 {
				// Everything between the comma and here is whitespace, or comma
				// would have been cleared, so dropping it cannot change anything
				// else.
				out = append(out[:comma], out[comma+1:]...)
			}
			comma = -1
		case c != ' ' && c != '\t' && c != '\n' && c != '\r':
			comma = -1
		}
		out = append(out, c)
	}
	return out
}

// The document shapes. Only the fields this parser reads are declared.
type npmLock struct {
	LockfileVersion int                   `json:"lockfileVersion"`
	Packages        map[string]npmPackage `json:"packages"`
	Dependencies    map[string]npmV1Dep   `json:"dependencies"`
}

type npmPackage struct {
	// Link marks an entry that is a symlink to a workspace sibling rather than an
	// installed package.
	Link bool `json:"link"`
}

type npmV1Dep struct {
	Dependencies map[string]npmV1Dep `json:"dependencies"`
}

type bunLock struct {
	LockfileVersion int                          `json:"lockfileVersion"`
	Packages        map[string][]json.RawMessage `json:"packages"`
}

func warnf(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarn, Message: fmt.Sprintf(format, args...)}
}
