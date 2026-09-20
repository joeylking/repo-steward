// Package steward runs maintenance tasks. Baseline mode is the deterministic
// upgrade pipeline with no model: select the smallest eligible upgrade,
// stage it under Gate A, normalize under Gate B, validate the exact
// candidate tree, evaluate readiness, and freeze a proposal. It is the
// floor the agent is measured against and it exercises every control the
// agent path will reuse.
package steward

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/lock"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/validate"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// Options configure a maintenance run.
type Options struct {
	SourcePath      string
	DataDir         string
	FixtureProxyDir string
	AllowPull       bool
	Socket          string
	Policy          deps.Policy
	// Author is the identity of the proposal commit. Required.
	Author       gitx.Identity
	CheckTimeout time.Duration
	Limits       snapshot.Limits
	// Scope holds the hard limits readiness enforces in every mode.
	Scope proposal.ScopeLimits
	// ScopeConfig, Budgets, RuntimeLimits, and Observer apply to agent modes.
	ScopeConfig   session.ScopeConfig
	Budgets       session.Budgets
	RuntimeLimits agentrt.Limits
	Observer      agentrt.Observer
	// Model and Prices apply to model mode.
	Model  *ModelSpec
	Prices agentrt.PriceTable
}

func defaultSnapshotLimits() snapshot.Limits { return snapshot.DefaultLimits() }

// Outcomes of a run. Only proposal_prepared is a success; every other
// outcome is an explained non-result.
const (
	OutcomeProposalPrepared       = "proposal_prepared"
	OutcomeUnsupported            = "unsupported"
	OutcomeNoCandidate            = "no_candidate"
	OutcomeBaselineFailing        = "baseline_failing"
	OutcomeBaselineInconclusive   = "baseline_inconclusive"
	OutcomeAdmissionRefused       = "admission_refused"
	OutcomeRequiresNewerToolchain = "requires_newer_toolchain"
	OutcomeNormalizationRefused   = "normalization_refused"
	OutcomeRegressed              = "regressed"
	OutcomeInconclusive           = "validation_inconclusive"
	OutcomeNotReady               = "not_ready"
)

// Result is the structured outcome of a run.
type Result struct {
	RunID         string                 `json:"run_id"`
	Mode          string                 `json:"mode"`
	Outcome       string                 `json:"outcome"`
	Detail        map[string]any         `json:"detail,omitempty"`
	Workspace     *workspace.Workspace   `json:"workspace,omitempty"`
	Profile       *repo.Profile          `json:"profile,omitempty"`
	Candidates    []deps.Candidate       `json:"candidates,omitempty"`
	Selected      *manifest.Target       `json:"selected,omitempty"`
	Baseline      *ValidationSummary     `json:"baseline,omitempty"`
	Admission     *manifest.Verification `json:"admission,omitempty"`
	Normalization *manifest.Verification `json:"normalization,omitempty"`
	Post          *ValidationSummary     `json:"post,omitempty"`
	Introduced    []validate.Finding     `json:"introduced,omitempty"`
	Readiness     *proposal.Readiness    `json:"readiness,omitempty"`
	Proposal      *proposal.Proposal     `json:"proposal,omitempty"`
	Run           *RunInfo               `json:"run,omitempty"`
	// Accounting and control counters, filled from the runtime run.
	ModelCalls    int              `json:"model_calls,omitempty"`
	InputTokens   int              `json:"input_tokens,omitempty"`
	OutputTokens  int              `json:"output_tokens,omitempty"`
	CostMicros    int64            `json:"cost_micros,omitempty"`
	PolicyDenials int              `json:"policy_denials,omitempty"`
	PolicyAborts  int              `json:"policy_aborts,omitempty"`
	Timings       map[string]int64 `json:"timings_ms"`
}

// ValidationSummary is the per-check summary kept in the result.
type ValidationSummary struct {
	ID         string            `json:"id"`
	TreeHash   string            `json:"tree_hash"`
	Conclusive bool              `json:"conclusive"`
	Clean      bool              `json:"clean"`
	Checks     map[string]string `json:"checks"`
	Findings   int               `json:"findings"`
	DurationMS int64             `json:"duration_ms"`
}

