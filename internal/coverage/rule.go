package coverage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Hunk is one hunk header of a zero-context unified diff: OldLines lines
// from OldStart were replaced by NewLines lines from NewStart. A count of
// zero means the start is the line after which lines were added or removed.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
}

var hunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

// ParseHunks reads the hunk headers of a zero-context unified diff of one
// file. Lines other than headers are skipped; a header that does not parse
// is an error.
func ParseHunks(diff []byte) ([]Hunk, error) {
	var out []Hunk
	for _, line := range strings.Split(string(diff), "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		m := hunkHeader.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("coverage: malformed hunk header %q", line)
		}
		num := func(s string, def int) int {
			if s == "" {
				return def
			}
			n, _ := strconv.Atoi(s)
			return n
		}
		out = append(out, Hunk{OldStart: num(m[1], 0), OldLines: num(m[2], 1), NewStart: num(m[3], 0), NewLines: num(m[4], 1)})
	}
	return out, nil
}

// FileChange is one changed file of the candidate tree against the base.
// Status is A, M, or D. Old is empty for an added file and New for a
// deleted one.
type FileChange struct {
	Path     string
	Status   string
	Old, New []byte
	Hunks    []Hunk
	// Binary is true when git could not diff the file as text.
	Binary bool
}

// Input is everything the rule judges.
type Input struct {
	// ModulePath maps a repository path to the name the profile uses.
	ModulePath string
	// Files are the changed files, manifests excluded.
	Files []FileChange
	// Evidence is the coverage run of the candidate tree.
	Evidence *Evidence
	// PackageFuncs lists the functions and methods, as "Name" or
	// "Recv.Name", declared by the non-test Go files of a directory of the
	// candidate tree.
	PackageFuncs func(dir string) (map[string]bool, error)
}

// Range is a span of new-side lines of one file, or a whole file when Start
// is zero, that the rule could not verify, with the reason. A Range with no
// path is a reason that applies to the whole change.
type Range struct {
	Path   string `json:"path,omitempty"`
	Start  int    `json:"start,omitempty"`
	End    int    `json:"end,omitempty"`
	Reason string `json:"reason"`
}

// String renders the range as path:start-end (reason).
func (r Range) String() string {
	switch {
	case r.Path == "":
		return r.Reason
	case r.Start == 0:
		return r.Path + " (" + r.Reason + ")"
	case r.End == r.Start:
		return fmt.Sprintf("%s:%d (%s)", r.Path, r.Start, r.Reason)
	}
	return fmt.Sprintf("%s:%d-%d (%s)", r.Path, r.Start, r.End, r.Reason)
}

// Verdict is the rule's answer.
type Verdict struct {
	// Verified is true only when every changed line that needs a test to
	// execute it was executed, and nothing was undecidable.
	Verified bool `json:"verified"`
	// Blocks is the number of distinct coverage blocks the changed lines
	// required; Executed how many of them the tests executed.
	Blocks   int `json:"blocks"`
	Executed int `json:"executed"`
	// Files is the number of changed files the rule judged.
	Files int `json:"files"`
	// Unverified lists what failed, in path and line order.
	Unverified []Range `json:"unverified,omitempty"`
}

// Reasons a line or file is not verified.
const (
	ReasonNotExecuted    = "no test executes it"
	ReasonNoBlock        = "no coverage block covers it"
	ReasonNotInProfile   = "the coverage profile has no blocks for this file: it is excluded from the build, or no test binary compiled it"
	ReasonPackageDecl    = "package-level declaration: it takes effect wherever it is used, which coverage of these lines cannot show"
	ReasonInitImport     = "blank or dot import: it runs package initialization, which coverage cannot attribute"
	ReasonDirective      = "compiler directive"
	ReasonLineDirective  = "the file has //line directives, which move coverage positions"
	ReasonNotGo          = "not Go source: coverage cannot show whether it is used"
	ReasonUnparsable     = "the file does not parse"
	ReasonBinary         = "binary change"
	ReasonRemovedFunc    = "a function or method was removed or renamed: nothing shows the code that relied on it still behaves"
	ReasonRemovedDecl    = "a package-level declaration was removed"
	ReasonRemovedNoBlock = "lines were removed here and no coverage block surrounds the place"
	ReasonOutside        = "code outside any declaration"
)

