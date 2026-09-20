package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/policy"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
)

var ctx = context.Background()

type fakeFacts struct {
	phase     string
	eligible  map[string]bool
	protected map[string]bool
	scopeOf   func(path string, content []byte) (int, int)
	scope     session.ScopeConfig
	budgets   session.Budgets
	proposal  *policy.ProposalFacts
}

func (f *fakeFacts) Phase(context.Context) (string, error) { return f.phase, nil }
func (f *fakeFacts) EligibleTarget(m, v string) (bool, []string) {
	if f.eligible[m+"@"+v] {
		return true, nil
	}
	return false, []string{"not eligible"}
}
func (f *fakeFacts) ProjectedScope(_ context.Context, p string, c []byte) (int, int, error) {
	if f.scopeOf == nil {
		return 1, len(strings.Split(string(c), "\n")), nil
	}
	a, b := f.scopeOf(p, c)
	return a, b, nil
}
func (f *fakeFacts) CheckWritable(_ context.Context, p string, _ []byte) error {
	if f.protected[p] {
		return errors.New(p + " is protected")
	}
	return nil
}
func (f *fakeFacts) Scope() session.ScopeConfig { return f.scope }
func (f *fakeFacts) Budgets() session.Budgets   { return f.budgets }
func (f *fakeFacts) CurrentProposal(context.Context) (*policy.ProposalFacts, bool, error) {
	if f.proposal == nil {
		return nil, false, nil
	}
	return f.proposal, true, nil
}

func facts() *fakeFacts {
	return &fakeFacts{phase: session.PhaseSelect, eligible: map[string]bool{"example.com/lib@v1.2.4": true}, protected: map[string]bool{"main_test.go": true, "go.mod": true},
		scope: session.ScopeConfig{FilesSoft: 2, FilesHard: 4, LinesSoft: 10, LinesHard: 20}, budgets: session.Budgets{MaxValidationCycles: 3, MaxUnchangedValidations: 2}}
}

func req(tool string, args string) agentrt.ToolRequest {
	se := agentrt.ReadOnly
	switch tool {
	case names.WriteFile, names.ApplyUpgrade, names.Normalize, names.Prepare:
		se = agentrt.LocalMutation
	}
	return agentrt.ToolRequest{RunID: "r", StepID: "s", Spec: agentrt.ToolSpec{Name: tool, SideEffect: se, Timeout: time.Second}, Args: json.RawMessage(args)}
}