func summarize(id string, r *validate.Run) *ValidationSummary {
	s := &ValidationSummary{ID: id, TreeHash: r.TreeHash, Conclusive: r.Conclusive, Clean: r.Clean, Checks: map[string]string{}, DurationMS: r.Duration.Milliseconds()}
	for name, c := range r.Checks {
		st := string(c.Status)
		if !c.Conclusive {
			st += " (inconclusive: " + c.Reason + ")"
		}
		s.Checks[name] = st
		s.Findings += len(c.Findings)
	}
	return s
}

// run holds the state shared by pipeline stages.
type run struct {
	opts        Options
	id          string
	dataDir     string
	runDir      string
	store       *task.Store
	ws          *workspace.Workspace
	sb          *sandbox.Docker
	profile     *repo.Profile
	configHash  string
	res         *Result
	start       time.Time
	modCacheDir string
	buildCache  string
	stagingRoot string
	baseRun     *validate.Run
	baseID      string
	cands       []deps.Candidate
}

func (r *run) mark(name string, since time.Time) {
	r.res.Timings[name] = time.Since(since).Milliseconds()
}

// RunBaseline executes the deterministic pipeline.
func RunBaseline(ctx context.Context, opts Options) (*Result, error) {
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
	r.res = &Result{RunID: r.id, Mode: "baseline", Timings: map[string]int64{}}
	runLock, err := lock.Acquire(filepath.Join(r.runDir, "run.lock"))
	if err != nil {
		return nil, err
	}
	defer runLock.Release()
	if r.store, err = task.Open(filepath.Join(opts.DataDir, "steward.db")); err != nil {
		return nil, err
	}
	defer r.store.Close()

	outcome, err := r.pipeline(ctx)
	if err != nil {
		r.store.FinishTask(ctx, r.id, "failed", map[string]any{"error": err.Error()})
		return r.res, err
	}
	r.res.Outcome = outcome
	r.mark("total", r.start)
	if err := r.store.FinishTask(ctx, r.id, outcome, r.res.Detail); err != nil {
		return r.res, err
	}
	return r.res, nil
}