// Check applies the rule:
//
//   - Every new-side changed line of a non-test .go file that holds code
//     is mapped to coverage blocks, and each such block must have been
//     executed at least once. A line inside a function body needs the
//     blocks that contain its code; braces count only on a line that has
//     nothing else, so a changed if or for header needs the block that
//     evaluates it, not the body it may skip. Code inside a body that no
//     block contains (a case label, an else) needs every block of that
//     function that spans the line. A line of a function's signature needs
//     the function's entry block, that is, the function was called.
//   - Blank and comment-only lines, the package clause, and ordinary
//     imports need nothing of their own: the compiler checks them.
//   - A changed or removed const, var, or type declaration at package level
//     is not verified: its effect is at its uses, which the diff does not
//     show. A blank or dot import, added or removed, is not verified: it
//     changes what package initialization runs. A compiler directive on a
//     changed line, and any //line directive in a changed file, are not
//     verified.
//   - Removed lines inside a function that still exists need the blocks
//     around the place they were removed from. A function or method that
//     no longer exists in its package is not verified: it is a deletion or
//     a rename. Moved code is judged as new code where it now is.
//   - A changed file that is not Go source, a binary change, a file that
//     does not parse, and a file the profile does not mention while it has
//     lines that need blocks, are not verified. _test.go files are left to
//     the protected-path rule.
//   - Without usable evidence nothing is verified.
func Check(in Input) Verdict {
	v := Verdict{}
	prof, why := in.Evidence.Usable()
	if prof == nil {
		v.Unverified = append(v.Unverified, Range{Reason: why})
	}
	required := map[string]Block{} // file + key -> block
	funcs := in.PackageFuncs
	if funcs != nil {
		type entry struct {
			m   map[string]bool
			err error
		}
		cache := map[string]entry{}
		inner := funcs
		funcs = func(dir string) (map[string]bool, error) {
			e, ok := cache[dir]
			if !ok {
				e.m, e.err = inner(dir)
				cache[dir] = e
			}
			return e.m, e.err
		}
	}
	for _, f := range in.Files {
		if strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		v.Files++
		switch {
		case !strings.HasSuffix(f.Path, ".go"):
			v.Unverified = append(v.Unverified, Range{Path: f.Path, Reason: ReasonNotGo})
			continue
		case f.Binary:
			v.Unverified = append(v.Unverified, Range{Path: f.Path, Reason: ReasonBinary})
			continue
		}
		var blocks []Block
		inProfile := false
		if prof != nil {
			blocks, inProfile = prof.Files[profileName(in.ModulePath, f.Path)]
		}
		ranges, req := checkFile(f, blocks, inProfile, prof != nil, funcs)
		v.Unverified = append(v.Unverified, ranges...)
		for _, b := range req {
			required[f.Path+"@"+b.Key()] = b
		}
	}
	for _, b := range required {
		v.Blocks++
		if b.Executed {
			v.Executed++
		}
	}
	sort.SliceStable(v.Unverified, func(i, j int) bool {
		a, b := v.Unverified[i], v.Unverified[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Start < b.Start
	})
	v.Verified = len(v.Unverified) == 0
	return v
}

func profileName(module, rel string) string {
	return path.Join(module, rel)
}

// lineFail collects per-line failures and merges consecutive lines with the
// same reason into ranges.
type lineFail map[int]string

func (lf lineFail) add(line int, reason string) {
	if _, ok := lf[line]; !ok {
		lf[line] = reason
	}
}

func (lf lineFail) ranges(p string) []Range {
	lines := make([]int, 0, len(lf))
	for l := range lf {
		lines = append(lines, l)
	}
	sort.Ints(lines)
	var out []Range
	for _, l := range lines {
		if n := len(out); n > 0 && out[n-1].End == l-1 && out[n-1].Reason == lf[l] {
			out[n-1].End = l
			continue
		}
		out = append(out, Range{Path: p, Start: l, End: l, Reason: lf[l]})
	}
	return out
}

func checkFile(f FileChange, blocks []Block, inProfile, haveProfile bool, funcs func(string) (map[string]bool, error)) ([]Range, []Block) {
	fails := lineFail{}
	var required []Block
	var whole []Range
	needBlocks := false

	var nf *srcFile
	if f.Status != "D" {
		var err error
		if nf, err = parseSrc(f.New); err != nil {
			return []Range{{Path: f.Path, Reason: ReasonUnparsable}}, nil
		}
		if hasMalformedBlocks(blocks, nf) {
			return []Range{{Path: f.Path, Reason: "the coverage profile does not match this file"}}, nil
		}
	}
	var of *srcFile
	if f.Status != "A" {
		var err error
		if of, err = parseSrc(f.Old); err != nil {
			return []Range{{Path: f.Path, Reason: ReasonUnparsable + " at base"}}, nil
		}
	}

	// need records that a line requires every block in bs.
	need := func(line int, bs []Block) {
		needBlocks = true
		if len(bs) == 0 {
			fails.add(line, ReasonNoBlock)
			return
		}
		for _, b := range bs {
			required = append(required, b)
			if !b.Executed {
				fails.add(line, ReasonNotExecuted)
			}
		}
	}

	for _, h := range f.Hunks {
		// New side: every added line.
		newCode := false
		if nf != nil {
			for line := h.NewStart; line < h.NewStart+h.NewLines; line++ {
				if checkNewLine(nf, line, blocks, fails, need) {
					newCode = true
				}
			}
		}
		// Old side: what the removed lines were. Code removed from a
		// function is anchored to the blocks around the place, unless the
		// hunk put function code there, which the new side already judged.
		if of == nil || h.OldLines == 0 {
			continue
		}
		anchored := newCode
		for line := h.OldStart; line < h.OldStart+h.OldLines; line++ {
			lt := of.lines[line]
			if lt.directive {
				fails.add(anchorLine(h), ReasonDirective)
			}
			if !lt.code {
				continue
			}
			switch d := of.declAt(line).(type) {
			case *ast.GenDecl:
				if d.Tok == token.IMPORT {
					if of.initImportAt(line) {
						fails.add(anchorLine(h), ReasonInitImport)
					}
					continue
				}
				fails.add(anchorLine(h), ReasonRemovedDecl)
			case *ast.FuncDecl:
				inHunk := func(p token.Pos) bool {
					l := of.fset.Position(p).Line
					return l >= h.OldStart && l < h.OldStart+h.OldLines
				}
				headerRemoved, endRemoved := inHunk(d.Pos()), inHunk(d.End())
				if headerRemoved && !funcStillExists(f.Path, d, funcs) {
					// Deleted, or renamed: the old name is gone.
					fails.add(anchorLine(h), ReasonRemovedFunc)
					continue
				}
				if headerRemoved && endRemoved {
					// The whole function moved: where it is now, it is
					// judged as new code.
					continue
				}
				if !anchored && nf != nil {
					anchored = true
					var bs []Block
					for _, a := range anchorLines(h) {
						bs = append(bs, nf.spanningBlocks(a, blocks)...)
					}
					if len(bs) == 0 {
						needBlocks = true
						fails.add(anchorLine(h), ReasonRemovedNoBlock)
					} else {
						need(anchorLine(h), dedupe(bs))
					}
				}
			case nil:
				if !of.isPackageClause(line) {
					fails.add(anchorLine(h), ReasonOutside)
				}
			}
		}
	}
	if nf != nil && nf.lineDirective && len(fails)+len(required) > 0 {
		whole = append(whole, Range{Path: f.Path, Reason: ReasonLineDirective})
	}
	if needBlocks && haveProfile && !inProfile {
		// Nothing about this file can be verified; report it once.
		for l, r := range fails {
			if r == ReasonNoBlock {
				delete(fails, l)
			}
		}
		whole = append(whole, Range{Path: f.Path, Reason: ReasonNotInProfile})
	}
	return append(whole, fails.ranges(f.Path)...), required
}

// checkNewLine applies the new-side rule to one line and reports whether
// it judged code inside a function body or signature.
func checkNewLine(nf *srcFile, line int, blocks []Block, fails lineFail, need func(int, []Block)) bool {
	lt := nf.lines[line]
	if lt.directive {
		fails.add(line, ReasonDirective)
	}
	if !lt.code {
		return false
	}
	spans := lt.content()
	switch d := nf.declAt(line).(type) {
	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			if nf.initImportAt(line) {
				fails.add(line, ReasonInitImport)
			}
			return false
		}
		fails.add(line, ReasonPackageDecl)
	case *ast.FuncDecl:
		if d.Body == nil {
			fails.add(line, ReasonNoBlock)
			return false
		}
		lb := nf.fset.Position(d.Body.Lbrace)
		rb := nf.fset.Position(d.Body.Rbrace)
		var bs []Block
		fallback := false
		for _, s := range spans {
			if before(line, s.start, lb.Line, lb.Column) {
				// Part of the signature: the function must have been entered.
				if e, ok := entryBlock(blocks, lb, rb); ok {
					bs = append(bs, e)
				} else {
					fallback = true
				}
				continue
			}
			found := false
			for _, b := range blocks {
				if overlaps(b, line, s.start, s.end) {
					bs = append(bs, b)
					found = true
				}
			}
			if !found {
				fallback = true
			}
		}
		if fallback {
			bs = append(bs, nf.spanningBlocks(line, blocks)...)
		}
		need(line, dedupe(bs))
		return true
	case nil:
		if !nf.isPackageClause(line) {
			fails.add(line, ReasonOutside)
		}
	default:
		fails.add(line, ReasonOutside)
	}
	return false
}

