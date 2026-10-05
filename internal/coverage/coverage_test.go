package coverage

import (
	"fmt"
	"strings"
	"testing"
)

// mainGo is the candidate file the rule tests judge. Its blocks below are
// what go test -coverpkg=./... reported for it in the golang:1.22 image
// with a test that calls greeting and kind(0).
const mainGo = `package main

import (
	"context"
	"fmt"

	"example.com/app/lib"
)

// greeting wraps the library call so tests do not depend on its signature.
func greeting(name string) string {
	return lib.Greet(context.Background(), name)
}

func kind(n int) string {
	if n < 0 {
		return "negative"
	}
	switch n {
	case 0:
		return "zero"
	default:
		return "positive"
	}
}

func main() {
	fmt.Println(greeting("steward"))
}
`

var mainBlocks = []string{
	"example.com/app/main.go:11.35,13.2 1 1",
	"example.com/app/main.go:15.25,16.11 1 1",
	"example.com/app/main.go:16.11,18.3 1 0",
	"example.com/app/main.go:19.2,19.11 1 1",
	"example.com/app/main.go:20.9,21.16 1 1",
	"example.com/app/main.go:22.10,23.20 1 0",
	"example.com/app/main.go:27.13,29.2 1 0",
}

// output is what the coverage script prints for a profile with these
// block lines.
func output(blocks ...string) string {
	lines := append([]string{"mode: set"}, blocks...)
	return strings.Join(lines, "\n") + "\n" + fmt.Sprintf("%s%d\n", EndMarker, len(lines))
}

func evidence(blocks ...string) *Evidence {
	return &Evidence{Profile: output(blocks...)}
}

// replace returns mainGo with line n (1-based) replaced by text, which may
// hold several lines or none.
func replace(src string, n int, text string) string {
	lines := strings.Split(src, "\n")
	var repl []string
	if text != "" || n == 0 {
		repl = strings.Split(text, "\n")
	}
	out := append([]string{}, lines[:n-1]...)
	out = append(out, repl...)
	return strings.Join(append(out, lines[n:]...), "\n")
}

func modified(old string, hunks ...Hunk) FileChange {
	return FileChange{Path: "main.go", Status: "M", Old: []byte(old), New: []byte(mainGo), Hunks: hunks}
}

func funcsOf(names ...string) func(string) (map[string]bool, error) {
	return func(string) (map[string]bool, error) {
		m, err := DeclaredFuncs([]byte(mainGo))
		for _, n := range names {
			m[n] = true
		}
		return m, err
	}
}

func check(ev *Evidence, files ...FileChange) Verdict {
	return Check(Input{ModulePath: "example.com/app", Files: files, Evidence: ev, PackageFuncs: funcsOf()})
}

func wantVerified(t *testing.T, v Verdict, blocks, executed int) {
	t.Helper()
	if !v.Verified || v.Blocks != blocks || v.Executed != executed || len(v.Unverified) != 0 {
		t.Fatalf("verdict %+v, want verified with %d of %d blocks", v, executed, blocks)
	}
}