// prelude performs everything that precedes selection in every mode:
// workspace, profile, sandbox, cache population, baseline validation, and
// candidate discovery. It returns a non-empty outcome when the run cannot
// proceed to an upgrade.
func (r *run) prelude(ctx context.Context, mode string) (string, error) {
	res := r.res
	t := time.Now()
	ws, err := workspace.Create(ctx, r.opts.SourcePath, r.runDir)
	if err != nil {
		return "", err
	}
	r.ws = ws
	res.Workspace = ws
	if err := r.store.CreateTask(ctx, task.Task{RunID: r.id, Mode: mode, SourcePath: ws.SourcePath, BaseCommit: ws.BaseCommit, BaseTree: ws.BaseTree, BaseRef: ws.BaseRef, WorkspaceDir: ws.Dir, NamedDependency: r.opts.Policy.NamedDependency}); err != nil {
		return "", err
	}
	r.mark("workspace", t)

	// Profile the base tree.
	t = time.Now()
	entries, err := snapshot.List(ctx, ws.Git(), ws.BaseTree, r.opts.Limits)
	if err != nil {
		if ref := repo.RefusalForListError(err); ref != "" {
			res.Detail = map[string]any{"refusals": []string{ref}}
			return OutcomeUnsupported, nil
		}
		return "", err
	}
	baseSnap, _, err := ws.Materialize(ctx, ws.BaseTree, r.snapshotDir(ws.BaseTree), r.opts.Limits)
	if err != nil {
		return "", err
	}
	prof, err := repo.Inspect(baseSnap, entries)
	if err != nil {
		return "", err
	}
	r.profile = prof
	res.Profile = prof
	if !prof.Supported() {
		res.Detail = map[string]any{"refusals": prof.Refusals}
		return OutcomeUnsupported, nil
	}
	r.store.UpdateTaskModule(ctx, r.id, prof.ModulePath)
	r.configHash = configHash(r.opts, prof)
	r.mark("profile", t)

	// Sandbox.
	t = time.Now()
	key, err := module.EscapePath(prof.ModulePath)
	if err != nil {
		return "", err
	}
	r.modCacheDir = filepath.Join(r.dataDir, "cache", "mod", filepath.FromSlash(key))
	cfg := sandbox.Config{
		Image:     prof.Toolchain.Ref(),
		SourceDir: baseSnap,
		CacheDir:  r.modCacheDir,
		// Unique per invocation: a deleted and recreated path can stay
		// invisible to a VM-backed engine.
		BuildCacheDir: filepath.Join(r.runDir, "gocache-"+strconv.FormatInt(time.Now().UnixNano(), 36)),
	}
	if r.opts.FixtureProxyDir != "" {
		if cfg.ProxyDir, err = filepath.Abs(r.opts.FixtureProxyDir); err != nil {
			return "", err
		}
	}
	sb, err := sandbox.NewDocker(cfg, r.opts.Socket)
	if err != nil {
		return "", err
	}
	r.sb = sb
	if err := sb.EnsureImage(ctx, r.opts.AllowPull); err != nil {
		return "", err
	}
	if _, err := sb.ReapOrphans(ctx); err != nil {
		return "", err
	}
	if err := sb.Probe(ctx); err != nil {
		return "", err
	}
	r.buildCache = cfg.BuildCacheDir
	r.mark("sandbox", t)

	// Baseline validation and candidate discovery on the base tree.
	t = time.Now()
	if err := deps.Download(ctx, sb); err != nil {
		return "", err
	}
	baseRun, baseID, err := r.validate(ctx, sb, ws.BaseTree, "baseline")
	if err != nil {
		return "", err
	}
	r.baseRun, r.baseID = baseRun, baseID
	res.Baseline = summarize(baseID, baseRun)
	r.mark("baseline", t)
	if !baseRun.Conclusive {
		return OutcomeBaselineInconclusive, nil
	}
	if !baseRun.Clean {
		res.Detail = map[string]any{"findings": allFindings(baseRun)}
		return OutcomeBaselineFailing, nil
	}
	t = time.Now()
	cands, err := deps.Discover(ctx, sb, r.opts.Policy)
	if err != nil {
		return "", err
	}
	r.cands = cands
	res.Candidates = cands
	r.mark("discover", t)
	r.stagingRoot = filepath.Join(r.runDir, "staging")
	return "", nil
}

