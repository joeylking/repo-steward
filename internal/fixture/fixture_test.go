package fixture_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
)

func TestNames(t *testing.T) {
	names, err := fixture.Names()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"breaking-minor", "export-ignore-test", "ignore-rules", "patch-safe"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

// Every fixture is pinned. Setup fails with ErrDrift if the generated commit
// differs from fixture.json, so a change to fixture content is a deliberate
// act that updates the pin.
func TestSetup_PinnedIDsAreReproducible(t *testing.T) {
	names, _ := fixture.Names()
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			def, err := fixture.Load(name)
			if err != nil {
				t.Fatal(err)
			}
			if def.ExpectedBaseCommit == "" || def.ExpectedBaseTree == "" {
				t.Fatalf("fixture %s is not pinned", name)
			}
			a, err := fixture.Setup(context.Background(), name, filepath.Join(t.TempDir(), "a"))
			if err != nil {
				t.Fatal(err)
			}
			b, err := fixture.Setup(context.Background(), name, filepath.Join(t.TempDir(), "b"))
			if err != nil {
				t.Fatal(err)
			}
			if a.BaseCommit != b.BaseCommit || a.BaseTree != b.BaseTree || a.BaseCommit != def.ExpectedBaseCommit {
				t.Fatalf("ids differ: %+v vs %+v, pinned %s", a, b, def.ExpectedBaseCommit)
			}
		})
	}
}

func TestSetup_DriftIsDetected(t *testing.T) {
	def, err := fixture.Load("patch-safe")
	if err != nil {
		t.Fatal(err)
	}
	def.Files["README.md"] = append(def.Files["README.md"], []byte("changed\n")...)
	_, err = def.Setup(context.Background(), t.TempDir())
	if !errors.Is(err, fixture.ErrDrift) {
		t.Fatalf("err = %v, want ErrDrift", err)
	}
}

// The commit recipe fixes author, committer, dates, and message. Ambient Git
// identity in the environment or a global config must not leak into ids.
func TestSetup_IgnoresAmbientGitIdentity(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Mallory\n\temail = mallory@example.com\n[core]\n\thooksPath = /nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_AUTHOR_NAME", "Mallory")
	t.Setenv("GIT_AUTHOR_EMAIL", "mallory@example.com")
	t.Setenv("GIT_AUTHOR_DATE", "1234567890 +0500")
	t.Setenv("GIT_COMMITTER_NAME", "Mallory")
	t.Setenv("GIT_COMMITTER_EMAIL", "mallory@example.com")
	t.Setenv("GIT_COMMITTER_DATE", "1234567890 +0500")
	t.Setenv("TZ", "Asia/Kolkata")

	def, _ := fixture.Load("patch-safe")
	repo, err := fixture.Setup(context.Background(), "patch-safe", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if repo.BaseCommit != def.ExpectedBaseCommit {
		t.Fatalf("ambient identity changed the commit id: %s", repo.BaseCommit)
	}
	out, err := gitx.New(repo.Path).Run(context.Background(), "log", "-1", "--format=%an <%ae> %ad|%cn <%ce> %cd", "--date=raw")
	if err != nil {
		t.Fatal(err)
	}
	want := "Repo Steward Fixture <fixture@repo-steward.invalid> 1700000000 +0000|Repo Steward Fixture <fixture@repo-steward.invalid> 1700000000 +0000"
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("commit identity = %q", strings.TrimSpace(string(out)))
	}
}

func TestSetup_WorktreeModesAndCleanStatus(t *testing.T) {
	repo, err := fixture.Setup(context.Background(), "patch-safe", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(repo.Path, "scripts", "check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatal("scripts/check.sh is not executable in the working tree")
	}
	g := gitx.New(repo.Path)
	entries, err := g.LsTree(context.Background(), repo.BaseTree)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, e := range entries {
		modes[e.Path] = e.Mode
	}
	if modes["scripts/check.sh"] != "100755" || modes["main.go"] != "100644" {
		t.Fatalf("tree modes = %v", modes)
	}
	status, err := g.Run(context.Background(), "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 0 {
		t.Fatalf("working tree not clean after setup:\n%s", status)
	}
	if head, _ := g.RevParse(context.Background(), "HEAD"); head != repo.BaseCommit {
		t.Fatalf("HEAD = %s, want %s", head, repo.BaseCommit)
	}
}

func TestSetup_UntrackedLeftoverIsIgnored(t *testing.T) {
	repo, err := fixture.Setup(context.Background(), "ignore-rules", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo.Path, "build", "out.txt")); err != nil {
		t.Fatal("leftover was not written:", err)
	}
	g := gitx.New(repo.Path)
	status, _ := g.Run(context.Background(), "status", "--porcelain")
	if len(status) != 0 {
		t.Fatalf("leftover shows in status:\n%s", status)
	}
	ignored, _ := g.Run(context.Background(), "status", "--porcelain", "--ignored")
	if !strings.Contains(string(ignored), "!! build/") {
		t.Fatalf("leftover is not ignored:\n%s", ignored)
	}
	// The tracked file matches an ignore rule and is still tracked.
	out, _ := g.Run(context.Background(), "ls-files", "--", "config/local.yaml")
	if strings.TrimSpace(string(out)) != "config/local.yaml" {
		t.Fatalf("config/local.yaml not tracked: %q", out)
	}
}

func TestSetup_RefusesNonEmptyDest(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Setup(context.Background(), "patch-safe", dest); err == nil {
		t.Fatal("expected error for non-empty destination")
	}
}

func TestLoad_RejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "..", "a/b", "nope"} {
		if _, err := fixture.Load(name); err == nil {
			t.Errorf("Load(%q) succeeded", name)
		}
	}
}
