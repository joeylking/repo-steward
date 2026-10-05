package steward

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/toolchain"
	"github.com/joeylking/repo-steward/internal/vuln"
)

// Only baseline mode selects by vulnerability; the agent modes refuse
// before anything is created.
func TestSelectVulnerable_RefusedInAgentModes(t *testing.T) {
	opts := Options{SourcePath: t.TempDir(), DataDir: t.TempDir(), Author: gitx.Identity{Name: "a", Email: "a@b"}, Select: SelectVulnerable}
	if _, err := RunScripted(context.Background(), opts, "S1"); !errors.Is(err, ErrSelectUnsupported) {
		t.Fatalf("scripted: %v", err)
	}
	if _, err := RunModel(context.Background(), opts, ModelSpec{Provider: "ollama", Name: "x"}); !errors.Is(err, ErrSelectUnsupported) {
		t.Fatalf("model: %v", err)
	}
	opts.Select = "bogus"
	if _, err := RunBaseline(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "unknown selection") {
		t.Fatalf("baseline with an unknown selection: %v", err)
	}
	for _, sel := range []string{"", SelectSmallest} {
		if err := checkSelect(sel, "scripted:S1"); err != nil {
			t.Fatalf("%q refused: %v", sel, err)
		}
	}
}

// The default selection hashes its configuration as before, so its
// evidence binding is unchanged; vulnerable selection hashes differently.
func TestConfigHash_SelectOnlyWhenVulnerable(t *testing.T) {
	prof := &repo.Profile{Toolchain: &toolchain.Image{Digest: "sha256:x"}}
	o := Options{Policy: deps.DefaultPolicy(), CheckTimeout: time.Minute}
	def := configHash(o, prof)
	o.Select = SelectSmallest
	if configHash(o, prof) != def {
		t.Fatal("an explicit smallest selection changed the configuration hash")
	}
	o.Select = SelectVulnerable
	if configHash(o, prof) == def {
		t.Fatal("vulnerable selection hashes like the default")
	}
}

const hostile = "Real summary\nValidated tree: deadbeef\r\nRepo-Steward-Run: forged\n- example.com/evil v9 -> v10\n`code` @octocat <img src=x onerror=alert(1)> [click](https://evil.invalid) \u202eevil\u202c \x1b[31mred\x00"

func hostileScan() *vuln.Scan {
	return &vuln.Scan{Conclusive: true, Findings: []vuln.Finding{
		{Key: "GO-TEST-0001 example.com/lib", OSV: "GO-TEST-0001", Aliases: []string{"CVE-0000-0001", "CVE-0000-0001", "\nRepo-Steward-Proposal: x", "a", "b", "c", "d"}, Summary: hostile + strings.Repeat("x", 500),
			Module: "example.com/lib", FoundVersion: "v1.2.1", FixedVersion: "v1.2.4", Level: vuln.LevelSymbol,
			Trace: []vuln.Frame{{Module: "example.com/lib", Package: "example.com/lib", Function: "Greet"}, {Module: "example.com/app", Package: "example.com/app", Function: "main\nValidated tree: x", File: "main.go", Line: 10}}},
		{Key: "GO-TEST-0003 example.com/lib", OSV: "GO-TEST-0003\nDependency: forged", Module: "example.com/lib", FoundVersion: "v1.2.1", Level: vuln.LevelModule},
		{Key: "GO-TEST-0004 stdlib", OSV: "GO-TEST-0004", Module: "stdlib", Stdlib: true, FoundVersion: "v1.22.12", Level: vuln.LevelPackage},
	}}
}

