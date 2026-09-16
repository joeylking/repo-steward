// Package repo derives a repository profile from a materialized snapshot.
// Profiling parses files and never executes anything. It records the facts
// the agent may read and the refusals that make a repository unsupported.
package repo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/toolchain"
)

// Requirement is one require line.
type Requirement struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect"`
}

// Profile is what deterministic inspection learned.
type Profile struct {
	ModulePath         string           `json:"module_path"`
	GoDirective        string           `json:"go_directive"`
	ToolchainDirective string           `json:"toolchain_directive,omitempty"`
	Requires           []Requirement    `json:"requires"`
	HasReplace         bool             `json:"has_replace"`
	HasExclude         bool             `json:"has_exclude"`
	Toolchain          *toolchain.Image `json:"toolchain,omitempty"`
	ProtectedGlobs     []string         `json:"protected_globs"`
	FileCount          int              `json:"file_count"`
	TotalBytes         int64            `json:"total_bytes"`
	GoFiles            int              `json:"go_files"`
	// Refusals lists why the repository is unsupported. Empty means supported.
	Refusals []string `json:"refusals"`
}

// Supported reports whether the profile has no refusals.
func (p *Profile) Supported() bool { return len(p.Refusals) == 0 }

// DefaultProtectedGlobs are paths the agent may never write. Patterns use
// path.Match semantics against slash-separated repository paths, with "**"
// meaning any directory prefix.
var DefaultProtectedGlobs = []string{
	"**/*_test.go",
	"**/testdata/**",
	".github/**",
	".gitlab-ci.yml",
	".circleci/**",
	"Jenkinsfile",
	".repo-steward/**",
	"SECURITY.md",
	"go.mod",
	"go.sum",
	"go.work",
	"go.work.sum",
	"vendor/**",
	".gitignore",
	".gitattributes",
	".gitmodules",
}

var cgoImport = regexp.MustCompile(`^\s*(import\s+)?"C"\s*$`)

// RefusalForListError converts a snapshot listing error into a refusal
// string, or returns "" if the error is not a supported-content refusal.
func RefusalForListError(err error) string {
	if errors.Is(err, snapshot.ErrUnsupportedEntry) {
		return err.Error()
	}
	return ""
}

// Inspect profiles the materialized snapshot at dir, whose entries were
// produced by snapshot.List for the same tree.
func Inspect(dir string, entries []snapshot.Entry) (*Profile, error) {
	p := &Profile{ProtectedGlobs: append([]string(nil), DefaultProtectedGlobs...), FileCount: len(entries)}
	paths := make(map[string]bool, len(entries))
	for _, e := range entries {
		paths[e.Path] = true
		p.TotalBytes += e.Size
		if strings.HasSuffix(e.Path, ".go") {
			p.GoFiles++
		}
	}
	refuse := func(format string, args ...any) {
		p.Refusals = append(p.Refusals, fmt.Sprintf(format, args...))
	}

	if !paths["go.mod"] {
		refuse("no go.mod at the repository root")
		return p, nil
	}
	if paths["go.work"] {
		refuse("go.work present: workspaces are unsupported")
	}
	if paths[".gitmodules"] {
		refuse(".gitmodules present: submodules are unsupported")
	}
	var nested []string
	for path := range paths {
		if path != "go.mod" && strings.HasSuffix(path, "/go.mod") {
			nested = append(nested, path)
		}
		if strings.HasPrefix(path, "vendor/") {
			if !contains(p.Refusals, "vendor/ present: vendored repositories are unsupported") {
				refuse("vendor/ present: vendored repositories are unsupported")
			}
		}
	}
	if len(nested) > 0 {
		sort.Strings(nested)
		refuse("nested modules are unsupported: %s", strings.Join(nested, ", "))
	}

	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, err
	}
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		refuse("go.mod does not parse: %v", err)
		return p, nil
	}
	if mf.Module == nil || mf.Module.Mod.Path == "" {
		refuse("go.mod has no module directive")
		return p, nil
	}
	p.ModulePath = mf.Module.Mod.Path
	if mf.Go != nil {
		p.GoDirective = mf.Go.Version
	}
	if mf.Toolchain != nil {
		p.ToolchainDirective = mf.Toolchain.Name
	}
	for _, r := range mf.Require {
		p.Requires = append(p.Requires, Requirement{Path: r.Mod.Path, Version: r.Mod.Version, Indirect: r.Indirect})
	}
	sort.Slice(p.Requires, func(i, j int) bool { return p.Requires[i].Path < p.Requires[j].Path })
	p.HasReplace = len(mf.Replace) > 0
	p.HasExclude = len(mf.Exclude) > 0
	for _, r := range mf.Replace {
		if r.New.Version == "" {
			refuse("replace %s => %s points at a local path: unsupported", r.Old.Path, r.New.Path)
		}
	}
	if p.GoDirective == "" {
		refuse("go.mod has no go directive")
	} else if img, err := toolchain.ForGoDirective(p.GoDirective); err != nil {
		refuse("%v", err)
	} else {
		p.Toolchain = &img
	}

	cgo, err := findCgo(dir, entries)
	if err != nil {
		return nil, err
	}
	if len(cgo) > 0 {
		refuse("cgo is unsupported: %s", strings.Join(cgo, ", "))
	}
	return p, nil
}

func findCgo(dir string, entries []snapshot.Entry) ([]string, error) {
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Path, ".go") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		found := false
		for sc.Scan() {
			line := sc.Bytes()
			if bytes.Contains(line, []byte(`"C"`)) && cgoImport.Match(line) {
				found = true
				break
			}
		}
		f.Close()
		if found {
			out = append(out, e.Path)
		}
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
