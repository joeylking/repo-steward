// Package inspect implements the no-model inspection workflow: profile the
// repository at HEAD, materialize an exact snapshot, populate the module
// cache, discover upgrade candidates, and validate the baseline. Every fact
// in the report comes from deterministic code.
package inspect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/module"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/lock"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/validate"
)

// Options configure an inspection.
type Options struct {
	RepoPath string
	DataDir  string
	// FixtureProxyDir, when set, replaces the network module proxy with a
	// file-based one and disables the checksum database. Fixture use only.
	FixtureProxyDir string
	// AllowPull permits the one-time image pull.
	AllowPull    bool
	Socket       string
	Policy       deps.Policy
	Limits       snapshot.Limits
	CheckTimeout time.Duration
}

// Outcome classifies the result.
type Outcome string

const (
	OutcomeOK                   Outcome = "ok"
	OutcomeUnsupported          Outcome = "unsupported"
	OutcomeBaselineFailing      Outcome = "baseline_failing"
	OutcomeBaselineInconclusive Outcome = "baseline_inconclusive"
)

// Report is the structured result.
type Report struct {
	Repo          string           `json:"repo"`
	HeadCommit    string           `json:"head_commit"`
	TreeHash      string           `json:"tree_hash"`
	WorktreeDirty bool             `json:"worktree_dirty"`
	Snapshot      *SnapshotSummary `json:"snapshot,omitempty"`
	Profile       *repo.Profile    `json:"profile,omitempty"`
	Outcome       Outcome          `json:"outcome"`
	Refusals      []string         `json:"refusals,omitempty"`
	ImageRef      string           `json:"image_ref,omitempty"`
	Candidates    []deps.Candidate `json:"candidates,omitempty"`
	Baseline      *validate.Run    `json:"baseline,omitempty"`
	Timings       map[string]int64 `json:"timings_ms"`
}

// SnapshotSummary omits the entry list from the report.
type SnapshotSummary struct {
	Dir        string `json:"dir"`
	FileCount  int    `json:"file_count"`
	TotalBytes int64  `json:"total_bytes"`
	Verified   bool   `json:"verified"`
}

// DefaultDataDir is $XDG_DATA_HOME/repo-steward or ~/.local/share/repo-steward.
// It lives under the home directory so that VM-backed container engines can
// share it.
func DefaultDataDir() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "repo-steward"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "repo-steward"), nil
}

// Run performs the inspection.
func Run(ctx context.Context, opts Options) (*Report, error) {
	start := time.Now()
	rep := &Report{Timings: map[string]int64{}}
	mark := func(name string, since time.Time) { rep.Timings[name] = time.Since(since).Milliseconds() }

	repoPath, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		return nil, err
	}
	rep.Repo = repoPath
	if opts.DataDir == "" {
		if opts.DataDir, err = DefaultDataDir(); err != nil {
			return nil, err
		}
	}
	if opts.Limits == (snapshot.Limits{}) {
		opts.Limits = snapshot.DefaultLimits()
	}
	if err := lock.CheckLocal(opts.DataDir); err != nil {
		return nil, err
	}
	// One executing inspection or maintenance run per data directory.
	exec, err := lock.Acquire(filepath.Join(opts.DataDir, "executor.lock"))
	if err != nil {
		return nil, err
	}
	defer exec.Release()

	h, err := readHead(ctx, repoPath, opts.DataDir, opts.Limits, mark)
	if err != nil {
		return nil, err
	}
	rep.HeadCommit, rep.TreeHash, rep.WorktreeDirty, rep.Snapshot, rep.Profile = h.headCommit, h.treeHash, h.dirty, h.snapshot, h.profile
	if h.refusals != nil {
		rep.Outcome = OutcomeUnsupported
		rep.Refusals = h.refusals
		mark("total", start)
		return rep, nil
	}
	prof := h.profile

	// Sandbox.
	t := time.Now()
	sb, cleanup, err := startSandbox(ctx, "inspect", opts.DataDir, h, opts.FixtureProxyDir, opts.Socket, opts.AllowPull)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rep.ImageRef = prof.Toolchain.Ref()
	mark("sandbox", t)

	// Dependencies: populate the cache, then discover.
	t = time.Now()
	if err := deps.Download(ctx, sb); err != nil {
		return nil, err
	}
	mark("download", t)
	t = time.Now()
	if rep.Candidates, err = deps.Discover(ctx, sb, opts.Policy); err != nil {
		return nil, err
	}
	mark("discover", t)

	// Baseline validation.
	t = time.Now()
	vsb, err := sb.WithFreshBuildCache()
	if err != nil {
		return nil, err
	}
	rep.Baseline, err = validate.Baseline(ctx, vsb, validate.Options{Kind: "baseline", TreeHash: rep.TreeHash, ToolchainDigest: prof.Toolchain.Digest, CheckTimeout: opts.CheckTimeout, StepID: "baseline"})
	if err != nil {
		return nil, err
	}
	mark("baseline", t)
	switch {
	case !rep.Baseline.Conclusive:
		rep.Outcome = OutcomeBaselineInconclusive
	case !rep.Baseline.Clean:
		rep.Outcome = OutcomeBaselineFailing
	default:
		rep.Outcome = OutcomeOK
	}
	mark("total", start)
	return rep, nil
}

