package vuln

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden outputs in testdata are real govulncheck runs, made in an
// execute-like container (no network, read-only source, module cache, and
// database, read-only root, unprivileged user) against the fixture
// database written by WriteFixtureDB, for the three scanner releases the
// pin table uses. Directory names are govulncheck-<release>-go<minor>;
// each case has .stdout, .stderr, and .exit.
var goldenDirs = []struct {
	dir, version, goVersion string
}{
	{"govulncheck-v1.1.4-go1.22", "v1.1.4", "go1.22.12"},
	{"govulncheck-v1.7.0-go1.25", "v1.7.0", "go1.25.14"},
	{"govulncheck-v1.8.0-go1.27", "v1.8.0", "go1.27.1"},
}

func loadGolden(t *testing.T, dir, name string) Output {
	t.Helper()
	base := filepath.Join("testdata", dir, name)
	stdout, err := os.ReadFile(base + ".stdout")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(base + ".stderr")
	if err != nil {
		t.Fatal(err)
	}
	exit, err := os.ReadFile(base + ".exit")
	if err != nil {
		t.Fatal(err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(exit)))
	if err != nil {
		t.Fatal(err)
	}
	return Output{Stdout: stdout, Stderr: stderr, ExitCode: code}
}

func fixtureExpect(version, goVersion string) Expect {
	return Expect{ScannerVersion: version, DB: "file:///vulndb", DBModified: FixtureTime, GoVersion: goVersion}
}

type wantFinding struct {
	key     string
	level   Level
	found   string
	fixed   string
	stdlib  bool
	symbols string
	path    string
}

func TestParse_Goldens(t *testing.T) {
	stdlibFmt := func(goVersion string) wantFinding {
		return wantFinding{key: "GO-TEST-0004 stdlib", level: LevelPackage, found: "v" + strings.TrimPrefix(goVersion, "go"), fixed: "v1.99.0", stdlib: true}
	}
	cases := map[string]func(goVersion string) []wantFinding{
		"patch-safe": func(g string) []wantFinding {
			return []wantFinding{
				{key: "GO-TEST-0001 example.com/lib", level: LevelSymbol, found: "v1.2.1", fixed: "v1.2.4", symbols: "lib.Greet", path: "app.main (main.go:10) -> lib.Greet"},
				stdlibFmt(g),
			}
		},
		"patch-safe-v124": func(g string) []wantFinding { return []wantFinding{stdlibFmt(g)} },
		"patch-safe-v130": func(g string) []wantFinding {
			return []wantFinding{
				{key: "GO-TEST-0002 example.com/lib", level: LevelSymbol, found: "v1.3.0", fixed: "", symbols: "lib.Greet", path: "app.main (main.go:11) -> lib.Greet"},
				stdlibFmt(g),
			}
		},
		"closure-regression": func(g string) []wantFinding {
			return []wantFinding{
				stdlibFmt(g),
				{key: "GO-TEST-0005 example.com/util", level: LevelSymbol, found: "v0.1.0", fixed: "v0.2.0", symbols: "util.Trim", path: "app.main (main.go:10) -> core.Run (core.go:7) -> util.Trim"},
			}
		},
		"moved-package": func(g string) []wantFinding {
			return []wantFinding{
				{key: "GO-TEST-0003 example.com/toolkit", level: LevelModule, found: "v1.0.0", fixed: ""},
				stdlibFmt(g),
			}
		},
	}
	for _, gd := range goldenDirs {
		for name, want := range cases {
			t.Run(gd.dir+"/"+name, func(t *testing.T) {
				s := Parse(loadGolden(t, gd.dir, name), fixtureExpect(gd.version, gd.goVersion))
				if !s.Conclusive {
					t.Fatalf("inconclusive: %s", s.Reason)
				}
				if s.ScannerVersion != gd.version || s.GoVersion != gd.goVersion || !s.DBModified.Equal(FixtureTime) || s.DB != "file:///vulndb" {
					t.Fatalf("config = %+v", s)
				}
				if len(s.Roots) != 1 || s.Roots[0] != "example.com/app" {
					t.Fatalf("roots = %v", s.Roots)
				}
				w := want(gd.goVersion)
				if len(s.Findings) != len(w) {
					t.Fatalf("findings = %+v, want %d", s.Findings, len(w))
				}
				for i, f := range s.Findings {
					x := w[i]
					if f.Key != x.key || f.Level != x.level || f.FoundVersion != x.found || f.FixedVersion != x.fixed || f.Stdlib != x.stdlib {
						t.Errorf("finding %d = %+v, want %+v", i, f, x)
					}
					if got := strings.Join(f.Symbols, ","); got != x.symbols {
						t.Errorf("finding %d symbols = %q, want %q", i, got, x.symbols)
					}
					if got := f.CallPath(); got != x.path {
						t.Errorf("finding %d call path = %q, want %q", i, got, x.path)
					}
					if f.Level == LevelSymbol && f.Traces == 0 {
						t.Errorf("finding %d has no trace count", i)
					}
				}
				// The withdrawn advisory is never reported.
				if _, ok := s.Finding("GO-TEST-0006", "example.com/lib"); ok {
					t.Fatal("withdrawn advisory reported")
				}
			})
		}
	}
}