func anchorLine(h Hunk) int {
	if h.NewLines > 0 {
		return h.NewStart
	}
	if h.NewStart == 0 {
		return 1
	}
	return h.NewStart
}

// anchorLines are the new-side lines that stand for a removal: the
// replacement lines, or the lines on either side of a pure deletion.
func anchorLines(h Hunk) []int {
	if h.NewLines > 0 {
		var out []int
		for l := h.NewStart; l < h.NewStart+h.NewLines; l++ {
			out = append(out, l)
		}
		return out
	}
	if h.NewStart == 0 {
		return []int{1}
	}
	return []int{h.NewStart, h.NewStart + 1}
}

func funcStillExists(file string, d *ast.FuncDecl, funcs func(string) (map[string]bool, error)) bool {
	if funcs == nil || d.Name.Name == "init" || d.Name.Name == "_" {
		return false
	}
	have, err := funcs(path.Dir(file))
	if err != nil {
		return false
	}
	return have[FuncKey(d)]
}

// FuncKey names a function declaration as "Name" or "Recv.Name".
func FuncKey(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + d.Name.Name
		}
		return "?." + d.Name.Name
	}
}

// DeclaredFuncs parses Go source and returns its functions by FuncKey.
func DeclaredFuncs(src []byte) (map[string]bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name != "init" {
			out[FuncKey(fd)] = true
		}
	}
	return out, nil
}

