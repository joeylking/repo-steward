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
	files     map[string]string
	scopeOf   func(path string, content []byte) (int, int)
	scope     session.ScopeConfig
	budgets   session.Budgets
	proposal  *policy.ProposalFacts
	// ask is the -ask-unexercised flag; unexercised what the current tree
	// lacks, nil when its changed source is verified.
	ask         bool
	unexercised *policy.UnexercisedFacts
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

// ProjectWrite projects a call with the tools' own projection over an
// in-memory tree, so edit_file's matching is the real one; the path rule
// stands in for the session's.
func (f *fakeFacts) ProjectWrite(_ context.Context, tool string, args json.RawMessage) (string, []byte, error) {
	p, content, err := tools.Project(tool, args, func(p string) ([]byte, error) {
		if f.protected[p] {
			return nil, errors.New(p + " is protected")
		}
		c, ok := f.files[p]
		if !ok {
			return nil, errors.New(p + " does not exist")
		}
		return []byte(c), nil
	})
	if err != nil {
		return "", nil, err
	}
	if f.protected[p] {
		return "", nil, errors.New(p + " is protected")
	}
	return p, content, nil
}
func (f *fakeFacts) Scope() session.ScopeConfig { return f.scope }
func (f *fakeFacts) Budgets() session.Budgets   { return f.budgets }
func (f *fakeFacts) CurrentProposal(context.Context) (*policy.ProposalFacts, bool, error) {
	if f.proposal == nil {
		return nil, false, nil
	}
	return f.proposal, true, nil
}

func (f *fakeFacts) AskUnexercised() bool { return f.ask }
func (f *fakeFacts) Unexercised(context.Context) (*policy.UnexercisedFacts, bool, error) {
	return f.unexercised, f.unexercised != nil, nil
}

func facts() *fakeFacts {
	return &fakeFacts{phase: session.PhaseSelect, eligible: map[string]bool{"example.com/lib@v1.2.4": true}, protected: map[string]bool{"main_test.go": true, "go.mod": true},
		files: map[string]string{"a.go": "package a\n\nvar x = 1\n", "twice.go": "f()\nf()\n", "main_test.go": "package main\n", "go.mod": "module m\n"},
		scope: session.ScopeConfig{FilesSoft: 2, FilesHard: 4, LinesSoft: 10, LinesHard: 20}, budgets: session.Budgets{MaxValidationCycles: 3, MaxUnchangedValidations: 2}}
}

// allTools is the real tool set, publication included, for the specs the
// runtime validates against. Nothing here calls a tool, so the session is
// empty.
func allTools() []agentrt.Tool {
	return tools.All(&session.Session{Publish: &session.Publication{}})
}

const write = `{"path":"a.go","content":"x"}`

// edit is write's counterpart through edit_file: one exact occurrence.
const edit = `{"path":"a.go","old_text":"x = 1","new_text":"x = 2"}`

// writes are the two tools that change source files; every write rule is
// checked for both.
var writes = []struct{ tool, args string }{{names.WriteFile, write}, {names.EditFile, edit}}

func TestPhaseGating(t *testing.T) {
	f := facts()
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "write in select", Tool: names.WriteFile, Args: `{"path":"main.go","content":"x"}`, Want: agentrt.Deny, Reason: "select phase"},
		{Name: "edit in select", Tool: names.EditFile, Args: edit, Want: agentrt.Deny, Reason: "select phase"},
		{Name: "validate in select", Tool: names.Validate, Args: `{}`, Want: agentrt.Deny},
		{Name: "read in select", Tool: names.ReadFile, Args: `{"path":"main.go"}`, Want: agentrt.Allow},
	})
	f.phase = session.PhaseRepair
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "second upgrade in repair", Tool: names.ApplyUpgrade, Args: `{"module":"example.com/lib","version":"v1.2.4"}`, Want: agentrt.Deny},
		{Name: "report_blocked in repair", Tool: names.Blocked, Args: `{"reason":"x"}`, Want: agentrt.Allow},
		{Name: "write in repair", Tool: names.WriteFile, Args: write, Want: agentrt.Allow},
		{Name: "edit in repair", Tool: names.EditFile, Args: edit, Want: agentrt.Allow},
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
		{Name: "edit main_test.go", Tool: names.EditFile, Args: `{"path":"main_test.go","old_text":"package main","new_text":"package x"}`, Want: agentrt.Deny, Reason: "protected"},
		{Name: "edit go.mod", Tool: names.EditFile, Args: `{"path":"go.mod","old_text":"module m","new_text":"module n"}`, Want: agentrt.Deny, Reason: "protected"},
		// A protected path is refused as protected even when the text does
		// not occur in it.
		{Name: "edit main_test.go, absent text", Tool: names.EditFile, Args: `{"path":"main_test.go","old_text":"nowhere","new_text":"x"}`, Want: agentrt.Deny, Reason: "protected"},
	})
}

