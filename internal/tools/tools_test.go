package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func editArgs(path, old, new string) string {
	b, _ := json.Marshal(map[string]string{"path": path, "old_text": old, "new_text": new})
	return string(b)
}

func TestSpecs_AreCompleteAndTerminalToolsMarked(t *testing.T) {
	_, byName := newSession(t, "patch-safe")
	if len(byName) != 14 {
		t.Fatalf("%d tools without publication", len(byName))
	}
	for name, tl := range byName {
		sp := tl.Spec()
		if len(sp.InputSchema) == 0 || sp.Timeout <= 0 || sp.Description == "" {
			t.Errorf("%s: incomplete spec %+v", name, sp)
		}
		// Without publication, prepare_proposal ends the run; publish_proposal
		// is the only remote tool and is terminal.
		if (name == names.Prepare || name == names.Blocked || name == names.Publish) != sp.Terminal {
			t.Errorf("%s: terminal = %v", name, sp.Terminal)
		}
		if (name == names.Publish) != (sp.SideEffect == agentrt.RemoteMutation) || sp.SideEffect == agentrt.Destructive {
			t.Errorf("%s: side effect %s", name, sp.SideEffect)
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

func TestSpecs_PublicationChangesToolSet(t *testing.T) {
	s, _ := newSession(t, "patch-safe")
	s.Publish = &session.Publication{}
	all := tools.All(s)
	if len(all) != 15 {
		t.Fatalf("%d tools with publication", len(all))
	}
	seen := false
	for _, tl := range all {
		sp := tl.Spec()
		if sp.Name == names.Prepare && sp.Terminal {
			t.Fatal("prepare_proposal must not be terminal when publication is enabled")
		}
		if sp.Name == names.Publish {
			seen = true
			if !sp.Terminal || sp.SideEffect != agentrt.RemoteMutation {
				t.Fatalf("publish spec = %+v", sp)
			}
		}
	}
	if !seen {
		t.Fatal("publish tool missing with publication enabled")
	}
}

// edit_file is held to every rule write_file is, through the same code:
// each path below is refused by both tools, and nothing on disk changes.
func TestEditFile_Rules(t *testing.T) {
	s, byName := newSession(t, "ignore-rules")
	e := byName[names.EditFile]
	os.Symlink("config", filepath.Join(s.WS.Dir, "cfg"))
	os.WriteFile(filepath.Join(s.WS.Dir, "x_test.go"), []byte("package main\n"), 0o644)
	os.MkdirAll(filepath.Join(s.WS.Dir, "adir.go"), 0o755)
	if err := syscall.Mkfifo(filepath.Join(s.WS.Dir, "fifo.go"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tree(t, s.WS.Dir)
	cases := map[string]struct{ path, want string }{
		"protected test":     {"x_test.go", "protected"},
		"protected manifest": {"go.mod", "protected"},
		"protected ci":       {".github/workflows/ci.yml", "protected"},
		"ignored path":       {"build/out.go", "ignore rule"},
		"symlink component":  {"cfg/local.yaml", "symbolic link"},
		"traversal":          {"../escape.go", "escapes"},
		"git dir":            {".git/config", ".git"},
		"root":               {".", "root"},
		"directory":          {"adir.go", "not a regular file"},
		"fifo":               {"fifo.go", "not a regular file"},
		"absent file":        {"nope.go", "does not exist"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, eerr := call(t, e, editArgs(c.path, "package", "pkg"))
			_, werr := call(t, byName[names.WriteFile], writeArgs(c.path, "x\n"))
			if eerr == nil || !strings.Contains(eerr.Error(), c.want) {
				t.Fatalf("edit_file(%s) = %v, want an error containing %q", c.path, eerr, c.want)
			}
			// write_file creates files, so only an absent path is its to take.
			if c.path != "nope.go" && werr == nil {
				t.Fatalf("write_file(%s) allowed", c.path)
			}
		})
	}
	os.Remove(filepath.Join(s.WS.Dir, "nope.go"))
	if after := tree(t, s.WS.Dir); after != before {
		t.Fatalf("a refused write changed the tree:\n%s\n---\n%s", before, after)
	}
}

// edit_file replaces exactly one occurrence, atomically, keeps the file's
// mode, and reports what it found otherwise without writing.
func TestEditFile_ReplacesExactlyOnce(t *testing.T) {
	s, byName := newSession(t, "breaking-minor")
	e := byName[names.EditFile]
	main := filepath.Join(s.WS.Dir, "main.go")
	os.Chmod(main, 0o755)
	orig, _ := os.ReadFile(main)
	for _, c := range []struct{ name, args, want string }{
		{"several", editArgs("main.go", "greeting", "hello"), "occurs 3 times in main.go (at lines 9, 10, 15)"},
		{"none", editArgs("main.go", "lib.Greet(ctx, name)", "x"), "occurs 0 times"},
		{"numbered", editArgs("main.go", "    11\treturn lib.Greet(name)", "x"), "line numbers"},
		{"whitespace", editArgs("main.go", "return  lib.Greet(name)\n}", "x"), "whitespace"},
		{"same", editArgs("main.go", "lib.Greet(name)", "lib.Greet(name)"), "nothing would change"},
		{"empty", editArgs("main.go", "", "x"), "empty"},
	} {
		if _, err := call(t, e, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if now, _ := os.ReadFile(main); string(now) != string(orig) {
		t.Fatal("a refused edit changed main.go")
	}
	out, err := call(t, e, editArgs("main.go", "lib.Greet(name)", "lib.Greet(context.Background(), name)"))
	if err != nil {
		t.Fatal(err)
	}
	now, _ := os.ReadFile(main)
	if want := strings.Replace(string(orig), "lib.Greet(name)", "lib.Greet(context.Background(), name)", 1); string(now) != want {
		t.Fatalf("main.go = %s", now)
	}
	if out["files_changed"].(float64) != 1 || !strings.Contains(out["edited_lines"].(string), "    11\t\treturn lib.Greet(context.Background(), name)") {
		t.Fatalf("result = %v", out)
	}
	if info, _ := os.Stat(main); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode())
	}
	entries, _ := os.ReadDir(s.WS.Dir)
	for _, en := range entries {
		if strings.Contains(en.Name(), "repo-steward-tmp") {
			t.Fatalf("temp file left behind: %s", en.Name())
		}
	}
}

// The scope check sizes the diff an edit would leave, from the same
// projection the tool writes, before anything is written.
func TestProjectedScope_CountsProposedEdit(t *testing.T) {
	s, _ := newSession(t, "breaking-minor")
	orig, _ := s.WS.ReadFile("main.go")
	rel, content, err := tools.ProjectWrite(ctx, s, names.EditFile, json.RawMessage(editArgs("./main.go", "lib.Greet(name)", "lib.Greet(context.Background(), name)")))
	if err != nil || rel != "main.go" {
		t.Fatalf("project = %q, %v", rel, err)
	}
	files, lines, err := s.ProjectedScope(ctx, rel, content)
	if err != nil || files != 1 || lines != 2 {
		t.Fatalf("projected files=%d lines=%d err=%v", files, lines, err)
	}
	if now, _ := s.WS.ReadFile("main.go"); string(now) != string(orig) {
		t.Fatal("projection wrote the file")
	}
}

// Reads come in numbered windows of whole lines, and a missing path names
// what does exist nearby.
func TestReadTools_WindowsAndAbsentPaths(t *testing.T) {
	s, byName := newSession(t, "patch-safe")
	var long strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&long, "// line %d of a long file\n", i)
	}
	os.WriteFile(filepath.Join(s.WS.Dir, "long.go"), []byte(long.String()), 0o644)
	out, err := call(t, byName[names.ReadFile], `{"path":"long.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	next := int(out["next_start_line"].(float64))
	if out["start_line"].(float64) != 1 || out["total_lines"].(float64) != 2000 || next < 2 || !strings.HasPrefix(out["content"].(string), "     1\t// line 1 of") {
		t.Fatalf("first window = %v", out["start_line"])
	}
	seen := next - 1
	for next > 0 {
		out, err = call(t, byName[names.ReadFile], fmt.Sprintf(`{"path":"long.go","start_line":%d}`, next))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out["content"].(string), fmt.Sprintf("%6d\t// line %d of", next, next)) {
			t.Fatalf("window at %d = %.80s", next, out["content"])
		}
		seen = int(out["end_line"].(float64))
		next = 0
		if n, ok := out["next_start_line"]; ok {
			next = int(n.(float64))
		}
	}
	if seen != 2000 {
		t.Fatalf("windows ended at %d", seen)
	}
	if _, err := call(t, byName[names.ReadFile], `{"path":"long.go","start_line":2001}`); err == nil || !strings.Contains(err.Error(), "2000 lines") {
		t.Fatalf("past the end: %v", err)
	}
	_, err = call(t, byName[names.ReadFile], `{"path":"lib/missing.go"}`)
	if err == nil || !strings.Contains(err.Error(), "the top-level directory contains") || !strings.Contains(err.Error(), `"main.go"`) {
		t.Fatalf("absent = %v", err)
	}
	_, err = call(t, byName[names.ListDir], `{"path":"nowhere"}`)
	if err == nil || !strings.Contains(err.Error(), `"go.mod"`) {
		t.Fatalf("absent dir = %v", err)
	}
}

// read_dependency_source and search_files reach a dependency's source in
// the module cache, and a guessed path is answered with the module's layout.
func TestDependencySource_ReadSearchAndAbsent(t *testing.T) {
	s, byName := newSession(t, "patch-safe")
	s.ModCacheDir = t.TempDir()
	dir, _ := s.ModuleDir("example.com/Lib", "v1.2.4")
	os.MkdirAll(filepath.Join(dir, "api"), 0o755)
	os.WriteFile(filepath.Join(dir, "api", "types.go"), []byte("package api\n\ntype Request struct {\n\tFormat string\n}\n"), 0o644)
	out, err := call(t, byName[names.DepSource], `{"module":"example.com/Lib","version":"v1.2.4","path":"api/types.go"}`)
	if err != nil || !strings.Contains(out["content"].(string), "     4\t\tFormat string") {
		t.Fatalf("read = %v, %v", out, err)
	}
	_, err = call(t, byName[names.DepSource], `{"module":"example.com/Lib","version":"v1.2.4","path":"types.go"}`)
	if err == nil || !strings.Contains(err.Error(), `contains: "api/"`) {
		t.Fatalf("absent = %v", err)
	}
	out, err = call(t, byName[names.Search], `{"pattern":"Format\\s","module":"example.com/Lib","version":"v1.2.4"}`)
	if err != nil {
		t.Fatal(err)
	}
	hits := out["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["path"] != "api/types.go" || hits[0].(map[string]any)["line"].(float64) != 4 {
		t.Fatalf("hits = %v", hits)
	}
	if _, err := call(t, byName[names.Search], `{"pattern":"x","module":"example.com/Lib"}`); err == nil {
		t.Fatal("module without version accepted")
	}
}

// tree lists every path under dir with its content, for comparing a tree
// before and after refused writes.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, ".git") {
			return nil
		}
		fmt.Fprintf(&b, "%s %v", rel, d.Type())
		if d.Type().IsRegular() {
			c, _ := os.ReadFile(p)
			fmt.Fprintf(&b, " %q", c)
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}
