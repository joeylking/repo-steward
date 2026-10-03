//go:build smoke

package smoke

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/testtmp"
)

//go:embed testdata/repair/*.json
var repairDeclarations embed.FS

// ModelEnv names the model the repair scenarios run with, as provider:name.
// They are skipped when it is unset, so neither TestSmoke nor the smoke
// workflow runs them. Only a local (ollama) model is accepted.
const ModelEnv = "REPO_STEWARD_SMOKE_MODEL"

// OutEnv, when set, names a directory that keeps each run's result JSON and
// its model recording after the test ends.
const OutEnv = "REPO_STEWARD_SMOKE_OUT"

// TestRepair runs the repair scenarios: real repositories where upgrading
// one dependency breaks the build and a small source change repairs it.
// Each scenario first runs the baseline pipeline, which must end
// regressed, so the break is real; then the model runs once.
//
// The model's result is reported, not required: a run that ends without a
// proposal passes and is logged with its outcome, because a model failing
// to repair is a result. A proposal is held to every check a correct one
// must pass, and failing any of them fails the test: files within the
// declared set, the required files present, the hidden oracles, clean and
// conclusive post-validation bound to the proposal tree, the proposal on
// the pinned commit, and the operator's checkout untouched.
//
//	REPO_STEWARD_SMOKE_MODEL=ollama:qwen3:30b-a3b \
//	  go test -tags smoke -count=1 -p 1 -v -timeout 2h -run TestRepair ./internal/smoke/
func TestRepair(t *testing.T) {
	model := os.Getenv(ModelEnv)
	if model == "" {
		t.Skipf("%s is not set; the repair scenarios need a model", ModelEnv)
	}
	spec, err := steward.ParseModelSpec(model)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "ollama" {
		t.Fatalf("%s=%s: the repair scenarios run only a local model (ollama:<name>)", ModelEnv, model)
	}
	root := testtmp.Dir(t)
	out := os.Getenv(OutEnv)
	clones := map[string]string{}
	author := gitx.Identity{Name: "Smoke", Email: "smoke@example.invalid", When: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, sc := range loadFrom(t, repairDeclarations, "testdata/repair") {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Expected != steward.OutcomeRegressed {
				t.Fatalf("a repair scenario's baseline must be expected %s, got %s", steward.OutcomeRegressed, sc.Expected)
			}
			src := clone(t, root, sc, clones)
			pol := deps.DefaultPolicy()
			pol.NamedDependency = sc.Dependency + "@" + sc.Version
			opts := steward.Options{SourcePath: src, Policy: pol, Author: author}

			// The baseline proves the break: no model, regressed.
			opts.DataDir = filepath.Join(root, "data-baseline")
			base, err := steward.RunBaseline(ctx, opts)
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}
			if base.Outcome != sc.Expected || base.Baseline == nil || !base.Baseline.Clean || len(base.Introduced) == 0 || base.Proposal != nil {
				t.Fatalf("baseline outcome %s, want a clean baseline and %s with introduced findings", base.Outcome, sc.Expected)
			}
			untouched(t, src)

			// The model run.
			ms := spec
			keep := root
			if out != "" {
				keep = out
			}
			stamp := time.Now().UTC().Format("20060102-150405")
			ms.RecordDir = filepath.Join(keep, sc.Name+"-"+stamp, "recording")
			opts.DataDir = filepath.Join(root, "data-model")
			opts.RuntimeLimits = steward.DefaultModelLimits()
			if sc.MaxModelCalls > 0 {
				opts.RuntimeLimits.MaxModelCalls = sc.MaxModelCalls
			}
			res, err := steward.RunModel(ctx, opts, ms)
			if res != nil {
				b, _ := json.MarshalIndent(res, "", "  ")
				os.MkdirAll(filepath.Join(keep, sc.Name+"-"+stamp), 0o755)
				os.WriteFile(filepath.Join(keep, sc.Name+"-"+stamp, "result.json"), b, 0o644)
			}
			if err != nil {
				t.Fatalf("model run: %v", err)
			}
			untouched(t, src)
			steps := 0
			if res.Run != nil {
				steps = res.Run.Steps
			}
			if res.Proposal == nil {
				if res.Outcome == steward.OutcomeProposalPrepared {
					t.Fatal("proposal_prepared without a proposal")
				}
				t.Logf("%s: not repaired: outcome %s after %d model calls, %d steps", sc.Name, res.Outcome, res.ModelCalls, steps)
				return
			}
			checkRepair(t, sc, res)
			t.Logf("%s: repaired: proposal %s changing %v after %d model calls, %d steps", sc.Name, res.Proposal.HeadCommit[:12], proposalFiles(res), res.ModelCalls, steps)
		})
	}
}

func untouched(t *testing.T, src string) {
	t.Helper()
	if status, err := gitx.New(src).Run(ctx, "status", "--porcelain"); err != nil || len(status) != 0 {
		t.Fatalf("source checkout modified: %s %v", status, err)
	}
}

func proposalFiles(res *steward.Result) []string {
	var files []string
	for _, f := range res.Proposal.Files {
		files = append(files, f.Path)
	}
	sort.Strings(files)
	return files
}

// checkRepair holds a proposal to everything a correct repair must satisfy.
// Any failure is a false success.
func checkRepair(t *testing.T, sc Scenario, res *steward.Result) {
	t.Helper()
	if res.Outcome != steward.OutcomeProposalPrepared {
		t.Fatalf("a proposal with outcome %s", res.Outcome)
	}
	files := proposalFiles(res)
	for _, f := range files {
		if !slices.Contains(sc.AllowedFiles, f) {
			t.Errorf("proposal changes %s, outside %v", f, sc.AllowedFiles)
		}
		if strings.HasSuffix(f, "_test.go") {
			t.Errorf("proposal changes test file %s", f)
		}
	}
	for _, f := range sc.RequiredFiles {
		if !slices.Contains(files, f) {
			t.Errorf("proposal does not change required file %s (changes %v)", f, files)
		}
	}
	if res.Post == nil || !res.Post.Conclusive || !res.Post.Clean || len(res.Introduced) != 0 {
		t.Fatalf("post validation %+v introduced %v", res.Post, res.Introduced)
	}
	if res.Readiness == nil || !res.Readiness.Ready {
		t.Fatalf("readiness %+v", res.Readiness)
	}
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
	for _, o := range sc.Oracles {
		content, err := g.Run(ctx, "show", res.Proposal.HeadCommit+":"+o.File)
		if err != nil {
			t.Errorf("oracle %s: file missing from the proposal tree", o.File)
			continue
		}
		for _, m := range o.MustContain {
			if !strings.Contains(string(content), m) {
				t.Errorf("oracle %s: missing %q", o.File, m)
			}
		}
		for _, m := range o.MustNotContain {
			if strings.Contains(string(content), m) {
				t.Errorf("oracle %s: contains %q", o.File, m)
			}
		}
	}
}
