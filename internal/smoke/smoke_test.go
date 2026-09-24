//go:build smoke

package smoke

import (
	"context"
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/testtmp"
)

//go:embed testdata/*.json
var declarations embed.FS

// Scenario pins one upgrade on one public repository.
type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Repository  string `json:"repository"`
	Tag         string `json:"tag"`
	Commit      string `json:"commit"`
	Dependency  string `json:"dependency"`
	Version     string `json:"version"`
	Expected    string `json:"expected"`
	// ExpectedFinding names a baseline finding that must be present when
	// the expected outcome is baseline_failing.
	ExpectedFinding string   `json:"expected_finding,omitempty"`
	Files           []string `json:"files,omitempty"`
}

var ctx = context.Background()

func load(t *testing.T) []Scenario {
	t.Helper()
	entries, err := declarations.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var out []Scenario
	for _, e := range entries {
		b, _ := declarations.ReadFile("testdata/" + e.Name())
		var sc Scenario
		if err := json.Unmarshal(b, &sc); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if sc.Name == "" || sc.Commit == "" || sc.Dependency == "" || sc.Version == "" || sc.Expected == "" {
			t.Fatalf("%s: incomplete declaration %+v", e.Name(), sc)
		}
		out = append(out, sc)
	}
	return out
}

// clone fetches the repository once per (repository, commit) and returns a
// checkout on a local branch at exactly the pinned commit.
func clone(t *testing.T, root string, sc Scenario, cache map[string]string) string {
	t.Helper()
	key := sc.Repository + "@" + sc.Commit
	if dir, ok := cache[key]; ok {
		return dir
	}
	dir := filepath.Join(root, "src", strings.TrimSuffix(filepath.Base(sc.Repository), ".git")+"-"+sc.Commit[:8])
	g := gitx.New(root)
	if _, err := g.Run(ctx, "clone", "-q", sc.Repository, dir); err != nil {
		t.Fatalf("clone %s: %v", sc.Repository, err)
	}
	g = gitx.New(dir)
	if _, err := g.Run(ctx, "checkout", "-q", "-b", "smoke", sc.Commit); err != nil {
		t.Fatalf("checkout %s: %v", sc.Commit, err)
	}
	head, err := g.RevParse(ctx, "HEAD")
	if err != nil || head != sc.Commit {
		t.Fatalf("HEAD = %s, want pinned %s (%v)", head, sc.Commit, err)
	}
	if sc.Tag != "" {
		if tagged, err := g.RevParse(ctx, sc.Tag+"^{commit}"); err == nil && tagged != sc.Commit {
			t.Fatalf("tag %s now points at %s, not the pinned %s", sc.Tag, tagged, sc.Commit)
		}
	}
	cache[key] = dir
	return dir
}

