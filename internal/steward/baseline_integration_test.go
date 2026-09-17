//go:build integration

package steward_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/workspace"
)

var ctx = context.Background()

var author = gitx.Identity{Name: "Fixture Operator", Email: "op@example.invalid"}

func runBaseline(t *testing.T, name string, pol deps.Policy) (*steward.Result, string) {
	t.Helper()
	root := testtmp.Dir(t)
	r, err := fixture.Setup(ctx, name, filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modproxy.Build(filepath.Join(root, "proxy")); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	res, err := steward.RunBaseline(ctx, steward.Options{SourcePath: r.Path, DataDir: data, FixtureProxyDir: filepath.Join(root, "proxy"), Policy: pol, Author: author})
	if err != nil {
		t.Fatalf("%v (result %+v)", err, res)
	}
	// The operator's checkout is never modified.
	status, _ := gitx.New(r.Path).Run(ctx, "status", "--porcelain")
	if len(status) != 0 {
		t.Fatalf("source checkout modified:\n%s", status)
	}
	if head, _ := gitx.New(r.Path).RevParse(ctx, "HEAD"); head != r.BaseCommit {
		t.Fatal("source HEAD moved")
	}
	return res, data
}

func TestBaseline_PatchSafeProducesVerifiedProposal(t *testing.T) {
	res, data := runBaseline(t, "patch-safe", deps.DefaultPolicy())
	if res.Outcome != steward.OutcomeProposalPrepared {
		t.Fatalf("outcome %s detail %v readiness %+v", res.Outcome, res.Detail, res.Readiness)
	}
	if res.Selected == nil || res.Selected.Version != "v1.2.4" {
		t.Fatalf("selected %+v", res.Selected)
	}
	if !res.Admission.OK() || !res.Normalization.OK() || !res.Post.Clean {
		t.Fatalf("admission %+v normalization %+v post %+v", res.Admission, res.Normalization, res.Post)
	}
	p := res.Proposal
	if p == nil || p.HeadRef != "repo-steward/lib-v1.2.4" || p.BaseRef != "main" || p.BaseCommit != res.Workspace.BaseCommit {
		t.Fatalf("proposal %+v", p)
	}
	paths := []string{}
	for _, f := range p.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "go.mod,go.sum" {
		t.Fatalf("files = %v, want manifests only", paths)
	}

	// The frozen proposal verifies against the workspace independently of
	// the checkout's HEAD, and the recorded evidence is bound to its tree.
	store, err := task.Open(filepath.Join(data, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ws, err := workspace.Open(ctx, res.Workspace.Dir)
	if err != nil {
		t.Fatal(err)
	}
	vp, err := proposal.Verify(ctx, store, ws, res.RunID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vp.TreeHash != res.Post.TreeHash || vp.ValidationID != res.Post.ID {
		t.Fatalf("proposal evidence not bound: %+v vs %+v", vp, res.Post)
	}
	if head, _ := ws.Git().RevParse(ctx, "HEAD"); head != ws.BaseCommit {
		t.Fatal("scratch HEAD moved during freeze")
	}
	out, _ := ws.Git().Run(ctx, "show", "--stat", "--format=%an <%ae>%n%B", p.HeadCommit)
	if !strings.Contains(string(out), "Fixture Operator <op@example.invalid>") || !strings.Contains(string(out), "Repo-Steward-Proposal: "+p.ID) {
		t.Fatalf("commit = %s", out)
	}
	// go.mod in the proposal tree names the target version.
	mod, _ := ws.Git().Run(ctx, "show", p.TreeHash+":go.mod")
	if !strings.Contains(string(mod), "example.com/lib v1.2.4") {
		t.Fatalf("go.mod in proposal tree:\n%s", mod)
	}
	// Task and journal state.
	tk, _ := store.GetTask(ctx, res.RunID)
	if tk.Outcome != steward.OutcomeProposalPrepared || tk.ModulePath != "example.com/app" {
		t.Fatalf("task = %+v", tk)
	}
	proms, _ := store.ListPromotions(ctx, res.RunID)
	if len(proms) != 2 || proms[0].Kind != "upgrade" || proms[0].Status != task.PromotionPromoted || proms[1].Kind != "normalize" || proms[1].Status != task.PromotionPromoted {
		t.Fatalf("promotions = %+v", proms)
	}
	for _, pr := range proms {
		if _, err := os.Stat(pr.StagingDir); err == nil {
			t.Fatal("staging directory retained after a terminal promotion")
		}
	}
	vals, _ := store.ListValidations(ctx, res.RunID)
	if len(vals) != 2 {
		t.Fatalf("validations = %d", len(vals))
	}
}

func TestBaseline_BreakingMinorIsRegressedNotProposed(t *testing.T) {
	res, data := runBaseline(t, "breaking-minor", deps.DefaultPolicy())
	if res.Outcome != steward.OutcomeRegressed {
		t.Fatalf("outcome %s detail %v", res.Outcome, res.Detail)
	}
	if res.Selected.Version != "v1.3.0" || !res.Admission.OK() || !res.Normalization.OK() {
		t.Fatalf("selected %+v admission %+v normalization %+v", res.Selected, res.Admission, res.Normalization)
	}
	if res.Post.Conclusive != true || res.Post.Clean {
		t.Fatalf("post = %+v", res.Post)
	}
	keys := []string{}
	for _, f := range res.Introduced {
		keys = append(keys, f.Key)
	}
	joined := strings.Join(keys, " ")
	if !strings.Contains(joined, "build:./main.go:not enough arguments") || !strings.Contains(joined, "vet:") || !strings.Contains(joined, "test:example.com/app:package") {
		t.Fatalf("introduced = %v", keys)
	}
	if res.Proposal != nil {
		t.Fatal("a proposal was produced for a regressed upgrade")
	}
	store, _ := task.Open(filepath.Join(data, "steward.db"))
	defer store.Close()
	if rows, _ := store.ListProposals(ctx, res.RunID); len(rows) != 0 {
		t.Fatal("proposal rows exist")
	}
}

func TestBaseline_PolicyRestrictsSelection(t *testing.T) {
	// Patch-only policy: breaking-minor's only upgrade is a minor, so
	// nothing is eligible and nothing is mutated.
	res, data := runBaseline(t, "breaking-minor", deps.Policy{AllowPatch: true})
	if res.Outcome != steward.OutcomeNoCandidate || res.Selected != nil {
		t.Fatalf("outcome %s selected %+v", res.Outcome, res.Selected)
	}
	store, _ := task.Open(filepath.Join(data, "steward.db"))
	defer store.Close()
	if proms, _ := store.ListPromotions(ctx, res.RunID); len(proms) != 0 {
		t.Fatal("manifests were promoted without a candidate")
	}
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	if tree, _ := ws.CandidateTree(ctx); tree != ws.BaseTree {
		t.Fatal("workspace changed without a candidate")
	}

	// A named dependency that is not present yields no candidate either.
	res, _ = runBaseline(t, "patch-safe", deps.Policy{AllowPatch: true, AllowMinor: true, NamedDependency: "example.com/other"})
	if res.Outcome != steward.OutcomeNoCandidate {
		t.Fatalf("named-dependency outcome = %s", res.Outcome)
	}
}

func TestBaseline_ProposalTargetMatchesPolicyTarget(t *testing.T) {
	// With minors allowed, patch-safe still selects the patch first.
	res, _ := runBaseline(t, "patch-safe", deps.Policy{AllowPatch: true, AllowMinor: true, AllowMajor: true})
	if res.Outcome != steward.OutcomeProposalPrepared || res.Proposal.Target != (manifest.Target{Module: "example.com/lib", Version: "v1.2.4"}) {
		t.Fatalf("outcome %s target %+v", res.Outcome, res.Proposal)
	}
}
