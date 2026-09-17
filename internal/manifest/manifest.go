// Package manifest owns every change to go.mod and go.sum. Changes are made
// by the Go toolchain against a staging copy through -modfile, verified
// against explicit rules, and promoted into the workspace under a journal
// that makes an interrupted promotion recoverable.
//
// Gate A (admission) applies when an upgrade is accepted into the
// workspace; it permits an untidy state so repair can begin. Gate B
// (normalization) applies when manifests are tidied and again before a
// proposal; it adds tidy idempotence and direct-dependency preservation.
package manifest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/joeylking/repo-steward/internal/faultpoint"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// Manifests is a go.mod and go.sum pair. Sum may be empty.
type Manifests struct {
	Mod []byte
	Sum []byte
}

// Hashes returns the SHA-256 of each file.
func (m Manifests) Hashes() (mod, sum string) {
	return hash(m.Mod), hash(m.Sum)
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ReadWorkspace reads the workspace manifests.
func ReadWorkspace(ws *workspace.Workspace) (Manifests, error) {
	mod, err := ws.ReadFile("go.mod")
	if err != nil {
		return Manifests{}, err
	}
	sum, err := ws.ReadFile("go.sum")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifests{}, err
	}
	return Manifests{Mod: mod, Sum: sum}, nil
}

// Target is the module version an upgrade moves to.
type Target struct {
	Module  string `json:"module"`
	Version string `json:"version"`
}

// Req is one require line.
type Req struct {
	Version  string
	Indirect bool
}

// Facts is the parsed view of a go.mod used by the rules.
type Facts struct {
	ModulePath         string
	GoDirective        string
	ToolchainDirective string
	Require            map[string]Req
	Replace            []string
	Exclude            []string
	Retract            []string
}

// Parse parses go.mod into Facts.
func Parse(mod []byte) (*Facts, error) {
	mf, err := modfile.Parse("go.mod", mod, nil)
	if err != nil {
		return nil, err
	}
	f := &Facts{Require: map[string]Req{}}
	if mf.Module != nil {
		f.ModulePath = mf.Module.Mod.Path
	}
	if mf.Go != nil {
		f.GoDirective = mf.Go.Version
	}
	if mf.Toolchain != nil {
		f.ToolchainDirective = mf.Toolchain.Name
	}
	for _, r := range mf.Require {
		f.Require[r.Mod.Path] = Req{Version: r.Mod.Version, Indirect: r.Indirect}
	}
	for _, r := range mf.Replace {
		f.Replace = append(f.Replace, r.Old.Path+"@"+r.Old.Version+"=>"+r.New.Path+"@"+r.New.Version)
	}
	for _, e := range mf.Exclude {
		f.Exclude = append(f.Exclude, e.Mod.Path+"@"+e.Mod.Version)
	}
	for _, r := range mf.Retract {
		f.Retract = append(f.Retract, r.Low+"-"+r.High)
	}
	sort.Strings(f.Replace)
	sort.Strings(f.Exclude)
	sort.Strings(f.Retract)
	return f, nil
}

// Violation is one broken rule.
type Violation struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Change is a require line that moved.
type Change struct {
	Module   string `json:"module"`
	From     string `json:"from"`
	To       string `json:"to"`
	Indirect bool   `json:"indirect"`
}

// Verification is the result of applying the rules.
type Verification struct {
	Gate             string      `json:"gate"`
	Target           Target      `json:"target"`
	ClosureIncreases []Change    `json:"closure_increases,omitempty"`
	IndirectChanges  []Change    `json:"indirect_changes,omitempty"`
	Violations       []Violation `json:"violations,omitempty"`
}

// OK reports whether no rule was broken.
func (v Verification) OK() bool { return len(v.Violations) == 0 }

