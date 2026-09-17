package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/workspace"
)

var ctx = context.Background()

func TestCreate_SeparateGitDirAndBase(t *testing.T) {
	r, err := fixture.Setup(ctx, "patch-safe", filepath.Join(t.TempDir(), "src"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Create(ctx, r.Path, filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	if ws.BaseCommit != r.BaseCommit || ws.BaseTree != r.BaseTree || ws.BaseRef != "main" {
		t.Fatalf("workspace = %+v", ws)
	}
	info, err := os.Lstat(filepath.Join(ws.Dir, ".git"))
	if err != nil || info.IsDir() {
		t.Fatalf(".git inside the working tree must be a pointer file, got %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(ws.GitDir, "HEAD")); err != nil {
		t.Fatal("separate git dir missing:", err)
	}
	tree, err := ws.CandidateTree(ctx)
	if err != nil || tree != ws.BaseTree {
		t.Fatalf("clean candidate tree = %s, %v", tree, err)
	}
	// Changes in the workspace never reach the source checkout.
	os.WriteFile(filepath.Join(ws.Dir, "main.go"), []byte("package main\n"), 0o644)
	src, _ := os.ReadFile(filepath.Join(r.Path, "main.go"))
	if string(src) == "package main\n" {
		t.Fatal("source checkout modified")
	}
	tree2, _ := ws.CandidateTree(ctx)
	cs, err := ws.Diff(ctx, tree2)
	if err != nil || len(cs.Files) != 1 || cs.Files[0].Path != "main.go" || cs.Files[0].Status != "M" || cs.LinesRemoved == 0 {
		t.Fatalf("diff = %+v, %v", cs, err)
	}
}

func TestCreate_RefusesDirtySource(t *testing.T) {
	r, _ := fixture.Setup(ctx, "patch-safe", filepath.Join(t.TempDir(), "src"))
	os.WriteFile(filepath.Join(r.Path, "extra.txt"), []byte("x"), 0o644)
	_, err := workspace.Create(ctx, r.Path, filepath.Join(t.TempDir(), "run"))
	if !errors.Is(err, workspace.ErrDirtyWorktree) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadFile_RefusesSymlinkComponents(t *testing.T) {
	r, _ := fixture.Setup(ctx, "patch-safe", filepath.Join(t.TempDir(), "src"))
	ws, err := workspace.Create(ctx, r.Path, filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	os.Symlink("scripts", filepath.Join(ws.Dir, "alias"))
	os.Symlink("/etc/hosts", filepath.Join(ws.Dir, "hosts"))
	for _, p := range []string{"alias/check.sh", "hosts"} {
		if _, err := ws.ReadFile(p); !errors.Is(err, workspace.ErrSymlink) {
			t.Errorf("%s: err = %v, want ErrSymlink", p, err)
		}
	}
	if _, err := ws.ReadFile("../../../etc/hosts"); err == nil {
		t.Error("traversal succeeded")
	}
	if b, err := ws.ReadFile("scripts/check.sh"); err != nil || !strings.HasPrefix(string(b), "#!/bin/sh") {
		t.Errorf("regular read failed: %v", err)
	}
}