func wantUnverified(t *testing.T, v Verdict, want ...string) {
	t.Helper()
	var got []string
	for _, r := range v.Unverified {
		got = append(got, r.String())
	}
	if v.Verified || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unverified:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The repair of a call site that a test executes, with the import it
// needs: one block, executed.
func TestCheck_ExecutedRepairIsVerified(t *testing.T) {
	old := replace(replace(mainGo, 12, "\treturn lib.Greet(name)"), 4, "")
	v := check(evidence(mainBlocks...), modified(old, Hunk{3, 0, 4, 1}, Hunk{11, 1, 12, 1}))
	wantVerified(t, v, 1, 1)
}

func TestCheck_LinesNoTestExecutes(t *testing.T) {
	// A changed line in main, which no test calls.
	v := check(evidence(mainBlocks...), modified(replace(mainGo, 28, `	fmt.Println(greeting("x"))`), Hunk{28, 1, 28, 1}))
	wantUnverified(t, v, "main.go:28 (no test executes it)")
	if v.Blocks != 1 || v.Executed != 0 {
		t.Fatalf("blocks %d executed %d", v.Blocks, v.Executed)
	}
	// A changed signature needs the function to have been entered.
	v = check(evidence(mainBlocks...), modified(replace(mainGo, 27, "func main()  {"), Hunk{27, 1, 27, 1}))
	wantUnverified(t, v, "main.go:27 (no test executes it)")
	// The body of an if the tests never enter.
	v = check(evidence(mainBlocks...), modified(replace(mainGo, 17, `		return "neg"`), Hunk{17, 1, 17, 1}))
	wantUnverified(t, v, "main.go:17 (no test executes it)")
	// A default branch the tests never reach, label and body.
	v = check(evidence(mainBlocks...), modified(replace(replace(mainGo, 22, "\tdefault :"), 23, `		return "pos"`), Hunk{22, 2, 22, 2}))
	wantUnverified(t, v, "main.go:22-23 (no test executes it)")
}

// A changed if header needs the block that evaluates the condition, not
// the body the tests skip; a changed case label needs its own body.
func TestCheck_HeadersAndLabels(t *testing.T) {
	// The profile is of the changed file, so blocks after a changed column
	// move with it.
	with := func(from, to string) []string {
		out := append([]string{}, mainBlocks...)
		for i, b := range out {
			if strings.HasPrefix(b, from) {
				out[i] = strings.Replace(b, from, to, 1)
			}
		}
		return out
	}
	v := check(evidence(mainBlocks...), modified(replace(mainGo, 16, "\tif n < 1 {"), Hunk{16, 1, 16, 1}))
	wantVerified(t, v, 1, 1)
	v = check(evidence(with("example.com/app/main.go:20.9,", "example.com/app/main.go:20.11,")...), modified(replace(mainGo, 20, "\tcase 0x0:"), Hunk{20, 1, 20, 1}))
	wantVerified(t, v, 1, 1)
	// A signature line of a function the tests call.
	v = check(evidence(with("example.com/app/main.go:11.35,", "example.com/app/main.go:11.36,")...), modified(replace(mainGo, 11, "func greeting(name string) string  {"), Hunk{11, 1, 11, 1}))
	wantVerified(t, v, 1, 1)
}

func TestCheck_Removals(t *testing.T) {
	// A statement removed from a function the tests call.
	old := replace(mainGo, 12, "\t_ = name\n\treturn lib.Greet(context.Background(), name)")
	wantVerified(t, check(evidence(mainBlocks...), modified(old, Hunk{12, 1, 11, 0})), 1, 1)
	// ... and from one they do not.
	old = replace(mainGo, 28, "\t_ = 1\n\tfmt.Println(greeting(\"steward\"))")
	wantUnverified(t, check(evidence(mainBlocks...), modified(old, Hunk{28, 1, 27, 0})), "main.go:27 (no test executes it)")
	// A statement replaced by a comment still needs the block it was in.
	old = replace(mainGo, 28, "\tprintln()\n\tfmt.Println(greeting(\"steward\"))")
	src := replace(mainGo, 28, "\t// nothing\n\tfmt.Println(greeting(\"steward\"))")
	f := FileChange{Path: "main.go", Status: "M", Old: []byte(old), New: []byte(src), Hunks: []Hunk{{28, 1, 28, 1}}}
	blocks := append(append([]string{}, mainBlocks[:6]...), "example.com/app/main.go:27.13,30.2 1 0")
	wantUnverified(t, check(evidence(blocks...), f), "main.go:28 (no test executes it)")
	// A function deleted outright.
	old = mainGo + "\nfunc helper() int { return 1 }\n"
	wantUnverified(t, check(evidence(mainBlocks...), modified(old, Hunk{30, 2, 29, 0})), "main.go:29 (a function or method was removed or renamed: nothing shows the code that relied on it still behaves)")
	// The same function moved to another file of the package is judged
	// where it now is.
	v := Check(Input{ModulePath: "example.com/app", Files: []FileChange{modified(old, Hunk{30, 2, 29, 0})}, Evidence: evidence(mainBlocks...), PackageFuncs: funcsOf("helper")})
	wantVerified(t, v, 0, 0)
	// A removed package-level declaration.
	old = replace(mainGo, 9, "\nvar unused = 1\n")
	wantUnverified(t, check(evidence(mainBlocks...), modified(old, Hunk{10, 2, 9, 0})), "main.go:9 (a package-level declaration was removed)")
}

func TestCheck_DeclarationsImportsDirectives(t *testing.T) {
	notes := func(src string) FileChange {
		return FileChange{Path: "notes.go", Status: "A", New: []byte(src), Hunks: []Hunk{{0, 0, 1, strings.Count(src, "\n")}}}
	}
	// Comments and the package clause need nothing; there is no block to
	// find and none is asked for.
	wantVerified(t, check(evidence(mainBlocks...), notes("package main\n\n// Notes on the upgrade.\n")), 0, 0)
	wantUnverified(t, check(evidence(mainBlocks...), notes("package main\n\n// Notes.\nvar upgradeNotes = \"x\"\n")),
		"notes.go:4 (package-level declaration: it takes effect wherever it is used, which coverage of these lines cannot show)")
	wantUnverified(t, check(evidence(mainBlocks...), notes("package main\n\nconst limit = 3\n")),
		"notes.go:3 (package-level declaration: it takes effect wherever it is used, which coverage of these lines cannot show)")
	wantUnverified(t, check(evidence(mainBlocks...), notes("package main\n\nimport _ \"embed\"\n")),
		"notes.go:3 (blank or dot import: it runs package initialization, which coverage cannot attribute)")
	// An ordinary import needs nothing.
	old := replace(mainGo, 4, "")
	wantVerified(t, check(evidence(mainBlocks...), modified(old, Hunk{3, 0, 4, 1})), 0, 0)
	// A directive is never verified.
	src := replace(mainGo, 10, "//go:noinline")
	f := FileChange{Path: "main.go", Status: "M", Old: []byte(mainGo), New: []byte(src), Hunks: []Hunk{{10, 1, 10, 1}}}
	wantUnverified(t, check(evidence(mainBlocks...), f), "main.go:10 (compiler directive)")
	// Neither is any change to a file with //line directives.
	src = replace(mainGo, 28, "\tfmt.Println(greeting(\"x\")) //line other.go:1")
	f = FileChange{Path: "main.go", Status: "M", Old: []byte(mainGo), New: []byte(src), Hunks: []Hunk{{28, 1, 28, 1}}}
	v := check(evidence(mainBlocks...), f)
	if v.Verified || v.Unverified[0].Reason != ReasonLineDirective {
		t.Fatalf("verdict %+v", v)
	}
}

func TestCheck_Files(t *testing.T) {
	// Not Go source.
	wantUnverified(t, check(evidence(mainBlocks...), FileChange{Path: "templates/page.tmpl", Status: "M", Old: []byte("a\n"), New: []byte("b\n"), Hunks: []Hunk{{1, 1, 1, 1}}}),
		"templates/page.tmpl (not Go source: coverage cannot show whether it is used)")
	// Tests are the protected-path rule's business.
	wantVerified(t, check(evidence(mainBlocks...), FileChange{Path: "main_test.go", Status: "M", Old: []byte("x"), New: []byte("y"), Hunks: []Hunk{{1, 1, 1, 1}}}), 0, 0)
	// A file the build excludes has no blocks at all.
	win := "package main\n\nfunc win() int {\n\treturn 3\n}\n"
	wantUnverified(t, check(evidence(mainBlocks...), FileChange{Path: "other_windows.go", Status: "A", New: []byte(win), Hunks: []Hunk{{0, 0, 1, 5}}}),
		"other_windows.go (the coverage profile has no blocks for this file: it is excluded from the build, or no test binary compiled it)")
	// A file that does not parse, and a binary change.
	wantUnverified(t, check(evidence(mainBlocks...), FileChange{Path: "x.go", Status: "A", New: []byte("package main\nfunc {"), Hunks: []Hunk{{0, 0, 1, 2}}}), "x.go (the file does not parse)")
	wantUnverified(t, check(evidence(mainBlocks...), FileChange{Path: "y.go", Status: "M", Binary: true}), "y.go (binary change)")
	// A generated file is judged like any other.
	gen := "// Code generated by hand. DO NOT EDIT.\n\npackage main\n\nfunc generated() int { return 2 }\n"
	wantUnverified(t, check(evidence(append(mainBlocks, "example.com/app/gen.go:5.22,5.34 1 0")...), FileChange{Path: "gen.go", Status: "A", New: []byte(gen), Hunks: []Hunk{{0, 0, 1, 5}}}),
		"gen.go:5 (no test executes it)")
}

// Without usable evidence nothing is verified, not even a change that
// would need no block.
func TestCheck_EvidenceFailClosed(t *testing.T) {
	old := replace(replace(mainGo, 12, "\treturn lib.Greet(name)"), 4, "")
	f := modified(old, Hunk{3, 0, 4, 1}, Hunk{11, 1, 12, 1})
	good := output(mainBlocks...)
	for name, ev := range map[string]*Evidence{
		"none":            nil,
		"failed":          {ExitCode: 1, Profile: good, Stderr: "FAIL\texample.com/app"},
		"timed out":       {TimedOut: true, Profile: good},
		"truncated":       {StdoutTruncated: true, Profile: good},
		"empty":           {Profile: ""},
		"cut mid-line":    {Profile: good[:len(good)-30]},
		"cut at a line":   {Profile: strings.Join(strings.SplitAfter(good, "\n")[:4], "")},
		"marker miscount": {Profile: strings.Replace(good, EndMarker+"8", EndMarker+"7", 1)},
		"not a profile":   {Profile: "ok  \texample.com/app\n" + EndMarker + "1\n"},
		"other tree":      {Profile: output(append(mainBlocks, "example.com/app/main.go:400.2,401.2 1 1")...)},
	} {
		v := check(ev, f)
		if v.Verified || len(v.Unverified) == 0 {
			t.Errorf("%s: verdict %+v", name, v)
		}
	}
	// Comment-only changes too.
	notes := FileChange{Path: "notes.go", Status: "A", New: []byte("package main\n\n// x\n"), Hunks: []Hunk{{0, 0, 1, 3}}}
	if v := check(nil, notes); v.Verified {
		t.Fatalf("verified without evidence: %+v", v)
	}
}

func TestParseProfile(t *testing.T) {
	p, err := ParseProfile([]byte(output(append(mainBlocks, "example.com/app/main.go:27.13,29.2 1 1", "example.com/app/lib/lib.go:5.48,5.72 1 0")...)))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files["example.com/app/main.go"]) != 7 || len(p.Files["example.com/app/lib/lib.go"]) != 1 {
		t.Fatalf("files %+v", p.Files)
	}
	// A block listed by two test binaries counts as executed if either
	// executed it.
	for _, b := range p.Files["example.com/app/main.go"] {
		if b.StartLine == 27 && !b.Executed {
			t.Fatal("duplicate executed entry lost")
		}
	}
	for name, raw := range map[string]string{
		"no newline":       strings.TrimSuffix(output(mainBlocks...), "\n"),
		"bad mode":         strings.Replace(output(mainBlocks...), "mode: set", "mode: fast", 1),
		"malformed":        output("example.com/app/main.go:11.35-13.2 1 1"),
		"impossible range": output("example.com/app/main.go:13.35,11.2 1 1"),
		"zero line":        output("example.com/app/main.go:0.1,1.2 1 1"),
		"inconsistent dup": output("example.com/app/main.go:11.35,13.2 1 1", "example.com/app/main.go:11.35,13.2 2 0"),
		"marker not last":  output(mainBlocks...) + "extra\n",
		"marker malformed": strings.Replace(output(mainBlocks...), EndMarker+"8", EndMarker+"eight", 1),
		"only marker":      EndMarker + "0\n",
		"negative count":   output("example.com/app/main.go:11.35,13.2 1 -1"),
	} {
		if _, err := ParseProfile([]byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestParseHunks(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\nindex 1..2 100644\n--- a/main.go\n+++ b/main.go\n@@ -3,0 +4 @@ import (\n+\t\"context\"\n@@ -11 +12 @@ func greeting(name string) string {\n-\treturn lib.Greet(name)\n+\treturn lib.Greet(context.Background(), name)\n@@ -30,2 +29,0 @@\n"
	h, err := ParseHunks([]byte(diff))
	if err != nil {
		t.Fatal(err)
	}
	want := []Hunk{{3, 0, 4, 1}, {11, 1, 12, 1}, {30, 2, 29, 0}}
	if fmt.Sprint(h) != fmt.Sprint(want) {
		t.Fatalf("hunks %v, want %v", h, want)
	}
	if _, err := ParseHunks([]byte("@@ -x +1 @@\n")); err == nil {
		t.Fatal("malformed header parsed")
	}
}

func TestDeclaredFuncs(t *testing.T) {
	m, err := DeclaredFuncs([]byte("package p\n\ntype T[K any] struct{}\n\nfunc (t *T[K]) M() {}\nfunc (T[K]) N() {}\nfunc F() {}\nfunc init() {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !m["T.M"] || !m["T.N"] || !m["F"] || m["init"] || len(m) != 3 {
		t.Fatalf("funcs %v", m)
	}
}
