package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/testkit"

	"github.com/joeylking/repo-steward/internal/policy"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/tools"
)

// The policy is tested through agent-runtime's testkit: every case is a
// row the kit puts through the runtime's own argument validation against
// the real tool specs, then hands to the policy with the request the
// runtime built.

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

// allTools is the real tool set, publication included, for the specs the
// runtime validates against. Nothing here calls a tool, so the session is
// empty.
func allTools() []agentrt.Tool {
	return tools.All(&session.Session{Publish: &session.Publication{}})
}

const write = `{"path":"a.go","content":"x"}`

func TestPhaseGating(t *testing.T) {
	f := facts()
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "write in select", Tool: names.WriteFile, Args: `{"path":"main.go","content":"x"}`, Want: agentrt.Deny, Reason: "select phase"},
		{Name: "validate in select", Tool: names.Validate, Args: `{}`, Want: agentrt.Deny},
		{Name: "read in select", Tool: names.ReadFile, Args: `{"path":"main.go"}`, Want: agentrt.Allow},
	})
	f.phase = session.PhaseRepair
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "second upgrade in repair", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib","version":"v1.2.4"}`, Want: agentrt.Deny},
		{Name: "report_blocked in repair", Tool: names.Blocked, Args: `{"reason":"x"}`, Want: agentrt.Allow},
	})
}

func TestApplyUpgrade_ExactEligibility(t *testing.T) {
	testkit.CheckPolicy(t, policy.New(facts()), allTools(), []testkit.PolicyCase{
		{Name: "eligible", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib","version":"v1.2.4"}`, Want: agentrt.Allow},
		{Name: "other version", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib","version":"v2.0.0"}`, Want: agentrt.Deny},
		{Name: "other module", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/other","version":"v1.2.4"}`, Want: agentrt.Deny},
		{Name: "pseudo-version", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib","version":"v1.2.4-0.20240101000000-abcdefabcdef"}`, Want: agentrt.Deny},
		// The runtime refuses these before the policy is asked.
		{Name: "missing version", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib"}`, Want: testkit.Refused},
		{Name: "unknown field", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib","version":"v1.2.4","force":true}`, Want: testkit.Refused},
	})
}

func TestWrite_ProtectedDenied(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "main_test.go", Tool: names.WriteFile, Args: `{"path":"main_test.go","content":"x"}`, Want: agentrt.Deny, Reason: "protected"},
		{Name: "go.mod", Tool: names.WriteFile, Args: `{"path":"go.mod","content":"x"}`, Want: agentrt.Deny, Reason: "protected"},
	})
}

func TestWrite_ScopeBeforeApply(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	for _, tc := range []struct {
		files, lines int
		row          testkit.PolicyCase
	}{
		{2, 10, testkit.PolicyCase{Name: "at soft limit", Want: agentrt.Allow}},
		{3, 10, testkit.PolicyCase{Name: "over soft", Want: agentrt.RequireApproval, Kind: policy.KindScopeExpansion, Presentation: `"projected_files":3`}},
		{5, 10, testkit.PolicyCase{Name: "over hard", Want: agentrt.Abort}},
		{2, 21, testkit.PolicyCase{Name: "over hard lines", Want: agentrt.Abort}},
	} {
		f.scopeOf = func(string, []byte) (int, int) { return tc.files, tc.lines }
		row := tc.row
		row.Tool, row.Args = names.WriteFile, write
		testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{row})
	}
}

func TestWrite_SingleExpansion(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	granted := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalApproved}}}
	pending := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalPending}}}
	other := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: "publication", Status: agentrt.ApprovalApproved}}}
	f.scopeOf = func(string, []byte) (int, int) { return 3, 10 }
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "granted", Tool: names.WriteFile, Args: write, View: granted, Want: agentrt.Allow, Reason: "expanded"},
		// A second request after one was already asked for is an abort,
		// whatever its state.
		{Name: "second ask", Tool: names.WriteFile, Args: write, View: pending, Want: agentrt.Abort},
		// A grant of another kind is not a scope grant.
		{Name: "other kind is no scope grant", Tool: names.WriteFile, Args: write, View: other, Want: agentrt.RequireApproval},
	})
	// Beyond the hard limit even with a grant.
	f.scopeOf = func(string, []byte) (int, int) { return 5, 10 }
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "granted over hard", Tool: names.WriteFile, Args: write, View: granted, Want: agentrt.Abort},
	})
}

func validateStep(status agentrt.StepStatus, introduced []string) agentrt.Step {
	content, _ := json.Marshal(map[string]any{"introduced": introduced, "introduced_hash": strings.Join(introduced, ",")})
	return agentrt.Step{Status: status, Decision: &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: names.Validate}, Observation: &agentrt.Observation{Kind: agentrt.ObserveToolResult, Content: content}}
}