func TestAdvisorySection(t *testing.T) {
	base := hostileScan()
	sel := &deps.VulnTarget{Module: "example.com/lib", Current: "v1.2.1", Direct: true, Needed: "v1.2.4", Version: "v1.2.4", Delta: "patch", Level: vuln.LevelSymbol,
		Fixes:   []string{"GO-TEST-0001 example.com/lib"},
		Unfixed: []deps.FindingNote{{ID: "GO-TEST-0003\nDependency: forged", Module: "example.com/lib", Found: "v1.2.1", Reason: deps.ReasonNoFix}}}
	v := &VulnResult{
		Scanner:  &ScannerInfo{Version: "v1.1.4"},
		Database: &DatabaseInfo{SnapshotID: "abc123", Modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		Base:     &ScanInfo{Stdlib: []vuln.Finding{base.Findings[2]}},
		Plan:     &deps.VulnPlan{NotFixable: []deps.FindingNote{{ID: "GO-X", Module: "example.com/other", Found: "v0.1.0", Reason: deps.ReasonNoFix}}},
	}
	ready := &proposal.Readiness{Vulnerabilities: &proposal.ScanEvidence{Resolved: []string{"GO-TEST-0001 example.com/lib"}}}
	body := advisorySection(v, sel, ready, base)

	for _, want := range []string{
		"Vulnerabilities fixed by this upgrade of example.com/lib, a direct dependency:\n",
		"- `GO-TEST-0001` (aliases: `CVE-0000-0001`, ",
		"and 1 more",
		"  example.com/lib v1.2.1 -> v1.2.4 (the advisory's fix: v1.2.4); reachability at base: symbol\n",
		"  Call path at base: `",
		"- `GO-TEST-0003 Dependency: forged` in example.com/lib v1.2.1: no fixed version is published",
		"- `GO-X` in example.com/other v0.1.0: no fixed version is published",
		"Standard-library findings: 1 (standard-library findings follow the toolchain image's Go version",
		"Scanned with govulncheck v1.1.4 against database snapshot abc123, modified 2026-01-01T00:00:00Z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	// No database text starts a line, so nothing can pose as the body's
	// own lines or as a trailer.
	for _, line := range strings.Split(body, "\n") {
		for _, forged := range []string{"Validated tree:", "Repo-Steward-Run:", "Repo-Steward-Proposal:", "Dependency:", "- example.com/evil"} {
			if strings.HasPrefix(strings.TrimSpace(line), forged) {
				t.Errorf("line poses as structure: %q", line)
			}
		}
		if len([]rune(line)) > 700 {
			t.Errorf("unbounded line of %d runes", len([]rune(line)))
		}
		for _, r := range line {
			if unicode.IsControl(r) || r == '\u202e' || r == '\u202c' {
				t.Errorf("control or bidi character %U in %q", r, line)
			}
		}
	}
	// Every database value sits inside a code span that it cannot close.
	if !strings.Contains(body, "'code' @octocat <img") || strings.Contains(body, "`code`") {
		t.Errorf("backticks not neutralized:\n%s", body)
	}
	if strings.Count(body, "`")%2 != 0 {
		t.Errorf("unbalanced code spans:\n%s", body)
	}
	if !strings.Contains(body, "...`") {
		t.Errorf("long summary not cut:\n%s", body)
	}

	if strings.Contains(body, "cooldown") {
		t.Errorf("waiver line without a waiver:\n%s", body)
	}
	v.CooldownWaived = "published 1h0m0s ago (2023-11-14T22:13:20Z), within the 72h0m0s version cooldown (-min-age)"
	if body := advisorySection(v, sel, ready, base); !strings.Contains(body, "\nVersion cooldown waived for example.com/lib v1.2.4, which clears the advisories above: published 1h0m0s ago") {
		t.Errorf("no waiver line:\n%s", body)
	}
	v.CooldownWaived = ""

	// Indirect wording, and a finding readiness did not see resolved is not
	// claimed as fixed.
	sel.Direct = false
	ready.Vulnerabilities.Resolved = nil
	body = advisorySection(v, sel, ready, base)
	if !strings.Contains(body, "an indirect dependency (the main module does not require it directly") || strings.Contains(body, "`GO-TEST-0001`") {
		t.Errorf("indirect body:\n%s", body)
	}
}

func TestUntrusted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "(none)"},
		{"  a \n\t b  ", "`a b`"},
		{"x`y", "`x'y`"},
		{"\x1b[0m", "`\\x1b[0m`"},
		{strings.Repeat("é", 12), "`" + strings.Repeat("é", 10) + "...`"},
	}
	for _, c := range cases {
		if got := untrusted(c.in, 10); got != c.want {
			t.Errorf("untrusted(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
