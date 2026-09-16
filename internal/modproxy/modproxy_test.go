package modproxy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/modproxy"
)

func TestBuild_LayoutAndDeterminism(t *testing.T) {
	a, err := modproxy.Build(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := modproxy.Build(filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatal(err)
	}
	lib := a.Modules["example.com/lib"]
	if len(lib) < 2 {
		t.Fatalf("expected at least two versions of example.com/lib, got %v", lib)
	}
	for v, s := range lib {
		if b.Modules["example.com/lib"][v] != s {
			t.Fatalf("sums differ between builds for %s", v)
		}
		if !strings.HasPrefix(s.Zip, "h1:") || !strings.HasPrefix(s.Mod, "h1:") {
			t.Fatalf("sums not h1: %+v", s)
		}
		for _, ext := range []string{".info", ".mod", ".zip"} {
			if _, err := os.Stat(filepath.Join(a.Dir, "example.com", "lib", "@v", v+ext)); err != nil {
				t.Fatal(err)
			}
		}
	}
	list, _ := os.ReadFile(filepath.Join(a.Dir, "example.com", "lib", "@v", "list"))
	if strings.TrimSpace(string(list)) != strings.Join(a.Versions("example.com/lib"), "\n") {
		t.Fatalf("list = %q", list)
	}
	if !strings.HasPrefix(modproxy.URL(a.Dir), "file:///") {
		t.Fatalf("URL = %s", modproxy.URL(a.Dir))
	}
}

// Every fixture application's go.sum must match the proxy's sums for its
// requirements, otherwise the fixture cannot resolve offline.
func TestFixtureGoSumsMatchProxy(t *testing.T) {
	ix, err := modproxy.Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names, _ := fixture.Names()
	for _, name := range names {
		def, err := fixture.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		gosum, ok := def.Files["go.sum"]
		if !ok {
			t.Errorf("fixture %s has no go.sum", name)
			continue
		}
		requires := fixture.Requires(def)
		want, err := ix.GoSum(requires)
		if err != nil {
			t.Errorf("fixture %s: %v", name, err)
			continue
		}
		if string(gosum) != want {
			t.Errorf("fixture %s go.sum drift:\n got:\n%s\n want:\n%s", name, gosum, want)
		}
	}
	_ = context.Background()
}
