// Package fixture materializes reproducible Git repositories from embedded
// definitions. Every fixture commit is built from an explicit recipe with a
// fixed identity, timestamp, and message, so its id is stable across machines.
package fixture

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/gitx"
)

//go:embed all:testdata/fixtures
var embedded embed.FS

const root = "testdata/fixtures/repos"

// Identity is the author and committer of every fixture commit.
var Identity = gitx.Identity{
	Name:  "Repo Steward Fixture",
	Email: "fixture@repo-steward.invalid",
	When:  time.Unix(1700000000, 0).UTC(),
}

// Definition is a fixture as declared in fixture.json plus its files.
type Definition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// GitIgnore and GitAttributes become tracked dotfiles. They are declared
	// here rather than stored as files so that they never act on this
	// repository's own checkout.
	GitIgnore     string `json:"gitignore,omitempty"`
	GitAttributes string `json:"gitattributes,omitempty"`
	// Executable lists tracked paths committed with mode 100755.
	Executable []string `json:"executable,omitempty"`
	// Untracked files are written to the working tree after the commit and
	// never added to it.
	Untracked map[string]string `json:"untracked,omitempty"`
	// ExpectedBaseCommit and ExpectedBaseTree pin the ids Setup must produce.
	// Empty means unpinned.
	ExpectedBaseCommit string `json:"expected_base_commit,omitempty"`
	ExpectedBaseTree   string `json:"expected_base_tree,omitempty"`

	// Files maps repository path to content, loaded from files/. A file
	// stored as <name>.embed is committed as <name>; go:embed refuses to
	// embed a directory that contains a go.mod, so fixture manifests are
	// stored as go.mod.embed.
	Files map[string][]byte `json:"-"`
}

// embedSuffix is stripped from embedded file names.
const embedSuffix = ".embed"

// Names lists the embedded fixtures.
func Names() ([]string, error) {
	entries, err := fs.ReadDir(embedded, root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Load reads one fixture definition.
func Load(name string) (*Definition, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return nil, fmt.Errorf("fixture: invalid name %q", name)
	}
	dir := path.Join(root, name)
	raw, err := fs.ReadFile(embedded, path.Join(dir, "fixture.json"))
	if err != nil {
		return nil, fmt.Errorf("fixture %q: %w", name, err)
	}
	var def Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		return nil, fmt.Errorf("fixture %q: fixture.json: %w", name, err)
	}
	if def.Name != name {
		return nil, fmt.Errorf("fixture %q: fixture.json declares name %q", name, def.Name)
	}
	def.Files = map[string][]byte{}
	filesDir := path.Join(dir, "files")
	err = fs.WalkDir(embedded, filesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, filesDir+"/"), embedSuffix)
		content, err := fs.ReadFile(embedded, p)
		if err != nil {
			return err
		}
		def.Files[rel] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fixture %q: files: %w", name, err)
	}
	for _, x := range def.Executable {
		if _, ok := def.Files[x]; !ok {
			return nil, fmt.Errorf("fixture %q: executable %q is not a tracked file", name, x)
		}
	}
	return &def, nil
}