func TestParse_GoldenFailures(t *testing.T) {
	cases := []struct {
		dir, name, version, goVersion, reason string
	}{
		// The candidate does not type-check: the scanner prints its config
		// and fails while loading packages.
		{"govulncheck-v1.1.4-go1.22", "patch-safe-v130-broken", "v1.1.4", "go1.22.12", "scanner exited 1: govulncheck: loading packages:"},
		{"govulncheck-v1.7.0-go1.25", "patch-safe-v130-broken", "v1.7.0", "go1.25.14", "scanner exited 1"},
		{"govulncheck-v1.8.0-go1.27", "patch-safe-v130-broken", "v1.8.0", "go1.27.1", "scanner exited 1"},
		// v1.1.4 exits 0 after printing only its config when there is no
		// pattern or the pattern matches no package. Exit status alone would
		// read this as "no vulnerabilities".
		{"govulncheck-v1.1.4-go1.22", "nopattern", "v1.1.4", "go1.22.12", "no SBOM message"},
		{"govulncheck-v1.1.4-go1.22", "emptymatch", "v1.1.4", "go1.22.12", "no SBOM message"},
		{"govulncheck-v1.1.4-go1.22", "nomatch", "v1.1.4", "go1.22.12", "scanner exited 1"},
		// v1.8.0 reports the same two cases with exit status 2.
		{"govulncheck-v1.8.0-go1.27", "nopattern", "v1.8.0", "go1.27.1", "scanner exited 2: govulncheck: no package patterns provided"},
		{"govulncheck-v1.8.0-go1.27", "emptymatch", "v1.8.0", "go1.27.1", "scanner exited 2: govulncheck: no packages matched"},
		{"govulncheck-v1.8.0-go1.27", "nomatch", "v1.8.0", "go1.27.1", "scanner exited 1"},
		// An empty, read-only module cache: the offline load fails.
		{"govulncheck-v1.8.0-go1.27", "nocache", "v1.8.0", "go1.27.1", "could not create module cache"},
	}
	for _, c := range cases {
		t.Run(c.dir+"/"+c.name, func(t *testing.T) {
			s := Parse(loadGolden(t, c.dir, c.name), fixtureExpect(c.version, c.goVersion))
			if s.Conclusive {
				t.Fatalf("conclusive: %+v", s)
			}
			if !strings.Contains(s.Reason, c.reason) {
				t.Fatalf("reason = %q, want it to contain %q", s.Reason, c.reason)
			}
			if len(s.Findings) != 0 {
				t.Fatal("an inconclusive scan carries findings")
			}
		})
	}
}