func eval(t *testing.T, f *fakeFacts, r agentrt.ToolRequest, view agentrt.RunView) agentrt.PolicyDecision {
	t.Helper()
	d, err := policy.New(f).Evaluate(ctx, r, view)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPhaseGating(t *testing.T) {
	f := facts()
	if d := eval(t, f, req(names.WriteFile, `{"path":"main.go","content":"x"}`), agentrt.RunView{}); d.Outcome != agentrt.Deny || !strings.Contains(d.Reason, "select phase") {
		t.Fatalf("write in select = %+v", d)
	}
	if d := eval(t, f, req(names.Validate, `{}`), agentrt.RunView{}); d.Outcome != agentrt.Deny {
		t.Fatalf("validate in select = %+v", d)
	}
	if d := eval(t, f, req(names.ReadFile, `{"path":"main.go"}`), agentrt.RunView{}); d.Outcome != agentrt.Allow {
		t.Fatalf("read in select = %+v", d)
	}
	f.phase = session.PhaseRepair
	if d := eval(t, f, req(names.ApplyUpgrade, `{"module":"example.com/lib","version":"v1.2.4"}`), agentrt.RunView{}); d.Outcome != agentrt.Deny {
		t.Fatalf("second upgrade in repair = %+v", d)
	}
	if d := eval(t, f, req(names.Blocked, `{"reason":"x"}`), agentrt.RunView{}); d.Outcome != agentrt.Allow {
		t.Fatalf("report_blocked in repair = %+v", d)
	}
}

func TestApplyUpgrade_ExactEligibility(t *testing.T) {
	f := facts()
	if d := eval(t, f, req(names.ApplyUpgrade, `{"module":"example.com/lib","version":"v1.2.4"}`), agentrt.RunView{}); d.Outcome != agentrt.Allow {
		t.Fatalf("eligible = %+v", d)
	}
	for _, args := range []string{`{"module":"example.com/lib","version":"v2.0.0"}`, `{"module":"example.com/other","version":"v1.2.4"}`, `{"module":"example.com/lib","version":"v1.2.4-0.20240101000000-abcdefabcdef"}`} {
		if d := eval(t, f, req(names.ApplyUpgrade, args), agentrt.RunView{}); d.Outcome != agentrt.Deny {
			t.Fatalf("%s = %+v", args, d)
		}
	}
}

func TestWrite_ProtectedDenied(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	for _, p := range []string{"main_test.go", "go.mod"} {
		if d := eval(t, f, req(names.WriteFile, `{"path":"`+p+`","content":"x"}`), agentrt.RunView{}); d.Outcome != agentrt.Deny || !strings.Contains(d.Reason, "protected") {
			t.Fatalf("%s = %+v", p, d)
		}
	}
}

func TestWrite_ScopeBeforeApply(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	write := req(names.WriteFile, `{"path":"a.go","content":"x"}`)
	f.scopeOf = func(string, []byte) (int, int) { return 2, 10 }
	if d := eval(t, f, write, agentrt.RunView{}); d.Outcome != agentrt.Allow {
		t.Fatalf("at soft limit = %+v", d)
	}
	f.scopeOf = func(string, []byte) (int, int) { return 3, 10 }
	d := eval(t, f, write, agentrt.RunView{})
	if d.Outcome != agentrt.RequireApproval || d.Kind != policy.KindScopeExpansion || !strings.Contains(string(d.Presentation), `"projected_files":3`) {
		t.Fatalf("over soft = %+v", d)
	}
	f.scopeOf = func(string, []byte) (int, int) { return 5, 10 }
	if d := eval(t, f, write, agentrt.RunView{}); d.Outcome != agentrt.Abort {
		t.Fatalf("over hard = %+v", d)
	}
	f.scopeOf = func(string, []byte) (int, int) { return 2, 21 }
	if d := eval(t, f, write, agentrt.RunView{}); d.Outcome != agentrt.Abort {
		t.Fatalf("over hard lines = %+v", d)
	}
}

func TestWrite_SingleExpansion(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	write := req(names.WriteFile, `{"path":"a.go","content":"x"}`)
	f.scopeOf = func(string, []byte) (int, int) { return 3, 10 }
	granted := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalApproved}}}
	if d := eval(t, f, write, granted); d.Outcome != agentrt.Allow || !strings.Contains(d.Reason, "expanded") {
		t.Fatalf("granted = %+v", d)
	}
	// Beyond the hard limit even with a grant.
	f.scopeOf = func(string, []byte) (int, int) { return 5, 10 }
	if d := eval(t, f, write, granted); d.Outcome != agentrt.Abort {
		t.Fatalf("granted over hard = %+v", d)
	}
	// A second request after one was already asked for is an abort, whatever its state.
	f.scopeOf = func(string, []byte) (int, int) { return 3, 10 }
	pending := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalPending}}}
	if d := eval(t, f, write, pending); d.Outcome != agentrt.Abort {
		t.Fatalf("second ask = %+v", d)
	}
	// A grant of another kind is not a scope grant.
	other := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: "publication", Status: agentrt.ApprovalApproved}}}
	if d := eval(t, f, write, other); d.Outcome != agentrt.RequireApproval {
		t.Fatalf("other kind treated as scope grant = %+v", d)
	}
}

