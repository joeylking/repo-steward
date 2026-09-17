// Package proposal decides whether a candidate tree is ready to publish and
// freezes it into an immutable proposal commit. Readiness is a pure
// function of the workspace, the store, and configuration. The frozen
// commit's identity comes from a persisted recipe, so an interrupted freeze
// is finished by re-running the recipe, and the scratch checkout's HEAD is
// never consulted.
package proposal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/faultpoint"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/validate"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// Failure is one unmet readiness condition.
type Failure struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Readiness is the evaluator's report. Every check runs; all failures are
// listed.
type Readiness struct {
	Ready        bool                   `json:"ready"`
	TreeHash     string                 `json:"tree_hash"`
	ValidationID string                 `json:"validation_id,omitempty"`
	Manifest     *manifest.Verification `json:"manifest,omitempty"`
	ChangeSet    workspace.ChangeSet    `json:"change_set"`
	Failures     []Failure              `json:"failures,omitempty"`
	EvaluatedAt  time.Time              `json:"evaluated_at"`
}

// ScopeLimits bound the source diff. Manifests are exempt; the manifest
// rules bound them instead.
type ScopeLimits struct {
	MaxFiles int `json:"max_files"`
	MaxLines int `json:"max_lines"`
}

// DefaultScopeLimits are the hard limits for a single upgrade.
func DefaultScopeLimits() ScopeLimits { return ScopeLimits{MaxFiles: 20, MaxLines: 400} }

// Inputs are what readiness evaluates.
type Inputs struct {
	RunID     string
	Workspace *workspace.Workspace
	Store     *task.Store
	// Sandbox is bound to a verified snapshot of the candidate tree.
	Sandbox         *sandbox.Docker
	SnapshotDir     string
	Target          manifest.Target
	Baseline        *validate.Run
	ConfigHash      string
	ToolchainDigest string
	Scope           ScopeLimits
	ProtectedGlobs  []string
	StagingRoot     string
}

// Failure codes.
const (
	CodeCandidateMismatch = "candidate_tree_mismatch"
	CodeValidationMissing = "validation_not_bound"
	CodeValidationUnclean = "validation_inconclusive"
	CodeRegression        = "validation_regressed"
	CodeManifest          = "manifest_violation"
	CodeProtected         = "protected_file_changed"
	CodeScope             = "scope_exceeded"
)

