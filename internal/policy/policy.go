// Package policy is repo-steward's tool policy. It is evaluated by the
// runtime before every tool call and has no model input. It encodes the
// run phase, dependency eligibility, protected paths, scope limits with a
// single approvable expansion, and the repair budgets.
package policy

import (
	"context"
	"encoding/json"
	"fmt"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/tools"
)

// KindScopeExpansion is the approval kind for raising soft scope limits.
const KindScopeExpansion = "scope_expansion"

// KindPublication is the approval kind for pushing and opening a pull
// request. It grants exactly one publish of one frozen proposal.
const KindPublication = "publication"

// KindUnexercisedRepair is the approval kind for preparing a proposal whose
// source change no test exercises. It is asked for only when the run was
// started with -ask-unexercised, and it is bound to one candidate tree.
const KindUnexercisedRepair = names.KindUnexercisedRepair

// maxApprovalDiff bounds the patch an unexercised-repair approval shows.
const maxApprovalDiff = 32 << 10

// Facts is what the policy needs from the session. It is an interface so
// the policy can be tested without a workspace or engine.
type Facts interface {
	Phase(ctx context.Context) (string, error)
	EligibleTarget(module, version string) (bool, []string)
	ProjectedScope(ctx context.Context, path string, content []byte) (files, lines int, err error)
	// ProjectWrite applies every write rule to a write_file or edit_file
	// call and returns the cleaned path and the full content the file
	// would have, or why the call cannot be made.
	ProjectWrite(ctx context.Context, tool string, args json.RawMessage) (path string, content []byte, err error)
	Scope() session.ScopeConfig
	Budgets() session.Budgets
	// CurrentProposal describes the frozen proposal, if one exists.
	CurrentProposal(ctx context.Context) (*ProposalFacts, bool, error)
	// AskUnexercised reports whether a repair no test exercises pauses for
	// approval instead of failing readiness.
	AskUnexercised() bool
	// Unexercised describes the current candidate tree when it has a clean
	// bound validation and changed source the tests do not verifiably
	// execute; ok is false otherwise.
	Unexercised(ctx context.Context) (*UnexercisedFacts, bool, error)
}

// UnexercisedFacts is what an unexercised-repair approval presents: the
// tree it is bound to, what no test executes, and the patch of the files
// concerned. The patch is model-written text; the command line prints it
// escaped, like every presentation.
type UnexercisedFacts struct {
	Tree          string   `json:"tree"`
	Unexercised   []string `json:"unexercised"`
	Blocks        int      `json:"blocks"`
	Executed      int      `json:"executed"`
	Diff          string   `json:"diff"`
	DiffTruncated bool     `json:"diff_truncated,omitempty"`
}

// ProposalFacts is what the publication approval presents and binds.
type ProposalFacts struct {
	ID         string   `json:"id"`
	Hash       string   `json:"hash"`
	Title      string   `json:"title"`
	HeadRef    string   `json:"head_ref"`
	HeadCommit string   `json:"head_commit"`
	BaseRef    string   `json:"base_ref"`
	Files      []string `json:"files"`
	Target     string   `json:"target"`
}

// Steward is the policy.
type Steward struct {
	Facts Facts
	base  agentrt.SideEffectPolicy
}

// New returns the policy over the facts.
func New(f Facts) *Steward {
	return &Steward{Facts: f, base: agentrt.DefaultPolicy()}
}

var phaseTools = map[string]map[string]bool{
	session.PhaseSelect:   set(names.ReadOnly, names.ApplyUpgrade, names.Blocked),
	session.PhaseRepair:   set(names.ReadOnly, names.WriteFile, names.EditFile, names.Normalize, names.Validate, names.Prepare, names.Blocked),
	session.PhaseProposal: set(names.ReadOnly, names.Publish, names.Blocked),
}

func set(base []string, more ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range base {
		m[n] = true
	}
	for _, n := range more {
		m[n] = true
	}
	return m
}

func deny(format string, args ...any) agentrt.PolicyDecision {
	return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: fmt.Sprintf(format, args...)}
}

func abort(format string, args ...any) agentrt.PolicyDecision {
	return agentrt.PolicyDecision{Outcome: agentrt.Abort, Reason: fmt.Sprintf(format, args...)}
}

