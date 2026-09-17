package manifest

import (
	"strings"
	"testing"
)

const baseMod = `module example.com/app

go 1.22

require (
	example.com/lib v1.2.1
	example.com/other v0.5.0
	example.com/ind v0.1.0 // indirect
)
`

func facts(t *testing.T, mod string) *Facts {
	t.Helper()
	f, err := Parse([]byte(mod))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func codes(v Verification) string {
	var out []string
	for _, x := range v.Violations {
		out = append(out, x.Code)
	}
	return strings.Join(out, ",")
}

var target = Target{Module: "example.com/lib", Version: "v1.2.4"}

func TestVerifyAdmission_CleanUpgrade(t *testing.T) {
	cand := strings.Replace(baseMod, "example.com/lib v1.2.1", "example.com/lib v1.2.4", 1)
	v := VerifyAdmission(facts(t, baseMod), facts(t, cand), target, nil)
	if !v.OK() || v.Gate != "A" {
		t.Fatalf("violations = %v", v.Violations)
	}
}

func TestVerifyAdmission_Rules(t *testing.T) {
	cases := map[string]struct {
		cand    string
		closure map[string]bool
		want    string
	}{
		"target not set":         {baseMod, nil, CodeTargetNotSet},
		"target wrong version":   {strings.Replace(baseMod, "lib v1.2.1", "lib v1.3.0", 1), nil, CodeTargetNotSet},
		"reversion":              {strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "other v0.5.0", "other v0.4.0").Replace(baseMod), nil, CodeVersionDecreased},
		"increase outside":       {strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "other v0.5.0", "other v0.6.0").Replace(baseMod), nil, CodeOutsideClosure},
		"go directive":           {strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "go 1.22", "go 1.23").Replace(baseMod), nil, CodeGoDirective},
		"toolchain added":        {strings.Replace(baseMod, "lib v1.2.1", "lib v1.2.4", 1) + "\ntoolchain go1.23.0\n", nil, CodeToolchainChanged},
		"replace added":          {strings.Replace(baseMod, "lib v1.2.1", "lib v1.2.4", 1) + "\nreplace example.com/lib => ../lib\n", nil, CodeReplaceChanged},
		"exclude added":          {strings.Replace(baseMod, "lib v1.2.1", "lib v1.2.4", 1) + "\nexclude example.com/other v0.4.0\n", nil, CodeExcludeChanged},
		"direct added":           {strings.Replace(baseMod, "lib v1.2.1", "lib v1.2.4\n\texample.com/new v1.0.0", 1), nil, CodeDirectAdded},
		"direct removed":         {strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "\texample.com/other v0.5.0\n", "").Replace(baseMod), nil, CodeDirectRemoved},
		"indirect added outside": {strings.Replace(baseMod, "lib v1.2.1", "lib v1.2.4\n\texample.com/dep v1.0.0 // indirect", 1), nil, CodeOutsideClosure},
		"module path":            {strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "module example.com/app", "module example.com/other").Replace(baseMod), nil, CodeModulePathChanged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := VerifyAdmission(facts(t, baseMod), facts(t, tc.cand), target, tc.closure)
			if !strings.Contains(codes(v), tc.want) {
				t.Fatalf("codes = %q, want %s", codes(v), tc.want)
			}
		})
	}
}

func TestVerifyAdmission_PermittedTransitives(t *testing.T) {
	cand := strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "other v0.5.0", "other v0.6.0", "ind v0.1.0 // indirect", "ind v0.2.0 // indirect\n\texample.com/dep v1.0.0 // indirect").Replace(baseMod)
	closure := map[string]bool{"example.com/other": true, "example.com/ind": true, "example.com/dep": true}
	v := VerifyAdmission(facts(t, baseMod), facts(t, cand), target, closure)
	if !v.OK() {
		t.Fatalf("violations = %v", v.Violations)
	}
	if len(v.ClosureIncreases) != 2 || len(v.IndirectChanges) != 1 {
		t.Fatalf("increases %v indirect %v", v.ClosureIncreases, v.IndirectChanges)
	}
	// Removing an indirect requirement is a tidy consequence.
	cand = strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "\texample.com/ind v0.1.0 // indirect\n", "").Replace(baseMod)
	if v := VerifyAdmission(facts(t, baseMod), facts(t, cand), target, nil); !v.OK() {
		t.Fatalf("indirect removal refused: %v", v.Violations)
	}
}

func TestVerifyNormalized_DirectBecameIndirect(t *testing.T) {
	cand := strings.NewReplacer("lib v1.2.1", "lib v1.2.4", "other v0.5.0", "other v0.5.0 // indirect").Replace(baseMod)
	v := VerifyNormalized(facts(t, baseMod), facts(t, cand), target, nil)
	if !strings.Contains(codes(v), CodeDirectRemoved) || v.Gate != "B" {
		t.Fatalf("codes = %q gate %s", codes(v), v.Gate)
	}
	// Gate A permits the same manifests.
	if a := VerifyAdmission(facts(t, baseMod), facts(t, cand), target, nil); !a.OK() {
		t.Fatalf("gate A should admit: %v", a.Violations)
	}
}

func TestClosureFromGraph(t *testing.T) {
	graph := `example.com/app example.com/lib@v1.2.4
example.com/app example.com/other@v0.5.0
example.com/lib@v1.2.4 example.com/ind@v0.2.0
example.com/lib@v1.2.4 golang.org/x/text@v0.14.0
example.com/ind@v0.2.0 example.com/deep@v1.0.0
example.com/lib@v1.2.1 example.com/old@v0.0.1
example.com/other@v0.5.0 example.com/unrelated@v1.0.0
`
	c := ClosureFromGraph(graph, target)
	for _, want := range []string{"example.com/ind", "golang.org/x/text", "example.com/deep"} {
		if !c[want] {
			t.Errorf("%s missing from closure %v", want, c)
		}
	}
	for _, no := range []string{"example.com/old", "example.com/unrelated", "example.com/other", "example.com/lib"} {
		if c[no] {
			t.Errorf("%s wrongly in closure", no)
		}
	}
}

func TestOpError_RequiresNewerToolchain(t *testing.T) {
	e := &OpError{Stderr: "go: example.com/lib@v2.0.0 requires go >= 1.25 (running go 1.22.12; GOTOOLCHAIN=local)"}
	if !e.RequiresNewerToolchain() {
		t.Fatal("not detected")
	}
	if (&OpError{Stderr: "go: module example.com/lib: not found"}).RequiresNewerToolchain() {
		t.Fatal("false positive")
	}
}