func (r *run) pipeline(ctx context.Context) (string, error) {
	res := r.res
	if outcome, err := r.prelude(ctx, "baseline"); err != nil || outcome != "" {
		return outcome, err
	}
	defer removeAll(r.buildCache)
	ws, sb, prof, baseRun, baseID := r.ws, r.sb, r.profile, r.baseRun, r.baseID
	target, ok := Select(r.cands)
	if !ok {
		return OutcomeNoCandidate, nil
	}
	res.Selected = &target
	stagingRoot := r.stagingRoot

	// Gate A: stage the upgrade against the base snapshot.
	t := time.Now()
	st, err := manifest.Stage(ctx, sb, ws, stagingRoot, manifest.Op{Kind: "upgrade", Target: target})
	if err != nil {
		var oe *manifest.OpError
		if errors.As(err, &oe) {
			res.Detail = map[string]any{"stderr": oe.Stderr}
			if oe.RequiresNewerToolchain() {
				return OutcomeRequiresNewerToolchain, nil
			}
			return OutcomeAdmissionRefused, nil
		}
		return "", err
	}
	closure, err := manifest.Closure(ctx, sb, st, target)
	if err != nil {
		return "", err
	}
	baseFacts, err := manifest.Parse(st.Before.Mod)
	if err != nil {
		return "", err
	}
	candFacts, err := manifest.Parse(st.After.Mod)
	if err != nil {
		return "", err
	}
	adm := manifest.VerifyAdmission(baseFacts, candFacts, target, closure)
	res.Admission = &adm
	if !adm.OK() {
		os.RemoveAll(st.Dir)
		res.Detail = map[string]any{"violations": adm.Violations}
		return OutcomeAdmissionRefused, nil
	}
	if _, err := manifest.Promote(ctx, r.store, ws, r.id, "", st); err != nil {
		return "", err
	}
	r.mark("admission", t)

	// Gate B: normalize against a snapshot of the upgraded tree.
	t = time.Now()
	tree1, snap1, err := r.candidateSnapshot(ctx)
	if err != nil {
		return "", err
	}
	sb1 := sb.WithSource(snap1)
	if err := deps.Download(ctx, sb1); err != nil {
		return "", err
	}
	nst, err := manifest.Stage(ctx, sb1, ws, stagingRoot, manifest.Op{Kind: "normalize"})
	if err != nil {
		var oe *manifest.OpError
		if errors.As(err, &oe) {
			res.Detail = map[string]any{"stderr": oe.Stderr, "tree": tree1}
			return OutcomeNormalizationRefused, nil
		}
		return "", err
	}
	nFacts, err := manifest.Parse(nst.After.Mod)
	if err != nil {
		return "", err
	}
	norm := manifest.VerifyNormalized(baseFacts, nFacts, target, closure)
	if !nst.Idempotent {
		norm.AddViolation(manifest.CodeNotTidy, "tidy is not idempotent")
	}
	res.Normalization = &norm
	if !norm.OK() {
		os.RemoveAll(nst.Dir)
		res.Detail = map[string]any{"violations": norm.Violations}
		return OutcomeNormalizationRefused, nil
	}
	if _, err := manifest.Promote(ctx, r.store, ws, r.id, "", nst); err != nil {
		return "", err
	}
	r.mark("normalization", t)

	// Post-change validation of the exact candidate tree.
	t = time.Now()
	tree2, snap2, err := r.candidateSnapshot(ctx)
	if err != nil {
		return "", err
	}
	sb2 := sb.WithSource(snap2)
	if err := deps.Download(ctx, sb2); err != nil {
		return "", err
	}
	postRun, postID, err := r.validate(ctx, sb2, tree2, "post")
	if err != nil {
		return "", err
	}
	res.Post = summarize(postID, postRun)
	r.mark("validation", t)
	if !postRun.Conclusive {
		return OutcomeInconclusive, nil
	}
	if intro := proposal.Introduced(baseRun, postRun); len(intro) > 0 {
		res.Introduced = intro
		return OutcomeRegressed, nil
	}

	// Readiness and freeze.
	t = time.Now()
	ready, err := proposal.Evaluate(ctx, proposal.Inputs{
		RunID: r.id, Workspace: ws, Store: r.store, Sandbox: sb2, SnapshotDir: snap2, Target: target, Baseline: baseRun,
		ConfigHash: r.configHash, ToolchainDigest: prof.Toolchain.Digest, Scope: r.opts.Scope, ProtectedGlobs: prof.ProtectedGlobs, StagingRoot: stagingRoot,
	})
	if err != nil {
		return "", err
	}
	res.Readiness = ready
	if !ready.Ready {
		return OutcomeNotReady, nil
	}
	current, _ := currentVersion(baseFacts, target.Module)
	title := fmt.Sprintf("Upgrade %s from %s to %s", target.Module, current, target.Version)
	body := proposalBody(target, current, adm, ready, res.Post)
	p, err := proposal.Freeze(ctx, proposal.FreezeInput{
		RunID: r.id, Workspace: ws, Store: r.store, Readiness: ready, Target: target,
		BaseRef: ws.BaseRef, HeadRef: HeadRef(target), Title: title, Body: body,
		Author: r.opts.Author, When: time.Now(), BaselineID: baseID, ConfigHash: r.configHash, ToolchainDigest: prof.Toolchain.Digest,
	})
	if err != nil {
		return "", err
	}
	res.Proposal = p
	r.mark("proposal", t)
	return OutcomeProposalPrepared, nil
}

func (r *run) snapshotDir(tree string) string {
	return filepath.Join(r.runDir, "snapshots", tree)
}

// candidateSnapshot builds and materializes the current candidate tree.
func (r *run) candidateSnapshot(ctx context.Context) (string, string, error) {
	tree, err := r.ws.CandidateTree(ctx)
	if err != nil {
		return "", "", err
	}
	dir, _, err := r.ws.Materialize(ctx, tree, r.snapshotDir(tree), r.opts.Limits)
	if err != nil {
		return "", "", err
	}
	return tree, dir, nil
}

