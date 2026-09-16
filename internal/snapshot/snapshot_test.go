package snapshot_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/snapshot"
)

var ctx = context.Background()

func setupFixture(t *testing.T, name string) (*fixture.Repo, *gitx.Git) {
	t.Helper()
	repo, err := fixture.Setup(ctx, name, filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return repo, gitx.New(repo.Path)
}

func paths(entries []snapshot.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}

func snapshotDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "snap")
}

// A tracked test file marked export-ignore is included unchanged in the
// snapshot, while git archive would silently omit it.
func TestMaterialize_ExportIgnoreIncluded(t *testing.T) {
	repo, g := setupFixture(t, "export-ignore-test")
	dest := snapshotDir(t)
	m, err := snapshot.Materialize(ctx, g, repo.BaseTree, dest, snapshot.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Verified {
		t.Fatal("snapshot not verified")
	}
	got, err := os.ReadFile(filepath.Join(dest, "main_test.go"))
	if err != nil {
		t.Fatal("main_test.go missing from snapshot:", err)
	}
	def, _ := fixture.Load("export-ignore-test")
	if !bytes.Equal(got, def.Files["main_test.go"]) {
		t.Fatal("main_test.go content differs from fixture")
	}

	// Demonstrate the hazard the design avoids.
	archive, err := g.Run(ctx, "archive", "--format=tar", repo.BaseTree)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	for _, n := range names {
		if n == "main_test.go" {
			t.Fatal("expected git archive to omit main_test.go; the test premise is wrong")
		}
	}
}

// The candidate index is seeded from the base tree: a tracked file matching a
// later ignore rule stays, an ignored leftover stays out, a new unignored file
// enters, and a modification to the tracked-but-ignored file is captured.
func TestCandidateTree_IgnoreRules(t *testing.T) {
	repo, g := setupFixture(t, "ignore-rules")
	if err := os.MkdirAll(filepath.Join(repo.Path, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(repo.Path, "internal", "newfile.go")
	if err := os.WriteFile(newFile, []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	localYAML := filepath.Join(repo.Path, "config", "local.yaml")
	if err := os.WriteFile(localYAML, []byte("mode: modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo.Path, "build", "out.txt")); err != nil {
		t.Fatal("leftover missing before test")
	}

	tree, err := snapshot.BuildCandidateTree(ctx, g, repo.BaseTree)
	if err != nil {
		t.Fatal(err)
	}
	if tree == repo.BaseTree {
		t.Fatal("candidate tree equals base tree despite changes")
	}
	entries, err := snapshot.List(ctx, g, tree, snapshot.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(paths(entries), " ")
	want := ".gitignore config/local.yaml go.mod go.sum internal/newfile.go main.go"
	if got != want {
		t.Fatalf("candidate tree paths:\n got  %s\n want %s", got, want)
	}
	dest := snapshotDir(t)
	if _, err := snapshot.Materialize(ctx, g, tree, dest, snapshot.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "config", "local.yaml")); string(b) != "mode: modified\n" {
		t.Fatalf("modified tracked file not captured: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dest, "build")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ignored leftover leaked into the snapshot")
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git metadata leaked into the snapshot")
	}
	// The repository index itself is untouched by candidate construction.
	status, _ := g.Run(ctx, "status", "--porcelain")
	if !strings.Contains(string(status), "?? internal/") || !strings.Contains(string(status), " M config/local.yaml") {
		t.Fatalf("repository index was modified by candidate construction:\n%s", status)
	}
}

// An ignored file that would change validation results cannot affect a
// snapshot: it is absent from the candidate tree and its hash.
func TestCandidateTree_IgnoredFileCannotAffectSnapshot(t *testing.T) {
	repo, g := setupFixture(t, "ignore-rules")
	before, err := snapshot.BuildCandidateTree(ctx, g, repo.BaseTree)
	if err != nil {
		t.Fatal(err)
	}
	if before != repo.BaseTree {
		t.Fatal("clean worktree should reproduce the base tree")
	}
	// build/ is ignored; drop a Go file there that would otherwise compile.
	if err := os.WriteFile(filepath.Join(repo.Path, "build", "extra.go"), []byte("package main\nfunc init() { panic(\"poison\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := snapshot.BuildCandidateTree(ctx, g, repo.BaseTree)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("ignored file changed the candidate tree: %s -> %s", before, after)
	}
}

type rawFile struct {
	path string
	mode string
	data []byte
}

func buildRawRepo(t *testing.T, files []rawFile) (*gitx.Git, string) {
	t.Helper()
	g := gitx.New(filepath.Join(t.TempDir(), "raw"))
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var entries []gitx.IndexEntry
	for _, f := range files {
		blob, err := g.HashObject(ctx, f.data)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, gitx.IndexEntry{Mode: f.mode, Blob: blob, Path: f.path})
	}
	if err := g.UpdateIndex(ctx, "", entries); err != nil {
		t.Fatal(err)
	}
	tree, err := g.WriteTree(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	return g, tree
}

func TestMaterialize_UnusualNamesModesAndContents(t *testing.T) {
	files := []rawFile{
		{"with space.txt", "100644", []byte("space\n")},
		{"ünïcödé/файл.txt", "100644", []byte("unicode\n")},
		{"new\nline.txt", "100644", []byte("newline in name\n")},
		{"tab\tname.txt", "100644", []byte("tab in name\n")},
		{"-leading-dash", "100644", []byte("dash\n")},
		{"bin/run.sh", "100755", []byte("#!/bin/sh\necho hi\n")},
		{"data/blob.bin", "100644", []byte("\x00\x01\x02\nabcdef0123456789abcdef0123456789abcdef01 blob 3\nxyz\n\x00")},
		{"empty.txt", "100644", nil},
		{"dup/a.txt", "100644", []byte("same\n")},
		{"dup/b.txt", "100644", []byte("same\n")},
		{"no-trailing-newline", "100644", []byte("abc")},
	}
	g, tree := buildRawRepo(t, files)
	dest := snapshotDir(t)
	m, err := snapshot.Materialize(ctx, g, tree, dest, snapshot.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Verified || m.FileCount != len(files) {
		t.Fatalf("manifest = %+v", m)
	}
	for _, f := range files {
		p := filepath.Join(dest, filepath.FromSlash(f.path))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%q: %v", f.path, err)
		}
		if !bytes.Equal(got, f.data) {
			t.Fatalf("%q: content differs: %q vs %q", f.path, got, f.data)
		}
		info, _ := os.Stat(p)
		exec := info.Mode().Perm()&0o100 != 0
		if exec != (f.mode == "100755") {
			t.Fatalf("%q: exec bit %v for mode %s", f.path, exec, f.mode)
		}
	}
}

func TestMaterialize_RefusesSymlinkAndSubmodule(t *testing.T) {
	cases := map[string]string{"symlink": "120000", "submodule": "160000"}
	for name, mode := range cases {
		t.Run(name, func(t *testing.T) {
			g, tree := buildRawRepo(t, []rawFile{
				{"ok.txt", "100644", []byte("ok\n")},
				{"entry", mode, []byte("target")},
			})
			dest := snapshotDir(t)
			_, err := snapshot.Materialize(ctx, g, tree, dest, snapshot.DefaultLimits())
			if !errors.Is(err, snapshot.ErrUnsupportedEntry) {
				t.Fatalf("err = %v, want ErrUnsupportedEntry", err)
			}
			if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("destination was created despite refusal")
			}
		})
	}
}

func TestMaterialize_LimitsEnforcedBeforeWriting(t *testing.T) {
	files := []rawFile{
		{"a.txt", "100644", bytes.Repeat([]byte("a"), 100)},
		{"b.txt", "100644", bytes.Repeat([]byte("b"), 100)},
		{"c.txt", "100644", bytes.Repeat([]byte("c"), 100)},
	}
	g, tree := buildRawRepo(t, files)
	cases := map[string]snapshot.Limits{
		"files":      {MaxFiles: 2},
		"total":      {MaxTotalBytes: 250},
		"singlefile": {MaxFileBytes: 99},
	}
	for name, limits := range cases {
		t.Run(name, func(t *testing.T) {
			dest := snapshotDir(t)
			_, err := snapshot.Materialize(ctx, g, tree, dest, limits)
			if !errors.Is(err, snapshot.ErrLimit) {
				t.Fatalf("err = %v, want ErrLimit", err)
			}
			if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("destination was created despite limit")
			}
		})
	}
	if _, err := snapshot.Materialize(ctx, g, tree, snapshotDir(t), snapshot.Limits{MaxFiles: 3, MaxTotalBytes: 300, MaxFileBytes: 100}); err != nil {
		t.Fatalf("limits at exact size should pass: %v", err)
	}
}

func TestCheck_PathRules(t *testing.T) {
	bad := []string{".git/config", "a/.GIT/x", "a/../b", "/abs", "", "a//b", "./a", "a/."}
	for _, p := range bad {
		err := snapshot.Check([]snapshot.Entry{{Path: p, Mode: "100644", Blob: "x", Size: 1}}, snapshot.Limits{})
		if !errors.Is(err, snapshot.ErrPath) {
			t.Errorf("Check(%q) = %v, want ErrPath", p, err)
		}
	}
	dup := []snapshot.Entry{{Path: "a", Mode: "100644", Blob: "x", Size: 1}, {Path: "a", Mode: "100644", Blob: "y", Size: 1}}
	if err := snapshot.Check(dup, snapshot.Limits{}); !errors.Is(err, snapshot.ErrPath) {
		t.Errorf("duplicate path = %v", err)
	}
	if err := snapshot.Check([]snapshot.Entry{{Path: "a/b.git/c", Mode: "100755", Blob: "x", Size: 0}}, snapshot.Limits{}); err != nil {
		t.Errorf("legitimate path rejected: %v", err)
	}
}

func TestVerify_DetectsTampering(t *testing.T) {
	g, tree := buildRawRepo(t, []rawFile{
		{"a.txt", "100644", []byte("alpha\n")},
		{"b/c.txt", "100644", []byte("gamma\n")},
	})
	fresh := func(t *testing.T) (string, []snapshot.Entry) {
		dest := snapshotDir(t)
		m, err := snapshot.Materialize(ctx, g, tree, dest, snapshot.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return dest, m.Entries
	}
	t.Run("content", func(t *testing.T) {
		dest, entries := fresh(t)
		os.WriteFile(filepath.Join(dest, "a.txt"), []byte("alphA\n"), 0o644)
		if err := snapshot.Verify(dest, entries); !errors.Is(err, snapshot.ErrVerify) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("mode", func(t *testing.T) {
		dest, entries := fresh(t)
		os.Chmod(filepath.Join(dest, "a.txt"), 0o755)
		if err := snapshot.Verify(dest, entries); !errors.Is(err, snapshot.ErrVerify) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("extra", func(t *testing.T) {
		dest, entries := fresh(t)
		os.WriteFile(filepath.Join(dest, "b", "extra.txt"), []byte("x"), 0o644)
		if err := snapshot.Verify(dest, entries); !errors.Is(err, snapshot.ErrVerify) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		dest, entries := fresh(t)
		os.Remove(filepath.Join(dest, "b", "c.txt"))
		if err := snapshot.Verify(dest, entries); !errors.Is(err, snapshot.ErrVerify) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dest, entries := fresh(t)
		os.Remove(filepath.Join(dest, "a.txt"))
		os.Symlink(filepath.Join(dest, "b", "c.txt"), filepath.Join(dest, "a.txt"))
		if err := snapshot.Verify(dest, entries); !errors.Is(err, snapshot.ErrVerify) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("clean", func(t *testing.T) {
		dest, entries := fresh(t)
		if err := snapshot.Verify(dest, entries); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMaterialize_RefusesNonEmptyDest(t *testing.T) {
	repo, g := setupFixture(t, "patch-safe")
	dest := t.TempDir()
	os.WriteFile(filepath.Join(dest, "x"), []byte("x"), 0o644)
	if _, err := snapshot.Materialize(ctx, g, repo.BaseTree, dest, snapshot.DefaultLimits()); err == nil {
		t.Fatal("expected error for non-empty destination")
	}
}

func TestMaterialize_FixtureRoundTrip(t *testing.T) {
	names, _ := fixture.Names()
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			repo, g := setupFixture(t, name)
			dest := snapshotDir(t)
			m, err := snapshot.Materialize(ctx, g, repo.BaseTree, dest, snapshot.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			// Re-adding the materialized directory as a tree must reproduce
			// the same tree id: the mapping is exact in both directions.
			g2 := gitx.New(dest)
			if err := g2.Init(ctx); err != nil {
				t.Fatal(err)
			}
			var entries []gitx.IndexEntry
			for _, e := range m.Entries {
				data, _ := os.ReadFile(filepath.Join(dest, filepath.FromSlash(e.Path)))
				blob, err := g2.HashObject(ctx, data)
				if err != nil {
					t.Fatal(err)
				}
				entries = append(entries, gitx.IndexEntry{Mode: e.Mode, Blob: blob, Path: e.Path})
			}
			if err := g2.UpdateIndex(ctx, "", entries); err != nil {
				t.Fatal(err)
			}
			tree, err := g2.WriteTree(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if tree != repo.BaseTree {
				t.Fatalf("round trip tree %s != base tree %s", tree, repo.BaseTree)
			}
		})
	}
}
