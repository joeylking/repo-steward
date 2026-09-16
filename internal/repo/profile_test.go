package repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/snapshot"
)

func write(t *testing.T, dir string, files map[string]string) []snapshot.Entry {
	t.Helper()
	var entries []snapshot.Entry
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, snapshot.Entry{Path: p, Mode: "100644", Size: int64(len(c))})
	}
	return entries
}

const goodMod = "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.2.1\n"

func TestInspect_Supported(t *testing.T) {
	dir := t.TempDir()
	entries := write(t, dir, map[string]string{"go.mod": goodMod, "main.go": "package main\n", "a/a_test.go": "package a\n"})
	p, err := repo.Inspect(dir, entries)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Supported() || p.ModulePath != "example.com/app" || p.GoDirective != "1.22" || p.Toolchain == nil || p.Toolchain.GoMinor != "1.22" {
		t.Fatalf("profile = %+v", p)
	}
	if len(p.Requires) != 1 || p.Requires[0].Path != "example.com/lib" || p.Requires[0].Indirect {
		t.Fatalf("requires = %+v", p.Requires)
	}
	if p.GoFiles != 2 || p.FileCount != 3 {
		t.Fatalf("counts = %+v", p)
	}
}

func TestInspect_Refusals(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"no go.mod":       {map[string]string{"main.go": "package main\n"}, "no go.mod"},
		"workspace":       {map[string]string{"go.mod": goodMod, "go.work": "go 1.22\n"}, "go.work"},
		"submodules":      {map[string]string{"go.mod": goodMod, ".gitmodules": ""}, "submodules"},
		"vendor":          {map[string]string{"go.mod": goodMod, "vendor/modules.txt": ""}, "vendored"},
		"nested module":   {map[string]string{"go.mod": goodMod, "tools/go.mod": "module x\n\ngo 1.22\n"}, "nested modules"},
		"cgo":             {map[string]string{"go.mod": goodMod, "c.go": "package main\n\n// #include <stdio.h>\nimport \"C\"\n"}, "cgo"},
		"cgo block":       {map[string]string{"go.mod": goodMod, "c.go": "package main\n\nimport (\n\t\"fmt\"\n\t\"C\"\n)\n"}, "cgo"},
		"old go":          {map[string]string{"go.mod": "module x\n\ngo 1.19\n"}, "no pinned image"},
		"no go directive": {map[string]string{"go.mod": "module x\n"}, "no go directive"},
		"local replace":   {map[string]string{"go.mod": goodMod + "\nreplace example.com/lib => ../lib\n"}, "local path"},
		"unparseable":     {map[string]string{"go.mod": "module\n"}, "does not parse"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p, err := repo.Inspect(dir, write(t, dir, tc.files))
			if err != nil {
				t.Fatal(err)
			}
			if p.Supported() || !strings.Contains(strings.Join(p.Refusals, "\n"), tc.want) {
				t.Fatalf("refusals = %v, want one containing %q", p.Refusals, tc.want)
			}
		})
	}
	// A string "C" that is not an import must not count as cgo.
	dir := t.TempDir()
	p, _ := repo.Inspect(dir, write(t, dir, map[string]string{"go.mod": goodMod, "x.go": "package main\n\nvar s = \"C\"\n"}))
	if !p.Supported() {
		t.Fatalf("false cgo detection: %v", p.Refusals)
	}
}

func TestRefusalForListError(t *testing.T) {
	if repo.RefusalForListError(snapshot.ErrUnsupportedEntry) == "" {
		t.Fatal("unsupported entry should be a refusal")
	}
	if repo.RefusalForListError(snapshot.ErrLimit) != "" {
		t.Fatal("limit errors are not refusals")
	}
}