// Violation codes.
const (
	CodeTargetNotSet      = "target_not_set"
	CodeVersionDecreased  = "version_decreased"
	CodeOutsideClosure    = "increase_outside_closure"
	CodeDirectRemoved     = "direct_dependency_removed"
	CodeDirectAdded       = "direct_dependency_added"
	CodeReplaceChanged    = "replace_changed"
	CodeExcludeChanged    = "exclude_changed"
	CodeRetractChanged    = "retract_changed"
	CodeGoDirective       = "go_directive_changed"
	CodeToolchainChanged  = "toolchain_changed"
	CodeModulePathChanged = "module_path_changed"
	CodeNotTidy           = "not_tidy"
	CodeVerifyFailed      = "verify_failed"
	CodeTargetNotResolved = "target_not_resolved"
)

// VerifyAdmission applies Gate A. closure is the set of module paths in
// the requirement graph reachable from the target at its new version.
func VerifyAdmission(base, cand *Facts, target Target, closure map[string]bool) Verification {
	v := Verification{Gate: "A", Target: target}
	add := func(code, format string, args ...any) {
		v.Violations = append(v.Violations, Violation{Code: code, Detail: fmt.Sprintf(format, args...)})
	}
	if base.ModulePath != cand.ModulePath {
		add(CodeModulePathChanged, "%s -> %s", base.ModulePath, cand.ModulePath)
	}
	if base.GoDirective != cand.GoDirective {
		add(CodeGoDirective, "%s -> %s", base.GoDirective, cand.GoDirective)
	}
	if base.ToolchainDirective != cand.ToolchainDirective {
		add(CodeToolchainChanged, "%q -> %q", base.ToolchainDirective, cand.ToolchainDirective)
	}
	if strings.Join(base.Replace, ",") != strings.Join(cand.Replace, ",") {
		add(CodeReplaceChanged, "replace directives differ")
	}
	if strings.Join(base.Exclude, ",") != strings.Join(cand.Exclude, ",") {
		add(CodeExcludeChanged, "exclude directives differ")
	}
	if strings.Join(base.Retract, ",") != strings.Join(cand.Retract, ",") {
		add(CodeRetractChanged, "retract directives differ")
	}
	if got, ok := cand.Require[target.Module]; !ok || got.Version != target.Version {
		add(CodeTargetNotSet, "%s is %q, want %s", target.Module, got.Version, target.Version)
	}
	mods := map[string]bool{}
	for m := range base.Require {
		mods[m] = true
	}
	for m := range cand.Require {
		mods[m] = true
	}
	sorted := make([]string, 0, len(mods))
	for m := range mods {
		sorted = append(sorted, m)
	}
	sort.Strings(sorted)
	for _, m := range sorted {
		if m == target.Module {
			continue
		}
		b, inBase := base.Require[m]
		c, inCand := cand.Require[m]
		switch {
		case inBase && inCand:
			cmp := semver.Compare(c.Version, b.Version)
			if cmp < 0 {
				add(CodeVersionDecreased, "%s %s -> %s", m, b.Version, c.Version)
			} else if cmp > 0 {
				if !closure[m] {
					add(CodeOutsideClosure, "%s %s -> %s is not required by %s@%s", m, b.Version, c.Version, target.Module, target.Version)
				}
				v.ClosureIncreases = append(v.ClosureIncreases, Change{Module: m, From: b.Version, To: c.Version, Indirect: c.Indirect})
			} else if b.Indirect != c.Indirect {
				v.IndirectChanges = append(v.IndirectChanges, Change{Module: m, From: b.Version, To: c.Version, Indirect: c.Indirect})
			}
		case inCand && !inBase:
			if !c.Indirect {
				add(CodeDirectAdded, "%s %s", m, c.Version)
			} else if !closure[m] {
				add(CodeOutsideClosure, "%s %s added but not required by %s@%s", m, c.Version, target.Module, target.Version)
			} else {
				v.IndirectChanges = append(v.IndirectChanges, Change{Module: m, To: c.Version, Indirect: true})
			}
		case inBase && !inCand:
			if !b.Indirect {
				add(CodeDirectRemoved, "%s %s", m, b.Version)
			} else {
				v.IndirectChanges = append(v.IndirectChanges, Change{Module: m, From: b.Version, Indirect: true})
			}
		}
	}
	return v
}

