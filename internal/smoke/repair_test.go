//go:build smoke

package smoke

import (
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
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/validate"
	"github.com/joeylking/repo-steward/internal/workspace"
)

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
// to repair is a result; the tree it left is judged by the oracles and its
// diff kept. A proposal is held to every check a correct one must pass,
// and failing any of them fails the test: the scenario's declared
// repair_outcome is proposal_prepared, files within the declared set, the
// required files present, the hidden oracles, clean and conclusive post
// validation bound to the proposal tree, verified coverage of the changed
// source, the proposal on the pinned commit, and the operator's checkout
// untouched.
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
			dir := filepath.Join(keep, sc.Name+"-"+stamp)
			switch {
			case res.Proposal != nil:
				if sc.RepairOutcome != steward.OutcomeProposalPrepared {
					t.Fatalf("%s: proposal %s, but a correct repair here ends %s: the repository's tests do not execute the lines a repair changes (%s)", sc.Name, res.Proposal.HeadCommit, sc.RepairOutcome, sc.RepairEvidence)
				}
				checkRepair(t, sc, res, opts.DataDir)
				t.Logf("%s: repaired: proposal %s changing %v after %d model calls, %d steps", sc.Name, res.Proposal.HeadCommit[:12], proposalFiles(res), res.ModelCalls, steps)
			case res.Outcome == steward.OutcomeProposalPrepared:
				t.Fatal("proposal_prepared without a proposal")
			case res.Outcome == steward.OutcomeRepairNotExercised:
				// The coverage rule stopped a repair that builds, vets, and
				// passes the tests. Whether the repair was right is recorded,
				// not required: the oracles judge the tree it left.
				t.Logf("%s: repair_not_exercised after %d model calls, %d steps (runtime outcome %v); the unexercised repair %s", sc.Name, res.ModelCalls, steps, res.Detail["run_outcome"], judgeTree(t, sc, res, dir))
			default:
				t.Logf("%s: not repaired: outcome %s after %d model calls, %d steps; the tree it left %s", sc.Name, res.Outcome, res.ModelCalls, steps, judgeTree(t, sc, res, dir))
			}
		})
	}
}

// judgeTree applies the oracles to the candidate tree a run left, writes
// its diff from the base next to the result, and says what it found.
func judgeTree(t *testing.T, sc Scenario, res *steward.Result, dir string) string {
	t.Helper()
	ws, err := workspace.Open(ctx, res.Workspace.Dir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ws.CandidateTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tree == ws.BaseTree {
		return "is the base tree"
	}
	if diff, err := ws.Git().Run(ctx, "diff-tree", "-p", "--no-color", ws.BaseTree, tree, "--", ".", ":(exclude)go.sum"); err == nil {
		os.WriteFile(filepath.Join(dir, "candidate.diff"), diff, 0o644)
	}
	fails := checkOracles(sc.Oracles, func(file string) ([]byte, error) { return ws.Git().Run(ctx, "show", tree+":"+file) })
	if len(fails) == 0 {
		return "passes the oracles (tree " + tree + ")"
	}
	return "fails the oracles (tree " + tree + "): " + strings.Join(fails, "; ")
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
// Any failure is a false success. The validation evidence is read from the
// run's task store, where every mode records it: the post validation
// readiness bound (its id is in the proposal) must be on the proposal tree
// under the proposal's configuration, conclusive, clean, and introduce
// nothing over the baseline the proposal names.
func checkRepair(t *testing.T, sc Scenario, res *steward.Result, dataDir string) {
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
	if res.Readiness == nil || !res.Readiness.Ready {
		t.Fatalf("readiness %+v", res.Readiness)
	}
	if c := res.Readiness.Coverage; c == nil || !c.Verified || c.Approval != nil {
		t.Fatalf("coverage evidence %+v: a repair proposal needs the changed source executed by the tests", c)
	}
	post, base := validations(t, dataDir, res)
	if !post.Conclusive || !post.Clean || len(proposal.Introduced(base, post)) != 0 {
		t.Fatalf("post validation conclusive=%v clean=%v introduced %v", post.Conclusive, post.Clean, proposal.Introduced(base, post))
	}
	g := res.Workspace.Git()
	if at, err := g.RevParse(ctx, res.Proposal.ProposalRef); err != nil || at != res.Proposal.HeadCommit {
		t.Fatalf("proposal ref at %s, want %s (%v)", at, res.Proposal.HeadCommit, err)
	}
	if parent, err := g.RevParse(ctx, res.Proposal.HeadCommit+"^"); err != nil || parent != sc.Commit {
		t.Fatalf("proposal parent %s, want %s (%v)", parent, sc.Commit, err)
	}
	if tree, err := g.RevParse(ctx, res.Proposal.HeadCommit+"^{tree}"); err != nil || tree != post.TreeHash || tree != res.Proposal.TreeHash {
		t.Fatalf("proposal tree %s, validated tree %s (%v)", tree, post.TreeHash, err)
	}
	for _, f := range checkOracles(sc.Oracles, func(file string) ([]byte, error) { return g.Run(ctx, "show", res.Proposal.HeadCommit+":"+file) }) {
		t.Error(f)
	}
}

// validations reads the proposal's post validation and its baseline from
// the task store and checks their bindings.
func validations(t *testing.T, dataDir string, res *steward.Result) (post, base *validate.Run) {
	t.Helper()
	ts, err := task.Open(filepath.Join(dataDir, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	recs, err := ts.ListValidations(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Proposal
	for _, rec := range recs {
		var run validate.Run
		if err := json.Unmarshal(rec.Run, &run); err != nil {
			t.Fatal(err)
		}
		switch rec.ID {
		case p.ValidationID:
			if rec.Kind != "post" || rec.TreeHash != p.TreeHash || rec.ConfigHash != p.ConfigHash || rec.ToolchainDigest != p.ToolchainDigest {
				t.Fatalf("post validation %s is %s of tree %s, want post of %s under the proposal's configuration", rec.ID, rec.Kind, rec.TreeHash, p.TreeHash)
			}
			post = &run
		case p.BaselineID:
			if rec.Kind != "baseline" || rec.TreeHash != res.Workspace.BaseTree {
				t.Fatalf("baseline validation %s is %s of tree %s", rec.ID, rec.Kind, rec.TreeHash)
			}
			base = &run
		}
	}
	if post == nil || base == nil {
		t.Fatalf("validation records %s and %s not found", p.ValidationID, p.BaselineID)
	}
	return post, base
}