func readGzip(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Against the real database of 2026-10-01, standard library findings are
// judged against the image's Go version: go1.22.12 is past the end of its
// support and carries dozens, go1.27.1 none. The fixed version reported
// for go1.22 is in a later release line, because 1.22 receives no fixes.
func TestParse_RealDatabase(t *testing.T) {
	modified := time.Date(2026, 10, 1, 20, 24, 15, 0, time.UTC)
	old := Parse(Output{Stdout: readGzip(t, "testdata/realdb-20261001-v1.1.4-go1.22-patch-safe.stdout.gz")}, Expect{ScannerVersion: "v1.1.4", DB: "file:///vulndb", DBModified: modified, GoVersion: "go1.22.12"})
	if !old.Conclusive {
		t.Fatal(old.Reason)
	}
	symbols, packages := 0, 0
	for _, f := range old.Findings {
		if !f.Stdlib || f.Module != "stdlib" || f.FoundVersion != "v1.22.12" {
			t.Fatalf("unexpected finding %+v", f)
		}
		if f.FixedVersion == "" || !strings.HasPrefix(f.FixedVersion, "v1.2") {
			t.Fatalf("fixed version %q", f.FixedVersion)
		}
		switch f.Level {
		case LevelSymbol:
			symbols++
			if len(f.Trace) < 2 || f.CallPath() == "" {
				t.Fatalf("symbol finding without a trace: %+v", f)
			}
		case LevelPackage:
			packages++
		}
	}
	if len(old.Findings) < 40 || symbols == 0 || packages == 0 {
		t.Fatalf("findings %d, symbol %d, package %d", len(old.Findings), symbols, packages)
	}
	f, ok := old.Finding("GO-2025-3750", "stdlib")
	if !ok || f.Level != LevelSymbol || f.FixedVersion != "v1.23.10" || !contains(f.Packages, "os") || !contains(f.Packages, "syscall") {
		t.Fatalf("GO-2025-3750 = %+v", f)
	}

	cur := Parse(Output{Stdout: readGzip(t, "testdata/realdb-20261001-v1.8.0-go1.27-patch-safe.stdout.gz")}, Expect{ScannerVersion: "v1.8.0", DB: "file:///vulndb", DBModified: modified, GoVersion: "go1.27.1"})
	if !cur.Conclusive || len(cur.Findings) != 0 {
		t.Fatalf("go1.27.1: conclusive %v (%s), %d findings", cur.Conclusive, cur.Reason, len(cur.Findings))
	}
}

func TestParse_ExpectMismatches(t *testing.T) {
	out := loadGolden(t, "govulncheck-v1.8.0-go1.27", "patch-safe")
	base := fixtureExpect("v1.8.0", "go1.27.1")
	cases := map[string]func(*Expect){
		"scanner version": func(e *Expect) { e.ScannerVersion = "v1.7.0" },
		"database":        func(e *Expect) { e.DB = "file:///other" },
		"database time":   func(e *Expect) { e.DBModified = FixtureTime.Add(time.Second) },
		"go version":      func(e *Expect) { e.GoVersion = "go1.27.0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if s := Parse(out, e); s.Conclusive {
				t.Fatal("conclusive despite mismatch")
			}
		})
	}
	if s := Parse(out, Expect{}); !s.Conclusive {
		t.Fatalf("empty expectation: %s", s.Reason)
	}
}

func TestParse_ProcessFailures(t *testing.T) {
	good := loadGolden(t, "govulncheck-v1.8.0-go1.27", "patch-safe")
	cases := map[string]func(*Output){
		"timed out":          func(o *Output) { o.TimedOut = true },
		"stdout truncated":   func(o *Output) { o.StdoutTruncated = true },
		"stderr truncated":   func(o *Output) { o.StderrTruncated = true },
		"exit 1":             func(o *Output) { o.ExitCode = 1 },
		"exit 3 (text mode)": func(o *Output) { o.ExitCode = 3 },
		"killed":             func(o *Output) { o.ExitCode = 137 },
		"empty stdout":       func(o *Output) { o.Stdout = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := good
			mutate(&o)
			if s := Parse(o, Expect{}); s.Conclusive || len(s.Findings) != 0 {
				t.Fatalf("conclusive: %+v", s)
			}
		})
	}
}

// splitMessages returns the golden's top-level JSON objects as text.
func splitMessages(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	depth, start, inString, escaped := 0, -1, false, false
	for i, c := range b {
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				out = append(out, string(b[start:i+1]))
			}
		}
	}
	return out
}

