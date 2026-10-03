//go:build integration

package steward_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/testtmp"
)

// cacheMarkerTest is a test that fails when the build cache it runs
// against already holds what an earlier run of it wrote there. It stands
// in for executed code that poisons the cache to fake a later result.
const cacheMarkerTest = `package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildCacheIsFresh(t *testing.T) {
	marker := filepath.Join(os.Getenv("GOCACHE"), "repo-steward-marker")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the build cache holds a marker written by an earlier validation")
	}
	if err := os.WriteFile(marker, []byte("poisoned"), 0o644); err != nil {
		t.Fatal(err)
	}
}
`

// markerFixture is patch-safe plus cacheMarkerTest, with a fixture proxy.
func markerFixture(t *testing.T) (repo, proxy, data string) {
	t.Helper()
	def, err := fixture.Load("patch-safe")
	if err != nil {
		t.Fatal(err)
	}
	def.Files["cache_marker_test.go"] = []byte(cacheMarkerTest)
	def.ExpectedBaseCommit, def.ExpectedBaseTree = "", ""
	root := testtmp.Dir(t)
	r, err := def.Setup(ctx, filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modproxy.Build(filepath.Join(root, "proxy")); err != nil {
		t.Fatal(err)
	}
	return r.Path, filepath.Join(root, "proxy"), filepath.Join(root, "data")
}

// checkCachesRemoved asserts that the run left none of its build caches
// behind.
func checkCachesRemoved(t *testing.T, data, runID string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(data, "runs", runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gocache-") {
			t.Fatalf("build cache %s left behind", e.Name())
		}
	}
}

// The baseline validation's test writes a marker into its build cache; the
// post validation runs the same test and would fail, and the run end
// regressed, if it saw that cache.
func TestBaseline_BuildCacheFreshPerValidation(t *testing.T) {
	repo, proxy, data := markerFixture(t)
	res, err := steward.RunBaseline(ctx, steward.Options{SourcePath: repo, DataDir: data, FixtureProxyDir: proxy, Policy: deps.DefaultPolicy(), Author: author})
	if err != nil {
		t.Fatalf("%v (result %+v)", err, res)
	}
	if res.Outcome != steward.OutcomeProposalPrepared || !res.Baseline.Clean || !res.Post.Clean {
		t.Fatalf("outcome %s introduced %v baseline %+v post %+v", res.Outcome, res.Introduced, res.Baseline, res.Post)
	}
	checkCachesRemoved(t, data, res.RunID)
}

// The same through the agent path: the prelude's baseline validation and
// both run_validation calls of scenario S1 each see a cache of their own.
func TestScripted_BuildCacheFreshPerValidation(t *testing.T) {
	repo, proxy, data := markerFixture(t)
	res, err := steward.RunScripted(ctx, steward.Options{SourcePath: repo, DataDir: data, FixtureProxyDir: proxy, Policy: deps.DefaultPolicy(), Author: author}, "S1")
	if err != nil {
		t.Fatalf("%v (result %+v)", err, res)
	}
	if res.Outcome != steward.OutcomeProposalPrepared {
		t.Fatalf("outcome %s run %+v detail %v", res.Outcome, res.Run, res.Detail)
	}
	checkCachesRemoved(t, data, res.RunID)
}
