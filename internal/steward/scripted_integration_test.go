//go:build integration

package steward_test

import (
	"encoding/json"
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
	"github.com/joeylking/repo-steward/internal/validate"
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
	// A manifest-only proposal carries no coverage evidence and says
	// nothing about it.
	if res.Readiness.Coverage != nil || strings.Contains(res.Proposal.Body, "executed by tests") || strings.Contains(res.Proposal.Body, "WARNING") {
		t.Fatalf("coverage %+v in a manifest-only proposal:\n%s", res.Readiness.Coverage, res.Proposal.Body)
	}
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
	// The repaired line is executed by TestGreeting: the coverage rule
	// passes on the validation readiness bound, and the body says so.
	c := res.Readiness.Coverage
	if c == nil || !c.Verified || c.Blocks != 1 || c.Executed != 1 || c.ValidationID != res.Readiness.ValidationID || c.Approval != nil {
		t.Fatalf("coverage %+v", c)
	}
	if !strings.Contains(res.Proposal.Body, "Changed source executed by tests: 1 of 1 required coverage blocks in 1 changed source file(s)") || strings.Contains(res.Proposal.Body, "WARNING") {
		t.Fatalf("body:\n%s", res.Proposal.Body)
	}
}

// A repair that builds, vets, and passes the tests, but changes main(),
// which no test executes: readiness refuses it as repair_not_exercised,
// naming the line, nothing is frozen, and after the agent reports blocked
// the run ends repair_not_exercised.
func TestScripted_S2U_UnexercisedRepairIsNotReady(t *testing.T) {
	res, data, _ := runScenario(t, "S2U")
	ts, rt := openStores(t, data)
	if res.Proposal != nil || res.Readiness != nil {
		t.Fatalf("proposal %+v readiness %+v", res.Proposal, res.Readiness)
	}
	if rows, _ := ts.ListProposals(ctx, res.RunID); len(rows) != 0 {
		t.Fatal("proposal rows exist")
	}
	if res.Detail["run_outcome"] != steward.OutcomeBlocked || !strings.Contains(res.Detail["not_exercised"].(string), "main.go:16 (no test executes it)") {
		t.Fatalf("detail %v", res.Detail)
	}
	steps, _ := rt.ListSteps(ctx, res.RunID)
	var prepare *agentrt.Step
	for i := range steps {
		if steps[i].Decision != nil && steps[i].Decision.Tool == "prepare_proposal" {
			prepare = &steps[i]
		}
	}
	if prepare == nil || prepare.Status != agentrt.StepFailed || prepare.Observation.Kind != agentrt.ObserveToolError || prepare.Policy.Outcome != agentrt.Allow {
		t.Fatalf("prepare step %+v", prepare)
	}
	var obs struct{ Error string }
	json.Unmarshal(prepare.Observation.Content, &obs)
	// Only the coverage rule failed: everything else about the tree is
	// ready. main.go:16 is the one line no test executes.
	if want := `not ready: [{"code":"repair_not_exercised","detail":"the repository's tests do not verifiably execute this change: main.go:16 (no test executes it)"}]`; obs.Error != want {
		t.Fatalf("prepare error %q, want %q", obs.Error, want)
	}
	tk, _ := ts.GetTask(ctx, res.RunID)
	if tk.Outcome != steward.OutcomeRepairNotExercised {
		t.Fatalf("task outcome = %s", tk.Outcome)
	}
	// The coverage run was recorded with the validation of the final tree
	// and is a usable profile.
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	tree, _ := ws.CandidateTree(ctx)
	vals, _ := ts.ListValidations(ctx, res.RunID)
	found := false
	for _, v := range vals {
		var run validate.Run
		json.Unmarshal(v.Run, &run)
		if v.TreeHash == tree && run.Coverage != nil {
			if p, why := run.Coverage.Usable(); p == nil {
				t.Fatalf("coverage evidence unusable: %s", why)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no coverage run recorded for the final tree")
	}
}

// edit_file through the runtime and the real policy: the protected edit
// and the ambiguous one are policy denials that change nothing, and the two
// exact edits produce the same repair S2 writes in full.
func TestScripted_S2E_EditFileRepaired(t *testing.T) {
	res, data, _ := runScenario(t, "S2E")
	verifyProposal(t, res, data, "go.mod,go.sum,main.go")
	_, rt := openStores(t, data)
	steps, _ := rt.ListSteps(ctx, res.RunID)
	var denied []string
	edits := 0
	for _, st := range steps {
		if st.Decision == nil || st.Decision.Tool != "edit_file" {
			continue
		}
		if st.Status == agentrt.StepDone {
			edits++
			continue
		}
		if st.Policy == nil || st.Policy.Outcome != agentrt.Deny || st.Observation.Kind != agentrt.ObservePolicyDenied {
			t.Fatalf("failed edit step was not a policy denial: %+v policy %+v", st, st.Policy)
		}
		denied = append(denied, st.Policy.Reason)
	}
	if len(denied) != 2 || !strings.Contains(denied[0], "main_test.go is protected") || !strings.Contains(denied[1], "occurs 3 times in main.go (at lines 9, 10, 15)") || edits != 2 {
		t.Fatalf("denials %q, %d edits", denied, edits)
	}
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	main, _ := ws.Git().Run(ctx, "show", res.Proposal.TreeHash+":main.go")
	if !strings.Contains(string(main), "\t\"context\"\n\t\"fmt\"") || !strings.Contains(string(main), "lib.Greet(context.Background(), name)") || !strings.Contains(string(main), "// greeting wraps") {
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
		known := map[string]bool{steward.OutcomeProposalPrepared: true, steward.OutcomeProposalPublished: true, steward.OutcomeBlocked: true, steward.OutcomeNoCandidate: true, steward.OutcomeRepairNotExercised: true,
			steward.OutcomeScopeExceeded: true, steward.OutcomeBaselineFailing: true, steward.OutcomeRequiresNewerToolchain: true, steward.OutcomeRegressed: true}
		for _, o := range append([]string{sc.Expected}, sc.Acceptable...) {
			if !known[o] {
				t.Errorf("%s: unexpected outcome %q", n, o)
			}
		}
	}
	_ = os.Getenv
}