// Evaluate implements agentrt.Policy.
func (p *Steward) Evaluate(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	phase, err := p.Facts.Phase(ctx)
	if err != nil {
		return agentrt.PolicyDecision{}, err
	}
	if !phaseTools[phase][req.Spec.Name] {
		return deny("%s is not available in the %s phase", req.Spec.Name, phase), nil
	}
	switch req.Spec.Name {
	case names.ApplyUpgrade:
		var a struct{ Module, Version string }
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return deny("invalid arguments: %v", err), nil
		}
		if ok, reasons := p.Facts.EligibleTarget(a.Module, a.Version); !ok {
			return deny("%s@%s is not an eligible target: %v", a.Module, a.Version, reasons), nil
		}
		return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "eligible target in the select phase"}, nil
	case names.WriteFile, names.EditFile:
		return p.evaluateWrite(ctx, req, view)
	case names.Validate:
		return p.evaluateValidate(view)
	case names.Publish:
		return p.evaluatePublish(ctx)
	case names.Prepare:
		return p.evaluatePrepare(ctx, req, view)
	}
	return p.base.Evaluate(ctx, req, view)
}

// evaluatePublish always requires a publication approval whose capability
// and presentation name the frozen proposal and its hash. A different
// proposal, or a changed one, produces a different approval hash, so an
// old approval can never authorize it.
func (p *Steward) evaluatePublish(ctx context.Context) (agentrt.PolicyDecision, error) {
	facts, ok, err := p.Facts.CurrentProposal(ctx)
	if err != nil {
		return agentrt.PolicyDecision{}, err
	}
	if !ok {
		return deny("no frozen proposal to publish"), nil
	}
	cap := map[string]any{"tool": names.Publish, "proposal_id": facts.ID, "proposal_hash": facts.Hash}
	reason := fmt.Sprintf("publishing proposal %s pushes %s and opens a pull request against %s", facts.ID, facts.HeadRef, facts.BaseRef)
	return agentrt.NeedApproval(KindPublication, reason, cap, facts)
}

// evaluatePrepare holds prepare_proposal for an operator when the run asks
// about unexercised repairs and the candidate tree's changed source is not
// verifiably executed by the tests. It never allows such a request itself:
// only the runtime's match of a granted approval's hash, which binds the
// tree, runs it. Without the flag the request is allowed and readiness
// refuses the tree.
func (p *Steward) evaluatePrepare(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	if !p.Facts.AskUnexercised() {
		return p.base.Evaluate(ctx, req, view)
	}
	u, ok, err := p.Facts.Unexercised(ctx)
	if err != nil {
		return agentrt.PolicyDecision{}, err
	}
	if !ok {
		return p.base.Evaluate(ctx, req, view)
	}
	cap := map[string]any{"tool": names.Prepare, "tree": u.Tree, "unexercised": u.Unexercised}
	reason := fmt.Sprintf("no test exercises part of this repair (%d of %d required coverage blocks executed); preparing a proposal of tree %s needs an approval without test coverage", u.Executed, u.Blocks, u.Tree)
	return agentrt.NeedApproval(KindUnexercisedRepair, reason, cap, u)
}

// evaluateWrite decides write_file and edit_file alike, on the content
// the call would leave: every path rule, then the scope of the diff that
// content would produce, before anything is written.
func (p *Steward) evaluateWrite(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	path, content, err := p.Facts.ProjectWrite(ctx, req.Spec.Name, req.Args)
	if err != nil {
		return deny("%v", err), nil
	}
	files, lines, err := p.Facts.ProjectedScope(ctx, path, content)
	if err != nil {
		return agentrt.PolicyDecision{}, err
	}
	sc := p.Facts.Scope()
	if files > sc.FilesHard || lines > sc.LinesHard {
		return abort("hard scope limit: the write would leave %d source files and %d lines changed (limits %d files, %d lines)", files, lines, sc.FilesHard, sc.LinesHard), nil
	}
	granted := scopeGranted(view)
	if files > sc.FilesSoft || lines > sc.LinesSoft {
		if granted {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "within the approved expanded scope"}, nil
		}
		if scopeRequested(view) {
			// The one expansion was already used or refused: no second ask.
			return abort("soft scope limit crossed again after an expansion was already requested"), nil
		}
		cap := map[string]any{"limit": "scope", "files_soft": sc.FilesHard, "lines_soft": sc.LinesHard}
		pres := map[string]any{"tool": req.Spec.Name, "path": path, "projected_files": files, "projected_lines": lines, "soft": map[string]int{"files": sc.FilesSoft, "lines": sc.LinesSoft}, "hard": map[string]int{"files": sc.FilesHard, "lines": sc.LinesHard}}
		reason := fmt.Sprintf("the write would leave %d source files and %d lines changed, beyond the soft limits of %d files and %d lines", files, lines, sc.FilesSoft, sc.LinesSoft)
		return agentrt.NeedApproval(KindScopeExpansion, reason, cap, pres)
	}
	return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "within scope"}, nil
}

