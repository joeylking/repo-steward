// Package session holds the per-run state that tools and policy share:
// the workspace, sandbox, stores, profile, candidate facts, baseline
// evidence, and limits. Tools act through it; policy asks it questions.
// It performs no policy decisions itself.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/validate"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// ScopeConfig bounds source edits. Soft limits may be raised once to the
// hard limits by an approval; hard limits are absolute.
type ScopeConfig struct {
	FilesSoft int `json:"files_soft"`
	FilesHard int `json:"files_hard"`
	LinesSoft int `json:"lines_soft"`
	LinesHard int `json:"lines_hard"`
}

// DefaultScope is conservative for a single dependency upgrade.
func DefaultScope() ScopeConfig {
	return ScopeConfig{FilesSoft: 10, FilesHard: 20, LinesSoft: 200, LinesHard: 400}
}

// Budgets bound the repair loop independently of runtime limits.
type Budgets struct {
	MaxValidationCycles     int `json:"max_validation_cycles"`
	MaxUnchangedValidations int `json:"max_unchanged_validations"`
}

// DefaultBudgets allow a handful of repair iterations.
func DefaultBudgets() Budgets { return Budgets{MaxValidationCycles: 8, MaxUnchangedValidations: 3} }

// Outcome is what a terminal tool recorded.
type Outcome struct {
	Code   string `json:"code"`
	Detail any    `json:"detail,omitempty"`
}

// Session is one run's shared state.
type Session struct {
	RunID        string
	Store        *task.Store
	Runtime      *agentrt.Store
	WS           *workspace.Workspace
	SB           *sandbox.Docker
	Profile      *repo.Profile
	Candidates   []deps.Candidate
	Policy       deps.Policy
	Baseline     *validate.Run
	BaselineID   string
	ConfigHash   string
	RunDir       string
	StagingRoot  string
	ModCacheDir  string
	Limits       snapshot.Limits
	Scope        ScopeConfig
	Budgets      Budgets
	Author       gitx.Identity
	CheckTimeout time.Duration
	BaseRef      string

	// Outcome is set by terminal tools.
	Outcome *Outcome
}

// Phases of a run, derived from the promotion journal.
const (
	PhaseSelect = "select"
	PhaseRepair = "repair"
)

// Phase reports whether an upgrade has been admitted.
func (s *Session) Phase(ctx context.Context) (string, error) {
	proms, err := s.Store.ListPromotions(ctx, s.RunID)
	if err != nil {
		return "", err
	}
	if manifest.UpgradeCommitted(proms) {
		return PhaseRepair, nil
	}
	return PhaseSelect, nil
}

// Target returns the admitted upgrade target, if any.
func (s *Session) Target(ctx context.Context) (manifest.Target, bool, error) {
	proms, err := s.Store.ListPromotions(ctx, s.RunID)
	if err != nil {
		return manifest.Target{}, false, err
	}
	for _, p := range proms {
		if p.Kind == "upgrade" && (p.Status == task.PromotionPromoting || p.Status == task.PromotionPromoted) {
			return manifest.Target{Module: p.TargetModule, Version: p.TargetVersion}, true, nil
		}
	}
	return manifest.Target{}, false, nil
}

// EligibleTarget reports whether the exact module and version pair is an
// eligible target according to the discovered facts and policy.
func (s *Session) EligibleTarget(module, version string) (bool, []string) {
	for _, c := range s.Candidates {
		if c.Module != module {
			continue
		}
		for _, t := range c.Targets {
			if t.Version == version {
				return t.Eligible, t.Reasons
			}
		}
		return false, []string{"version is not among the listed versions"}
	}
	return false, []string{"module is not an outdated direct dependency"}
}

// IsProtected reports whether the path may never be written by the agent.
func (s *Session) IsProtected(path string) bool {
	return repo.IsProtected(path, s.Profile.ProtectedGlobs)
}

