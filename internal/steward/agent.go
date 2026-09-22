package steward

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/module"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/github"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/lock"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/policy"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/publish"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/scenario"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/tools"
	"github.com/joeylking/repo-steward/internal/validate"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// Agent-mode outcomes in addition to the baseline ones.
const (
	OutcomeProposalPublished    = "proposal_published"
	OutcomePublicationConflict  = "publication_conflict"
	OutcomeBlocked              = "blocked"
	OutcomeAwaitingApproval     = "awaiting_approval"
	OutcomeScopeExceeded        = "scope_exceeded"
	OutcomeBudgetExhausted      = "repair_budget_exhausted"
	OutcomeLimitExhausted       = "limit_exhausted"
	OutcomeLoopDetected         = "loop_detected"
	OutcomeEndedWithoutProposal = "ended_without_proposal"
	OutcomeApprovalRejected     = "approval_rejected"
	OutcomeCancelled            = "cancelled"
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

// persistedOptions is the subset of Options a resumed run must reuse so
// that its configuration hash, policy, and limits are unchanged.
type persistedOptions struct {
	Policy        deps.Policy          `json:"policy"`
	Author        gitx.Identity        `json:"author"`
	CheckTimeout  time.Duration        `json:"check_timeout"`
	Limits        snapshot.Limits      `json:"limits"`
	Scope         proposal.ScopeLimits `json:"scope"`
	ScopeConfig   session.ScopeConfig  `json:"scope_config"`
	Budgets       session.Budgets      `json:"budgets"`
	RuntimeLimits agentrt.Limits       `json:"runtime_limits"`
	FixtureProxy  bool                 `json:"fixture_proxy"`
	Model         *ModelSpec           `json:"model,omitempty"`
	Destination   *publish.Destination `json:"destination,omitempty"`
}

func persist(o Options) persistedOptions {
	return persistedOptions{Policy: o.Policy, Author: o.Author, CheckTimeout: o.CheckTimeout, Limits: o.Limits, Scope: o.Scope, ScopeConfig: o.ScopeConfig, Budgets: o.Budgets, RuntimeLimits: o.RuntimeLimits, FixtureProxy: o.FixtureProxyDir != "", Model: o.Model, Destination: o.Destination}
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
	// Publication is verified before anything is created on disk, so a
	// refused destination leaves no run behind.
	if opts.Publish {
		if err := r.captureDestination(ctx, opts.SourcePath); err != nil {
			return nil, err
		}
	}
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
	if err := r.store.SetTaskContext(ctx, r.id, persist(r.opts), r.cands); err != nil {
		return nil, err
	}

	rt, err := agentrt.OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	sess, drv, err := r.driver(rt, agent)
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
	return r.finalize(ctx, rt, sess, rr)
}

// driver builds the session, tools, policy, and runtime driver for a run
// whose prelude state is loaded.
func (r *run) driver(rt *agentrt.Store, agent agentrt.Agent) (*session.Session, *agentrt.Driver, error) {
	opts := r.opts
	sess := &session.Session{
		RunID: r.id, Store: r.store, Runtime: rt, WS: r.ws, SB: r.sb, Profile: r.profile, Candidates: r.cands, Policy: opts.Policy,
		Baseline: r.baseRun, BaselineID: r.baseID, ConfigHash: r.configHash, RunDir: r.runDir, StagingRoot: r.stagingRoot,
		ModCacheDir: r.modCacheDir, Limits: opts.Limits, Scope: opts.ScopeConfig, Budgets: opts.Budgets, Author: opts.Author,
		CheckTimeout: opts.CheckTimeout, BaseRef: r.ws.BaseRef,
	}
	var pub *publish.Publisher
	if r.opts.Publish {
		if r.opts.Destination == nil {
			return nil, nil, fmt.Errorf("steward: publication enabled without a destination")
		}
		pub = &publish.Publisher{Store: r.store, WS: r.ws, Client: github.New(r.opts.Destination.APIBase, r.opts.GitHubToken), Dest: *r.opts.Destination, Token: r.opts.GitHubToken}
		sess.Publish = &session.Publication{Publisher: publisherAdapter{pub}}
	}
	mc, modelAgent, err := r.modelConfig()
	if err != nil {
		return nil, nil, err
	}
	if agent == nil {
		if modelAgent == nil {
			return nil, nil, fmt.Errorf("steward: no agent and no model configured")
		}
		agent = modelAgent
	}
	drv, err := agentrt.NewDriver(agentrt.Config{
		Store: rt, Agent: agent, Policy: policy.New(policy.SessionFacts{S: sess}), Tools: tools.All(sess), Model: mc,
		Observer: opts.Observer,
		Reconcile: func(ctx context.Context, _ agentrt.RunView) (agentrt.Reconciliation, error) {
			if _, err := manifest.Recover(ctx, r.store, r.ws, r.id); err != nil {
				return agentrt.Reconciliation{}, err
			}
			if _, err := proposal.Recover(ctx, r.store, r.ws, r.id); err != nil {
				return agentrt.Reconciliation{}, err
			}
			if pub == nil {
				return agentrt.Reconciliation{Outcome: agentrt.ReconcileContinue}, nil
			}
			rec, err := pub.Reconcile(ctx, r.id)
			if err != nil {
				return agentrt.Reconciliation{}, err
			}
			switch rec.Outcome {
			case "completed":
				sess.Outcome = &session.Outcome{Code: OutcomeProposalPublished, Detail: map[string]any{"proposal_id": rec.Result.ProposalID, "pr_number": rec.Result.PRNumber, "pr_url": rec.Result.PRURL, "branch": rec.Result.Branch, "head_commit": rec.Result.HeadCommit, "recovered": true, "actions": rec.Actions}}
				r.store.SetProposalStatus(ctx, rec.Result.ProposalID, task.ProposalPublished)
				b, _ := json.Marshal(rec.Result)
				return agentrt.Reconciliation{Outcome: agentrt.ReconcileCompleted, Result: b, Detail: rec.Detail}, nil
			case "conflict":
				sess.Outcome = &session.Outcome{Code: OutcomePublicationConflict, Detail: map[string]any{"detail": rec.Detail, "actions": rec.Actions}}
				return agentrt.Reconciliation{Outcome: agentrt.ReconcileConflict, Detail: rec.Detail}, nil
			}
			return agentrt.Reconciliation{Outcome: agentrt.ReconcileContinue, Detail: rec.Detail}, nil
		},
		NewID: func() string { return session.NewID() },
	})
	if err != nil {
		return nil, nil, err
	}
	return sess, drv, nil
}

// publisherAdapter satisfies session.Publisher with the publish package.
type publisherAdapter struct{ p *publish.Publisher }

func (a publisherAdapter) Publish(ctx context.Context, runID, stepID string, prop *proposal.Proposal) (*session.PublishResult, error) {
	res, err := a.p.Publish(ctx, runID, stepID, prop)
	if err != nil {
		return nil, err
	}
	return &session.PublishResult{ProposalID: res.ProposalID, Branch: res.Branch, HeadCommit: res.HeadCommit, PRNumber: res.PRNumber, PRURL: res.PRURL, BaseMoved: res.BaseMoved}, nil
}

// finalize maps the runtime result to a maintenance outcome and records it
// when the run is terminal.
func (r *run) finalize(ctx context.Context, rt *agentrt.Store, sess *session.Session, rr agentrt.Run) (*Result, error) {
	r.res.Run = describeRun(ctx, rt, rr)
	r.res.ModelCalls, r.res.InputTokens, r.res.OutputTokens, r.res.CostMicros = rr.ModelCalls, rr.Usage.InputTokens, rr.Usage.OutputTokens, int64(rr.EstimatedCost)
	if steps, err := rt.ListSteps(ctx, rr.ID); err == nil {
		for _, st := range steps {
			if st.Policy == nil {
				continue
			}
			switch st.Policy.Outcome {
			case agentrt.Deny:
				r.res.PolicyDenials++
			case agentrt.Abort:
				r.res.PolicyAborts++
			}
		}
	}
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

// ResumeOptions identify a run to continue. Configuration is reloaded from
// the task; only values that cannot be persisted are supplied.
type ResumeOptions struct {
	RunID           string
	DataDir         string
	FixtureProxyDir string
	Socket          string
	AllowPull       bool
	Observer        agentrt.Observer
	Prices          agentrt.PriceTable
	// GitHubToken is supplied again on resume; it is never persisted.
	GitHubToken string
}

// Resume continues a run that is waiting for an approved approval or was
// interrupted. The session is rebuilt from the persisted task, workspace,
// options, candidate facts, and baseline evidence; the agent is chosen
// from the task's mode.
func Resume(ctx context.Context, ro ResumeOptions) (*Result, error) {
	if ro.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		ro.DataDir = filepath.Join(home, ".local", "share", "repo-steward")
	}
	if err := lock.CheckLocal(ro.DataDir); err != nil {
		return nil, err
	}
	exec, err := lock.Acquire(filepath.Join(ro.DataDir, "executor.lock"))
	if err != nil {
		return nil, err
	}
	defer exec.Release()
	runDir := filepath.Join(ro.DataDir, "runs", ro.RunID)
	runLock, err := lock.Acquire(filepath.Join(runDir, "run.lock"))
	if err != nil {
		return nil, err
	}
	defer runLock.Release()

	dbPath := filepath.Join(ro.DataDir, "steward.db")
	store, err := task.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	tk, err := store.GetTask(ctx, ro.RunID)
	if err != nil {
		return nil, err
	}
	if tk.Outcome != "" {
		return nil, fmt.Errorf("steward: run %s already ended with outcome %s", ro.RunID, tk.Outcome)
	}
	var po persistedOptions
	var cands []deps.Candidate
	if err := store.TaskContext(ctx, ro.RunID, &po, &cands); err != nil {
		return nil, err
	}
	if po.FixtureProxy && ro.FixtureProxyDir == "" {
		return nil, fmt.Errorf("steward: run %s started with a fixture proxy; pass -fixture-proxy again", ro.RunID)
	}
	if !po.FixtureProxy && ro.FixtureProxyDir != "" {
		return nil, fmt.Errorf("steward: run %s did not use a fixture proxy", ro.RunID)
	}
	opts := Options{SourcePath: tk.SourcePath, DataDir: ro.DataDir, FixtureProxyDir: ro.FixtureProxyDir, AllowPull: ro.AllowPull, Socket: ro.Socket,
		Policy: po.Policy, Author: po.Author, CheckTimeout: po.CheckTimeout, Limits: po.Limits, Scope: po.Scope, ScopeConfig: po.ScopeConfig,
		Budgets: po.Budgets, RuntimeLimits: po.RuntimeLimits, Observer: ro.Observer, Model: po.Model, Prices: ro.Prices,
		Publish: po.Destination != nil, Destination: po.Destination, GitHubToken: ro.GitHubToken}
	if err := applyDefaults(&opts); err != nil {
		return nil, err
	}
	var agent agentrt.Agent
	switch {
	case strings.HasPrefix(tk.Mode, "scripted:"):
		sc, err := scenario.Load(strings.TrimPrefix(tk.Mode, "scripted:"))
		if err != nil {
			return nil, err
		}
		agent = &scenario.Agent{Scenario: sc}
	case strings.HasPrefix(tk.Mode, "model:"):
		if po.Model == nil {
			return nil, fmt.Errorf("steward: run %s has no persisted model spec", ro.RunID)
		}
		agent = nil // built from the model spec by driver()
	default:
		return nil, fmt.Errorf("steward: run %s has mode %q, which cannot be resumed", ro.RunID, tk.Mode)
	}

	r := &run{opts: opts, id: ro.RunID, dataDir: ro.DataDir, runDir: runDir, store: store, start: time.Now(), cands: cands}
	r.res = &Result{RunID: r.id, Mode: tk.Mode, Timings: map[string]int64{}}
	if err := r.reattach(ctx, tk); err != nil {
		return nil, err
	}
	defer removeAll(r.buildCache)
	rt, err := agentrt.OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	sess, drv, err := r.driver(rt, agent)
	if err != nil {
		return nil, err
	}
	t := time.Now()
	rr, err := drv.Resume(ctx, r.id)
	if err != nil {
		return r.res, err
	}
	r.mark("agent", t)
	return r.finalize(ctx, rt, sess, rr)
}

// reattach rebuilds workspace, profile, sandbox, and baseline evidence for
// an existing run without re-running discovery or validation.
func (r *run) reattach(ctx context.Context, tk task.Task) error {
	ws, err := workspace.Open(ctx, tk.WorkspaceDir)
	if err != nil {
		return err
	}
	if ws.BaseCommit != tk.BaseCommit {
		return fmt.Errorf("steward: workspace HEAD %s differs from the task's base commit %s", ws.BaseCommit, tk.BaseCommit)
	}
	ws.BaseRef, ws.SourcePath = tk.BaseRef, tk.SourcePath
	r.ws = ws
	r.res.Workspace = ws
	entries, err := snapshot.List(ctx, ws.Git(), ws.BaseTree, r.opts.Limits)
	if err != nil {
		return err
	}
	baseSnap, _, err := ws.Materialize(ctx, ws.BaseTree, r.snapshotDir(ws.BaseTree), r.opts.Limits)
	if err != nil {
		return err
	}
	prof, err := repo.Inspect(baseSnap, entries)
	if err != nil {
		return err
	}
	if !prof.Supported() {
		return fmt.Errorf("steward: repository no longer supported: %v", prof.Refusals)
	}
	r.profile = prof
	r.res.Profile = prof
	r.configHash = configHash(r.opts, prof)
	key, err := module.EscapePath(prof.ModulePath)
	if err != nil {
		return err
	}
	r.modCacheDir = filepath.Join(r.dataDir, "cache", "mod", filepath.FromSlash(key))
	cfg := sandbox.Config{Image: prof.Toolchain.Ref(), SourceDir: baseSnap, CacheDir: r.modCacheDir, BuildCacheDir: filepath.Join(r.runDir, "gocache-"+strconv.FormatInt(time.Now().UnixNano(), 36))}
	if r.opts.FixtureProxyDir != "" {
		if cfg.ProxyDir, err = filepath.Abs(r.opts.FixtureProxyDir); err != nil {
			return err
		}
	}
	sb, err := sandbox.NewDocker(cfg, r.opts.Socket)
	if err != nil {
		return err
	}
	if err := sb.EnsureImage(ctx, r.opts.AllowPull); err != nil {
		return err
	}
	if _, err := sb.ReapOrphans(ctx); err != nil {
		return err
	}
	if err := sb.Probe(ctx); err != nil {
		return err
	}
	r.sb, r.buildCache, r.stagingRoot = sb, cfg.BuildCacheDir, filepath.Join(r.runDir, "staging")
	// Baseline evidence: the accepted baseline record under the same configuration.
	vals, err := r.store.ListValidations(ctx, r.id)
	if err != nil {
		return err
	}
	for _, v := range vals {
		if v.Kind == "baseline" && v.Accepted && v.ConfigHash == r.configHash {
			var run validate.Run
			if err := json.Unmarshal(v.Run, &run); err != nil {
				return err
			}
			r.baseRun, r.baseID = &run, v.ID
			break
		}
	}
	if r.baseRun == nil {
		return fmt.Errorf("steward: no baseline evidence for run %s under the current configuration", r.id)
	}
	r.res.Baseline = summarize(r.baseID, r.baseRun)
	r.res.Candidates = r.cands
	return nil
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
		case agentrt.ReasonReconcileConflict:
			return OutcomePublicationConflict, detail
		case agentrt.ReasonToolAbort:
			if strings.Contains(rr.ReasonDetail, "proposal invalidated") {
				return "proposal_invalidated", detail
			}
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

// captureDestination records where a proposal would be published and
// verifies it before any credential is used for anything else: the host
// is supported, the repository and base branch exist, and the base branch
// head is exactly the local base commit.
func (r *run) captureDestination(ctx context.Context, sourcePath string) error {
	sg := gitx.New(sourcePath)
	baseCommit, err := sg.RevParse(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("steward: %s has no HEAD commit: %w", sourcePath, err)
	}
	baseRef := ""
	if out, err := sg.Run(ctx, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		baseRef = strings.TrimSpace(string(out))
	}
	d := r.opts.Destination
	if d == nil {
		out, err := sg.Run(ctx, "remote", "get-url", "origin")
		if err != nil {
			return fmt.Errorf("steward: no destination given and the source has no origin remote: %w", err)
		}
		parsed, err := publish.ParseRemote(string(out))
		if err != nil {
			return err
		}
		d = &parsed
	}
	if r.opts.APIBaseOverride != "" {
		d.APIBase = r.opts.APIBaseOverride
	}
	if r.opts.PushURLOverride != "" {
		d.PushURL = r.opts.PushURLOverride
	}
	client := github.New(d.APIBase, r.opts.GitHubToken)
	if err := publish.Verify(ctx, client, d, baseRef, baseCommit); err != nil {
		return err
	}
	if r.opts.GitHubToken == "" && !strings.HasPrefix(d.PushURL, "file://") {
		return fmt.Errorf("steward: publication to %s needs a token in GITHUB_TOKEN", d.PushURL)
	}
	r.opts.Destination = d
	r.res.Destination = d
	return nil
}

// Publication in baseline mode is not offered: the baseline pipeline ends
// at a frozen proposal by design.
