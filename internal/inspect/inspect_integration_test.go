//go:build integration

package inspect_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/inspect"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/validate"
)

func inspectFixture(t *testing.T, name string) *inspect.Report {
	t.Helper()
	root := testtmp.Dir(t)
	r, err := fixture.Setup(ctx, name, filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modproxy.Build(filepath.Join(root, "proxy")); err != nil {
		t.Fatal(err)
	}
	// A fresh data directory means a cold module cache: discovery and
	// validation must work from nothing but the file proxy.
	rep, err := inspect.Run(ctx, inspect.Options{RepoPath: r.Path, DataDir: filepath.Join(root, "data"), FixtureProxyDir: filepath.Join(root, "proxy"), Policy: deps.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.HeadCommit != r.BaseCommit || rep.TreeHash != r.BaseTree {
		t.Fatalf("identity mismatch: %+v vs %+v", rep, r)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "cache", "mod", "example.com", "app", "mod", "cache", "download", "example.com", "lib")); err != nil {
		t.Fatalf("module cache not populated under the data directory: %v", err)
	}
	return rep
}

func TestInspect_PatchSafeColdCache(t *testing.T) {
	rep := inspectFixture(t, "patch-safe")
	if rep.Outcome != inspect.OutcomeOK || rep.WorktreeDirty || !rep.Profile.Supported() {
		t.Fatalf("report outcome %s dirty %v refusals %v", rep.Outcome, rep.WorktreeDirty, rep.Refusals)
	}
	if rep.ImageRef != rep.Profile.Toolchain.Ref() || !strings.HasPrefix(rep.ImageRef, "golang@sha256:") {
		t.Fatalf("image = %s", rep.ImageRef)
	}
	if len(rep.Candidates) != 1 {
		t.Fatalf("candidates = %+v", rep.Candidates)
	}
	c := rep.Candidates[0]
	if c.Module != "example.com/lib" || c.Current != "v1.2.1" || c.Latest != "v1.3.0" || c.Delta != "minor" {
		t.Fatalf("candidate = %+v", c)
	}
	if el := c.EligibleTargets(); len(el) != 2 || el[0].Version != "v1.2.4" || el[1].Version != "v1.3.0" {
		t.Fatalf("eligible targets = %+v", c.Targets)
	}
	b := rep.Baseline
	if !b.Conclusive || !b.Clean || len(b.Packages) != 1 || b.Packages[0] != "example.com/app" {
		t.Fatalf("baseline = %+v", b)
	}
	for _, name := range validate.Required {
		if c := b.Checks[name]; !c.Conclusive || c.Status != validate.Pass || len(c.Findings) != 0 || len(c.Attempts) != 1 {
			t.Fatalf("%s = %+v", name, c)
		}
	}
	if b.ToolchainDigest != rep.Profile.Toolchain.Digest || b.TreeHash != rep.TreeHash {
		t.Fatal("baseline evidence not bound to tree and toolchain")
	}
}

// S11: the export-ignore test file must actually run.
func TestInspect_ExportIgnoreTestRuns(t *testing.T) {
	rep := inspectFixture(t, "export-ignore-test")
	if rep.Outcome != inspect.OutcomeOK {
		t.Fatalf("outcome = %s: %v", rep.Outcome, rep.Refusals)
	}
	stream := rep.Baseline.Checks["test"].Attempts[0].Stdout
	if !strings.Contains(stream, `"Test":"TestGreet"`) || !strings.Contains(stream, `"Action":"pass","Package":"example.com/app","Test":"TestGreet"`) {
		t.Fatalf("TestGreet from the export-ignore file did not run:\n%s", stream)
	}
}

func TestInspect_IgnoreRules(t *testing.T) {
	rep := inspectFixture(t, "ignore-rules")
	if rep.Outcome != inspect.OutcomeOK {
		t.Fatalf("outcome = %s: %v", rep.Outcome, rep.Refusals)
	}
	if _, err := os.Stat(filepath.Join(rep.Snapshot.Dir, "build")); err == nil {
		t.Fatal("ignored leftover reached the validated snapshot")
	}
	if _, err := os.Stat(filepath.Join(rep.Snapshot.Dir, "config", "local.yaml")); err != nil {
		t.Fatal("tracked file matching an ignore rule missing from the snapshot")
	}
}