// head is what both workflows learn before the sandbox starts.
type head struct {
	headCommit, treeHash string
	dirty                bool
	snapshot             *SnapshotSummary
	profile              *repo.Profile
	// refusals is non-nil when the repository is unsupported.
	refusals []string
}

// readHead reads HEAD's facts, materializes and verifies its tree, and
// profiles it. A tree or profile refusal is returned in head.refusals.
func readHead(ctx context.Context, repoPath, dataDir string, limits snapshot.Limits, mark func(string, time.Time)) (*head, error) {
	h := &head{}
	g := gitx.New(repoPath)
	var err error
	if h.headCommit, err = g.RevParse(ctx, "HEAD"); err != nil {
		return nil, fmt.Errorf("inspect: %s is not a Git repository with commits: %w", repoPath, err)
	}
	if h.treeHash, err = g.RevParse(ctx, "HEAD^{tree}"); err != nil {
		return nil, err
	}
	status, err := g.Run(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	h.dirty = len(strings.TrimSpace(string(status))) > 0

	// Listing and refusals that precede materialization.
	t := time.Now()
	entries, err := snapshot.List(ctx, g, h.treeHash, limits)
	if err != nil {
		if r := repo.RefusalForListError(err); r != "" {
			h.refusals = []string{r}
			return h, nil
		}
		return nil, err
	}
	snapDir, err := ensureSnapshot(ctx, g, h.treeHash, filepath.Join(dataDir, "snapshots", h.treeHash), entries, limits)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	h.snapshot = &SnapshotSummary{Dir: snapDir, FileCount: len(entries), TotalBytes: total, Verified: true}
	mark("snapshot", t)

	t = time.Now()
	prof, err := repo.Inspect(snapDir, entries)
	if err != nil {
		return nil, err
	}
	h.profile = prof
	mark("profile", t)
	if !prof.Supported() {
		h.refusals = prof.Refusals
		if h.refusals == nil {
			h.refusals = []string{}
		}
	}
	return h, nil
}

// startSandbox starts the sandbox for a supported head: image present,
// orphans reaped, mounts probed. cleanup removes the run's build caches,
// through a container when the host cannot, and must be called.
func startSandbox(ctx context.Context, name, dataDir string, h *head, fixtureProxy, socket string, allowPull bool) (*sandbox.Docker, func(), error) {
	key, err := module.EscapePath(h.profile.ModulePath)
	if err != nil {
		return nil, nil, err
	}
	buildCache := filepath.Join(dataDir, "tmp", name+"-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	cfg := sandbox.Config{
		Image:         h.profile.Toolchain.Ref(),
		SourceDir:     h.snapshot.Dir,
		CacheDir:      filepath.Join(dataDir, "cache", "mod", filepath.FromSlash(key)),
		BuildCacheDir: buildCache,
	}
	if fixtureProxy != "" {
		if cfg.ProxyDir, err = filepath.Abs(fixtureProxy); err != nil {
			return nil, nil, err
		}
	}
	sb, err := sandbox.NewDocker(cfg, socket)
	if err != nil {
		removeAll(buildCache)
		return nil, nil, err
	}
	// RemoveBuildCaches runs before removeAll: on a native Linux engine
	// only the container user can delete what a container wrote.
	cleanup := func() {
		sb.RemoveBuildCaches(context.WithoutCancel(ctx))
		removeAll(buildCache)
	}
	if err := sb.EnsureImage(ctx, allowPull); err != nil {
		cleanup()
		return nil, nil, err
	}
	if _, err := sb.ReapOrphans(ctx); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := sb.Probe(ctx); err != nil {
		cleanup()
		return nil, nil, err
	}
	return sb, cleanup, nil
}

// removeAll deletes a tree the toolchain may have made read-only.
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

// ensureSnapshot materializes tree into dir, reusing a directory that still
// verifies. One that does not verify is left alone and a fresh directory
// with a unique suffix is used, because a deleted and recreated path can
// stay invisible to a VM-backed container engine. The directory used is
// returned.
func ensureSnapshot(ctx context.Context, g *gitx.Git, tree, dir string, entries []snapshot.Entry, limits snapshot.Limits) (string, error) {
	if _, err := os.Stat(dir); err == nil {
		if snapshot.Verify(dir, entries) == nil {
			return dir, nil
		}
		dir = fmt.Sprintf("%s-%d", dir, time.Now().UnixNano())
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	m, err := snapshot.Materialize(ctx, g, tree, dir, limits)
	if err != nil {
		return "", err
	}
	if !m.Verified {
		return "", snapshot.ErrVerify
	}
	return dir, nil
}