// Evaluate runs every readiness check against the candidate tree.
func Evaluate(ctx context.Context, in Inputs) (*Readiness, error) {
	r := &Readiness{EvaluatedAt: time.Now()}
	fail := func(code, format string, args ...any) {
		r.Failures = append(r.Failures, Failure{Code: code, Detail: fmt.Sprintf(format, args...)})
	}
	tree, err := in.Workspace.CandidateTree(ctx)
	if err != nil {
		return nil, err
	}
	r.TreeHash = tree

	// 1. Bound, accepted, conclusive validation with no introduced findings.
	records, err := in.Store.ListValidations(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	var post *validate.Run
	for _, rec := range records {
		if rec.Kind == "post" && rec.Accepted && rec.TreeHash == tree && rec.ConfigHash == in.ConfigHash && rec.ToolchainDigest == in.ToolchainDigest {
			var run validate.Run
			if err := json.Unmarshal(rec.Run, &run); err != nil {
				return nil, err
			}
			post = &run
			r.ValidationID = rec.ID
			break
		}
	}
	switch {
	case post == nil:
		fail(CodeValidationMissing, "no accepted validation of tree %s under the current configuration", tree)
	case !post.Conclusive:
		for _, name := range validate.Required {
			if c := post.Checks[name]; !c.Conclusive {
				fail(CodeValidationUnclean, "%s: %s", name, c.Reason)
			}
		}
	default:
		for _, f := range Introduced(in.Baseline, post) {
			fail(CodeRegression, "%s", f.Key)
		}
	}

	// 2. Manifest rules on the final manifests, with closure, tidy
	// idempotence, cache verification, and target resolution.
	base, err := manifest.Parse(mustBlob(ctx, in.Workspace, in.Workspace.BaseTree, "go.mod"))
	if err != nil {
		return nil, err
	}
	candBytes, err := in.Workspace.ReadFile("go.mod")
	if err != nil {
		return nil, err
	}
	cand, err := manifest.Parse(candBytes)
	if err != nil {
		return nil, err
	}
	graph, err := execOut(ctx, in.Sandbox, "go", "mod", "graph")
	if err != nil {
		return nil, err
	}
	closure := manifest.ClosureFromGraph(graph, in.Target)
	v := manifest.VerifyNormalized(base, cand, in.Target, closure)
	if st, err := manifest.Stage(ctx, in.Sandbox, in.Workspace, in.StagingRoot, manifest.Op{Kind: "normalize"}); err != nil {
		v.AddViolation(manifest.CodeNotTidy, err.Error())
	} else {
		defer os.RemoveAll(st.Dir)
		if !st.Idempotent || string(st.After.Mod) != string(st.Before.Mod) || string(st.After.Sum) != string(st.Before.Sum) {
			v.AddViolation(manifest.CodeNotTidy, "go mod tidy would change the manifests")
		}
	}
	if out, err := execOut(ctx, in.Sandbox, "go", "mod", "verify"); err != nil {
		v.AddViolation(manifest.CodeVerifyFailed, err.Error())
	} else if !strings.Contains(out, "all modules verified") {
		v.AddViolation(manifest.CodeVerifyFailed, strings.TrimSpace(out))
	}
	if out, err := execOut(ctx, in.Sandbox, "go", "list", "-m", "-json", in.Target.Module); err != nil {
		v.AddViolation(manifest.CodeTargetNotResolved, err.Error())
	} else {
		var m struct{ Version string }
		if json.Unmarshal([]byte(out), &m) != nil || m.Version != in.Target.Version {
			v.AddViolation(manifest.CodeTargetNotResolved, fmt.Sprintf("resolved %q, want %s", m.Version, in.Target.Version))
		}
	}
	r.Manifest = &v
	for _, viol := range v.Violations {
		fail(CodeManifest, "%s: %s", viol.Code, viol.Detail)
	}

	// 3. Protected files and scope over the source diff.
	cs, err := in.Workspace.Diff(ctx, tree)
	if err != nil {
		return nil, err
	}
	r.ChangeSet = cs
	sourceFiles, lines := 0, 0
	for _, f := range cs.Files {
		if f.Path == "go.mod" || f.Path == "go.sum" {
			continue
		}
		if repo.IsProtected(f.Path, in.ProtectedGlobs) {
			fail(CodeProtected, "%s", f.Path)
		}
		sourceFiles++
		lines += f.Added + f.Removed
	}
	if in.Scope.MaxFiles > 0 && sourceFiles > in.Scope.MaxFiles {
		fail(CodeScope, "%d source files changed, limit %d", sourceFiles, in.Scope.MaxFiles)
	}
	if in.Scope.MaxLines > 0 && lines > in.Scope.MaxLines {
		fail(CodeScope, "%d lines changed, limit %d", lines, in.Scope.MaxLines)
	}
	r.Ready = len(r.Failures) == 0
	return r, nil
}

// Introduced returns post findings absent from the baseline, keyed by
// finding key. A nil baseline treats every finding as introduced.
func Introduced(baseline, post *validate.Run) []validate.Finding {
	have := map[string]bool{}
	if baseline != nil {
		for _, c := range baseline.Checks {
			for _, f := range c.Findings {
				have[f.Key] = true
			}
		}
	}
	var out []validate.Finding
	for _, name := range validate.Required {
		for _, f := range post.Checks[name].Findings {
			if !have[f.Key] {
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func mustBlob(ctx context.Context, ws *workspace.Workspace, tree, path string) []byte {
	out, err := ws.Git().Run(ctx, "show", tree+":"+path)
	if err != nil {
		return nil
	}
	return out
}

func execOut(ctx context.Context, sb *sandbox.Docker, argv ...string) (string, error) {
	res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: argv, Timeout: 5 * time.Minute, StepID: "readiness"})
	if err != nil {
		return "", err
	}
	if res.TimedOut || res.ExitCode != 0 {
		return string(res.Stdout), fmt.Errorf("%s: exit %d: %s", strings.Join(argv, " "), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return string(res.Stdout), nil
}

// Proposal is the frozen, immutable publication proposal.
type Proposal struct {
	ID              string                 `json:"id"`
	RunID           string                 `json:"run_id"`
	StepID          string                 `json:"step_id,omitempty"`
	BaseRef         string                 `json:"base_ref"`
	BaseCommit      string                 `json:"base_commit"`
	HeadRef         string                 `json:"head_ref"`
	HeadCommit      string                 `json:"head_commit"`
	ProposalRef     string                 `json:"proposal_ref"`
	TreeHash        string                 `json:"tree_hash"`
	Title           string                 `json:"title"`
	Body            string                 `json:"body"`
	Target          manifest.Target        `json:"target"`
	Files           []workspace.FileChange `json:"files"`
	BaselineID      string                 `json:"baseline_validation_id"`
	ValidationID    string                 `json:"post_validation_id"`
	Readiness       *Readiness             `json:"readiness"`
	ConfigHash      string                 `json:"config_hash"`
	ToolchainDigest string                 `json:"toolchain_digest"`
	CreatedAt       time.Time              `json:"created_at"`
}

// Hash returns the SHA-256 of the proposal's canonical JSON.
func (p Proposal) Hash() string {
	b, _ := json.Marshal(p)
	var v any
	json.Unmarshal(b, &v)
	c, _ := json.Marshal(v)
	s := sha256.Sum256(c)
	return hex.EncodeToString(s[:])
}

// FreezeInput is everything a proposal commit is built from.
type FreezeInput struct {
	RunID, StepID   string
	Workspace       *workspace.Workspace
	Store           *task.Store
	Readiness       *Readiness
	Target          manifest.Target
	BaseRef         string
	HeadRef         string
	Title, Body     string
	Author          gitx.Identity
	When            time.Time
	BaselineID      string
	ConfigHash      string
	ToolchainDigest string
}

// recipe is the persisted commit recipe.
type recipe struct {
	Tree      string        `json:"tree"`
	Parent    string        `json:"parent"`
	Author    gitx.Identity `json:"author"`
	Committer gitx.Identity `json:"committer"`
	Message   string        `json:"message"`
}

// ErrNotReady is returned when Freeze is asked to freeze an unready tree.
var ErrNotReady = errors.New("proposal: readiness failed")

// Freeze writes the proposal commit from a persisted recipe, points the
// proposal ref at it, and records the frozen proposal and its hash.
func Freeze(ctx context.Context, in FreezeInput) (*Proposal, error) {
	if in.Readiness == nil || !in.Readiness.Ready {
		return nil, ErrNotReady
	}
	id := newID()
	when := in.When.UTC().Truncate(time.Second)
	ident := in.Author
	ident.When = when
	msg := in.Title + "\n\n" + strings.TrimSpace(in.Body) + "\n\nRepo-Steward-Run: " + in.RunID + "\nRepo-Steward-Proposal: " + id + "\n"
	rc := recipe{Tree: in.Readiness.TreeHash, Parent: in.Workspace.BaseCommit, Author: ident, Committer: ident, Message: msg}
	p := Proposal{
		ID: id, RunID: in.RunID, StepID: in.StepID, BaseRef: in.BaseRef, BaseCommit: in.Workspace.BaseCommit,
		HeadRef: in.HeadRef, ProposalRef: "refs/repo-steward/proposals/" + id, TreeHash: in.Readiness.TreeHash,
		Title: in.Title, Body: strings.TrimSpace(in.Body), Target: in.Target, Files: in.Readiness.ChangeSet.Files,
		BaselineID: in.BaselineID, ValidationID: in.Readiness.ValidationID, Readiness: in.Readiness,
		ConfigHash: in.ConfigHash, ToolchainDigest: in.ToolchainDigest, CreatedAt: when,
	}
	rcJSON, _ := json.Marshal(rc)
	pJSON, _ := json.Marshal(p)
	rec := task.ProposalRecord{ID: id, RunID: in.RunID, StepID: in.StepID, Status: task.ProposalPreparing, TreeHash: rc.Tree, BaseCommit: rc.Parent, Ref: p.ProposalRef, Recipe: rcJSON, Proposal: pJSON}
	if err := in.Store.InsertProposal(ctx, rec); err != nil {
		return nil, err
	}
	faultpoint.Hit("freeze.preparing")
	return finish(ctx, in.Store, in.Workspace, rec)
}

// finish runs the recipe and freezes the row. It is idempotent: the same
// recipe always yields the same commit id.
func finish(ctx context.Context, store *task.Store, ws *workspace.Workspace, rec task.ProposalRecord) (*Proposal, error) {
	var rc recipe
	if err := json.Unmarshal(rec.Recipe, &rc); err != nil {
		return nil, err
	}
	var p Proposal
	if err := json.Unmarshal(rec.Proposal, &p); err != nil {
		return nil, err
	}
	commit, err := ws.Git().CommitTree(ctx, gitx.CommitRecipe{Tree: rc.Tree, Parents: []string{rc.Parent}, Author: rc.Author, Committer: rc.Committer, Message: []byte(rc.Message)})
	if err != nil {
		return nil, err
	}
	faultpoint.Hit("freeze.committed")
	if err := ws.Git().UpdateRef(ctx, rec.Ref, commit); err != nil {
		return nil, err
	}
	faultpoint.Hit("freeze.ref_updated")
	p.HeadCommit = commit
	pJSON, _ := json.Marshal(p)
	if err := store.FreezeProposal(ctx, rec.ID, commit, pJSON, p.Hash()); err != nil {
		return nil, err
	}
	faultpoint.Hit("freeze.frozen")
	return &p, nil
}

// Recover finishes or invalidates every proposal of a run left in the
// preparing state. The recipe is authoritative: re-running it reproduces
// the commit id, and the ref is then checked against it.
func Recover(ctx context.Context, store *task.Store, ws *workspace.Workspace, runID string) ([]string, error) {
	rows, err := store.ListProposals(ctx, runID)
	if err != nil {
		return nil, err
	}
	var actions []string
	for _, rec := range rows {
		if rec.Status != task.ProposalPreparing {
			continue
		}
		var rc recipe
		if err := json.Unmarshal(rec.Recipe, &rc); err != nil {
			return actions, err
		}
		expected, err := ws.Git().CommitTree(ctx, gitx.CommitRecipe{Tree: rc.Tree, Parents: []string{rc.Parent}, Author: rc.Author, Committer: rc.Committer, Message: []byte(rc.Message)})
		if err != nil {
			return actions, err
		}
		if current, err := ws.Git().RevParse(ctx, rec.Ref); err == nil && current != expected {
			store.SetProposalStatus(ctx, rec.ID, task.ProposalInvalidated)
			actions = append(actions, rec.ID+": ref points at "+current+" not "+expected+"; invalidated")
			continue
		}
		if _, err := finish(ctx, store, ws, rec); err != nil {
			return actions, err
		}
		actions = append(actions, rec.ID+": recipe re-run, frozen at "+expected)
	}
	return actions, nil
}

// Verify checks a frozen proposal against the repository: the commit
// exists with the recorded tree and parent, the ref points at it, and the
// stored hash matches the stored body.
func Verify(ctx context.Context, store *task.Store, ws *workspace.Workspace, runID, id string) (*Proposal, error) {
	rows, err := store.ListProposals(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, rec := range rows {
		if rec.ID != id {
			continue
		}
		if rec.Status != task.ProposalFrozen {
			return nil, fmt.Errorf("proposal %s is %s, not frozen", id, rec.Status)
		}
		var p Proposal
		if err := json.Unmarshal(rec.Proposal, &p); err != nil {
			return nil, err
		}
		if p.Hash() != rec.Hash {
			return nil, fmt.Errorf("proposal %s: stored hash does not match stored body", id)
		}
		g := ws.Git()
		if ref, err := g.RevParse(ctx, rec.Ref); err != nil || ref != p.HeadCommit {
			return nil, fmt.Errorf("proposal %s: ref %s does not point at %s", id, rec.Ref, p.HeadCommit)
		}
		if tree, err := g.RevParse(ctx, p.HeadCommit+"^{tree}"); err != nil || tree != p.TreeHash {
			return nil, fmt.Errorf("proposal %s: commit tree is not %s", id, p.TreeHash)
		}
		if parent, err := g.RevParse(ctx, p.HeadCommit+"^"); err != nil || parent != p.BaseCommit {
			return nil, fmt.Errorf("proposal %s: commit parent is not %s", id, p.BaseCommit)
		}
		return &p, nil
	}
	return nil, task.ErrNotFound
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