// validate runs the checks against sb's snapshot and records the result as
// accepted evidence for tree. In baseline mode there is no step to wait
// for, so acceptance is immediate.
func (r *run) validate(ctx context.Context, sb *sandbox.Docker, tree, kind string) (*validate.Run, string, error) {
	vr, err := validate.Baseline(ctx, sb, validate.Options{Kind: kind, TreeHash: tree, ToolchainDigest: r.profile.Toolchain.Digest, CheckTimeout: r.opts.CheckTimeout, RunID: r.id, StepID: kind})
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(vr)
	if err != nil {
		return nil, "", err
	}
	id := newID()
	rec := task.ValidationRecord{ID: id, RunID: r.id, StepID: kind, Kind: kind, TreeHash: tree, ConfigHash: r.configHash, ToolchainDigest: r.profile.Toolchain.Digest, Accepted: true, Run: raw}
	if err := r.store.InsertValidation(ctx, rec); err != nil {
		return nil, "", err
	}
	return vr, id, nil
}

// Select picks the smallest eligible upgrade: the lowest delta class
// (patch before minor before major), the highest eligible version within
// that class, and the alphabetically first module on ties.
func Select(cands []deps.Candidate) (manifest.Target, bool) {
	rank := map[string]int{"patch": 0, "minor": 1, "major": 2}
	best := manifest.Target{}
	bestRank := 99
	sorted := append([]deps.Candidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Module < sorted[j].Module })
	for _, c := range sorted {
		byClass := map[string]string{}
		for _, tg := range c.EligibleTargets() {
			if cur, ok := byClass[tg.Delta]; !ok || semver.Compare(tg.Version, cur) > 0 {
				byClass[tg.Delta] = tg.Version
			}
		}
		for class, v := range byClass {
			if rank[class] < bestRank {
				bestRank = rank[class]
				best = manifest.Target{Module: c.Module, Version: v}
			}
		}
	}
	return best, bestRank != 99
}

// HeadRef names the branch a proposal for target would be pushed to.
func HeadRef(t manifest.Target) string { return names.HeadRef(t) }

func currentVersion(base *manifest.Facts, mod string) (string, bool) {
	r, ok := base.Require[mod]
	return r.Version, ok
}

func proposalBody(target manifest.Target, current string, adm manifest.Verification, ready *proposal.Readiness, post *ValidationSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dependency: %s\nFrom: %s\nTo: %s\n\n", target.Module, current, target.Version)
	if len(adm.ClosureIncreases) > 0 {
		b.WriteString("Transitive requirements raised by this upgrade:\n")
		for _, c := range adm.ClosureIncreases {
			fmt.Fprintf(&b, "- %s %s -> %s\n", c.Module, c.From, c.To)
		}
		b.WriteString("\n")
	}
	b.WriteString("Validation (build, vet, test) ran against the exact proposed tree with no network:\n")
	names := make([]string, 0, len(post.Checks))
	for n := range post.Checks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "- %s: %s\n", n, post.Checks[n])
	}
	fmt.Fprintf(&b, "\nFiles changed: %d\n", len(ready.ChangeSet.Files))
	for _, f := range ready.ChangeSet.Files {
		fmt.Fprintf(&b, "- %s (+%d -%d)\n", f.Path, f.Added, f.Removed)
	}
	fmt.Fprintf(&b, "\nValidated tree: %s\n", ready.TreeHash)
	return b.String()
}

func allFindings(r *validate.Run) []validate.Finding {
	var out []validate.Finding
	for _, name := range validate.Required {
		out = append(out, r.Checks[name].Findings...)
	}
	return out
}

// configHash binds validation evidence to the configuration it ran under.
func configHash(o Options, p *repo.Profile) string {
	b, _ := json.Marshal(map[string]any{
		"check_timeout": o.CheckTimeout.String(), "toolchain": p.Toolchain.Digest, "scope": o.Scope,
		"protected": p.ProtectedGlobs, "policy": o.Policy, "fixture_proxy": o.FixtureProxyDir != "",
	})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func removeAll(dir string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			if d.IsDir() {
				os.Chmod(p, 0o755)
			} else {
				os.Chmod(p, 0o644)
			}
		}
		return nil
	})
	os.RemoveAll(dir)
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
