package steward

import (
	"testing"

	"github.com/joeylking/repo-steward/internal/deps"
)

func TestSelect(t *testing.T) {
	cands := []deps.Candidate{
		{Module: "example.com/zed", Targets: []deps.Target{{Version: "v2.0.0", Delta: "major", Eligible: true}, {Version: "v1.5.0", Delta: "minor", Eligible: true}}},
		{Module: "example.com/lib", Targets: []deps.Target{{Version: "v1.2.4", Delta: "patch", Eligible: true}, {Version: "v1.2.3", Delta: "patch", Eligible: true}, {Version: "v1.3.0", Delta: "minor", Eligible: true}}},
		{Module: "example.com/abc", Targets: []deps.Target{{Version: "v0.1.1", Delta: "patch", Eligible: false, Reasons: []string{"denied"}}}},
	}
	tg, ok := Select(cands)
	if !ok || tg.Module != "example.com/lib" || tg.Version != "v1.2.4" {
		t.Fatalf("selected %+v", tg)
	}
	// Ties on delta class resolve alphabetically by module.
	cands[0].Targets = []deps.Target{{Version: "v1.0.1", Delta: "patch", Eligible: true}}
	cands = append(cands, deps.Candidate{Module: "example.com/aaa", Targets: []deps.Target{{Version: "v3.0.1", Delta: "patch", Eligible: true}}})
	tie, _ := Select(cands)
	if tie.Module != "example.com/aaa" {
		t.Fatalf("tie broke to %+v", tie)
	}
	if _, ok := Select([]deps.Candidate{cands[2]}); ok {
		t.Fatal("ineligible-only candidate selected")
	}
	if HeadRef(tie) != "repo-steward/aaa-v3.0.1" || HeadRef(tg) != "repo-steward/lib-v1.2.4" {
		t.Fatalf("head refs = %s %s", HeadRef(tie), HeadRef(tg))
	}
}
