// Package modproxy builds a file-based Go module proxy from embedded fixture
// module sources. The layout is the one GOPROXY=file:// expects: for each
// module, @v/list, and per version .info, .mod, and .zip. Every output is
// derived from fixed inputs, so the module sums are stable and can be pinned
// into fixture go.sum files.
package modproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/zip"

	"github.com/joeylking/repo-steward/internal/fixture"
)

// Time is the publication time recorded for every fixture version.
var Time = time.Unix(1700000000, 0).UTC()

// Sums are the go.sum hashes of one module version.
type Sums struct {
	Zip string `json:"zip"` // h1: hash of the module zip
	Mod string `json:"mod"` // h1: hash of go.mod alone
}

// Index describes a generated proxy.
type Index struct {
	Dir     string                     `json:"dir"`
	Modules map[string]map[string]Sums `json:"modules"` // module path -> version -> sums
}

// Versions returns the versions of a module in semver order.
func (ix *Index) Versions(modPath string) []string {
	var vs []string
	for v := range ix.Modules[modPath] {
		vs = append(vs, v)
	}
	semver.Sort(vs)
	return vs
}

// GoSum renders go.sum lines for the given module versions.
func (ix *Index) GoSum(requires map[string]string) (string, error) {
	var lines []string
	for m, v := range requires {
		s, ok := ix.Modules[m][v]
		if !ok {
			return "", fmt.Errorf("modproxy: %s@%s not in proxy", m, v)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", m, v, s.Zip), fmt.Sprintf("%s %s/go.mod %s", m, v, s.Mod))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}

// Build writes the proxy into dir, which must not exist or be empty.
func Build(dir string) (*Index, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("modproxy: destination %s is not empty", dir)
	}
	sources, err := fixture.ModuleSources()
	if err != nil {
		return nil, err
	}
	ix := &Index{Dir: dir, Modules: map[string]map[string]Sums{}}
	for modPath, versions := range sources {
		esc, err := module.EscapePath(modPath)
		if err != nil {
			return nil, fmt.Errorf("modproxy: %s: %w", modPath, err)
		}
		vdir := filepath.Join(dir, filepath.FromSlash(esc), "@v")
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			return nil, err
		}
		var list []string
		ix.Modules[modPath] = map[string]Sums{}
		for version, files := range versions {
			if !semver.IsValid(version) {
				return nil, fmt.Errorf("modproxy: %s: invalid version %q", modPath, version)
			}
			list = append(list, version)
			mv := module.Version{Path: modPath, Version: version}
			var zf []zip.File
			names := make([]string, 0, len(files))
			for name := range files {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				zf = append(zf, memFile{name: name, data: files[name]})
			}
			var buf bytes.Buffer
			if err := zip.Create(&buf, mv, zf); err != nil {
				return nil, fmt.Errorf("modproxy: zip %s@%s: %w", modPath, version, err)
			}
			zipPath := filepath.Join(vdir, version+".zip")
			if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
				return nil, err
			}
			gomod, ok := files["go.mod"]
			if !ok {
				// A release without go.mod: the proxy serves a synthesized
				// .mod, as proxy.golang.org does for +incompatible versions.
				if !strings.HasSuffix(version, "+incompatible") {
					return nil, fmt.Errorf("modproxy: %s@%s has no go.mod and is not +incompatible", modPath, version)
				}
				gomod = []byte("module " + modPath + "\n")
			}
			if err := os.WriteFile(filepath.Join(vdir, version+".mod"), gomod, 0o644); err != nil {
				return nil, err
			}
			info, _ := json.Marshal(map[string]string{"Version": version, "Time": Time.Format(time.RFC3339)})
			if err := os.WriteFile(filepath.Join(vdir, version+".info"), info, 0o644); err != nil {
				return nil, err
			}
			zipSum, err := dirhash.HashZip(zipPath, dirhash.Hash1)
			if err != nil {
				return nil, err
			}
			modSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(gomod)), nil
			})
			if err != nil {
				return nil, err
			}
			ix.Modules[modPath][version] = Sums{Zip: zipSum, Mod: modSum}
		}
		semver.Sort(list)
		if err := os.WriteFile(filepath.Join(vdir, "list"), []byte(strings.Join(list, "\n")+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	indexJSON, _ := json.MarshalIndent(ix, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexJSON, 0o644); err != nil {
		return nil, err
	}
	return ix, nil
}

// URL returns the GOPROXY value for a generated proxy directory.
func URL(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return "file://" + filepath.ToSlash(abs)
}

type memFile struct {
	name string
	data []byte
}

func (f memFile) Path() string { return f.name }
func (f memFile) Lstat() (os.FileInfo, error) {
	return memInfo{name: path.Base(f.name), size: int64(len(f.data))}, nil
}
func (f memFile) Open() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.data)), nil }

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o644 }
func (i memInfo) ModTime() time.Time { return Time }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