func TestParse_MalformedStreams(t *testing.T) {
	good := loadGolden(t, "govulncheck-v1.8.0-go1.27", "patch-safe")
	msgs := splitMessages(t, good.Stdout)
	if len(msgs) != 13 {
		t.Fatalf("golden has %d messages", len(msgs))
	}
	join := func(ms ...string) []byte { return []byte(strings.Join(ms, "\n")) }
	without := func(i int) []byte {
		var ms []string
		ms = append(ms, msgs[:i]...)
		return join(append(ms, msgs[i+1:]...)...)
	}
	cases := map[string][]byte{
		"garbage line":             append(append([]byte{}, good.Stdout...), []byte("\nwarning: something\n")...),
		"text before config":       append([]byte("Scanning your code...\n"), good.Stdout...),
		"no config":                without(0),
		"no SBOM":                  without(1),
		"no checking progress":     without(7),
		"config not first":         join(append([]string{msgs[1], msgs[0]}, msgs[2:]...)...),
		"two configs":              join(append([]string{msgs[0]}, msgs...)...),
		"unknown kind":             join(append(append([]string{}, msgs...), `{"verdict": {"ok": true}}`)...),
		"two kinds in one message": join(append(append([]string{}, msgs[:8]...), `{"progress": {"message": "x"}, "finding": {"osv": "GO-TEST-0001"}}`)...),
		"null kind":                join(append(append([]string{}, msgs...), `{"finding": null}`)...),
		"finding before its osv":   join(append(append([]string{msgs[0], msgs[1], msgs[2], msgs[7]}, msgs[8:]...), msgs[3:7]...)...),
		"finding without trace":    join(append(append([]string{}, msgs...), `{"finding": {"osv": "GO-TEST-0001", "fixed_version": "v1.2.4"}}`)...),
		"finding for unlisted module": join(append(append([]string{}, msgs...),
			`{"finding": {"osv": "GO-TEST-0001", "fixed_version": "v1.2.4", "trace": [{"module": "example.com/other", "version": "v1.0.0"}]}}`)...),
		"finding at wrong version": join(append(append([]string{}, msgs...),
			`{"finding": {"osv": "GO-TEST-0001", "fixed_version": "v1.2.4", "trace": [{"module": "example.com/lib", "version": "v1.2.0"}]}}`)...),
		"symbol finding without caller": join(append(append([]string{}, msgs...),
			`{"finding": {"osv": "GO-TEST-0001", "fixed_version": "v1.2.4", "trace": [{"module": "example.com/lib", "version": "v1.2.1", "package": "example.com/lib", "function": "Greet"}]}}`)...),
		"conflicting fixed versions": join(append(append([]string{}, msgs...),
			`{"finding": {"osv": "GO-TEST-0001", "fixed_version": "v1.2.5", "trace": [{"module": "example.com/lib", "version": "v1.2.1"}]}}`)...),
		"invalid fixed version": join(append(append([]string{}, msgs...),
			`{"finding": {"osv": "GO-TEST-0001", "fixed_version": "1.2.4", "trace": [{"module": "example.com/lib", "version": "v1.2.1"}]}}`)...),
		"wrong protocol":   []byte(strings.Replace(string(good.Stdout), `"protocol_version": "v1.0.0"`, `"protocol_version": "v2.0.0"`, 1)),
		"wrong scan level": []byte(strings.Replace(string(good.Stdout), `"scan_level": "symbol"`, `"scan_level": "module"`, 1)),
		"binary mode":      []byte(strings.Replace(string(good.Stdout), `"scan_mode": "source"`, `"scan_mode": "binary"`, 1)),
		"no database time": []byte(strings.Replace(string(good.Stdout), `"db_last_modified": "2026-01-01T00:00:00Z",`, ``, 1)),
		"other scanner":    []byte(strings.Replace(string(good.Stdout), `"scanner_name": "govulncheck"`, `"scanner_name": "other"`, 1)),
		"sbom go version": []byte(strings.Replace(string(good.Stdout), `"go_version": "go1.27.1",
    "modules"`, `"go_version": "go1.26.0",
    "modules"`, 1)),
	}
	for name, stdout := range cases {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(stdout, good.Stdout) {
				t.Fatal("mutation did not change the stream")
			}
			s := Parse(Output{Stdout: stdout}, Expect{})
			if s.Conclusive || len(s.Findings) != 0 {
				t.Fatalf("conclusive: %+v", s)
			}
			if s.Reason == "" {
				t.Fatal("no reason")
			}
		})
	}
}

