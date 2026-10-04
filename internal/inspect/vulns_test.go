package inspect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/vuln"
)

// recorded parses a real scanner run captured by the groundwork batch.
func recorded(t *testing.T, name string, want vuln.Expect) (*vuln.Scan, []byte) {
	t.Helper()
	dir := filepath.Join("..", "vuln", "testdata", "govulncheck-v1.1.4-go1.22")
	stdout, err := os.ReadFile(filepath.Join(dir, name+".stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, _ := os.ReadFile(filepath.Join(dir, name+".stderr"))
	exitRaw, _ := os.ReadFile(filepath.Join(dir, name+".exit"))
	exit, _ := strconv.Atoi(strings.TrimSpace(string(exitRaw)))
	return vuln.Parse(vuln.Output{Stdout: stdout, Stderr: stderr, ExitCode: exit}, want), stdout
}

var fixtureExpect = vuln.Expect{ScannerVersion: "v1.1.4", DB: "file:///vulndb", DBModified: vuln.FixtureTime, GoVersion: "go1.22.12"}

// Third-party and standard-library findings are separated, sorted, and
// carry what the report promises; a third-party finding exits 4.
func TestVulnReport_SplitsStdlibAndExits4(t *testing.T) {
	s, stdout := recorded(t, "patch-safe", fixtureExpect)
	if !s.Conclusive {
		t.Fatalf("recorded scan inconclusive: %s", s.Reason)
	}
	r := &VulnReport{GoVersion: "go1.22.12"}
	r.setScan(s, stdout)
	if r.Outcome != VulnsFindings || r.ExitCode() != 4 || !r.Conclusive || len(r.OutputSHA256) != 64 {
		t.Fatalf("report = %+v", r)
	}
	if len(r.ThirdParty) != 1 || len(r.Stdlib) != 1 {
		t.Fatalf("third party %+v, stdlib %+v", r.ThirdParty, r.Stdlib)
	}
	f := r.ThirdParty[0]
	if f.ID != "GO-TEST-0001" || f.Module != "example.com/lib" || f.FoundVersion != "v1.2.1" || f.FixedVersion != "v1.2.4" || !f.FixAvailable ||
		f.Level != vuln.LevelSymbol || f.CallPath != "app.main (main.go:10) -> lib.Greet" || len(f.Aliases) != 1 || f.Aliases[0] != "CVE-0000-0001" {
		t.Fatalf("third-party finding = %+v", f)
	}
	sf := r.Stdlib[0]
	if sf.ID != "GO-TEST-0004" || sf.Module != "stdlib" || sf.Level != vuln.LevelPackage || sf.CallPath != "" {
		t.Fatalf("stdlib finding = %+v", sf)
	}
	sum := r.Summary()
	ti, si := strings.Index(sum, "third-party dependencies: 1"), strings.Index(sum, "standard library: 1")
	if ti < 0 || si < ti || strings.Index(sum, "GO-TEST-0001") > si || strings.Index(sum, "GO-TEST-0004") < si {
		t.Fatalf("summary does not separate the findings:\n%s", sum)
	}
	for _, want := range []string{"cannot fix them today", "go1.22.12", "only reports", "app.main (main.go:10) -> lib.Greet", "fixed in v1.2.4"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("summary lacks %q:\n%s", want, sum)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"third_party":[`, `"stdlib":[`, `"fix_available":true`, `"call_path":"app.main (main.go:10) -\u003e lib.Greet"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("JSON lacks %s: %s", key, raw)
		}
	}
}

// Standard-library findings alone are not actionable here: exit 0.
func TestVulnReport_StdlibOnlyIsClean(t *testing.T) {
	s, stdout := recorded(t, "patch-safe-v124", fixtureExpect)
	r := &VulnReport{}
	r.setScan(s, stdout)
	if r.Outcome != VulnsClean || r.ExitCode() != 0 || len(r.ThirdParty) != 0 || len(r.Stdlib) != 1 {
		t.Fatalf("report = %+v", r)
	}
	if raw, _ := json.Marshal(r); !strings.Contains(string(raw), `"third_party":[]`) {
		t.Fatalf("a conclusive scan with no findings must say so with an empty list: %s", raw)
	}
}

// An inconclusive scan exits 3, keeps the reason verbatim, and lists no
// findings at all, not an empty list.
func TestVulnReport_InconclusiveExits3(t *testing.T) {
	want := fixtureExpect
	want.GoVersion = "go1.22.13"
	s, stdout := recorded(t, "patch-safe", want)
	r := &VulnReport{}
	r.setScan(s, stdout)
	if r.Outcome != VulnsInconclusive || r.ExitCode() != 3 || r.Conclusive || r.Reason != s.Reason || !strings.Contains(r.Reason, "go1.22.13") {
		t.Fatalf("report = %+v", r)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"third_party":null`) || !strings.Contains(string(raw), `"stdlib":null`) {
		t.Fatalf("inconclusive JSON = %s", raw)
	}
	if !strings.Contains(r.Summary(), "says nothing about vulnerabilities") {
		t.Fatalf("summary = %s", r.Summary())
	}
	timedOut := vuln.Parse(vuln.Output{TimedOut: true}, fixtureExpect)
	r.setScan(timedOut, nil)
	if r.ExitCode() != 3 || r.Reason != "scanner timed out" {
		t.Fatalf("timed out report = %+v", r)
	}
}

func TestVulnReport_ExitCodes(t *testing.T) {
	for outcome, code := range map[VulnOutcome]int{VulnsClean: 0, VulnsUnsupported: 2, VulnsInconclusive: 3, VulnsFindings: 4} {
		if got := (&VulnReport{Outcome: outcome}).ExitCode(); got != code {
			t.Errorf("%s exits %d, want %d", outcome, got, code)
		}
	}
}

// An unsupported repository is decided without an engine and without
// fetching a database.
func TestVulns_UnsupportedStopsBeforeDatabaseAndSandbox(t *testing.T) {
	def, _ := fixture.Load("patch-safe")
	def.ExpectedBaseCommit, def.ExpectedBaseTree = "", ""
	def.Files["go.work"] = []byte("go 1.22\n")
	r, err := def.Setup(context.Background(), filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Vulns(context.Background(), VulnOptions{RepoPath: r.Path, DataDir: filepath.Join(t.TempDir(), "data"), Socket: "/nonexistent/docker.sock", VulnDBURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != VulnsUnsupported || rep.ExitCode() != 2 || rep.Database != nil || !strings.Contains(strings.Join(rep.Refusals, " "), "go.work") {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(rep.Summary(), "unsupported") {
		t.Fatalf("summary = %s", rep.Summary())
	}
}

// A supplied database that is not a database is refused before the engine
// is reached.
func TestVulns_BadDatabaseRefusedBeforeSandbox(t *testing.T) {
	r, err := fixture.Setup(context.Background(), "patch-safe", filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, "GO-1.json"), []byte(`{"id":"GO-1"}`), 0o644)
	start := time.Now()
	_, err = Vulns(context.Background(), VulnOptions{RepoPath: r.Path, DataDir: filepath.Join(t.TempDir(), "data"), Socket: "/nonexistent/docker.sock", VulnDBDir: bad})
	if err == nil || strings.Contains(err.Error(), "docker") || time.Since(start) > 30*time.Second {
		t.Fatalf("err = %v", err)
	}
}

// The public database lists some aliases twice; the report lists each once.
func TestVulnReport_AliasesListedOnce(t *testing.T) {
	s := &vuln.Scan{Conclusive: true, Findings: []vuln.Finding{{Key: "GO-1 stdlib", OSV: "GO-1", Aliases: []string{"CVE-1", "CVE-1", "GHSA-1"}, Module: "stdlib", Stdlib: true, FoundVersion: "v1.22.12", Level: vuln.LevelModule}}}
	r := &VulnReport{}
	r.setScan(s, nil)
	if got := strings.Join(r.Stdlib[0].Aliases, ","); got != "CVE-1,GHSA-1" {
		t.Fatalf("aliases = %s", got)
	}
}