func steps(st ...agentrt.Step) agentrt.RunView { return agentrt.RunView{Steps: st} }

func TestValidate_Budgets(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		// Three done validations exhaust MaxValidationCycles=3.
		{Name: "cycles exhausted", Tool: names.Validate, Args: `{}`, View: steps(validateStep(agentrt.StepDone, []string{"a"}), validateStep(agentrt.StepDone, []string{"b"}), validateStep(agentrt.StepDone, nil)), Want: agentrt.Abort, Reason: "budget"},
		// Failed validation steps do not count toward the cycle budget.
		{Name: "failed steps do not count", Tool: names.Validate, Args: `{}`, View: steps(validateStep(agentrt.StepFailed, nil), validateStep(agentrt.StepFailed, nil), validateStep(agentrt.StepFailed, nil)), Want: agentrt.Allow},
		// The same introduced set twice in a row (MaxUnchanged=2).
		{Name: "no progress", Tool: names.Validate, Args: `{}`, View: steps(validateStep(agentrt.StepDone, []string{"a", "b"}), validateStep(agentrt.StepDone, []string{"a", "b"})), Want: agentrt.Abort, Reason: "no progress"},
		// Changing findings are progress; clean results never count as
		// unchanged.
		{Name: "progress", Tool: names.Validate, Args: `{}`, View: steps(validateStep(agentrt.StepDone, []string{"a", "b"}), validateStep(agentrt.StepDone, []string{"a"})), Want: agentrt.Allow},
		{Name: "clean twice", Tool: names.Validate, Args: `{}`, View: steps(validateStep(agentrt.StepDone, nil), validateStep(agentrt.StepDone, nil)), Want: agentrt.Allow},
	})
}

func TestPublish_RequiresProposalBoundApproval(t *testing.T) {
	f := facts()
	f.phase = session.PhaseProposal
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "publish without proposal", Tool: names.Publish, Args: `{}`, Want: agentrt.Deny},
	})
	f.proposal = &policy.ProposalFacts{ID: "p1", Hash: "h1", HeadRef: "repo-steward/lib-v1.2.4", BaseRef: "main", Files: []string{"go.mod"}}
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "publish", Tool: names.Publish, Args: `{}`, Want: agentrt.RequireApproval, Kind: policy.KindPublication, Capability: `"proposal_hash":"h1"`, Presentation: "repo-steward/lib-v1.2.4"},
		// In the proposal phase, edits are over.
		{Name: "write in proposal phase", Tool: names.WriteFile, Args: write, Want: agentrt.Deny},
	})
	// Publication is not available before a proposal exists, whatever the
	// phase.
	f.phase = session.PhaseRepair
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "publish in repair", Tool: names.Publish, Args: `{}`, Want: agentrt.Deny},
	})
}

// The kit's Never generates arguments that pass the runtime's validation
// for every tool of a class and asserts the policy never gives an outcome,
// under every grant the run can hold.
func TestNever(t *testing.T) {
	views := []agentrt.RunView{
		{},
		{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalApproved}}},
		{Approvals: []agentrt.Approval{{Kind: policy.KindPublication, Status: agentrt.ApprovalApproved}}},
	}
	withDestructive := append(allTools(), testkit.StandIn("delete_path", agentrt.Destructive))
	for _, phase := range []string{session.PhaseSelect, session.PhaseRepair, session.PhaseProposal} {
		t.Run(phase, func(t *testing.T) {
			f := facts()
			f.phase = phase
			f.proposal = &policy.ProposalFacts{ID: "p1", Hash: "h1", HeadRef: "repo-steward/lib-v1.2.4", BaseRef: "main", Files: []string{"go.mod"}}
			p := policy.New(f)
			// Nothing read-only is ever refused or held for approval in the
			// repair phase. Elsewhere run_validation, which is read-only, is
			// gated by phase (TestPhaseGating).
			if phase == session.PhaseRepair {
				for _, outcome := range []agentrt.PolicyOutcome{agentrt.Deny, agentrt.Abort, agentrt.RequireApproval} {
					testkit.Never(t, p, allTools(), agentrt.ReadOnly, outcome, testkit.Fuzz{Count: 16}, views...)
				}
			}
			// Nothing destructive is ever allowed or offered for approval.
			for _, outcome := range []agentrt.PolicyOutcome{agentrt.Allow, agentrt.RequireApproval} {
				testkit.Never(t, p, withDestructive, agentrt.Destructive, outcome, testkit.Fuzz{Count: 16}, views...)
			}
			// Publishing is never allowed without an approval of its own.
			testkit.Never(t, p, allTools(), agentrt.RemoteMutation, agentrt.Allow, testkit.Fuzz{Count: 16}, views...)
		})
	}
}
