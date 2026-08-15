// Package golang parses the artifacts a Go test run leaves behind: coverage
// profiles and `go test -json` event streams.
//
// It is the only language-aware code in the tool. Everything upstream of it
// deals in (key, scope, value) metrics, so adding another language means adding
// a sibling package, not touching the storage or reporting layers.
package golang

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Profile is a parsed Go coverage profile, aggregated per package.
type Profile struct {
	// Mode is set, count, or atomic.
	Mode string
	// Packages is sorted by import path.
	Packages []PackageCoverage
}

// PackageCoverage is one package's statement counts.
//
// Counts, not a percentage. A package's percentage is Covered/Total, and a
// repo's is the sum of Covered over the sum of Total, which is emphatically not
// the mean of the per-package percentages.
type PackageCoverage struct {
	Package string
	Covered int
	Total   int
}

// Totals sums every package, which is how repo coverage is computed.
func (p *Profile) Totals() (covered, total int) {
	for _, pkg := range p.Packages {
		covered += pkg.Covered
		total += pkg.Total
	}
	return covered, total
}

// blockKey identifies a source block by its exact extent.
//
// This is the whole reason duplicate handling works. Under -coverpkg=./... the
// same block is emitted once per test binary that linked the package, so a
// profile from a repo with 40 test binaries can contain 40 copies of every
// block in the tree. Summing NumStmt naively inflates totals by that factor and
// produces a coverage percentage that looks plausible and is badly wrong.
type blockKey struct {
	file                                 string
	startLine, startCol, endLine, endCol int
}

// ParseCoverProfile reads a `go test -coverprofile` file.
func ParseCoverProfile(r io.Reader) (*Profile, error) {
	sc := bufio.NewScanner(r)
	// Coverage lines are short, but a pathological import path should not be
	// able to abort a parse with "token too long".
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var mode string
	// Merge duplicates by summing counts. A block is covered if ANY test
	// binary exercised it, which is exactly what a positive sum means.
	stmts := make(map[blockKey]int)
	counts := make(map[blockKey]int)

	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		if mode == "" {
			rest, ok := strings.CutPrefix(line, "mode:")
			if !ok {
				return nil, fmt.Errorf("golang: line %d: want a \"mode:\" header, got %q", lineNo, line)
			}
			if mode = strings.TrimSpace(rest); mode == "" {
				return nil, fmt.Errorf("golang: line %d: empty coverage mode", lineNo)
			}
			continue
		}

		key, numStmt, count, err := parseBlock(line)
		if err != nil {
			return nil, fmt.Errorf("golang: line %d: %w", lineNo, err)
		}
		stmts[key] = numStmt
		counts[key] += count
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("golang: reading coverage profile: %w", err)
	}
	if mode == "" {
		return nil, fmt.Errorf("golang: empty coverage profile: no \"mode:\" header")
	}

	byPkg := make(map[string]*PackageCoverage)
	for key, numStmt := range stmts {
		pkg := path.Dir(key.file)
		agg, ok := byPkg[pkg]
		if !ok {
			agg = &PackageCoverage{Package: pkg}
			byPkg[pkg] = agg
		}
		agg.Total += numStmt
		if counts[key] > 0 {
			agg.Covered += numStmt
		}
	}

	out := &Profile{Mode: mode, Packages: make([]PackageCoverage, 0, len(byPkg))}
	for _, agg := range byPkg {
		out.Packages = append(out.Packages, *agg)
	}
	sort.Slice(out.Packages, func(i, j int) bool {
		return out.Packages[i].Package < out.Packages[j].Package
	})
	return out, nil
}

// parseBlock reads one line of the form
//
//	path/to/file.go:12.34,15.2 3 1
//
// which is <file>:<startLine>.<startCol>,<endLine>.<endCol> <numStmt> <count>.
// Fields are split from the right, because an import path is far more likely to
// contain a space than the trailing numbers are.
func parseBlock(line string) (blockKey, int, int, error) {
	var zero blockKey

	rest, countStr, ok := cutLast(line, " ")
	if !ok {
		return zero, 0, 0, fmt.Errorf("malformed block %q: want three space-separated fields", line)
	}
	spec, stmtStr, ok := cutLast(rest, " ")
	if !ok {
		return zero, 0, 0, fmt.Errorf("malformed block %q: want three space-separated fields", line)
	}

	count, err := strconv.Atoi(countStr)
	if err != nil {
		return zero, 0, 0, fmt.Errorf("malformed block %q: bad execution count: %w", line, err)
	}
	numStmt, err := strconv.Atoi(stmtStr)
	if err != nil {
		return zero, 0, 0, fmt.Errorf("malformed block %q: bad statement count: %w", line, err)
	}
	if numStmt < 0 || count < 0 {
		return zero, 0, 0, fmt.Errorf("malformed block %q: negative counts", line)
	}

	// Split the file off at the LAST colon: the positions never contain one,
	// but a path conceivably could.
	file, rng, ok := cutLast(spec, ":")
	if !ok || file == "" {
		return zero, 0, 0, fmt.Errorf("malformed block %q: want <file>:<range>", line)
	}

	startSpec, endSpec, ok := strings.Cut(rng, ",")
	if !ok {
		return zero, 0, 0, fmt.Errorf("malformed block %q: want a comma-separated range", line)
	}
	startLine, startCol, err := parsePosition(startSpec)
	if err != nil {
		return zero, 0, 0, fmt.Errorf("malformed block %q: start position: %w", line, err)
	}
	endLine, endCol, err := parsePosition(endSpec)
	if err != nil {
		return zero, 0, 0, fmt.Errorf("malformed block %q: end position: %w", line, err)
	}

	return blockKey{
		file:      file,
		startLine: startLine,
		startCol:  startCol,
		endLine:   endLine,
		endCol:    endCol,
	}, numStmt, count, nil
}

// parsePosition reads a "line.column" pair.
func parsePosition(s string) (line, col int, err error) {
	lineStr, colStr, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("want line.column, got %q", s)
	}
	if line, err = strconv.Atoi(lineStr); err != nil {
		return 0, 0, fmt.Errorf("bad line in %q: %w", s, err)
	}
	if col, err = strconv.Atoi(colStr); err != nil {
		return 0, 0, fmt.Errorf("bad column in %q: %w", s, err)
	}
	return line, col, nil
}

// cutLast is strings.Cut anchored at the last occurrence of sep.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
