//go:build integration

package steward_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/scenario"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/workspace"
)

func runScenario(t *testing.T, name string) (*steward.Result, string, *fixture.Repo) {
	t.Helper()
	sc, err := scenario.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	root := testtmp.Dir(t)
	r, err := fixture.Setup(ctx, sc.Fixture, filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modproxy.Build(filepath.Join(root, "proxy")); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	var events []agentrt.Event
	res, err := steward.RunScripted(ctx, steward.Options{
		SourcePath: r.Path, DataDir: data, FixtureProxyDir: filepath.Join(root, "proxy"), Policy: deps.DefaultPolicy(), Author: author,
		Observer: func(e agentrt.Event) { events = append(events, e) },
	}, name)
	if err != nil {
		t.Fatalf("%v (result %+v)", err, res)
	}
	if res.Outcome != sc.Expected {
		t.Fatalf("outcome %s, scenario expects %s; run %+v detail %v", res.Outcome, sc.Expected, res.Run, res.Detail)
	}
	if len(events) == 0 {
		t.Fatal("no events observed")
	}
	status, _ := gitx.New(r.Path).Run(ctx, "status", "--porcelain")
	if len(status) != 0 {
		t.Fatalf("source checkout modified:\n%s", status)
	}
	return res, data, r
}

func openStores(t *testing.T, data string) (*task.Store, *agentrt.Store) {
	t.Helper()
	ts, err := task.Open(filepath.Join(data, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := agentrt.OpenStore(filepath.Join(data, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close(); rt.Close() })
	return ts, rt
}

func verifyProposal(t *testing.T, res *steward.Result, data string, wantFiles string) {
	t.Helper()
	ts, rt := openStores(t, data)
	ws, err := workspace.Open(ctx, res.Workspace.Dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := proposal.Verify(ctx, ts, ws, res.RunID, res.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range p.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != wantFiles {
		t.Fatalf("files = %v, want %s", paths, wantFiles)
	}
	// The evidence the proposal cites was produced by a step the runtime
	// recorded as done.
	vals, _ := ts.ListValidations(ctx, res.RunID)
	steps, _ := rt.ListSteps(ctx, res.RunID)
	done := map[string]bool{}
	for _, st := range steps {
		done[st.ID] = st.Status == agentrt.StepDone
	}
	found := false
	for _, v := range vals {
		if v.ID == p.ValidationID {
			found = true
			if !done[v.StepID] || v.TreeHash != p.TreeHash {
				t.Fatalf("cited validation %+v is not from a done step on the proposal tree", v)
			}
		}
	}
	if !found {
		t.Fatal("cited validation record missing")
	}
	run, _ := rt.GetRun(ctx, res.RunID)
	if run.Status != agentrt.StatusCompleted || run.ID != res.RunID {
		t.Fatalf("runtime run = %+v", run)
	}
	tk, _ := ts.GetTask(ctx, res.RunID)
	if tk.Outcome != steward.OutcomeProposalPrepared {
		t.Fatalf("task outcome = %s", tk.Outcome)
	}
}

func TestScripted_S1_PatchUpgrade(t *testing.T) {
	res, data, _ := runScenario(t, "S1")
	verifyProposal(t, res, data, "go.mod,go.sum")
}

func TestScripted_S2_BreakingMinorRepaired(t *testing.T) {
	res, data, _ := runScenario(t, "S2")
	verifyProposal(t, res, data, "go.mod,go.sum,main.go")
	if res.Selected.Version != "v1.3.0" {
		t.Fatalf("selected %+v", res.Selected)
	}
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	main, _ := ws.Git().Run(ctx, "show", res.Proposal.TreeHash+":main.go")
	if !strings.Contains(string(main), "context.Background()") {
		t.Fatalf("repair not in proposal tree:\n%s", main)
	}
	test, _ := ws.Git().Run(ctx, "show", res.Proposal.TreeHash+":main_test.go")
	base, _ := ws.Git().Run(ctx, "show", ws.BaseTree+":main_test.go")
	if string(test) != string(base) {
		t.Fatal("protected test file changed")
	}
}

func TestScripted_S3_MovedPackageRepaired(t *testing.T) {
	res, data, _ := runScenario(t, "S3")
	verifyProposal(t, res, data, "go.mod,go.sum,main.go")
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	main, _ := ws.Git().Run(ctx, "show", res.Proposal.TreeHash+":main.go")
	if !strings.Contains(string(main), `"example.com/toolkit/text/strutil"`) {
		t.Fatalf("import not repaired:\n%s", main)
	}
}

func TestScripted_S8_ProtectedChangeBlocked(t *testing.T) {
	res, data, _ := runScenario(t, "S8")
	ts, rt := openStores(t, data)
	if res.Proposal != nil {
		t.Fatal("a proposal was produced")
	}
	if rows, _ := ts.ListProposals(ctx, res.RunID); len(rows) != 0 {
		t.Fatal("proposal rows exist")
	}
	steps, _ := rt.ListSteps(ctx, res.RunID)
	denied := false
	for _, st := range steps {
		if st.Decision != nil && st.Decision.Tool == "write_file" && strings.Contains(string(st.Decision.Args), "main_test.go") {
			if st.Status != agentrt.StepFailed || st.Policy == nil || st.Policy.Outcome != agentrt.Deny || st.Observation.Kind != agentrt.ObservePolicyDenied {
				t.Fatalf("protected write step = %+v policy %+v", st, st.Policy)
			}
			denied = true
		}
	}
	if !denied {
		t.Fatal("no denied write of main_test.go recorded")
	}
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	cur, _ := ws.ReadFile("main_test.go")
	base, _ := ws.Git().Run(ctx, "show", ws.BaseTree+":main_test.go")
	if string(cur) != string(base) {
		t.Fatal("main_test.go was modified in the workspace")
	}
	if !strings.Contains(res.Detail["reason"].(string), "protected") {
		t.Fatalf("detail = %v", res.Detail)
	}
	tk, _ := ts.GetTask(ctx, res.RunID)
	if tk.Outcome != steward.OutcomeBlocked {
		t.Fatalf("task outcome = %s", tk.Outcome)
	}
}

// Every embedded scenario names a fixture that exists and an outcome the
// runner knows.
func TestScenarios_ReferToRealFixtures(t *testing.T) {
	names, err := scenario.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 4 {
		t.Fatalf("scenarios = %v", names)
	}
	for _, n := range names {
		sc, err := scenario.Load(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.Load(sc.Fixture); err != nil {
			t.Errorf("%s: %v", n, err)
		}
		if sc.Expected != steward.OutcomeProposalPrepared && sc.Expected != steward.OutcomeBlocked && sc.Expected != steward.OutcomeProposalPublished {
			t.Errorf("%s: unexpected expected outcome %q", n, sc.Expected)
		}
	}
	_ = os.Getenv
}