// VerifyNormalized applies Gate B's manifest rules: Gate A plus the direct
// require set (excluding the target) being unchanged. Tidy idempotence and
// cache verification are checked by the caller and appended with AddViolation.
func VerifyNormalized(base, cand *Facts, target Target, closure map[string]bool) Verification {
	v := VerifyAdmission(base, cand, target, closure)
	v.Gate = "B"
	for m, b := range base.Require {
		if m == target.Module || b.Indirect {
			continue
		}
		if c, ok := cand.Require[m]; ok && c.Indirect {
			v.Violations = append(v.Violations, Violation{Code: CodeDirectRemoved, Detail: m + " became indirect"})
		}
	}
	return v
}

// AddViolation appends a caller-detected violation.
func (v *Verification) AddViolation(code, detail string) {
	v.Violations = append(v.Violations, Violation{Code: code, Detail: detail})
}

// Op is a manifest operation.
type Op struct {
	Kind   string // upgrade normalize
	Target Target
}

// Staging is a manifest operation's result, not yet in the workspace.
type Staging struct {
	ID         string    `json:"id"`
	Dir        string    `json:"dir"`
	Op         Op        `json:"op"`
	Before     Manifests `json:"-"`
	After      Manifests `json:"-"`
	Idempotent bool      `json:"idempotent"` // normalize only: tidy twice gave the same result
	Stderr     string    `json:"stderr,omitempty"`
}

// Stage copies the workspace manifests into a fresh staging directory and
// runs the operation there through -modfile in the mutate profile. sb must
// be bound to a materialized snapshot of the workspace's current tree so the
// toolchain reads exactly the sources the candidate tree contains.
func Stage(ctx context.Context, sb *sandbox.Docker, ws *workspace.Workspace, stagingRoot string, op Op) (*Staging, error) {
	before, err := ReadWorkspace(ws)
	if err != nil {
		return nil, err
	}
	id := newID()
	dir := filepath.Join(stagingRoot, id)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), before.Mod, 0o666); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), before.Sum, 0o666); err != nil {
		return nil, err
	}
	os.Chmod(filepath.Join(dir, "go.mod"), 0o666)
	os.Chmod(filepath.Join(dir, "go.sum"), 0o666)
	st := &Staging{ID: id, Dir: dir, Op: op, Before: before}
	sbs := sb.WithStaging(dir)
	var argv []string
	switch op.Kind {
	case "upgrade":
		if op.Target.Module == "" || op.Target.Version == "" {
			return nil, errors.New("manifest: upgrade needs a module and version")
		}
		argv = []string{"go", "get", "-modfile=/staging/go.mod", op.Target.Module + "@" + op.Target.Version}
	case "normalize":
		argv = []string{"go", "mod", "tidy", "-modfile=/staging/go.mod"}
	default:
		return nil, fmt.Errorf("manifest: unknown operation %q", op.Kind)
	}
	res, err := sbs.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Mutate, Argv: argv, Timeout: 5 * time.Minute, StepID: "manifest-" + op.Kind})
	if err != nil {
		return nil, err
	}
	st.Stderr = string(res.Stderr)
	if res.TimedOut || res.ExitCode != 0 {
		return st, &OpError{Op: op, ExitCode: res.ExitCode, Stderr: strings.TrimSpace(string(res.Stderr)), TimedOut: res.TimedOut}
	}
	if st.After, err = readDir(dir); err != nil {
		return nil, err
	}
	if op.Kind == "normalize" {
		res, err := sbs.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Mutate, Argv: argv, Timeout: 5 * time.Minute, StepID: "manifest-normalize-check"})
		if err != nil {
			return nil, err
		}
		again, err := readDir(dir)
		if err != nil {
			return nil, err
		}
		st.Idempotent = res.ExitCode == 0 && string(again.Mod) == string(st.After.Mod) && string(again.Sum) == string(st.After.Sum)
		st.After = again
	}
	return st, nil
}

// OpError reports a toolchain failure during staging.
type OpError struct {
	Op       Op
	ExitCode int
	Stderr   string
	TimedOut bool
}