// edit_file is allowed only when old_text occurs exactly once; anything
// else is denied with what was found, and nothing is projected.
func TestEdit_ExactlyOneOccurrence(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "once", Tool: names.EditFile, Args: edit, Want: agentrt.Allow},
		{Name: "absent", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"y = 1","new_text":"y = 2"}`, Want: agentrt.Deny, Reason: "occurs 0 times in a.go"},
		{Name: "twice", Tool: names.EditFile, Args: `{"path":"twice.go","old_text":"f()","new_text":"g()"}`, Want: agentrt.Deny, Reason: "occurs 2 times in twice.go (at lines 1, 2)"},
		{Name: "overlapping", Tool: names.EditFile, Args: `{"path":"twice.go","old_text":"f()\nf","new_text":"g"}`, Want: agentrt.Allow},
		{Name: "line numbers copied", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"     3\tvar x = 1","new_text":"var x = 2"}`, Want: agentrt.Deny, Reason: "line numbers"},
		{Name: "whitespace differs", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"var  x =  1","new_text":"var x = 2"}`, Want: agentrt.Deny, Reason: "whitespace"},
		{Name: "missing file", Tool: names.EditFile, Args: `{"path":"nope.go","old_text":"a","new_text":"b"}`, Want: agentrt.Deny, Reason: "does not exist"},
		{Name: "empty old_text", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"","new_text":"b"}`, Want: agentrt.Deny, Reason: "empty"},
		{Name: "no change", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"x = 1","new_text":"x = 1"}`, Want: agentrt.Deny, Reason: "same"},
		// The runtime refuses these before the policy is asked.
		{Name: "missing new_text", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"x"}`, Want: testkit.Refused},
		{Name: "unknown field", Tool: names.EditFile, Args: `{"path":"a.go","old_text":"x","new_text":"y","all":true}`, Want: testkit.Refused},
	})
}

// The scope is computed on the content an edit would leave, not on the
// arguments: the projection the policy sizes is the whole file after the
// replacement.
func TestEdit_ScopeSeesProjectedContent(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	var seen string
	f.scopeOf = func(p string, c []byte) (int, int) { seen = p + ":" + string(c); return 1, 1 }
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{{Name: "edit", Tool: names.EditFile, Args: edit, Want: agentrt.Allow}})
	if want := "a.go:package a\n\nvar x = 2\n"; seen != want {
		t.Fatalf("scope saw %q, want %q", seen, want)
	}
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
		for _, w := range writes {
			row := tc.row
			row.Name = w.tool + ": " + row.Name
			row.Tool, row.Args = w.tool, w.args
			testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{row})
		}
	}
}

func TestWrite_SingleExpansion(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	granted := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalApproved}}}
	pending := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalPending}}}
	other := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: "publication", Status: agentrt.ApprovalApproved}}}
	for _, w := range writes {
		f.scopeOf = func(string, []byte) (int, int) { return 3, 10 }
		testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
			{Name: w.tool + " granted", Tool: w.tool, Args: w.args, View: granted, Want: agentrt.Allow, Reason: "expanded"},
			// A second request after one was already asked for is an abort,
			// whatever its state.
			{Name: w.tool + " second ask", Tool: w.tool, Args: w.args, View: pending, Want: agentrt.Abort},
			// A grant of another kind is not a scope grant.
			{Name: w.tool + " other kind is no scope grant", Tool: w.tool, Args: w.args, View: other, Want: agentrt.RequireApproval, Presentation: `"tool":"` + w.tool + `"`},
		})
		// Beyond the hard limit even with a grant.
		f.scopeOf = func(string, []byte) (int, int) { return 5, 10 }
		testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
			{Name: w.tool + " granted over hard", Tool: w.tool, Args: w.args, View: granted, Want: agentrt.Abort},
		})
	}
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
		{Name: "edit in proposal phase", Tool: names.EditFile, Args: edit, Want: agentrt.Deny},
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
			// Outside the repair phase no source file is ever written, by
			// either write tool, whatever the arguments or grants.
			if phase != session.PhaseRepair {
				for _, outcome := range []agentrt.PolicyOutcome{agentrt.Allow, agentrt.RequireApproval} {
					testkit.Never(t, p, writeTools(), agentrt.LocalMutation, outcome, testkit.Fuzz{Count: 32}, views...)
				}
			}
		})
	}
}

// unexercised is a candidate tree whose changed line no test executes.
func unexercised(tree string) *policy.UnexercisedFacts {
	return &policy.UnexercisedFacts{Tree: tree, Unexercised: []string{"main.go:16 (no test executes it)"}, Blocks: 2, Executed: 1,
		Diff: "--- a/main.go\n+++ b/main.go\n@@ -16 +16 @@\n-\tfmt.Println(greeting(\"steward\"))\n+\tfmt.Println(greeting(\"x\"))\n"}
}

const prepare = `{"title":"t","summary":"s"}`