// Every prefix of a real stream either fails or, when it ends exactly at a
// message boundary after the scanner reported checking the code, yields a
// subset of the full findings. The second case is the one truncation the
// stream itself cannot reveal, since the protocol has no end marker; the
// exit status and the capture flags must catch it, and Parse refuses both.
func TestParse_EveryTruncation(t *testing.T) {
	for _, gd := range goldenDirs {
		good := loadGolden(t, gd.dir, "patch-safe")
		full := Parse(good, Expect{})
		if !full.Conclusive {
			t.Fatal(full.Reason)
		}
		msgs := splitMessages(t, good.Stdout)
		// Byte offset at which the "checking the code" progress message ends.
		doneEnd := bytes.Index(good.Stdout, []byte(progressDone))
		if doneEnd < 0 {
			t.Fatal("golden lacks the progress message")
		}
		conclusivePrefixes := 0
		for n := 0; n < len(good.Stdout); n++ {
			s := Parse(Output{Stdout: good.Stdout[:n]}, Expect{})
			if !s.Conclusive {
				continue
			}
			conclusivePrefixes++
			if n < doneEnd {
				t.Fatalf("%s: prefix of %d bytes is conclusive before the scan finished", gd.dir, n)
			}
			for _, f := range s.Findings {
				if _, ok := full.Finding(f.OSV, f.Module); !ok {
					t.Fatalf("%s: prefix invents %s", gd.dir, f.Key)
				}
			}
		}
		// Boundaries after the progress message: before each finding and at the end.
		if want := len(msgs) - 7; conclusivePrefixes < want {
			t.Fatalf("%s: %d conclusive prefixes, want at least %d", gd.dir, conclusivePrefixes, want)
		}
	}
}

func TestParse_StderrIsNotedNotFatal(t *testing.T) {
	o := loadGolden(t, "govulncheck-v1.8.0-go1.27", "patch-safe")
	o.Stderr = []byte("telemetry: something odd\n")
	s := Parse(o, Expect{})
	if !s.Conclusive || len(s.Notes) != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestCompare(t *testing.T) {
	g := goldenDirs[2]
	e := fixtureExpect(g.version, g.goVersion)
	base := Parse(loadGolden(t, g.dir, "patch-safe"), e)
	fixed := Parse(loadGolden(t, g.dir, "patch-safe-v124"), e)
	minor := Parse(loadGolden(t, g.dir, "patch-safe-v130"), e)

	d, err := Compare(base, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resolved) != 1 || d.Resolved[0].Key != "GO-TEST-0001 example.com/lib" || len(d.Introduced) != 0 || len(d.Escalated) != 0 || len(d.Persisting) != 1 || !d.Persisting[0].Stdlib {
		t.Fatalf("v1.2.1 -> v1.2.4: %+v", d)
	}

	d, err = Compare(fixed, minor)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Introduced) != 1 || d.Introduced[0].Key != "GO-TEST-0002 example.com/lib" || d.Introduced[0].FixedVersion != "" {
		t.Fatalf("v1.2.4 -> v1.3.0: %+v", d)
	}

	// A package-level finding that becomes reachable is an escalation.
	pkgLevel := &Scan{Conclusive: true, Findings: []Finding{{Key: "GO-X example.com/m", OSV: "GO-X", Module: "example.com/m", Level: LevelPackage}}}
	called := &Scan{Conclusive: true, Findings: []Finding{{Key: "GO-X example.com/m", OSV: "GO-X", Module: "example.com/m", Level: LevelSymbol}}}
	if d, err := Compare(pkgLevel, called); err != nil || len(d.Escalated) != 1 || len(d.Persisting) != 0 {
		t.Fatalf("escalation: %+v %v", d, err)
	}

	broken := Parse(loadGolden(t, g.dir, "patch-safe-v130-broken"), e)
	if _, err := Compare(base, broken); err == nil {
		t.Fatal("compared against an inconclusive scan")
	}
	if _, err := Compare(nil, base); err == nil {
		t.Fatal("compared against a missing scan")
	}
}

func TestFrameSymbol(t *testing.T) {
	cases := map[Frame]string{
		{Package: "example.com/lib", Function: "Greet"}:            "lib.Greet",
		{Package: "os", Receiver: "*File", Function: "Read"}:       "os.(*File).Read",
		{Package: "net/http", Receiver: "Header", Function: "Get"}: "http.Header.Get",
		{Function: "main"}: "main",
	}
	for f, want := range cases {
		if got := f.Symbol(); got != want {
			t.Errorf("%+v = %q, want %q", f, got, want)
		}
	}
}