// IsIgnored reports whether the path would be ignored by the repository's
// ignore rules, in which case it could never enter a candidate tree.
func (s *Session) IsIgnored(ctx context.Context, path string) (bool, error) {
	_, err := s.WS.Git().Run(ctx, "check-ignore", "-q", "--", path)
	if err == nil {
		return true, nil
	}
	var ge *gitx.Error
	if errors.As(err, &ge) {
		if ee, ok := ge.Err.(interface{ ExitCode() int }); ok && ee.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, err
}

// CurrentDiff is the change set of the working tree against the base tree.
func (s *Session) CurrentDiff(ctx context.Context) (workspace.ChangeSet, error) {
	tree, err := s.WS.CandidateTree(ctx)
	if err != nil {
		return workspace.ChangeSet{}, err
	}
	return s.WS.Diff(ctx, tree)
}

// ProjectedScope computes the source-file and line counts the diff would
// have if path held content, without touching the workspace. Manifests
// are excluded; the manifest rules bound them.
func (s *Session) ProjectedScope(ctx context.Context, path string, content []byte) (files, lines int, err error) {
	cs, err := s.CurrentDiff(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, f := range cs.Files {
		if f.Path == "go.mod" || f.Path == "go.sum" || f.Path == path {
			continue
		}
		files++
		lines += f.Added + f.Removed
	}
	base, _ := s.WS.Git().Run(ctx, "show", s.WS.BaseTree+":"+path) // empty when absent
	added, removed, err := lineDelta(ctx, base, content)
	if err != nil {
		return 0, 0, err
	}
	if added+removed > 0 || len(base) == 0 {
		files++
		lines += added + removed
	}
	return files, lines, nil
}

// lineDelta counts added and removed lines between two contents using Git.
func lineDelta(ctx context.Context, before, after []byte) (int, int, error) {
	dir, err := os.MkdirTemp("", "repo-steward-delta-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(dir)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(a, before, 0o644); err != nil {
		return 0, 0, err
	}
	if err := os.WriteFile(b, after, 0o644); err != nil {
		return 0, 0, err
	}
	out, err := gitx.New(dir).Run(ctx, "diff", "--no-index", "--numstat", "--", "a", "b")
	var ge *gitx.Error
	if err != nil && !(errors.As(err, &ge) && strings.TrimSpace(string(out)) != "") {
		// exit 1 with output means differences; anything else is an error
		if err != nil && strings.TrimSpace(string(out)) == "" {
			if errors.As(err, &ge) {
				return 0, 0, nil // identical
			}
			return 0, 0, err
		}
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, 0, nil
	}
	var added, removed int
	fmt.Sscanf(fields[0], "%d", &added)
	fmt.Sscanf(fields[1], "%d", &removed)
	return added, removed, nil
}

// CandidateSnapshot builds and materializes the current candidate tree and
// returns a sandbox bound to it with the module cache populated.
func (s *Session) CandidateSnapshot(ctx context.Context) (string, string, *sandbox.Docker, error) {
	tree, err := s.WS.CandidateTree(ctx)
	if err != nil {
		return "", "", nil, err
	}
	dir, _, err := s.WS.Materialize(ctx, tree, filepath.Join(s.RunDir, "snapshots", tree), s.Limits)
	if err != nil {
		return "", "", nil, err
	}
	sb := s.SB.WithSource(dir)
	if err := deps.Download(ctx, sb); err != nil {
		return "", "", nil, err
	}
	return tree, dir, sb, nil
}

// Validate runs the checks on the current candidate tree and records the
// evidence under the producing step. Acceptance follows the step's status.
func (s *Session) Validate(ctx context.Context, stepID string) (*validate.Run, string, error) {
	tree, _, sb, err := s.CandidateSnapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	vr, err := validate.Baseline(ctx, sb, validate.Options{Kind: "post", TreeHash: tree, ToolchainDigest: s.Profile.Toolchain.Digest, CheckTimeout: s.CheckTimeout, RunID: s.RunID, StepID: stepID})
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(vr)
	if err != nil {
		return nil, "", err
	}
	id := NewID()
	if err := s.Store.InsertValidation(ctx, task.ValidationRecord{ID: id, RunID: s.RunID, StepID: stepID, Kind: "post", TreeHash: tree, ConfigHash: s.ConfigHash, ToolchainDigest: s.Profile.Toolchain.Digest, Run: raw}); err != nil {
		return nil, "", err
	}
	return vr, id, nil
}

// StepDone reports whether the runtime recorded the step as done, which
// is the acceptance rule for evidence produced by tools.
func (s *Session) StepDone(ctx context.Context, stepID string) bool {
	steps, err := s.Runtime.ListSteps(ctx, s.RunID)
	if err != nil {
		return false
	}
	for _, st := range steps {
		if st.ID == stepID {
			return st.Status == agentrt.StepDone
		}
	}
	return false
}

// ModuleDir returns the extracted module directory in the cache.
func (s *Session) ModuleDir(module, version string) (string, error) {
	esc, err := escapePath(module)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.ModCacheDir, "mod", filepath.FromSlash(esc)+"@"+version), nil
}

func escapePath(p string) (string, error) {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// NewID returns a random hex identifier.
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
