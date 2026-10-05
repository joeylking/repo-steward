// Package coverage decides whether the repository's tests execute the
// source lines a repair changed. It runs the tests with coverage in the
// execute profile, parses the profile fail-closed, and maps the changed
// lines of the diff to coverage blocks. Anything it cannot decide is
// reported as not verified, never as verified.
package coverage

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// EndMarker opens the line the coverage script writes after the profile,
// followed by the number of lines the profile has. A profile cut anywhere,
// even at a line boundary, lacks the marker or disagrees with its count.
const EndMarker = "repo-steward-coverage-end "

// Block is one coverage block of a profile: a source range in a file, by
// 1-based line and byte column, end exclusive.
type Block struct {
	StartLine, StartCol int
	EndLine, EndCol     int
	Stmts               int
	// Executed is true when any entry for this range counted at least one
	// execution.
	Executed bool
}

// Key identifies a block within its file.
func (b Block) Key() string {
	return fmt.Sprintf("%d.%d,%d.%d", b.StartLine, b.StartCol, b.EndLine, b.EndCol)
}

// Profile is a parsed coverage profile: blocks by file name as the profile
// names it (import path of the package, a slash, the file name).
type Profile struct {
	Mode  string
	Files map[string][]Block
}

var profileLine = regexp.MustCompile(`^(.+):([0-9]+)\.([0-9]+),([0-9]+)\.([0-9]+) ([0-9]+) ([0-9]+)$`)

// ParseProfile parses the coverage script's output: a profile in the go
// tool's text format followed by the end marker with the profile's line
// count. Anything else, including an empty output, a missing or wrong
// marker, a malformed line, or an impossible range, is an error: the
// caller treats it as no evidence.
func ParseProfile(raw []byte) (*Profile, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("coverage: empty output")
	}
	if raw[len(raw)-1] != '\n' {
		return nil, fmt.Errorf("coverage: output does not end with a newline")
	}
	lines := strings.Split(string(bytes.TrimSuffix(raw, []byte("\n"))), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, EndMarker) {
		return nil, fmt.Errorf("coverage: output does not end with the end marker")
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(last, EndMarker)))
	if err != nil {
		return nil, fmt.Errorf("coverage: malformed end marker %q", last)
	}
	lines = lines[:len(lines)-1]
	if n != len(lines) {
		return nil, fmt.Errorf("coverage: end marker counts %d lines, output has %d", n, len(lines))
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("coverage: no mode line")
	}
	mode, ok := strings.CutPrefix(lines[0], "mode: ")
	if !ok || (mode != "set" && mode != "count" && mode != "atomic") {
		return nil, fmt.Errorf("coverage: first line %q is not a mode line", lines[0])
	}
	p := &Profile{Mode: mode, Files: map[string][]Block{}}
	index := map[string]map[string]int{}
	for i, line := range lines[1:] {
		m := profileLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("coverage: line %d is malformed: %q", i+2, line)
		}
		var v [6]int
		for j := range v {
			if v[j], err = strconv.Atoi(m[j+2]); err != nil {
				return nil, fmt.Errorf("coverage: line %d: %v", i+2, err)
			}
		}
		b := Block{StartLine: v[0], StartCol: v[1], EndLine: v[2], EndCol: v[3], Stmts: v[4], Executed: v[5] > 0}
		if b.StartLine < 1 || b.StartCol < 1 || b.EndLine < b.StartLine || (b.EndLine == b.StartLine && b.EndCol < b.StartCol) {
			return nil, fmt.Errorf("coverage: line %d has an impossible range: %q", i+2, line)
		}
		file := m[1]
		if index[file] == nil {
			index[file] = map[string]int{}
		}
		// The go tool lists a block once per test binary that linked its
		// package; it was executed if any of them executed it.
		if at, seen := index[file][b.Key()]; seen {
			if b.Stmts != p.Files[file][at].Stmts {
				return nil, fmt.Errorf("coverage: line %d disagrees with an earlier entry for the same block", i+2)
			}
			p.Files[file][at].Executed = p.Files[file][at].Executed || b.Executed
			continue
		}
		index[file][b.Key()] = len(p.Files[file])
		p.Files[file] = append(p.Files[file], b)
	}
	return p, nil
}
