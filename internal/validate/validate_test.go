package validate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/validate"
)

// fakeSandbox returns scripted results keyed by the second argv word.
type fakeSandbox struct {
	results map[string]sandbox.ExecResult
	calls   []sandbox.ExecSpec
}

func (f *fakeSandbox) Run(_ context.Context, spec sandbox.ExecSpec) (sandbox.ExecResult, error) {
	f.calls = append(f.calls, spec)
	key := spec.Argv[1]
	if r, ok := f.results[key]; ok {
		return r, nil
	}
	return sandbox.ExecResult{ExitCode: 0}, nil
}

func ok(stdout string) sandbox.ExecResult {
	return sandbox.ExecResult{ExitCode: 0, Stdout: []byte(stdout)}
}

const pkgs = "example.com/app\nexample.com/app/internal/x\n"

func testJSON(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func baseline(t *testing.T, results map[string]sandbox.ExecResult) (*validate.Run, *fakeSandbox) {
	t.Helper()
	fs := &fakeSandbox{results: results}
	run, err := validate.Baseline(context.Background(), fs, validate.Options{TreeHash: "t", CheckTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return run, fs
}

func TestBaseline_CleanPass(t *testing.T) {
	run, fs := baseline(t, map[string]sandbox.ExecResult{
		"list": ok(pkgs),
		"test": ok(testJSON(
			`{"Action":"run","Package":"example.com/app","Test":"TestA"}`,
			`{"Action":"pass","Package":"example.com/app","Test":"TestA"}`,
			`{"Action":"pass","Package":"example.com/app"}`,
			`{"Action":"output","Package":"example.com/app/internal/x","Output":"?   \texample.com/app/internal/x\t[no test files]\n"}`,
			`{"Action":"skip","Package":"example.com/app/internal/x"}`,
		)),
	})
	if !run.Conclusive || !run.Clean {
		t.Fatalf("run = %+v", run)
	}
	if len(run.Packages) != 2 {
		t.Fatalf("packages = %v", run.Packages)
	}
	for _, c := range fs.calls {
		if c.Profile != sandbox.Execute {
			t.Fatalf("validation ran outside the execute profile: %+v", c)
		}
	}
}

func TestBaseline_BuildFailureParsed(t *testing.T) {
	passingTests := ok(testJSON(`{"Action":"pass","Package":"example.com/app"}`, `{"Action":"skip","Package":"example.com/app/internal/x"}`))
	run, _ := baseline(t, map[string]sandbox.ExecResult{
		"list":  ok(pkgs),
		"build": {ExitCode: 1, Stderr: []byte("# example.com/app\n./main.go:12:5: undefined: lib.Missing\n./main.go:14:2: too many arguments\n")},
		"test":  passingTests,
	})
	b := run.Checks["build"]
	if !b.Conclusive || b.Status != validate.Fail || len(b.Findings) != 2 {
		t.Fatalf("build = %+v", b)
	}
	if b.Findings[0].File != "./main.go" || b.Findings[0].Line != 12 || b.Findings[0].Package != "example.com/app" || !strings.HasPrefix(b.Findings[0].Key, "build:./main.go:") {
		t.Fatalf("finding = %+v", b.Findings[0])
	}
	if run.Conclusive != true || run.Clean {
		t.Fatalf("run = conclusive %v clean %v", run.Conclusive, run.Clean)
	}
}

func TestBaseline_VetTypeErrorParsed(t *testing.T) {
	run, _ := baseline(t, map[string]sandbox.ExecResult{
		"list": ok(pkgs),
		"vet":  {ExitCode: 1, Stderr: []byte("# example.com/app\n# [example.com/app]\nvet: ./main.go:10:33: not enough arguments in call to lib.Greet\n\thave (string)\n\twant (context.Context, string)\n")},
		"test": ok(testJSON(`{"Action":"pass","Package":"example.com/app"}`, `{"Action":"skip","Package":"example.com/app/internal/x"}`)),
	})
	v := run.Checks["vet"]
	if !v.Conclusive || v.Status != validate.Fail || len(v.Findings) != 1 || v.Findings[0].Package != "example.com/app" || v.Findings[0].Line != 10 {
		t.Fatalf("vet = %+v", v)
	}
}

func TestBaseline_MissingGoSumIsManifestFinding(t *testing.T) {
	run, _ := baseline(t, map[string]sandbox.ExecResult{
		"list":  ok(pkgs),
		"build": {ExitCode: 1, Stderr: []byte("go: example.com/lib@v1.2.4: missing go.sum entry for go.mod file; to add it:\n\tgo mod download example.com/lib\n")},
		"test":  ok(testJSON(`{"Action":"pass","Package":"example.com/app"}`, `{"Action":"skip","Package":"example.com/app/internal/x"}`)),
	})
	b := run.Checks["build"]
	if !b.Conclusive || len(b.Findings) != 1 || !strings.HasPrefix(b.Findings[0].Key, "build:manifest:") {
		t.Fatalf("build = %+v", b)
	}
}

func TestBaseline_FailClosed(t *testing.T) {
	cases := map[string]struct {
		results map[string]sandbox.ExecResult
		check   string
		reason  string
	}{
		"timeout":                                   {map[string]sandbox.ExecResult{"list": ok(pkgs), "test": {TimedOut: true, ExitCode: 137}}, "test", "timed out"},
		"truncated output":                          {map[string]sandbox.ExecResult{"list": ok(pkgs), "build": {ExitCode: 0, StdoutTruncated: true}}, "build", "capture limit"},
		"nonzero without findings":                  {map[string]sandbox.ExecResult{"list": ok(pkgs), "build": {ExitCode: 2, Stderr: []byte("segfault in compiler\n")}}, "build", "did not parse"},
		"exit zero with diagnostics":                {map[string]sandbox.ExecResult{"list": ok(pkgs), "vet": {ExitCode: 0, Stderr: []byte("./a.go:1:1: suspicious\n")}}, "vet", "exit code 0 but"},
		"missing package terminal":                  {map[string]sandbox.ExecResult{"list": ok(pkgs), "test": ok(testJSON(`{"Action":"pass","Package":"example.com/app"}`))}, "test", "did not parse"},
		"test exit mismatch":                        {map[string]sandbox.ExecResult{"list": ok(pkgs), "test": {ExitCode: 0, Stdout: []byte(testJSON(`{"Action":"fail","Package":"example.com/app"}`, `{"Action":"skip","Package":"example.com/app/internal/x"}`))}}, "test", "did not parse"},
		"missing package result with silent stderr": {map[string]sandbox.ExecResult{"list": ok(pkgs), "test": {ExitCode: 1, Stderr: []byte("something odd\n"), Stdout: []byte(testJSON(`{"Action":"skip","Package":"example.com/app/internal/x"}`))}}, "test", "did not parse"},
		"non-json test output":                      {map[string]sandbox.ExecResult{"list": ok(pkgs), "test": {ExitCode: 0, Stdout: []byte("garbage\n" + testJSON(`{"Action":"pass","Package":"example.com/app"}`, `{"Action":"skip","Package":"example.com/app/internal/x"}`))}}, "test", "did not parse"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run, _ := baseline(t, tc.results)
			c := run.Checks[tc.check]
			if c.Conclusive || !strings.Contains(c.Reason, tc.reason) {
				t.Fatalf("%s = conclusive %v reason %q, want reason containing %q", tc.check, c.Conclusive, c.Reason, tc.reason)
			}
			if run.Conclusive || run.Clean {
				t.Fatal("run must be inconclusive and not clean")
			}
		})
	}
}

func TestBaseline_TestFailuresKeyed(t *testing.T) {
	run, _ := baseline(t, map[string]sandbox.ExecResult{
		"list": ok(pkgs),
		"test": {ExitCode: 1, Stdout: []byte(testJSON(
			`{"Action":"fail","Package":"example.com/app","Test":"TestB/sub"}`,
			`{"Action":"fail","Package":"example.com/app","Test":"TestB"}`,
			`{"Action":"fail","Package":"example.com/app"}`,
			`{"Action":"output","Package":"example.com/app/internal/x","Output":"panic: boom\n"}`,
			`{"Action":"fail","Package":"example.com/app/internal/x"}`,
		))},
	})
	c := run.Checks["test"]
	if !c.Conclusive || c.Status != validate.Fail {
		t.Fatalf("test = %+v", c)
	}
	keys := []string{}
	for _, f := range c.Findings {
		keys = append(keys, f.Key)
	}
	want := "test:example.com/app:TestB test:example.com/app:TestB/sub test:example.com/app/internal/x:package"
	if strings.Join(keys, " ") != want {
		t.Fatalf("keys = %v", keys)
	}
	if !strings.Contains(c.Findings[2].Message, "panic: boom") {
		t.Fatalf("package failure message = %q", c.Findings[2].Message)
	}
}

func TestBaseline_ListFailureErrorsEveryCheck(t *testing.T) {
	run, fs := baseline(t, map[string]sandbox.ExecResult{
		"list": {ExitCode: 1, Stderr: []byte("go: cannot find main module\n")},
	})
	for _, name := range validate.Required {
		if c := run.Checks[name]; c.Conclusive || c.Status != validate.Error || !strings.Contains(c.Reason, "package listing failed") {
			t.Fatalf("%s = %+v", name, c)
		}
	}
	if len(fs.calls) != 1 {
		t.Fatalf("checks ran despite listing failure: %d calls", len(fs.calls))
	}
}

// A package whose test binary could not be built or set up produces no
// JSON result; the failure on stderr makes it a conclusive package finding.
func TestBaseline_TestSetupFailureIsConclusive(t *testing.T) {
	cases := map[string]string{
		"build failed":   "# example.com/app\n./main.go:3:1: syntax error\nFAIL\texample.com/app [build failed]\n",
		"setup failed":   "main.go:6:2: cannot find module providing package example.com/toolkit/strutil: import lookup disabled by -mod=readonly\nFAIL\texample.com/app [setup failed]\n",
		"fail line only": "FAIL\texample.com/app [setup failed]\n",
	}
	for name, stderr := range cases {
		t.Run(name, func(t *testing.T) {
			run, _ := baseline(t, map[string]sandbox.ExecResult{
				"list": ok(pkgs),
				"test": {ExitCode: 1, Stderr: []byte(stderr), Stdout: []byte(testJSON(`{"Action":"skip","Package":"example.com/app/internal/x"}`))},
			})
			c := run.Checks["test"]
			if !c.Conclusive || c.Status != validate.Fail || len(c.Findings) != 1 || c.Findings[0].Key != "test:example.com/app:package" {
				t.Fatalf("test = %+v", c)
			}
		})
	}
}
