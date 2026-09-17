package steward

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/lock"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/policy"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/scenario"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/tools"
)

// Agent-mode outcomes in addition to the baseline ones.
const (
	OutcomeBlocked              = "blocked"
	OutcomeAwaitingApproval     = "awaiting_approval"
	OutcomeScopeExceeded        = "scope_exceeded"
	OutcomeBudgetExhausted      = "repair_budget_exhausted"
	OutcomeLimitExhausted       = "limit_exhausted"
	OutcomeLoopDetected         = "loop_detected"
	OutcomeEndedWithoutProposal = "ended_without_proposal"
	OutcomeApprovalRejected     = "approval_rejected"
	OutcomeFailed               = "failed"
)

// RunInfo summarizes the runtime run in the result.
type RunInfo struct {
	Status       agentrt.RunStatus      `json:"status"`
	Reason       agentrt.TerminalReason `json:"reason,omitempty"`
	ReasonDetail string                 `json:"reason_detail,omitempty"`
	Steps        int                    `json:"steps"`
	Tools        []string               `json:"tools"`
	Approvals    []agentrt.Approval     `json:"approvals,omitempty"`
}

// RunScripted executes a scenario's decisions through the runtime with the
// full tool set and policy. It proves the control path; it does not
// measure model quality.
func RunScripted(ctx context.Context, opts Options, scenarioName string) (*Result, error) {
	sc, err := scenario.Load(scenarioName)
	if err != nil {
		return nil, err
	}
	return runAgent(ctx, opts, "scripted:"+scenarioName, &scenario.Agent{Scenario: sc})
}

func runAgent(ctx context.Context, opts Options, mode string, agent agentrt.Agent) (*Result, error) {
	if err := applyDefaults(&opts); err != nil {
		return nil, err
	}
	exec, err := lock.Acquire(filepath.Join(opts.DataDir, "executor.lock"))
	if err != nil {
		return nil, err
	}
	defer exec.Release()

	r := &run{opts: opts, id: newID(), dataDir: opts.DataDir, start: time.Now()}
	r.runDir = filepath.Join(opts.DataDir, "runs", r.id)
	r.res = &Result{RunID: r.id, Mode: mode, Timings: map[string]int64{}}
	runLock, err := lock.Acquire(filepath.Join(r.runDir, "run.lock"))
	if err != nil {
		return nil, err
	}
	defer runLock.Release()
	dbPath := filepath.Join(opts.DataDir, "steward.db")
	if r.store, err = task.Open(dbPath); err != nil {
		return nil, err
	}
	defer r.store.Close()

	outcome, err := r.prelude(ctx, mode)
	if err != nil {
		r.store.FinishTask(ctx, r.id, OutcomeFailed, map[string]any{"error": err.Error()})
		return r.res, err
	}
	if outcome != "" {
		r.res.Outcome = outcome
		r.mark("total", r.start)
		return r.res, r.store.FinishTask(ctx, r.id, outcome, r.res.Detail)
	}
	defer removeAll(r.buildCache)

	rt, err := agentrt.OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	sess := &session.Session{
		RunID: r.id, Store: r.store, Runtime: rt, WS: r.ws, SB: r.sb, Profile: r.profile, Candidates: r.cands, Policy: opts.Policy,
		Baseline: r.baseRun, BaselineID: r.baseID, ConfigHash: r.configHash, RunDir: r.runDir, StagingRoot: r.stagingRoot,
		ModCacheDir: r.modCacheDir, Limits: opts.Limits, Scope: opts.ScopeConfig, Budgets: opts.Budgets, Author: opts.Author,
		CheckTimeout: opts.CheckTimeout, BaseRef: r.ws.BaseRef,
	}
	drv, err := agentrt.NewDriver(agentrt.Config{
		Store: rt, Agent: agent, Policy: policy.New(policy.SessionFacts{S: sess}), Tools: tools.All(sess),
		Observer: opts.Observer,
		Reconcile: func(ctx context.Context, _ agentrt.RunView) error {
			if _, err := manifest.Recover(ctx, r.store, r.ws, r.id); err != nil {
				return err
			}
			_, err := proposal.Recover(ctx, r.store, r.ws, r.id)
			return err
		},
		NewID: func() string { return session.NewID() },
	})
	if err != nil {
		return nil, err
	}
	// The runtime run id is the task id so that steps, approvals, and
	// events line up with the task's own tables.
	t := time.Now()
	rr, err := drv.StartWithID(ctx, r.id, goalFor(sess), opts.RuntimeLimits)
	if err != nil {
		r.store.FinishTask(ctx, r.id, OutcomeFailed, map[string]any{"error": err.Error()})
		return r.res, err
	}
	r.mark("agent", t)
	r.res.Run = describeRun(ctx, rt, rr)
	r.res.Outcome, r.res.Detail = outcomeOf(rr, sess)
	if sess.Outcome != nil && sess.Outcome.Code == "proposal_prepared" {
		if p, ok := sess.Outcome.Detail.(*proposal.Proposal); ok {
			r.res.Proposal = p
			r.res.Readiness = p.Readiness
		}
	}
	if tg, ok, _ := sess.Target(ctx); ok {
		r.res.Selected = &tg
	}
	r.mark("total", r.start)
	if rr.Status.Terminal() {
		return r.res, r.store.FinishTask(ctx, r.id, r.res.Outcome, r.res.Detail)
	}
	return r.res, nil
}