func (e *OpError) Error() string {
	if e.TimedOut {
		return fmt.Sprintf("manifest: %s timed out", e.Op.Kind)
	}
	return fmt.Sprintf("manifest: %s exit %d: %s", e.Op.Kind, e.ExitCode, e.Stderr)
}

// RequiresNewerToolchain reports whether the toolchain refused because the
// target needs a newer Go than the pinned image provides.
func (e *OpError) RequiresNewerToolchain() bool {
	return strings.Contains(e.Stderr, "requires go >=") || strings.Contains(e.Stderr, "requires go ") && strings.Contains(e.Stderr, "running go ")
}

func readDir(dir string) (Manifests, error) {
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return Manifests{}, err
	}
	sum, err := os.ReadFile(filepath.Join(dir, "go.sum"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifests{}, err
	}
	return Manifests{Mod: mod, Sum: sum}, nil
}

// Closure returns the module paths reachable from target in the module
// graph of the staged manifests. It runs go mod graph in the mutate
// profile against the staging directory.
func Closure(ctx context.Context, sb *sandbox.Docker, st *Staging, target Target) (map[string]bool, error) {
	res, err := sb.WithStaging(st.Dir).Run(ctx, sandbox.ExecSpec{Profile: sandbox.Mutate, Argv: []string{"go", "mod", "graph", "-modfile=/staging/go.mod"}, Timeout: 5 * time.Minute, StepID: "manifest-graph"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 || res.TimedOut {
		return nil, fmt.Errorf("manifest: go mod graph exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return ClosureFromGraph(string(res.Stdout), target), nil
}

// ClosureFromGraph computes the closure from go mod graph output.
func ClosureFromGraph(graph string, target Target) map[string]bool {
	edges := map[string][]string{}
	for _, line := range strings.Split(graph, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			edges[f[0]] = append(edges[f[0]], f[1])
		}
	}
	start := target.Module + "@" + target.Version
	seen := map[string]bool{start: true}
	queue := []string{start}
	out := map[string]bool{}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, next := range edges[n] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
			if i := strings.LastIndex(next, "@"); i > 0 {
				out[next[:i]] = true
			}
		}
	}
	return out
}

// ErrConflict means the workspace manifests are not in a state the journal
// can explain.
var ErrConflict = errors.New("manifest: workspace manifests do not match the promotion journal")

// Promote moves a staged result into the workspace under the journal.
// Recovery can finish or abort an interrupted promotion from the journal
// and the workspace files alone; the staging directory is retained until
// the promotion is terminal but is not required.
func Promote(ctx context.Context, store *task.Store, ws *workspace.Workspace, runID, stepID string, st *Staging) (*task.Promotion, error) {
	current, err := ReadWorkspace(ws)
	if err != nil {
		return nil, err
	}
	bm, bs := current.Hashes()
	sbm, sbs := st.Before.Hashes()
	if bm != sbm || bs != sbs {
		return nil, fmt.Errorf("%w: manifests changed since staging", ErrConflict)
	}
	am, as := st.After.Hashes()
	p := task.Promotion{ID: newID(), RunID: runID, StepID: stepID, Kind: st.Op.Kind, TargetModule: st.Op.Target.Module, TargetVersion: st.Op.Target.Version,
		BeforeModSHA: bm, BeforeSumSHA: bs, AfterModSHA: am, AfterSumSHA: as, StagingDir: st.Dir, Status: task.PromotionStaged}
	if err := store.InsertPromotion(ctx, p); err != nil {
		return nil, err
	}
	faultpoint.Hit("promote.staged")
	if err := store.SetPromotionStatus(ctx, p.ID, task.PromotionPromoting, ""); err != nil {
		return nil, err
	}
	p.Status = task.PromotionPromoting
	faultpoint.Hit("promote.promoting")
	if err := writeAtomic(filepath.Join(ws.Dir, "go.mod"), st.After.Mod); err != nil {
		return nil, err
	}
	faultpoint.Hit("promote.after_mod")
	if err := writeAtomic(filepath.Join(ws.Dir, "go.sum"), st.After.Sum); err != nil {
		return nil, err
	}
	faultpoint.Hit("promote.after_sum")
	if err := store.SetPromotionStatus(ctx, p.ID, task.PromotionPromoted, ""); err != nil {
		return nil, err
	}
	p.Status = task.PromotionPromoted
	faultpoint.Hit("promote.promoted")
	os.RemoveAll(st.Dir)
	return &p, nil
}

// RecoveryAction describes what recovery did to one promotion.
type RecoveryAction struct {
	PromotionID string               `json:"promotion_id"`
	From        task.PromotionStatus `json:"from"`
	To          task.PromotionStatus `json:"to"`
	Action      string               `json:"action"`
}

// Recover reconciles every non-terminal promotion of a run against the
// workspace files. It never consults staging; the journal's after-hashes
// are sufficient to finish, and its before-hashes to know nothing happened.
func Recover(ctx context.Context, store *task.Store, ws *workspace.Workspace, runID string) ([]RecoveryAction, error) {
	promotions, err := store.ListPromotions(ctx, runID)
	if err != nil {
		return nil, err
	}
	var actions []RecoveryAction
	for _, p := range promotions {
		if p.Status.Terminal() {
			continue
		}
		current, err := ReadWorkspace(ws)
		if err != nil {
			return actions, err
		}
		cm, cs := current.Hashes()
		act := RecoveryAction{PromotionID: p.ID, From: p.Status}
		switch {
		case p.Status == task.PromotionStaged:
			act.To, act.Action = task.PromotionAborted, "never started; aborted"
		case cm == p.BeforeModSHA && cs == p.BeforeSumSHA:
			act.To, act.Action = task.PromotionAborted, "nothing changed; aborted"
		case cm == p.AfterModSHA && cs == p.BeforeSumSHA:
			// go.mod landed, go.sum did not: finish from the journal.
			if err := finishSum(store, ws, p); err != nil {
				return actions, err
			}
			act.To, act.Action = task.PromotionPromoted, "go.sum restored from staging; promoted"
		case cm == p.AfterModSHA && cs == p.AfterSumSHA:
			act.To, act.Action = task.PromotionPromoted, "both files present; promoted"
		default:
			act.To, act.Action = task.PromotionConflict, "workspace manifests match neither before nor after"
		}
		if err := store.SetPromotionStatus(ctx, p.ID, act.To, act.Action); err != nil {
			return actions, err
		}
		if act.To == task.PromotionConflict {
			actions = append(actions, act)
			return actions, fmt.Errorf("%w: promotion %s", ErrConflict, p.ID)
		}
		os.RemoveAll(p.StagingDir)
		actions = append(actions, act)
	}
	return actions, nil
}

// finishSum completes a promotion whose go.mod landed but go.sum did not.
// The after go.sum is taken from the retained staging directory when it
// exists; otherwise the promotion cannot be finished by content and is a
// conflict, because go.sum bytes are not derivable from a hash.
func finishSum(store *task.Store, ws *workspace.Workspace, p task.Promotion) error {
	sum, err := os.ReadFile(filepath.Join(p.StagingDir, "go.sum"))
	if err != nil {
		return fmt.Errorf("%w: go.mod promoted but go.sum content unavailable (staging %s): %v", ErrConflict, p.StagingDir, err)
	}
	if hash(sum) != p.AfterSumSHA {
		return fmt.Errorf("%w: staging go.sum does not match the journal", ErrConflict)
	}
	return writeAtomic(filepath.Join(ws.Dir, "go.sum"), sum)
}

// UpgradeCommitted reports whether an upgrade promotion has started or
// finished, which closes candidate selection for the run.
func UpgradeCommitted(promotions []task.Promotion) bool {
	for _, p := range promotions {
		if p.Kind == "upgrade" && (p.Status == task.PromotionPromoting || p.Status == task.PromotionPromoted) {
			return true
		}
	}
	return false
}

func writeAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repo-steward-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