// A repair no test exercises: without -ask-unexercised prepare_proposal
// runs and readiness refuses the tree (TestCheck_* in internal/coverage,
// TestScripted_S2U_UnexercisedRepairIsNotReady); with it, the request is
// held for an approval whose capability names the tree, so an approval of
// one tree can never run the request on another.
func TestPrepare_UnexercisedRepair(t *testing.T) {
	f := facts()
	f.phase = session.PhaseRepair
	f.unexercised = unexercised("t1")
	granted := agentrt.RunView{Approvals: []agentrt.Approval{{Kind: policy.KindUnexercisedRepair, Status: agentrt.ApprovalApproved}}}
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "flag off", Tool: names.Prepare, Args: prepare, Want: agentrt.Allow},
	})
	f.ask = true
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "flag on", Tool: names.Prepare, Args: prepare, Want: agentrt.RequireApproval, Kind: policy.KindUnexercisedRepair, Capability: `"tree":"t1"`, Presentation: `main.go:16 (no test executes it)`},
		{Name: "flag on, diff shown", Tool: names.Prepare, Args: prepare, Want: agentrt.RequireApproval, Kind: policy.KindUnexercisedRepair, Presentation: `greeting(\"x\")`},
		// A grant is matched by the runtime against the approval's hash;
		// the policy itself never allows the request.
		{Name: "flag on, granted", Tool: names.Prepare, Args: prepare, View: granted, Want: agentrt.RequireApproval, Kind: policy.KindUnexercisedRepair},
	})
	f.unexercised = unexercised("t2")
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "another tree", Tool: names.Prepare, Args: prepare, Want: agentrt.RequireApproval, Kind: policy.KindUnexercisedRepair, Capability: `"tree":"t2"`},
	})
	// Verified, or nothing to approve: readiness decides.
	f.unexercised = nil
	testkit.CheckPolicy(t, policy.New(f), allTools(), []testkit.PolicyCase{
		{Name: "flag on, exercised", Tool: names.Prepare, Args: prepare, Want: agentrt.Allow},
	})
}

// With -ask-unexercised and a tree whose changed source no test exercises,
// prepare_proposal, the only tool that freezes a proposal, is never allowed
// by the policy under any arguments or grants, the approval of its own kind
// included; and nothing is ever published without a publication approval.
func TestNever_FreezesAnUnexercisedRepair(t *testing.T) {
	views := []agentrt.RunView{
		{},
		{Approvals: []agentrt.Approval{{Kind: policy.KindUnexercisedRepair, Status: agentrt.ApprovalApproved}}},
		{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalApproved}}},
		{Approvals: []agentrt.Approval{{Kind: policy.KindPublication, Status: agentrt.ApprovalApproved}}},
	}
	f := facts()
	f.phase = session.PhaseRepair
	f.ask = true
	f.unexercised = unexercised("t1")
	p := policy.New(f)
	testkit.Never(t, p, prepareTools(), agentrt.LocalMutation, agentrt.Allow, testkit.Fuzz{Count: 32}, views...)
	testkit.Never(t, p, allTools(), agentrt.RemoteMutation, agentrt.Allow, testkit.Fuzz{Count: 16}, views...)
}

// prepareTools is prepare_proposal alone, so that Never's local-mutation
// class means exactly freezing a proposal.
func prepareTools() []agentrt.Tool {
	var out []agentrt.Tool
	for _, tl := range allTools() {
		if tl.Spec().Name == names.Prepare {
			out = append(out, tl)
		}
	}
	return out
}

// writeTools are write_file and edit_file alone, so that Never's
// local-mutation class means exactly the source-file writes.
func writeTools() []agentrt.Tool {
	var out []agentrt.Tool
	for _, tl := range allTools() {
		if n := tl.Spec().Name; n == names.WriteFile || n == names.EditFile {
			out = append(out, tl)
		}
	}
	return out
}

// In the repair phase, a write is never allowed or offered for approval
// when every path is protected, or when every projection is beyond the hard
// scope limit, whatever the arguments and whatever the run has been
// granted. Generated edit_file arguments mostly name no file or no
// occurrence; the rows above cover the ones that do.
func TestNever_WritesPastTheRules(t *testing.T) {
	views := []agentrt.RunView{
		{},
		{Approvals: []agentrt.Approval{{Kind: policy.KindScopeExpansion, Status: agentrt.ApprovalApproved}}},
	}
	allProtected := facts()
	allProtected.phase = session.PhaseRepair
	allProtected.files = map[string]string{}
	beyondHard := facts()
	beyondHard.phase = session.PhaseRepair
	beyondHard.scopeOf = func(string, []byte) (int, int) { return 5, 21 }
	for name, f := range map[string]*fakeFacts{"protected": allProtected, "beyond hard scope": beyondHard} {
		t.Run(name, func(t *testing.T) {
			p := policy.New(&protectAll{f, name == "protected"})
			for _, outcome := range []agentrt.PolicyOutcome{agentrt.Allow, agentrt.RequireApproval} {
				testkit.Never(t, p, writeTools(), agentrt.LocalMutation, outcome, testkit.Fuzz{Count: 32}, views...)
			}
		})
	}
}

// protectAll protects every path when on, through the same projection.
type protectAll struct {
	*fakeFacts
	on bool
}

func (p *protectAll) ProjectWrite(ctx context.Context, tool string, args json.RawMessage) (string, []byte, error) {
	path, content, err := p.fakeFacts.ProjectWrite(ctx, tool, args)
	if err == nil && p.on {
		return "", nil, errors.New(path + " is protected")
	}
	return path, content, err
}