// Repo describes a materialized fixture repository.
type Repo struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`
	BaseTree   string `json:"base_tree"`
}

// ErrDrift is returned when a pinned fixture produces a different commit.
var ErrDrift = errors.New("fixture: generated ids differ from pinned ids")

// Setup materializes the fixture into dest, which must not exist or must be
// an empty directory, and returns the repository description.
func Setup(ctx context.Context, name, dest string) (*Repo, error) {
	def, err := Load(name)
	if err != nil {
		return nil, err
	}
	return def.Setup(ctx, dest)
}

// Setup materializes the definition into dest.
func (def *Definition) Setup(ctx context.Context, dest string) (*Repo, error) {
	if err := requireEmptyDir(dest); err != nil {
		return nil, err
	}
	g := gitx.New(dest)
	if err := g.Init(ctx); err != nil {
		return nil, err
	}

	tracked := map[string][]byte{}
	for p, c := range def.Files {
		tracked[p] = c
	}
	if def.GitIgnore != "" {
		tracked[".gitignore"] = []byte(def.GitIgnore)
	}
	if def.GitAttributes != "" {
		tracked[".gitattributes"] = []byte(def.GitAttributes)
	}
	exec := map[string]bool{}
	for _, x := range def.Executable {
		exec[x] = true
	}
	paths := make([]string, 0, len(tracked))
	for p := range tracked {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var entries []gitx.IndexEntry
	for _, p := range paths {
		mode := "100644"
		perm := os.FileMode(0o644)
		if exec[p] {
			mode = "100755"
			perm = 0o755
		}
		if err := writeFile(filepath.Join(dest, filepath.FromSlash(p)), tracked[p], perm); err != nil {
			return nil, err
		}
		blob, err := g.HashObject(ctx, tracked[p])
		if err != nil {
			return nil, err
		}
		entries = append(entries, gitx.IndexEntry{Mode: mode, Blob: blob, Path: p})
	}
	if err := g.UpdateIndex(ctx, "", entries); err != nil {
		return nil, err
	}
	tree, err := g.WriteTree(ctx, "")
	if err != nil {
		return nil, err
	}
	commit, err := g.CommitTree(ctx, gitx.CommitRecipe{
		Tree:      tree,
		Author:    Identity,
		Committer: Identity,
		Message:   []byte("fixture " + def.Name + ": base\n"),
	})
	if err != nil {
		return nil, err
	}
	if err := g.UpdateRef(ctx, "refs/heads/main", commit); err != nil {
		return nil, err
	}
	if err := g.SymbolicRef(ctx, "HEAD", "refs/heads/main"); err != nil {
		return nil, err
	}
	// Refresh index stat information so the working tree reads as clean.
	if err := g.ResetHard(ctx); err != nil {
		return nil, err
	}
	for p, c := range def.Untracked {
		if err := writeFile(filepath.Join(dest, filepath.FromSlash(p)), []byte(c), 0o644); err != nil {
			return nil, err
		}
	}
	repo := &Repo{Name: def.Name, Path: dest, Branch: "main", BaseCommit: commit, BaseTree: tree}
	if def.ExpectedBaseCommit != "" && def.ExpectedBaseCommit != commit {
		return nil, fmt.Errorf("%w: commit %s, pinned %s", ErrDrift, commit, def.ExpectedBaseCommit)
	}
	if def.ExpectedBaseTree != "" && def.ExpectedBaseTree != tree {
		return nil, fmt.Errorf("%w: tree %s, pinned %s", ErrDrift, tree, def.ExpectedBaseTree)
	}
	return repo, nil
}

func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("fixture: destination %s is not empty", dir)
	}
	return nil
}

func writeFile(p string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, content, perm); err != nil {
		return err
	}
	return os.Chmod(p, perm)
}

const modulesRoot = "testdata/fixtures/modules"

// ModuleSources returns embedded fixture module sources as
// module path -> version -> file name -> content. A module's directory is its
// slash-separated path, with one subdirectory per version.
func ModuleSources() (map[string]map[string]map[string][]byte, error) {
	out := map[string]map[string]map[string][]byte{}
	err := fs.WalkDir(embedded, modulesRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, modulesRoot+"/")
		// <module path>/<version>/<file path>; the version is the first
		// component that looks like one.
		parts := strings.Split(rel, "/")
		vi := -1
		for i, c := range parts {
			if strings.HasPrefix(c, "v") && strings.Count(c, ".") >= 2 {
				vi = i
				break
			}
		}
		if vi <= 0 || vi == len(parts)-1 {
			return fmt.Errorf("fixture: module file %q is not <module>/<version>/<file>", rel)
		}
		modPath := strings.Join(parts[:vi], "/")
		version := parts[vi]
		file := strings.TrimSuffix(strings.Join(parts[vi+1:], "/"), embedSuffix)
		content, err := fs.ReadFile(embedded, p)
		if err != nil {
			return err
		}
		if out[modPath] == nil {
			out[modPath] = map[string]map[string][]byte{}
		}
		if out[modPath][version] == nil {
			out[modPath][version] = map[string][]byte{}
		}
		out[modPath][version][file] = content
		return nil
	})
	return out, err
}

// Requires parses the fixture's go.mod and returns its direct and indirect
// requirements as module path -> version.
func Requires(def *Definition) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(def.Files["go.mod"]), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "require ") {
			line = strings.TrimPrefix(line, "require ")
		} else if !strings.HasPrefix(line, "\t") && !strings.Contains(line, " v") {
			continue
		}
		f := strings.Fields(strings.TrimSuffix(line, "// indirect"))
		if len(f) == 2 && strings.HasPrefix(f[1], "v") && !strings.HasPrefix(f[0], "require") && f[0] != "go" && f[0] != "module" && f[0] != "toolchain" {
			out[f[0]] = f[1]
		}
	}
	return out
}