func dedupe(bs []Block) []Block {
	seen := map[string]bool{}
	var out []Block
	for _, b := range bs {
		if !seen[b.Key()] {
			seen[b.Key()] = true
			out = append(out, b)
		}
	}
	return out
}

// before reports whether (l1, c1) precedes (l2, c2).
func before(l1, c1, l2, c2 int) bool {
	return l1 < l2 || (l1 == l2 && c1 < c2)
}

// overlaps reports whether the span [start, end) of line intersects the
// block's range.
func overlaps(b Block, line, start, end int) bool {
	return before(line, start, b.EndLine, b.EndCol) && before(b.StartLine, b.StartCol, line, end)
}

// entryBlock is the first block of a function body: the one starting
// earliest at or after its opening brace and before its closing one.
func entryBlock(blocks []Block, lb, rb token.Position) (Block, bool) {
	var best Block
	ok := false
	for _, b := range blocks {
		if before(b.StartLine, b.StartCol, lb.Line, lb.Column) || before(rb.Line, rb.Column, b.StartLine, b.StartCol) {
			continue
		}
		if !ok || before(b.StartLine, b.StartCol, best.StartLine, best.StartCol) {
			best, ok = b, true
		}
	}
	return best, ok
}

func hasMalformedBlocks(blocks []Block, f *srcFile) bool {
	for _, b := range blocks {
		if b.EndLine > f.numLines+1 {
			return true
		}
	}
	return false
}

