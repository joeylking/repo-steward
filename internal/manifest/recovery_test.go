//go:build faultinject

package manifest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/faultpoint"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// These tests run Promote in a child process that crashes at a named fault
// point, then recover in the parent from the journal and the workspace
// files alone. The staging directory is removed before recovery in every
// case where the after-content is not needed, and deliberately removed in
// the promoted case to prove recovery does not depend on it.

var ctx = context.Background()

const (
	afterMod = "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.2.4\n"
	afterSum = "example.com/lib v1.2.4 h1:fake=\nexample.com/lib v1.2.4/go.mod h1:fake=\n"
)

func TestMain(m *testing.M) {
	if os.Getenv("PROMOTE_HELPER") == "1" {
		helperPromote()
		return
	}
	os.Exit(m.Run())
}

// helperPromote stages a fake result and promotes it; the fault point in
// REPO_STEWARD_FAULT terminates it mid-way.
func helperPromote() {
	root := os.Getenv("PROMOTE_ROOT")
	store, err := task.Open(filepath.Join(root, "steward.db"))
	if err != nil {
		panic(err)
	}
	ws, err := workspace.Open(ctx, filepath.Join(root, "run", "work"))
	if err != nil {
		panic(err)
	}
	before, err := manifest.ReadWorkspace(ws)
	if err != nil {
		panic(err)
	}
	st := &manifest.Staging{ID: "st1", Dir: filepath.Join(root, "staging"), Op: manifest.Op{Kind: "upgrade", Target: manifest.Target{Module: "example.com/lib", Version: "v1.2.4"}}, Before: before, After: manifest.Manifests{Mod: []byte(afterMod), Sum: []byte(afterSum)}}
	os.MkdirAll(st.Dir, 0o755)
	os.WriteFile(filepath.Join(st.Dir, "go.mod"), st.After.Mod, 0o644)
	os.WriteFile(filepath.Join(st.Dir, "go.sum"), st.After.Sum, 0o644)
	if _, err := manifest.Promote(ctx, store, ws, "run1", "step1", st); err != nil {
		panic(err)
	}
	store.Close()
}

type env struct {
	root  string
	store *task.Store
	ws    *workspace.Workspace
	base  manifest.Manifests
}

func setup(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	r, err := fixture.Setup(ctx, "patch-safe", filepath.Join(root, "src"))
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
	if err := store.CreateTask(ctx, task.Task{RunID: "run1", Mode: "test", SourcePath: r.Path, BaseCommit: ws.BaseCommit, BaseTree: ws.BaseTree, BaseRef: "main", WorkspaceDir: ws.Dir}); err != nil {
		t.Fatal(err)
	}
	base, _ := manifest.ReadWorkspace(ws)
	return &env{root: root, store: store, ws: ws, base: base}
}

func crashAt(t *testing.T, e *env, point string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "PROMOTE_HELPER=1", "PROMOTE_ROOT="+e.root, faultpoint.EnvVar+"="+point)
	out, err := cmd.CombinedOutput()
	if point == "" {
		if err != nil {
			t.Fatalf("helper failed: %v\n%s", err, out)
		}
		return
	}
	if err == nil || !strings.Contains(string(out), "crashing at "+point) {
		t.Fatalf("helper did not crash at %s: %v\n%s", point, err, out)
	}
	// The store connection in the child died with it; reopen ours fresh.
	e.store.Close()
	st, err := task.Open(filepath.Join(e.root, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.store = st
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func files(t *testing.T, e *env) (string, string) {
	t.Helper()
	m, _ := manifest.ReadWorkspace(e.ws)
	return sha(m.Mod), sha(m.Sum)
}

func TestPromotion_Interruptions(t *testing.T) {
	cases := []struct {
		point      string
		wantStatus task.PromotionStatus
		wantAfter  bool // workspace holds the after manifests once recovered
		dropStage  bool
		// recovers is false when the journal was already terminal at the
		// crash, so recovery has nothing to do.
		recovers bool
	}{
		{"promote.staged", task.PromotionAborted, false, true, true},
		{"promote.promoting", task.PromotionAborted, false, true, true},
		{"promote.after_mod", task.PromotionPromoted, true, false, true},
		{"promote.after_sum", task.PromotionPromoted, true, true, true},
		{"promote.promoted", task.PromotionPromoted, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.point, func(t *testing.T) {
			e := setup(t)
			crashAt(t, e, tc.point)
			if tc.dropStage {
				os.RemoveAll(filepath.Join(e.root, "staging"))
			}
			rows, _ := e.store.ListPromotions(ctx, "run1")
			if len(rows) != 1 {
				t.Fatalf("journal rows = %d", len(rows))
			}
			actions, err := manifest.Recover(ctx, e.store, e.ws, "run1")
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if tc.recovers && (len(actions) != 1 || actions[0].To != tc.wantStatus) {
				t.Fatalf("actions = %+v, want %s", actions, tc.wantStatus)
			}
			if !tc.recovers && len(actions) != 0 {
				t.Fatalf("terminal journal row should need no recovery: %+v", actions)
			}
			if rows, _ := e.store.ListPromotions(ctx, "run1"); rows[0].Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", rows[0].Status, tc.wantStatus)
			}
			m, s := files(t, e)
			bm, bs := e.base.Hashes()
			if tc.wantAfter {
				if m != sha([]byte(afterMod)) || s != sha([]byte(afterSum)) {
					t.Fatal("workspace does not hold the after manifests")
				}
				if !manifest.UpgradeCommitted(mustRows(t, e)) {
					t.Fatal("selection should be closed after a promoted upgrade")
				}
			} else {
				if m != bm || s != bs {
					t.Fatal("workspace should be untouched")
				}
				if manifest.UpgradeCommitted(mustRows(t, e)) {
					t.Fatal("selection should remain open after an aborted promotion")
				}
			}
			// Recovery is idempotent.
			again, err := manifest.Recover(ctx, e.store, e.ws, "run1")
			if err != nil || len(again) != 0 {
				t.Fatalf("second recover = %v, %v", again, err)
			}
		})
	}
}

// Selection is closed as soon as a promotion reaches promoting, even if it
// was interrupted before either file changed and later aborted: until
// recovery runs, the journal alone says an upgrade may be in flight.
func TestPromotion_InFlightClosesSelection(t *testing.T) {
	e := setup(t)
	crashAt(t, e, "promote.promoting")
	if !manifest.UpgradeCommitted(mustRows(t, e)) {
		t.Fatal("promoting row must close selection before recovery")
	}
}

// go.mod at the after-hash with go.sum at neither hash is a conflict.
func TestPromotion_UnexpectedStateConflicts(t *testing.T) {
	e := setup(t)
	crashAt(t, e, "promote.after_mod")
	os.WriteFile(filepath.Join(e.ws.Dir, "go.sum"), []byte("tampered\n"), 0o644)
	_, err := manifest.Recover(ctx, e.store, e.ws, "run1")
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("err = %v, want conflict", err)
	}
	rows := mustRows(t, e)
	if rows[0].Status != task.PromotionConflict {
		t.Fatalf("status = %s", rows[0].Status)
	}
}

func mustRows(t *testing.T, e *env) []task.Promotion {
	t.Helper()
	rows, err := e.store.ListPromotions(ctx, "run1")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
