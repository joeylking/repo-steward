package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/tools"
	"github.com/joeylking/repo-steward/internal/workspace"
)

var ctx = context.Background()

// newSession builds a session over a fixture workspace with no engine: the
// read and write tools need only the workspace, the store, and the profile.
func newSession(t *testing.T, name string) (*session.Session, map[string]agentrt.Tool) {
	t.Helper()
	root := t.TempDir()
	r, err := fixture.Setup(ctx, name, filepath.Join(root, "src"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Create(ctx, r.Path, filepath.Join(root, "run"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := task.Open(filepath.Join(root, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.CreateTask(ctx, task.Task{RunID: "run1", Mode: "test", SourcePath: r.Path, BaseCommit: ws.BaseCommit, BaseTree: ws.BaseTree, WorkspaceDir: ws.Dir})
	entries, _ := snapshot.List(ctx, ws.Git(), ws.BaseTree, snapshot.DefaultLimits())
	snap := filepath.Join(root, "snap")
	ws.Materialize(ctx, ws.BaseTree, snap, snapshot.DefaultLimits())
	prof, err := repo.Inspect(snap, entries)
	if err != nil {
		t.Fatal(err)
	}
	s := &session.Session{RunID: "run1", Store: store, WS: ws, Profile: prof, RunDir: filepath.Join(root, "run"), Limits: snapshot.DefaultLimits(), Scope: session.DefaultScope(), Budgets: session.DefaultBudgets()}
	byName := map[string]agentrt.Tool{}
	for _, tl := range tools.All(s) {
		byName[tl.Spec().Name] = tl
	}
	return s, byName
}

func call(t *testing.T, tl agentrt.Tool, args string) (map[string]any, error) {
	t.Helper()
	res, err := tl.Call(ctx, agentrt.ToolCall{RunID: "run1", StepID: "s1", Args: json.RawMessage(args)})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func writeArgs(path, content string) string {
	b, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return string(b)
}

func TestSpecs_AreCompleteAndTerminalToolsMarked(t *testing.T) {
	_, byName := newSession(t, "patch-safe")
	if len(byName) != 13 {
		t.Fatalf("%d tools", len(byName))
	}
	for name, tl := range byName {
		sp := tl.Spec()
		if len(sp.InputSchema) == 0 || sp.Timeout <= 0 || sp.Description == "" {
			t.Errorf("%s: incomplete spec %+v", name, sp)
		}
		if (name == names.Prepare || name == names.Blocked) != sp.Terminal {
			t.Errorf("%s: terminal = %v", name, sp.Terminal)
		}
		if sp.SideEffect == agentrt.RemoteMutation || sp.SideEffect == agentrt.Destructive {
			t.Errorf("%s: no tool in this milestone may be %s", name, sp.SideEffect)
		}
	}
}

func TestReadTools_Containment(t *testing.T) {
	s, byName := newSession(t, "patch-safe")
	os.Symlink("scripts", filepath.Join(s.WS.Dir, "alias"))
	os.Symlink("/etc", filepath.Join(s.WS.Dir, "etc"))
	for _, p := range []string{"../src/main.go", "/etc/hosts", "alias/check.sh", "etc/hosts", ".git/HEAD", "a/../../x"} {
		if _, err := call(t, byName[names.ReadFile], `{"path":"`+p+`"}`); err == nil {
			t.Errorf("read_file(%q) succeeded", p)
		}
		if _, err := call(t, byName[names.ListDir], `{"path":"`+p+`"}`); err == nil && p != "a/../../x" {
			t.Errorf("list_directory(%q) succeeded", p)
		}
	}
	out, err := call(t, byName[names.ReadFile], `{"path":"main.go"}`)
	if err != nil || !strings.Contains(out["content"].(string), "package main") || out["notice"] == nil {
		t.Fatalf("read main.go = %v, %v", out, err)
	}
	out, err = call(t, byName[names.ListDir], `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range out["entries"].([]any) {
		if e.(map[string]any)["name"] == ".git" {
			t.Fatal(".git listed")
		}
	}
	out, err = call(t, byName[names.Search], `{"pattern":"lib\\.Greet"}`)
	if err != nil || len(out["hits"].([]any)) == 0 {
		t.Fatalf("search = %v, %v", out, err)
	}
	if _, err := call(t, byName[names.Search], `{"pattern":"("}`); err == nil {
		t.Fatal("invalid pattern accepted")
	}
}

func TestWriteFile_Rules(t *testing.T) {
	s, byName := newSession(t, "ignore-rules")
	w := byName[names.WriteFile]
	os.Symlink("config", filepath.Join(s.WS.Dir, "cfg"))
	cases := map[string]string{
		"protected test":     writeArgs("x_test.go", "package main\n"),
		"protected manifest": writeArgs("go.mod", "module x\n"),
		"protected ci":       writeArgs(".github/workflows/ci.yml", "x"),
		"ignored path":       writeArgs("build/out.go", "package main\n"),
		"ignored new file":   writeArgs("config/local.new.yaml", "x"),
		"symlink component":  writeArgs("cfg/new.txt", "x"),
		"traversal":          writeArgs("../escape.go", "x"),
		"git dir":            writeArgs(".git/config", "x"),
		"root":               writeArgs(".", "x"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := call(t, w, args); err == nil {
				t.Fatalf("write allowed: %s", args)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(s.WS.Dir, "config", "local.new.yaml")); err == nil {
		t.Fatal("ignored write landed")
	}
	os.Remove(filepath.Join(s.WS.Dir, "cfg")) // the test's own symlink would enter the candidate tree
	// A permitted write lands atomically and shows in the diff.
	out, err := call(t, w, writeArgs("internal/new.go", "package internal\n"))
	if err != nil {
		t.Fatal(err)
	}
	if out["files_changed"].(float64) != 1 {
		t.Fatalf("files_changed = %v", out["files_changed"])
	}
	if entries, _ := os.ReadDir(filepath.Join(s.WS.Dir, "internal")); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	// Overwriting the tracked-but-ignored file is allowed: tracked files
	// are not subject to ignore rules.
	if _, err := call(t, w, writeArgs("config/local.yaml", "mode: changed\n")); err != nil {
		t.Fatalf("tracked file matching an ignore rule refused: %v", err)
	}
	if _, err := call(t, byName[names.Diff], `{}`); err != nil {
		t.Fatal(err)
	}
}

func TestProjectedScope_CountsProposedWrite(t *testing.T) {
	s, _ := newSession(t, "patch-safe")
	files, lines, err := s.ProjectedScope(ctx, "main.go", []byte("package main\n"))
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || lines == 0 {
		t.Fatalf("projected files=%d lines=%d", files, lines)
	}
	// Identical content projects no change.
	orig, _ := s.WS.ReadFile("main.go")
	files, lines, _ = s.ProjectedScope(ctx, "main.go", orig)
	if files != 0 || lines != 0 {
		t.Fatalf("identical content projected files=%d lines=%d", files, lines)
	}
	// A new file counts as one file with its line count.
	files, lines, _ = s.ProjectedScope(ctx, "new.go", []byte("package main\n\nvar x = 1\n"))
	if files != 1 || lines != 3 {
		t.Fatalf("new file projected files=%d lines=%d", files, lines)
	}
}

func TestPhaseAndTarget_FromJournal(t *testing.T) {
	s, _ := newSession(t, "patch-safe")
	if ph, _ := s.Phase(ctx); ph != session.PhaseSelect {
		t.Fatalf("phase = %s", ph)
	}
	s.Store.InsertPromotion(ctx, task.Promotion{ID: "p1", RunID: "run1", Kind: "upgrade", TargetModule: "example.com/lib", TargetVersion: "v1.2.4", Status: task.PromotionPromoting, BeforeModSHA: "a", BeforeSumSHA: "b", AfterModSHA: "c", AfterSumSHA: "d", StagingDir: "/nonexistent"})
	if ph, _ := s.Phase(ctx); ph != session.PhaseRepair {
		t.Fatalf("phase after promoting = %s", ph)
	}
	if tg, ok, _ := s.Target(ctx); !ok || tg.Version != "v1.2.4" {
		t.Fatalf("target = %+v %v", tg, ok)
	}
}