// span is one token's columns on a line, end exclusive.
type span struct {
	start, end int
	brace      bool
}

type lineInfo struct {
	code      bool
	directive bool
	spans     []span
}

// content is the code a block must contain for the line to count as
// executed: every token except braces, unless braces are all there is.
func (l lineInfo) content() []span {
	var out []span
	for _, s := range l.spans {
		if !s.brace {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return l.spans
	}
	return out
}

type srcFile struct {
	fset          *token.FileSet
	file          *ast.File
	lines         map[int]*lineInfo
	numLines      int
	lineDirective bool
}

func parseSrc(src []byte) (*srcFile, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	sf := &srcFile{fset: fset, file: file, lines: map[int]*lineInfo{}, numLines: strings.Count(string(src), "\n") + 1}
	for i := 1; i <= sf.numLines+1; i++ {
		sf.lines[i] = &lineInfo{}
	}
	var s scanner.Scanner
	tf := token.NewFileSet().AddFile("", -1, len(src))
	var scanErr error
	s.Init(tf, src, func(token.Position, string) { scanErr = fmt.Errorf("scan error") }, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		text := lit
		if text == "" {
			text = tok.String()
		}
		p := tf.Position(pos)
		if tok == token.COMMENT {
			if isDirective(text) {
				sf.lines[p.Line].directive = true
				if strings.HasPrefix(text, "//line ") || strings.HasPrefix(text, "/*line ") {
					sf.lineDirective = true
				}
			}
			continue
		}
		// A token may span lines (raw strings): mark every line it covers.
		line, col := p.Line, p.Column
		rest := text
		for {
			i := strings.IndexByte(rest, '\n')
			li := sf.lines[line]
			if li == nil {
				li = &lineInfo{}
				sf.lines[line] = li
			}
			li.code = true
			if i < 0 {
				li.spans = append(li.spans, span{start: col, end: col + len(rest), brace: tok == token.LBRACE || tok == token.RBRACE})
				break
			}
			li.spans = append(li.spans, span{start: col, end: col + i + 1})
			rest = rest[i+1:]
			line, col = line+1, 1
		}
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return sf, nil
}

func isDirective(c string) bool {
	for _, p := range []string{"//go:", "//line ", "/*line ", "//export ", "//extern ", "// +build", "//+build"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// declAt returns the top-level declaration that contains code on line.
func (f *srcFile) declAt(line int) ast.Decl {
	for _, d := range f.file.Decls {
		if f.fset.Position(d.Pos()).Line <= line && line <= f.fset.Position(d.End()).Line {
			return d
		}
	}
	return nil
}

func (f *srcFile) isPackageClause(line int) bool {
	return f.fset.Position(f.file.Package).Line == line || f.fset.Position(f.file.Name.End()).Line == line
}

// initImportAt reports whether a blank or dot import is on line.
func (f *srcFile) initImportAt(line int) bool {
	for _, imp := range f.file.Imports {
		if f.fset.Position(imp.Pos()).Line <= line && line <= f.fset.Position(imp.End()).Line && imp.Name != nil && (imp.Name.Name == "_" || imp.Name.Name == ".") {
			return true
		}
	}
	return false
}

// spanningBlocks are the blocks of the function containing line whose line
// range includes line.
func (f *srcFile) spanningBlocks(line int, blocks []Block) []Block {
	fd, ok := f.declAt(line).(*ast.FuncDecl)
	if !ok || fd.Body == nil {
		return nil
	}
	lb := f.fset.Position(fd.Body.Lbrace)
	rb := f.fset.Position(fd.Body.Rbrace)
	var out []Block
	for _, b := range blocks {
		if before(b.StartLine, b.StartCol, lb.Line, lb.Column) || before(rb.Line, rb.Column+1, b.EndLine, b.EndCol) {
			continue
		}
		if b.StartLine <= line && line <= b.EndLine {
			out = append(out, b)
		}
	}
	return out
}