func TestSmoke(t *testing.T) {
	root := testtmp.Dir(t)
	dataDir := filepath.Join(root, "data")
	clones := map[string]string{}
	author := gitx.Identity{Name: "Smoke", Email: "smoke@example.invalid", When: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, sc := range load(t) {
		t.Run(sc.Name, func(t *testing.T) {
			src := clone(t, root, sc, clones)
			pol := deps.DefaultPolicy()
			pol.NamedDependency = sc.Dependency + "@" + sc.Version
			res, err := steward.RunBaseline(ctx, steward.Options{SourcePath: src, DataDir: dataDir, Policy: pol, Author: author})
			if res != nil {
				b, _ := json.MarshalIndent(res, "", "  ")
				os.WriteFile(filepath.Join(root, sc.Name+".json"), b, 0o644)
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.Outcome != sc.Expected {
				t.Fatalf("outcome %s, want %s (detail %v)", res.Outcome, sc.Expected, res.Detail)
			}
			// The source checkout is never written.
			if status, err := gitx.New(src).Run(ctx, "status", "--porcelain"); err != nil || len(status) != 0 {
				t.Fatalf("source checkout modified: %s %v", status, err)
			}
			// Baseline evidence is conclusive and bound to the base tree.
			if res.Baseline == nil || !res.Baseline.Conclusive || res.Baseline.TreeHash != res.Workspace.BaseTree {
				t.Fatalf("baseline = %+v", res.Baseline)
			}
			if sc.Expected == "baseline_failing" {
				detail, _ := json.Marshal(res.Detail)
				if res.Baseline.Clean || !strings.Contains(string(detail), sc.ExpectedFinding) {
					t.Fatalf("baseline %+v detail %s, want finding %q", res.Baseline, detail, sc.ExpectedFinding)
				}
				if res.Selected != nil || res.Admission != nil || res.Proposal != nil {
					t.Fatal("a failing baseline must stop before selection and mutation")
				}
				return
			}
			if !res.Baseline.Clean {
				t.Fatalf("baseline not clean: %+v", res.Baseline)
			}
			// Exactly the pinned version is eligible, and nothing else.
			var found bool
			for _, c := range res.Candidates {
				for _, tg := range c.Targets {
					pinned := c.Module == sc.Dependency && tg.Version == sc.Version
					if tg.Eligible != pinned {
						t.Fatalf("%s@%s eligible=%v", c.Module, tg.Version, tg.Eligible)
					}
					found = found || pinned
				}
			}
			if sc.Expected == "no_candidate" {
				if found || res.Proposal != nil {
					t.Fatalf("no candidate expected; found=%v proposal=%v", found, res.Proposal != nil)
				}
				return
			}
			if !found {
				t.Fatalf("pinned %s@%s not among candidates: %+v", sc.Dependency, sc.Version, res.Candidates)
			}
			if sc.Expected == "requires_newer_toolchain" {
				if res.Admission != nil || res.Proposal != nil {
					t.Fatal("a toolchain requirement must stop before admission")
				}
				return
			}
			if res.Selected == nil || res.Selected.Module != sc.Dependency || res.Selected.Version != sc.Version {
				t.Fatalf("selected %+v", res.Selected)
			}
			if res.Admission == nil || !res.Admission.OK() || res.Normalization == nil || !res.Normalization.OK() {
				t.Fatalf("gates: admission %+v normalization %+v", res.Admission, res.Normalization)
			}
			if res.Post == nil || !res.Post.Conclusive || !res.Post.Clean || len(res.Introduced) != 0 {
				t.Fatalf("post validation %+v introduced %v", res.Post, res.Introduced)
			}
			if res.Readiness == nil || !res.Readiness.Ready || res.Proposal == nil {
				t.Fatalf("readiness %+v proposal %v", res.Readiness, res.Proposal)
			}
			var files []string
			for _, f := range res.Proposal.Files {
				files = append(files, f.Path)
			}
			sort.Strings(files)
			if strings.Join(files, ",") != strings.Join(sc.Files, ",") {
				t.Fatalf("proposal files %v, want %v", files, sc.Files)
			}
			// The frozen proposal is a real commit under its ref, on the base.
			g := res.Workspace.Git()
			if at, err := g.RevParse(ctx, res.Proposal.ProposalRef); err != nil || at != res.Proposal.HeadCommit {
				t.Fatalf("proposal ref at %s, want %s (%v)", at, res.Proposal.HeadCommit, err)
			}
			if parent, err := g.RevParse(ctx, res.Proposal.HeadCommit+"^"); err != nil || parent != sc.Commit {
				t.Fatalf("proposal parent %s, want %s (%v)", parent, sc.Commit, err)
			}
			if tree, err := g.RevParse(ctx, res.Proposal.HeadCommit+"^{tree}"); err != nil || tree != res.Post.TreeHash {
				t.Fatalf("proposal tree %s, validated tree %s (%v)", tree, res.Post.TreeHash, err)
			}
			t.Logf("%s: %s -> %s in %s; proposal %s", sc.Name, sc.Dependency, sc.Version, time.Duration(res.Timings["total"])*time.Millisecond, res.Proposal.HeadCommit[:12])
		})
	}
}
