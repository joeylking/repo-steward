//go:build faultinject

package proposal_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/faultpoint"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/workspace"
)

var ctx = context.Background()

var author = gitx.Identity{Name: "Operator", Email: "op@example.invalid"}

func TestMain(m *testing.M) {
	if os.Getenv("FREEZE_HELPER") == "1" {
		helperFreeze()
		return
	}
	os.Exit(m.Run())
}

func helperFreeze() {
	root := os.Getenv("FREEZE_ROOT")
	store, err := task.Open(filepath.Join(root, "steward.db"))
	if err != nil {
		panic(err)
	}
	ws, err := workspace.Open(ctx, filepath.Join(root, "run", "work"))
	if err != nil {
		panic(err)
	}
	// Ambient Git identity and configuration must not influence the commit.
	os.Setenv("GIT_AUTHOR_NAME", "Mallory")
	os.Setenv("GIT_COMMITTER_DATE", "1234567890 +0500")
	os.Setenv("HOME", root)
	os.WriteFile(filepath.Join(root, ".gitconfig"), []byte("[user]\n\tname = Mallory\n\temail = m@example.com\n"), 0o644)
	ready := &proposal.Readiness{Ready: true, TreeHash: ws.BaseTree}
	_, err = proposal.Freeze(ctx, proposal.FreezeInput{
		RunID: "run1", Workspace: ws, Store: store, Readiness: ready, Target: manifest.Target{Module: "example.com/lib", Version: "v1.2.4"},
		BaseRef: "main", HeadRef: "repo-steward/lib-v1.2.4", Title: "Upgrade", Body: "body", Author: author, When: time.Unix(1700000100, 0),
	})
	if err != nil {
		panic(err)
	}
	store.Close()
}

type env struct {
	root  string
	store *task.Store
	ws    *workspace.Workspace
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
	store.CreateTask(ctx, task.Task{RunID: "run1", Mode: "test", SourcePath: r.Path, BaseCommit: ws.BaseCommit, BaseTree: ws.BaseTree, BaseRef: "main", WorkspaceDir: ws.Dir})
	return &env{root: root, store: store, ws: ws}
}

func crashAt(t *testing.T, e *env, point string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FREEZE_HELPER=1", "FREEZE_ROOT="+e.root, faultpoint.EnvVar+"="+point)
	out, err := cmd.CombinedOutput()
	if point == "" {
		if err != nil {
			t.Fatalf("helper failed: %v\n%s", err, out)
		}
	} else if err == nil || !strings.Contains(string(out), "crashing at "+point) {
		t.Fatalf("helper did not crash at %s: %v\n%s", point, err, out)
	}
	e.store.Close()
	st, err := task.Open(filepath.Join(e.root, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.store = st
}

func TestFreeze_Interruptions(t *testing.T) {
	// A clean run establishes the commit id every interrupted run must reproduce.
	ref := setup(t)
	crashAt(t, ref, "")
	rows, _ := ref.store.ListProposals(ctx, "run1")
	if len(rows) != 1 || rows[0].Status != task.ProposalFrozen {
		t.Fatalf("clean run rows = %+v", rows)
	}
	if _, err := proposal.Verify(ctx, ref.store, ref.ws, "run1", rows[0].ID); err != nil {
		t.Fatalf("verify clean: %v", err)
	}

	for _, point := range []string{"freeze.preparing", "freeze.committed", "freeze.ref_updated", "freeze.frozen"} {
		t.Run(point, func(t *testing.T) {
			e := setup(t)
			crashAt(t, e, point)
			rows, _ := e.store.ListProposals(ctx, "run1")
			if len(rows) != 1 {
				t.Fatalf("rows = %d", len(rows))
			}
			// Whatever the crashed run already wrote must be what recovery
			// ends up with: the ref's target when the ref exists, and the
			// recorded head when the row was frozen.
			wantCommit := rows[0].HeadCommit
			if wantCommit == "" {
				if c, err := e.ws.Git().RevParse(ctx, rows[0].Ref); err == nil {
					wantCommit = c
				}
			}
			actions, err := proposal.Recover(ctx, e.store, e.ws, "run1")
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if point != "freeze.frozen" && len(actions) != 1 {
				t.Fatalf("actions = %v", actions)
			}
			rows, _ = e.store.ListProposals(ctx, "run1")
			if rows[0].Status != task.ProposalFrozen {
				t.Fatalf("status = %s", rows[0].Status)
			}
			if wantCommit != "" && rows[0].HeadCommit != wantCommit {
				t.Fatalf("recovered commit %s != the crashed run's %s", rows[0].HeadCommit, wantCommit)
			}
			// Re-running the persisted recipe reproduces the id exactly.
			var rc struct {
				Tree, Parent      string
				Author, Committer gitx.Identity
				Message           string
			}
			json.Unmarshal(rows[0].Recipe, &rc)
			again, err := e.ws.Git().CommitTree(ctx, gitx.CommitRecipe{Tree: rc.Tree, Parents: []string{rc.Parent}, Author: rc.Author, Committer: rc.Committer, Message: []byte(rc.Message)})
			if err != nil || again != rows[0].HeadCommit {
				t.Fatalf("recipe re-run gave %s, row has %s (%v)", again, rows[0].HeadCommit, err)
			}
			p, err := proposal.Verify(ctx, e.store, e.ws, "run1", rows[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if p.Hash() != rows[0].Hash {
				t.Fatal("hash mismatch after recovery")
			}
			// The scratch checkout's HEAD was never moved.
			if head, _ := e.ws.Git().RevParse(ctx, "HEAD"); head != e.ws.BaseCommit {
				t.Fatalf("HEAD moved to %s", head)
			}
			out, _ := e.ws.Git().Run(ctx, "log", "-1", "--format=%an <%ae> %ad", "--date=raw", p.HeadCommit)
			if strings.TrimSpace(string(out)) != "Operator <op@example.invalid> 1700000100 +0000" {
				t.Fatalf("commit identity = %q", out)
			}
		})
	}
}

func TestRecover_RefMovedIsInvalidated(t *testing.T) {
	e := setup(t)
	crashAt(t, e, "freeze.committed")
	rows, _ := e.store.ListProposals(ctx, "run1")
	// Someone pointed the proposal ref at a different commit.
	e.ws.Git().UpdateRef(ctx, rows[0].Ref, e.ws.BaseCommit)
	actions, err := proposal.Recover(ctx, e.store, e.ws, "run1")
	if err != nil || len(actions) != 1 || !strings.Contains(actions[0], "invalidated") {
		t.Fatalf("actions = %v, %v", actions, err)
	}
	rows, _ = e.store.ListProposals(ctx, "run1")
	if rows[0].Status != task.ProposalInvalidated {
		t.Fatalf("status = %s", rows[0].Status)
	}
}

func TestVerify_DetectsTamperedBody(t *testing.T) {
	e := setup(t)
	crashAt(t, e, "")
	rows, _ := e.store.ListProposals(ctx, "run1")
	var p proposal.Proposal
	json.Unmarshal(rows[0].Proposal, &p)
	p.Title = "Something else"
	body, _ := json.Marshal(p)
	e.store.FreezeProposal(ctx, rows[0].ID, p.HeadCommit, body, rows[0].Hash)
	if _, err := proposal.Verify(ctx, e.store, e.ws, "run1", rows[0].ID); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("err = %v", err)
	}
}
