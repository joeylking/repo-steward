// Package workspace manages the scratch clone a maintenance run works in.
// The operator's checkout is read once, at clone time, and never written.
// The clone keeps its Git directory outside the working tree so that no Git
// metadata is ever inside a directory a container mounts.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/snapshot"
)

// Workspace is a scratch clone at a known base commit.
type Workspace struct {
	Dir        string `json:"dir"`
	GitDir     string `json:"git_dir"`
	SourcePath string `json:"source_path"`
	BaseCommit string `json:"base_commit"`
	BaseTree   string `json:"base_tree"`
	// BaseRef is the branch the operator's checkout was on, or "" when detached.
	BaseRef string `json:"base_ref"`

	git *gitx.Git
}

// ErrDirtyWorktree means the operator's checkout has uncommitted changes.
var ErrDirtyWorktree = errors.New("workspace: source checkout has uncommitted changes; commit or stash them first")

// Create clones sourcePath into root/work with the Git directory at
// root/gitdir, checked out at the source's HEAD commit.
func Create(ctx context.Context, sourcePath, root string) (*Workspace, error) {
	src, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, err
	}
	sg := gitx.New(src)
	head, err := sg.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("workspace: %s has no HEAD commit: %w", src, err)
	}
	status, err := sg.Run(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(status))) > 0 {
		return nil, ErrDirtyWorktree
	}
	baseRef := ""
	if out, err := sg.Run(ctx, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		baseRef = strings.TrimSpace(string(out))
	}

	dir := filepath.Join(root, "work")
	gitDir := filepath.Join(root, "gitdir")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	// Clone through a Git bound to the parent so hooks and config isolation
	// apply to the clone command itself.
	pg := gitx.New(root)
	if _, err := pg.Run(ctx, "clone", "-q", "--no-checkout", "--separate-git-dir="+gitDir, src, dir); err != nil {
		return nil, err
	}
	g := gitx.New(dir)
	if _, err := g.Run(ctx, "checkout", "-q", "--detach", head); err != nil {
		return nil, err
	}
	tree, err := g.RevParse(ctx, "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	return &Workspace{Dir: dir, GitDir: gitDir, SourcePath: src, BaseCommit: head, BaseTree: tree, BaseRef: baseRef, git: g}, nil
}

// Open reattaches to an existing workspace.
func Open(ctx context.Context, dir string) (*Workspace, error) {
	g := gitx.New(dir)
	head, err := g.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	tree, err := g.RevParse(ctx, "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	gitDir, err := g.Run(ctx, "rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	return &Workspace{Dir: dir, GitDir: strings.TrimSpace(string(gitDir)), BaseCommit: head, BaseTree: tree, git: g}, nil
}

// Git returns the workspace's Git handle.
func (w *Workspace) Git() *gitx.Git { return w.git }

// CandidateTree writes the current working tree as a tree object seeded
// from the base tree.
func (w *Workspace) CandidateTree(ctx context.Context) (string, error) {
	return snapshot.BuildCandidateTree(ctx, w.git, w.BaseTree)
}

// Materialize writes tree into dir exactly, reusing a verified directory.
func (w *Workspace) Materialize(ctx context.Context, tree, dir string, limits snapshot.Limits) ([]snapshot.Entry, error) {
	entries, err := snapshot.List(ctx, w.git, tree, limits)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		if snapshot.Verify(dir, entries) == nil {
			return entries, nil
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
	}
	m, err := snapshot.Materialize(ctx, w.git, tree, dir, limits)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if !m.Verified {
		os.RemoveAll(dir)
		return nil, snapshot.ErrVerify
	}
	return entries, nil
}

// FileChange is one changed path between two trees.
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A M D
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary"`
}

// ChangeSet describes the difference between the base tree and a tree.
type ChangeSet struct {
	BaseTree     string       `json:"base_tree"`
	Tree         string       `json:"tree"`
	Files        []FileChange `json:"files"`
	LinesAdded   int          `json:"lines_added"`
	LinesRemoved int          `json:"lines_removed"`
}

// Paths returns the changed paths.
func (c ChangeSet) Paths() []string {
	out := make([]string, 0, len(c.Files))
	for _, f := range c.Files {
		out = append(out, f.Path)
	}
	return out
}

// Diff computes the change set from the base tree to tree.
func (w *Workspace) Diff(ctx context.Context, tree string) (ChangeSet, error) {
	cs := ChangeSet{BaseTree: w.BaseTree, Tree: tree}
	out, err := w.git.Run(ctx, "diff-tree", "-r", "-z", "--numstat", "--diff-filter=AMD", w.BaseTree, tree)
	if err != nil {
		return cs, err
	}
	statusOut, err := w.git.Run(ctx, "diff-tree", "-r", "-z", "--name-status", "--diff-filter=AMD", w.BaseTree, tree)
	if err != nil {
		return cs, err
	}
	status := map[string]string{}
	fields := strings.Split(string(statusOut), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status[fields[i+1]] = fields[i]
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			return cs, fmt.Errorf("workspace: malformed numstat %q", rec)
		}
		fc := FileChange{Path: parts[2], Status: status[parts[2]]}
		if parts[0] == "-" {
			fc.Binary = true
		} else {
			fmt.Sscanf(parts[0], "%d", &fc.Added)
			fmt.Sscanf(parts[1], "%d", &fc.Removed)
		}
		cs.Files = append(cs.Files, fc)
		cs.LinesAdded += fc.Added
		cs.LinesRemoved += fc.Removed
	}
	return cs, nil
}

// ReadFile reads a path from the working tree without following symlinks
// in any component.
func (w *Workspace) ReadFile(rel string) ([]byte, error) {
	root, err := os.OpenRoot(w.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := refuseSymlinks(root, rel); err != nil {
		return nil, err
	}
	return root.ReadFile(rel)
}

// ErrSymlink marks a path with a symbolic link component.
var ErrSymlink = errors.New("workspace: path contains a symbolic link")

func refuseSymlinks(root *os.Root, rel string) error {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, p)
		}
	}
	return nil
}
