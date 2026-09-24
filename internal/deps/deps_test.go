package deps

import (
	"context"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/sandbox"
)

type fakeSandbox struct {
	outputs map[string]string
	calls   []sandbox.ExecSpec
}

func (f *fakeSandbox) Run(_ context.Context, spec sandbox.ExecSpec) (sandbox.ExecResult, error) {
	f.calls = append(f.calls, spec)
	key := strings.Join(spec.Argv, " ")
	if out, ok := f.outputs[key]; ok {
		return sandbox.ExecResult{Stdout: []byte(out)}, nil
	}
	return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("unexpected: " + key)}, nil
}

const listAll = `{"Path":"example.com/app","Main":true}
{"Path":"example.com/lib","Version":"v1.2.1","Update":{"Version":"v1.2.4"}}
{"Path":"example.com/indirect","Version":"v0.1.0","Indirect":true,"Update":{"Version":"v0.2.0"}}
{"Path":"example.com/current","Version":"v3.0.0"}
{"Path":"example.com/legacy","Version":"v1.0.0"}
`

func TestDiscover(t *testing.T) {
	fs := &fakeSandbox{outputs: map[string]string{
		"go list -m -u -json all":                        listAll,
		"go list -m -versions -json example.com/lib":     `{"Path":"example.com/lib","Versions":["v1.2.1","v1.2.4","v1.3.0-rc.1","v1.3.0","v2.0.0+incompatible","v1.0.0"]}`,
		"go list -m -versions -json example.com/current": `{"Path":"example.com/current","Versions":["v3.0.0"]}`,
		// The toolchain suggests no update for a module whose only newer
		// version is an incompatible major; the version list still shows it.
		"go list -m -versions -json example.com/legacy": `{"Path":"example.com/legacy","Versions":["v1.0.0","v2.0.0+incompatible"]}`,
	}}
	cands, err := Discover(context.Background(), fs, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || cands[0].Module != "example.com/legacy" || cands[1].Module != "example.com/lib" {
		t.Fatalf("candidates = %+v", cands)
	}
	legacy := cands[0]
	if legacy.Latest != "v2.0.0+incompatible" || legacy.Delta != "major" || len(legacy.EligibleTargets()) != 0 {
		t.Fatalf("legacy = %+v", legacy)
	}
	cands = cands[1:]
	if cands[0].Latest != "v2.0.0+incompatible" || cands[0].Delta != "major" {
		t.Fatalf("lib latest = %+v", cands[0])
	}
	for _, c := range fs.calls {
		if c.Profile != sandbox.Acquire {
			t.Fatalf("discovery ran outside acquire: %+v", c)
		}
	}
	got := map[string]Target{}
	for _, tg := range cands[0].Targets {
		got[tg.Version] = tg
	}
	if _, ok := got["v1.0.0"]; ok {
		t.Fatal("older version listed as target")
	}
	if !got["v1.2.4"].Eligible || got["v1.2.4"].Delta != "patch" {
		t.Fatalf("v1.2.4 = %+v", got["v1.2.4"])
	}
	if !got["v1.3.0"].Eligible || got["v1.3.0"].Delta != "minor" {
		t.Fatalf("v1.3.0 = %+v", got["v1.3.0"])
	}
	if got["v1.3.0-rc.1"].Eligible || !strings.Contains(strings.Join(got["v1.3.0-rc.1"].Reasons, " "), "pre-release") {
		t.Fatalf("rc = %+v", got["v1.3.0-rc.1"])
	}
	if got["v2.0.0+incompatible"].Eligible || got["v2.0.0+incompatible"].Delta != "major" {
		t.Fatalf("major = %+v", got["v2.0.0+incompatible"])
	}
	if len(cands[0].EligibleTargets()) != 2 {
		t.Fatalf("eligible = %+v", cands[0].EligibleTargets())
	}
}

func TestTargets_PolicyAndNamedDependency(t *testing.T) {
	versions := []string{"v1.2.4", "v1.3.0", "v2.0.0"}
	pol := Policy{AllowMajor: true, AllowMinor: true, AllowPatch: true, NamedDependency: "example.com/other"}
	for _, tg := range targets("example.com/lib", "v1.2.1", versions, pol) {
		if tg.Eligible {
			t.Fatalf("named dependency mismatch still eligible: %+v", tg)
		}
	}
	// An exact pin leaves only that version eligible, and never an older one.
	pol = Policy{AllowMajor: true, AllowMinor: true, AllowPatch: true, NamedDependency: "example.com/lib@v1.3.0"}
	for _, tg := range targets("example.com/lib", "v1.2.1", versions, pol) {
		if tg.Eligible != (tg.Version == "v1.3.0") {
			t.Fatalf("pinned v1.3.0: %+v", tg)
		}
	}
	pol.NamedDependency = "example.com/lib@v1.0.0"
	for _, tg := range targets("example.com/lib", "v1.2.1", versions, pol) {
		if tg.Eligible {
			t.Fatalf("pin to an older version made %s eligible", tg.Version)
		}
	}
	pol = Policy{AllowMajor: true, AllowMinor: true, AllowPatch: true, Deny: []string{"example.com/lib"}}
	for _, tg := range targets("example.com/lib", "v1.2.1", versions, pol) {
		if tg.Eligible {
			t.Fatalf("denied module still eligible: %+v", tg)
		}
	}
	pol = Policy{AllowPatch: true}
	tgs := targets("example.com/lib", "v1.2.1", versions, pol)
	if !tgs[0].Eligible || tgs[1].Eligible || tgs[2].Eligible {
		t.Fatalf("patch-only policy = %+v", tgs)
	}
	if tgs[0].Version != "v1.2.4" || tgs[1].Version != "v1.3.0" {
		t.Fatalf("targets not sorted ascending: %+v", tgs)
	}
	if p := targets("m", "v1.2.1", []string{"v1.2.2-0.20240101000000-abcdefabcdef"}, DefaultPolicy()); p[0].Eligible {
		t.Fatalf("pseudo-version eligible: %+v", p)
	}
}
