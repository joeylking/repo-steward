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

	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/tools"
)

// KindScopeExpansion is the approval kind for raising soft scope limits.
const KindScopeExpansion = "scope_expansion"

// KindPublication is the approval kind for pushing and opening a pull
// request. It grants exactly one publish of one frozen proposal.
const KindPublication = "publication"

// Facts is what the policy needs from the session. It is an interface so
// the policy can be tested without a workspace or engine.
type Facts interface {
	Phase(ctx context.Context) (string, error)
	EligibleTarget(module, version string) (bool, []string)
	ProjectedScope(ctx context.Context, path string, content []byte) (files, lines int, err error)
	CheckWritable(ctx context.Context, path string, content []byte) error
	Scope() session.ScopeConfig
	Budgets() session.Budgets
	// CurrentProposal describes the frozen proposal, if one exists.
	CurrentProposal(ctx context.Context) (*ProposalFacts, bool, error)
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
	session.PhaseRepair:   set(names.ReadOnly, names.WriteFile, names.Normalize, names.Validate, names.Prepare, names.Blocked),
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
	case names.WriteFile:
		return p.evaluateWrite(ctx, req, view)
	case names.Validate:
		return p.evaluateValidate(view)
	case names.Publish:
		return p.evaluatePublish(ctx)
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
	return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: KindPublication,
		Reason:     fmt.Sprintf("publishing proposal %s pushes %s and opens a pull request against %s", facts.ID, facts.HeadRef, facts.BaseRef),
		Capability: mustJSON(cap), Presentation: mustJSON(facts)}, nil
}

func (p *Steward) evaluateWrite(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	var a struct{ Path, Content string }
	if err := json.Unmarshal(req.Args, &a); err != nil {
		return deny("invalid arguments: %v", err), nil
	}
	if err := p.Facts.CheckWritable(ctx, a.Path, []byte(a.Content)); err != nil {
		return deny("%v", err), nil
	}
	files, lines, err := p.Facts.ProjectedScope(ctx, a.Path, []byte(a.Content))
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
		pres := map[string]any{"path": a.Path, "projected_files": files, "projected_lines": lines, "soft": map[string]int{"files": sc.FilesSoft, "lines": sc.LinesSoft}, "hard": map[string]int{"files": sc.FilesHard, "lines": sc.LinesHard}}
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: KindScopeExpansion, Reason: fmt.Sprintf("the write would leave %d source files and %d lines changed, beyond the soft limits of %d files and %d lines", files, lines, sc.FilesSoft, sc.LinesSoft), Capability: mustJSON(cap), Presentation: mustJSON(pres)}, nil
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

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
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
func (f SessionFacts) CheckWritable(ctx context.Context, p string, c []byte) error {
	_, err := tools.CheckWritable(ctx, f.S, p, c)
	return err
}
func (f SessionFacts) Scope() session.ScopeConfig { return f.S.Scope }
func (f SessionFacts) Budgets() session.Budgets   { return f.S.Budgets }
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