func validateStep(status agentrt.StepStatus, introduced []string) agentrt.Step {
	content, _ := json.Marshal(map[string]any{"introduced": introduced, "introduced_hash": strings.Join(introduced, ",")})
	return agentrt.Step{Status: status, Decision: &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: names.Validate}, Observation: &agentrt.Observation{Kind: agentrt.ObserveToolResult, Content: content}}
}

func TestValidate_Budgets(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	v := req(names.Validate, `{}`)
	// Cycle budget: three done validations exhaust MaxValidationCycles=3.
	view := agentrt.RunView{Steps: []agentrt.Step{validateStep(agentrt.StepDone, []string{"a"}), validateStep(agentrt.StepDone, []string{"b"}), validateStep(agentrt.StepDone, nil)}}
	if d := eval(t, f, v, view); d.Outcome != agentrt.Abort || !strings.Contains(d.Reason, "budget") {
		t.Fatalf("cycles = %+v", d)
	}
	// Failed validation steps do not count toward the cycle budget.
	view = agentrt.RunView{Steps: []agentrt.Step{validateStep(agentrt.StepFailed, nil), validateStep(agentrt.StepFailed, nil), validateStep(agentrt.StepFailed, nil)}}
	if d := eval(t, f, v, view); d.Outcome != agentrt.Allow {
		t.Fatalf("failed steps counted = %+v", d)
	}
	// No progress: the same introduced set twice in a row (MaxUnchanged=2).
	view = agentrt.RunView{Steps: []agentrt.Step{validateStep(agentrt.StepDone, []string{"a", "b"}), validateStep(agentrt.StepDone, []string{"a", "b"})}}
	if d := eval(t, f, v, view); d.Outcome != agentrt.Abort || !strings.Contains(d.Reason, "no progress") {
		t.Fatalf("unchanged = %+v", d)
	}
	// Changing findings are progress; clean results never count as unchanged.
	view = agentrt.RunView{Steps: []agentrt.Step{validateStep(agentrt.StepDone, []string{"a", "b"}), validateStep(agentrt.StepDone, []string{"a"})}}
	if d := eval(t, f, v, view); d.Outcome != agentrt.Allow {
		t.Fatalf("progress = %+v", d)
	}
	view = agentrt.RunView{Steps: []agentrt.Step{validateStep(agentrt.StepDone, nil), validateStep(agentrt.StepDone, nil)}}
	if d := eval(t, f, v, view); d.Outcome != agentrt.Allow {
		t.Fatalf("clean twice = %+v", d)
	}
}

func TestPublish_RequiresProposalBoundApproval(t *testing.T) {
	f := facts()
	f.phase = session.PhaseProposal
	if d := eval(t, f, req(names.Publish, `{}`), agentrt.RunView{}); d.Outcome != agentrt.Deny {
		t.Fatalf("publish without proposal = %+v", d)
	}
	f.proposal = &policy.ProposalFacts{ID: "p1", Hash: "h1", HeadRef: "repo-steward/lib-v1.2.4", BaseRef: "main", Files: []string{"go.mod"}}
	d := eval(t, f, req(names.Publish, `{}`), agentrt.RunView{})
	if d.Outcome != agentrt.RequireApproval || d.Kind != policy.KindPublication || !strings.Contains(string(d.Capability), `"proposal_hash":"h1"`) || !strings.Contains(string(d.Presentation), "repo-steward/lib-v1.2.4") {
		t.Fatalf("publish = %+v", d)
	}
	// In the proposal phase, edits are over.
	if d := eval(t, f, req(names.WriteFile, `{"path":"a.go","content":"x"}`), agentrt.RunView{}); d.Outcome != agentrt.Deny {
		t.Fatalf("write in proposal phase = %+v", d)
	}
	// Publication is not available before a proposal exists, whatever the phase.
	f.phase = session.PhaseRepair
	if d := eval(t, f, req(names.Publish, `{}`), agentrt.RunView{}); d.Outcome != agentrt.Deny {
		t.Fatalf("publish in repair = %+v", d)
	}
}
