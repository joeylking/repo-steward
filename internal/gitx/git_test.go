package gitx_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/gitx"
)

var ctx = context.Background()

func newRepo(t *testing.T) *gitx.Git {
	t.Helper()
	g := gitx.New(filepath.Join(t.TempDir(), "repo"))
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return g
}

var ident = gitx.Identity{Name: "T", Email: "t@example.invalid", When: time.Unix(1700000000, 0).UTC()}

func TestCommitTree_DeterministicAndByteExact(t *testing.T) {
	g := newRepo(t)
	blob, _ := g.HashObject(ctx, []byte("x\n"))
	g.UpdateIndex(ctx, "", []gitx.IndexEntry{{Mode: "100644", Blob: blob, Path: "x"}})
	tree, _ := g.WriteTree(ctx, "")
	recipe := gitx.CommitRecipe{Tree: tree, Author: ident, Committer: ident, Message: []byte("msg\n")}
	a, err := g.CommitTree(ctx, recipe)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := g.CommitTree(ctx, recipe)
	if a != b {
		t.Fatalf("same recipe produced %s and %s", a, b)
	}
	recipe.Message = []byte("msg")
	c, _ := g.CommitTree(ctx, recipe)
	if c == a {
		t.Fatal("message bytes are not part of the identity")
	}
	raw, _ := g.Run(ctx, "cat-file", "commit", c)
	if !bytes.HasSuffix(raw, []byte("\n\nmsg")) {
		t.Fatalf("message not byte-exact: %q", raw)
	}
	recipe.Committer.When = recipe.Committer.When.Add(time.Second)
	d, _ := g.CommitTree(ctx, recipe)
	if d == c {
		t.Fatal("committer date is not part of the identity")
	}
	if _, err := g.CommitTree(ctx, gitx.CommitRecipe{Tree: tree, Author: ident}); err == nil {
		t.Fatal("incomplete recipe accepted")
	}
}

func TestEnv_IgnoresUserConfiguration(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Mallory\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_DIR", "/nonexistent")
	g := newRepo(t)
	out, err := g.Run(ctx, "config", "--get", "user.name")
	if err == nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("user configuration leaked: %q (err %v)", out, err)
	}
	out, _ = g.Run(ctx, "config", "--get", "core.hooksPath")
	if !strings.Contains(string(out), "repo-steward-git-isolation") {
		t.Fatalf("hooks path not isolated: %q", out)
	}
}

func TestHooksDoNotRun(t *testing.T) {
	g := newRepo(t)
	hooks := filepath.Join(g.Dir, ".git", "hooks")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	blob, _ := g.HashObject(ctx, []byte("x\n"))
	g.UpdateIndex(ctx, "", []gitx.IndexEntry{{Mode: "100644", Blob: blob, Path: "x"}})
	tree, _ := g.WriteTree(ctx, "")
	c, _ := g.CommitTree(ctx, gitx.CommitRecipe{Tree: tree, Author: ident, Committer: ident, Message: []byte("m\n")})
	g.UpdateRef(ctx, "refs/heads/main", c)
	if err := g.ResetHard(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "checkout", "-q", "-b", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook executed")
	}
}

func TestLsTreeAndCatFileBatch_SizeAware(t *testing.T) {
	g := newRepo(t)
	contents := map[string][]byte{
		"a b\tc\nd": []byte("weird name\n"),
		"hdr":       []byte("0123456789abcdef0123456789abcdef01234567 blob 5\n\x00\x00"),
		"empty":     {},
	}
	var entries []gitx.IndexEntry
	blobs := map[string]string{}
	for p, c := range contents {
		id, err := g.HashObject(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		blobs[p] = id
		entries = append(entries, gitx.IndexEntry{Mode: "100644", Blob: id, Path: p})
	}
	if err := g.UpdateIndex(ctx, "", entries); err != nil {
		t.Fatal(err)
	}
	tree, _ := g.WriteTree(ctx, "")
	listed, err := g.LsTree(ctx, tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(contents) {
		t.Fatalf("listed %d entries", len(listed))
	}
	var ids []string
	for _, e := range listed {
		if e.Blob != blobs[e.Path] || e.Size != int64(len(contents[e.Path])) || e.Type != "blob" {
			t.Fatalf("entry %+v does not match", e)
		}
		ids = append(ids, e.Blob)
	}
	got := map[string][]byte{}
	err = g.CatFileBatch(ctx, ids, func(id string, size int64, r io.Reader) error {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if int64(len(b)) != size {
			t.Fatalf("size %d, read %d", size, len(b))
		}
		got[id] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range contents {
		if !bytes.Equal(got[blobs[p]], c) {
			t.Fatalf("%q: got %q want %q", p, got[blobs[p]], c)
		}
	}
	// Consumers that read less than the declared size still leave the stream aligned.
	err = g.CatFileBatch(ctx, ids, func(id string, size int64, r io.Reader) error { return nil })
	if err != nil {
		t.Fatal("partial consumption broke alignment:", err)
	}
	if err := g.CatFileBatch(ctx, []string{strings.Repeat("0", 40)}, func(string, int64, io.Reader) error { return nil }); err == nil {
		t.Fatal("missing object not reported")
	}
}

func TestUpdateIndex_RejectsNULInPath(t *testing.T) {
	g := newRepo(t)
	if err := g.UpdateIndex(ctx, "", []gitx.IndexEntry{{Mode: "100644", Blob: strings.Repeat("0", 40), Path: "a\x00b"}}); err == nil {
		t.Fatal("NUL in path accepted")
	}
}