func goalFor(s *session.Session) string {
	if s.Policy.NamedDependency != "" {
		return fmt.Sprintf("Upgrade the direct dependency %s of module %s to an eligible version, repair any breakage within scope, validate, and prepare a proposal.", s.Policy.NamedDependency, s.Profile.ModulePath)
	}
	return fmt.Sprintf("Select one eligible dependency upgrade for module %s, apply it, repair any breakage within scope, validate, and prepare a proposal.", s.Profile.ModulePath)
}

func describeRun(ctx context.Context, rt *agentrt.Store, rr agentrt.Run) *RunInfo {
	info := &RunInfo{Status: rr.Status, Reason: rr.Reason, ReasonDetail: rr.ReasonDetail, Steps: rr.StepCount}
	steps, _ := rt.ListSteps(ctx, rr.ID)
	for _, st := range steps {
		if st.Decision != nil {
			info.Tools = append(info.Tools, st.Decision.Tool+":"+string(st.Status))
		}
	}
	info.Approvals, _ = rt.ListApprovals(ctx, rr.ID)
	return info
}

// outcomeOf maps the runtime result and the session's recorded outcome to
// a maintenance outcome. A completed run is a success only when a terminal
// tool recorded proposal_prepared.
func outcomeOf(rr agentrt.Run, s *session.Session) (string, map[string]any) {
	detail := map[string]any{"run_status": rr.Status, "run_reason": rr.Reason, "run_detail": rr.ReasonDetail}
	switch rr.Status {
	case agentrt.StatusCompleted:
		if s.Outcome != nil {
			if d, ok := s.Outcome.Detail.(map[string]any); ok {
				for k, v := range d {
					detail[k] = v
				}
			}
			return s.Outcome.Code, detail
		}
		return OutcomeEndedWithoutProposal, detail
	case agentrt.StatusWaitingForApproval:
		return OutcomeAwaitingApproval, detail
	case agentrt.StatusCancelled:
		return OutcomeApprovalRejected, detail
	case agentrt.StatusFailed:
		switch rr.Reason {
		case agentrt.ReasonPolicyAbort:
			if strings.Contains(rr.ReasonDetail, "scope") {
				return OutcomeScopeExceeded, detail
			}
			return OutcomeBudgetExhausted, detail
		case agentrt.ReasonLimitSteps, agentrt.ReasonRepeatedToolFailures:
			return OutcomeLimitExhausted, detail
		case agentrt.ReasonLoopDetected:
			return OutcomeLoopDetected, detail
		}
		return OutcomeFailed, detail
	}
	return OutcomeFailed, detail
}

// applyDefaults fills Options for any mode.
func applyDefaults(opts *Options) error {
	if opts.Author.Name == "" || opts.Author.Email == "" {
		return fmt.Errorf("steward: proposal author name and email are required")
	}
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = 10 * time.Minute
	}
	if opts.Limits.MaxFiles == 0 {
		opts.Limits = defaultSnapshotLimits()
	}
	if opts.Scope == (proposal.ScopeLimits{}) {
		opts.Scope = proposal.DefaultScopeLimits()
	}
	if opts.ScopeConfig == (session.ScopeConfig{}) {
		opts.ScopeConfig = session.DefaultScope()
	}
	if opts.Budgets == (session.Budgets{}) {
		opts.Budgets = session.DefaultBudgets()
	}
	if opts.RuntimeLimits.MaxSteps == 0 {
		opts.RuntimeLimits = agentrt.DefaultLimits()
	}
	if opts.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		opts.DataDir = filepath.Join(home, ".local", "share", "repo-steward")
	}
	return lock.CheckLocal(opts.DataDir)
}
