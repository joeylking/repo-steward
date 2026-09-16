package inspect_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/inspect"
	"github.com/joeylking/repo-steward/internal/lock"
)

var ctx = context.Background()

// Refusals decided from the tree or the profile must not require a
// container engine; these tests pass an unusable socket to prove it.
const noSocket = "/nonexistent/docker.sock"

func TestRun_TrackedSymlinkIsUnsupported(t *testing.T) {
	g := gitx.New(filepath.Join(t.TempDir(), "repo"))
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	blob, _ := g.HashObject(ctx, []byte("module x\n\ngo 1.22\n"))
	link, _ := g.HashObject(ctx, []byte("go.mod"))
	g.UpdateIndex(ctx, "", []gitx.IndexEntry{{Mode: "100644", Blob: blob, Path: "go.mod"}, {Mode: "120000", Blob: link, Path: "link"}})
	tree, _ := g.WriteTree(ctx, "")
	c, _ := g.CommitTree(ctx, gitx.CommitRecipe{Tree: tree, Author: fixture.Identity, Committer: fixture.Identity, Message: []byte("m\n")})
	g.UpdateRef(ctx, "refs/heads/main", c)
	g.SymbolicRef(ctx, "HEAD", "refs/heads/main")
	g.ResetHard(ctx)

	rep, err := inspect.Run(ctx, inspect.Options{RepoPath: g.Dir, DataDir: filepath.Join(t.TempDir(), "data"), Socket: noSocket})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != inspect.OutcomeUnsupported || !strings.Contains(strings.Join(rep.Refusals, " "), "symbolic link") {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Snapshot != nil {
		t.Fatal("snapshot must not be materialized for an unsupported tree")
	}
}

func TestRun_ProfileRefusalStopsBeforeSandbox(t *testing.T) {
	def, _ := fixture.Load("patch-safe")
	def.ExpectedBaseCommit, def.ExpectedBaseTree = "", ""
	def.Files["go.work"] = []byte("go 1.22\n")
	r, err := def.Setup(ctx, filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := inspect.Run(ctx, inspect.Options{RepoPath: r.Path, DataDir: filepath.Join(t.TempDir(), "data"), Socket: noSocket})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != inspect.OutcomeUnsupported || rep.Profile == nil || !strings.Contains(strings.Join(rep.Refusals, " "), "go.work") {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Snapshot == nil || !rep.Snapshot.Verified {
		t.Fatal("snapshot should be materialized and verified before profiling")
	}
}

func TestRun_ReportsDirtyWorktreeAndUsesHeadTree(t *testing.T) {
	r, err := fixture.Setup(ctx, "ignore-rules", filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.Path, "go.work"), []byte("go 1.22\n"), 0o644) // dirty, and would be a refusal if it were in the tree
	rep, err := inspect.Run(ctx, inspect.Options{RepoPath: r.Path, DataDir: filepath.Join(t.TempDir(), "data"), Socket: noSocket})
	// The tree is supported, so the run proceeds to the sandbox and fails on the socket.
	if err == nil {
		t.Fatalf("expected sandbox error, got report %+v", rep)
	}
	if !strings.Contains(err.Error(), noSocket) && !strings.Contains(err.Error(), "docker") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_ExecutorLockIsExclusive(t *testing.T) {
	r, err := fixture.Setup(ctx, "patch-safe", filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "data")
	held, err := lock.Acquire(filepath.Join(data, "executor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	_, err = inspect.Run(ctx, inspect.Options{RepoPath: r.Path, DataDir: data, Socket: noSocket})
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("err = %v, want ErrHeld", err)
	}
}
