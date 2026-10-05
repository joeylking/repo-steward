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

// The samples are the go command's own stderr (go 1.22, captured in the
// toolchain image) for an unreachable proxy or checksum database, and for
// failures that are about the module and must keep their gate outcome.
func TestOpError_AcquisitionFailed(t *testing.T) {
	transport := map[string]string{
		"no network":       `go: github.com/google/uuid@v1.6.0: Get "https://proxy.golang.org/github.com/google/uuid/@v/v1.6.0.mod": dial tcp: lookup proxy.golang.org on 192.168.5.1:53: dial udp 192.168.5.1:53: connect: network is unreachable`,
		"no such host":     `go: github.com/google/uuid@v1.6.0: Get "https://proxy.nonexistent.invalid/github.com/google/uuid/@v/v1.6.0.mod": dial tcp: lookup proxy.nonexistent.invalid on 192.168.5.1:53: no such host`,
		"refused":          `go: github.com/google/uuid@v1.6.0: Get "http://127.0.0.1:9/github.com/google/uuid/@v/v1.6.0.mod": dial tcp 127.0.0.1:9: connect: connection refused`,
		"dial timeout":     `go: github.com/google/uuid@v1.6.1: Get "http://10.255.255.1/github.com/google/uuid/@v/v1.6.1.info": dial tcp 10.255.255.1:80: i/o timeout`,
		"tls":              `go: github.com/google/uuid@v1.6.0: Get "https://self-signed.badssl.com/github.com/google/uuid/@v/v1.6.0.mod": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
		"checksum db":      `go: github.com/google/uuid@v1.6.0: verifying go.mod: github.com/google/uuid@v1.6.0/go.mod: Get "https://sum.nonexistent.invalid/lookup/github.com/google/uuid@v1.6.0": dial tcp: lookup sum.nonexistent.invalid on 192.168.5.1:53: no such host`,
		"proxy 500":        `go: github.com/google/uuid@v1.6.0: reading http://127.0.0.1:8010/github.com/google/uuid/@v/v1.6.0.mod: 500 Internal Server Error`,
		"proxy 502":        `go: github.com/google/uuid@v1.6.0: reading http://127.0.0.1:8012/github.com/google/uuid/@v/v1.6.0.mod: 502 Bad Gateway`,
		"proxy 503":        `go: github.com/google/uuid@v1.6.0: reading http://127.0.0.1:8013/github.com/google/uuid/@v/v1.6.0.mod: 503 Service Unavailable`,
		"proxy 504":        `go: github.com/google/uuid@v1.6.0: reading http://127.0.0.1:8014/github.com/google/uuid/@v/v1.6.0.mod: 504 Gateway Timeout`,
		"handshake (http)": `go: example.com/lib@v1.2.4: Get "https://proxy.golang.org/example.com/lib/@v/v1.2.4.info": net/http: TLS handshake timeout`,
	}
	for name, stderr := range transport {
		if !(&OpError{Stderr: stderr}).AcquisitionFailed() {
			t.Errorf("%s: not classified as an acquisition failure", name)
		}
	}
	refusal := map[string]string{
		"not found":        "go: github.com/google/uuid@v9.9.9: reading https://proxy.golang.org/github.com/google/uuid/@v/v9.9.9.info: 404 Not Found\n\tserver response: not found: github.com/google/uuid@v9.9.9: invalid version: unknown revision v9.9.9",
		"gone":             `go: github.com/google/uuid@v1.6.0: reading http://127.0.0.1:8020/github.com/google/uuid/@v/v1.6.0.mod: 410 Gone`,
		"lookup disabled":  `go: github.com/google/uuid@v1.6.0: module lookup disabled by GOPROXY=off`,
		"newer toolchain":  "go: example.com/lib@v2.0.0 requires go >= 1.25 (running go 1.22.12; GOTOOLCHAIN=local)",
		"checksum":         "verifying example.com/lib@v1.2.4: checksum mismatch\n\tdownloaded: h1:x\n\tgo.sum:     h1:y\n\nSECURITY ERROR",
		"module not found": "go: module example.com/lib: not found",
	}
	for name, stderr := range refusal {
		if (&OpError{Stderr: stderr}).AcquisitionFailed() {
			t.Errorf("%s: wrongly classified as an acquisition failure", name)
		}
	}
	if (&OpError{Stderr: transport["refused"], TimedOut: true}).AcquisitionFailed() {
		t.Error("a container timeout is classified")
	}
}

func TestVerifyBuildList(t *testing.T) {
	target := Target{Module: "example.com/inner", Version: "v1.0.1"}
	base := map[string]string{"example.com/wrap": "v1.0.0", "example.com/inner": "v1.0.0", "example.com/x": "v0.1.0"}
	vcodes := func(vs []Violation) string {
		var out []string
		for _, v := range vs {
			out = append(out, v.Code)
		}
		return strings.Join(out, ",")
	}
	// The target moved and nothing else did: clean.
	if vs := VerifyBuildList(base, map[string]string{"example.com/wrap": "v1.0.0", "example.com/inner": "v1.0.1", "example.com/x": "v0.1.0"}, target, nil); len(vs) != 0 {
		t.Fatalf("clean = %+v", vs)
	}
	// Tidy removed the requirement and the old version is selected again.
	vs := VerifyBuildList(base, base, target, nil)
	if vcodes(vs) != CodeTargetNotInBuildList || !strings.Contains(vs[0].Detail, "selects example.com/inner v1.0.0, want v1.0.1; the version before the change is selected again") {
		t.Fatalf("reselected = %+v", vs)
	}
	// The target left the build list entirely.
	if vs := VerifyBuildList(base, map[string]string{"example.com/wrap": "v1.0.0", "example.com/x": "v0.1.0"}, target, nil); vcodes(vs) != CodeTargetNotInBuildList {
		t.Fatalf("absent = %+v", vs)
	}
	// Increases and additions must be in the closure; decreases never pass.
	cand := map[string]string{"example.com/wrap": "v1.0.0", "example.com/inner": "v1.0.1", "example.com/x": "v0.2.0", "example.com/new": "v1.0.0"}
	if got := vcodes(VerifyBuildList(base, cand, target, nil)); got != CodeBuildListOutsideClosure+","+CodeBuildListOutsideClosure {
		t.Fatalf("outside closure = %s", got)
	}
	if vs := VerifyBuildList(base, cand, target, map[string]bool{"example.com/x": true, "example.com/new": true}); len(vs) != 0 {
		t.Fatalf("within closure = %+v", vs)
	}
	cand = map[string]string{"example.com/wrap": "v0.9.0", "example.com/inner": "v1.0.1"}
	if got := vcodes(VerifyBuildList(base, cand, target, map[string]bool{"example.com/wrap": true})); got != CodeVersionDecreased {
		t.Fatalf("decrease = %s", got)
	}
}

func TestParseBuildList(t *testing.T) {
	bl, err := ParseBuildList([]byte(`{"Path":"example.com/app","Main":true}
{"Path":"example.com/inner","Version":"v1.0.0"}
{"Path":"example.com/wrap","Version":"v1.0.0"}`))
	if err != nil || len(bl) != 2 || bl["example.com/inner"] != "v1.0.0" {
		t.Fatalf("build list = %v, %v", bl, err)
	}
	if _, err := ParseBuildList([]byte(`{"Path":"example.com/x","Error":{"Err":"boom"}}`)); err == nil {
		t.Fatal("a module error was accepted")
	}
	if _, err := ParseBuildList([]byte(`{"Path":`)); err == nil {
		t.Fatal("truncated output was accepted")
	}
}