func (p *Steward) evaluateValidate(view agentrt.RunView) (agentrt.PolicyDecision, error) {
	b := p.Facts.Budgets()
	cycles := 0
	var hashes []string
	for _, st := range view.Steps {
		if st.Decision == nil || st.Decision.Tool != names.Validate || st.Status != agentrt.StepDone || st.Observation == nil {
			continue
		}
		cycles++
		var obs struct {
			IntroducedHash string `json:"introduced_hash"`
			Introduced     []string
		}
		json.Unmarshal(st.Observation.Content, &obs)
		if len(obs.Introduced) > 0 {
			hashes = append(hashes, obs.IntroducedHash)
		} else {
			hashes = append(hashes, "")
		}
	}
	if b.MaxValidationCycles > 0 && cycles >= b.MaxValidationCycles {
		return abort("validation budget exhausted: %d cycles", cycles), nil
	}
	if b.MaxUnchangedValidations > 0 && len(hashes) >= b.MaxUnchangedValidations {
		tail := hashes[len(hashes)-b.MaxUnchangedValidations:]
		same := tail[0] != ""
		for _, h := range tail[1:] {
			if h != tail[0] {
				same = false
			}
		}
		if same {
			return abort("no progress: the same introduced findings %d times in a row", b.MaxUnchangedValidations), nil
		}
	}
	return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "within the repair budget"}, nil
}

func scopeGranted(view agentrt.RunView) bool {
	for _, a := range view.Approvals {
		if a.Kind == KindScopeExpansion && a.Status == agentrt.ApprovalApproved {
			return true
		}
	}
	return false
}

func scopeRequested(view agentrt.RunView) bool {
	for _, a := range view.Approvals {
		if a.Kind == KindScopeExpansion {
			return true
		}
	}
	return false
}

// SessionFacts adapts a session to Facts.
type SessionFacts struct{ S *session.Session }

func (f SessionFacts) Phase(ctx context.Context) (string, error) { return f.S.Phase(ctx) }
func (f SessionFacts) EligibleTarget(m, v string) (bool, []string) {
	return f.S.EligibleTarget(m, v)
}
func (f SessionFacts) ProjectedScope(ctx context.Context, p string, c []byte) (int, int, error) {
	return f.S.ProjectedScope(ctx, p, c)
}
func (f SessionFacts) ProjectWrite(ctx context.Context, tool string, args json.RawMessage) (string, []byte, error) {
	return tools.ProjectWrite(ctx, f.S, tool, args)
}
func (f SessionFacts) Scope() session.ScopeConfig { return f.S.Scope }
func (f SessionFacts) AskUnexercised() bool       { return f.S.AskUnexercised }
func (f SessionFacts) Unexercised(ctx context.Context) (*UnexercisedFacts, bool, error) {
	tree, v, ok, err := f.S.Unexercised(ctx)
	if err != nil || !ok {
		return nil, false, err
	}
	u := &UnexercisedFacts{Tree: tree, Unexercised: proposal.Unexercised(v), Blocks: v.Blocks, Executed: v.Executed}
	var paths []string
	seen := map[string]bool{}
	for _, r := range v.Unverified {
		if r.Path != "" && !seen[r.Path] {
			seen[r.Path] = true
			paths = append(paths, r.Path)
		}
	}
	if len(paths) == 0 {
		// The verdict names no file (the evidence itself was unusable):
		// show every changed source file.
		cs, err := f.S.WS.Diff(ctx, tree)
		if err != nil {
			return nil, false, err
		}
		for _, fc := range cs.Files {
			if fc.Path != "go.mod" && fc.Path != "go.sum" {
				paths = append(paths, fc.Path)
			}
		}
	}
	if u.Diff, u.DiffTruncated, err = f.S.DiffOf(ctx, tree, paths, maxApprovalDiff); err != nil {
		return nil, false, err
	}
	return u, true, nil
}
func (f SessionFacts) Budgets() session.Budgets { return f.S.Budgets }
func (f SessionFacts) CurrentProposal(ctx context.Context) (*ProposalFacts, bool, error) {
	rec, ok, err := f.S.CurrentProposal(ctx)
	if err != nil || !ok {
		return nil, ok, err
	}
	var p struct {
		Title      string `json:"title"`
		HeadRef    string `json:"head_ref"`
		HeadCommit string `json:"head_commit"`
		BaseRef    string `json:"base_ref"`
		Target     struct{ Module, Version string }
		Files      []struct{ Path string }
	}
	if err := json.Unmarshal(rec.Proposal, &p); err != nil {
		return nil, false, err
	}
	pf := &ProposalFacts{ID: rec.ID, Hash: rec.Hash, Title: p.Title, HeadRef: p.HeadRef, HeadCommit: p.HeadCommit, BaseRef: p.BaseRef, Target: p.Target.Module + "@" + p.Target.Version}
	for _, f := range p.Files {
		pf.Files = append(pf.Files, f.Path)
	}
	return pf, true, nil
}
