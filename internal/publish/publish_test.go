package publish_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/github"
	"github.com/joeylking/repo-steward/internal/github/fake"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/publish"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/workspace"
)

var ctx = context.Background()

var author = gitx.Identity{Name: "Operator", Email: "op@example.invalid"}

// env is a workspace with a frozen proposal, a bare repository standing in
// for the destination's git side, and a fake API server for its REST side.
type env struct {
	store *task.Store
	ws    *workspace.Workspace
	prop  *proposal.Proposal
	bare  string
	api   *fake.Server
	pub   *publish.Publisher
}

func newEnv(t *testing.T) *env {
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
	// A proposal whose tree differs from base: a modified README.
	os.WriteFile(filepath.Join(ws.Dir, "README.md"), []byte("# app\n\nchanged\n"), 0o644)
	tree, _ := ws.CandidateTree(ctx)
	cs, _ := ws.Diff(ctx, tree)
	ready := &proposal.Readiness{Ready: true, TreeHash: tree, ChangeSet: cs}
	prop, err := proposal.Freeze(ctx, proposal.FreezeInput{RunID: "run1", Workspace: ws, Store: store, Readiness: ready, Target: manifest.Target{Module: "example.com/lib", Version: "v1.2.4"},
		BaseRef: "main", HeadRef: "repo-steward/lib-v1.2.4", Title: "Upgrade lib", Body: "body", Author: author, When: time.Unix(1700000200, 0)})
	if err != nil {
		t.Fatal(err)
	}
	// Bare destination repository seeded with the base commit.
	bare := filepath.Join(root, "dest.git")
	if _, err := gitx.New(root).Run(ctx, "clone", "-q", "--bare", r.Path, bare); err != nil {
		t.Fatal(err)
	}
	api := fake.New()
	t.Cleanup(api.Close)
	api.AddRepo("acme", "app", "main", ws.BaseCommit).GitDir = bare
	dest := publish.Destination{Host: "github.com", Owner: "acme", Repo: "app", APIBase: api.URL(), PushURL: "file://" + bare, BaseRef: "main", BaseHeadAtStart: ws.BaseCommit}
	pub := &publish.Publisher{Store: store, WS: ws, Client: github.New(api.URL(), "tok"), Dest: dest, Token: ""}
	return &env{store: store, ws: ws, prop: prop, bare: bare, api: api, pub: pub}
}

func (e *env) remoteHead(t *testing.T) string {
	t.Helper()
	out, err := gitx.New(e.bare).Run(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+e.prop.HeadRef)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ops(t *testing.T, e *env) []task.PublicationOp {
	t.Helper()
	o, err := e.store.ListPublicationOps(ctx, "run1")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestParseRemote(t *testing.T) {
	for _, in := range []string{"git@github.com:acme/app.git", "https://github.com/acme/app", "https://github.com/acme/app.git", "ssh://git@github.com/acme/app.git"} {
		d, err := publish.ParseRemote(in)
		if err != nil || d.Host != "github.com" || d.Owner != "acme" || d.Repo != "app" || d.APIBase != "https://api.github.com" || d.PushURL != "https://github.com/acme/app.git" {
			t.Errorf("%s -> %+v %v", in, d, err)
		}
	}
	if _, err := publish.ParseRemote("/local/path"); err == nil {
		t.Error("local path parsed as remote")
	}
}

func TestVerify(t *testing.T) {
	e := newEnv(t)
	d := e.pub.Dest
	if err := publish.Verify(ctx, e.pub.Client, &d, "main", e.ws.BaseCommit); err != nil {
		t.Fatal(err)
	}
	if d.BaseHeadAtStart != e.ws.BaseCommit {
		t.Fatal("observed head not recorded")
	}
	if err := publish.Verify(ctx, e.pub.Client, &d, "main", strings.Repeat("a", 40)); !errors.Is(err, publish.ErrBaseMismatch) {
		t.Fatalf("mismatch = %v", err)
	}
	if err := publish.Verify(ctx, e.pub.Client, &d, "", e.ws.BaseCommit); err == nil {
		t.Fatal("detached HEAD accepted")
	}
	if err := publish.Verify(ctx, e.pub.Client, &d, "nope", e.ws.BaseCommit); err == nil {
		t.Fatal("missing base branch accepted")
	}
	other := publish.Destination{Host: "gitlab.com", Owner: "a", Repo: "b", APIBase: "https://gitlab.com/api/v4"}
	if err := publish.Verify(ctx, e.pub.Client, &other, "main", e.ws.BaseCommit); !errors.Is(err, publish.ErrUnsupportedHost) {
		t.Fatalf("host = %v", err)
	}
	// Requests happen before any credential use: the client's token is
	// the API token only; the push token is not involved in Verify.
	for _, r := range e.api.Requests {
		if strings.Contains(r, "pulls") {
			t.Fatal("verify touched pull requests")
		}
	}
}

func TestPublish_HappyPath(t *testing.T) {
	e := newEnv(t)
	res, err := e.pub.Publish(ctx, "run1", "step1", e.prop)
	if err != nil {
		t.Fatal(err)
	}
	if e.remoteHead(t) != e.prop.HeadCommit {
		t.Fatalf("remote head = %s", e.remoteHead(t))
	}
	if res.PRNumber != 1 || res.BaseMoved || res.PRURL == "" {
		t.Fatalf("result = %+v", res)
	}
	pr := e.api.Repo("acme", "app").PRs[0]
	if pr.Head.Ref != e.prop.HeadRef || pr.Base.Ref != "main" || !strings.Contains(pr.Body, publish.Marker(e.prop.ID)) || !strings.Contains(pr.Body, e.prop.BaseCommit) || pr.Title != "Upgrade lib" {
		t.Fatalf("pr = %+v", pr)
	}
	o := ops(t, e)
	if len(o) != 2 || o[0].Kind != "push" || o[0].Status != task.OpDone || o[1].Kind != "create_pr" || o[1].Status != task.OpDone || o[1].PRNumber != 1 {
		t.Fatalf("ops = %+v", o)
	}
	// A second publish of the same proposal is refused only if in flight;
	// finished operations do not block, but nothing here re-runs them.
	rec, err := e.pub.Reconcile(ctx, "run1")
	if err != nil || rec.Outcome != "completed" || rec.Result.PRNumber != 1 {
		t.Fatalf("reconcile after success = %+v %v", rec, err)
	}
}

func TestPublish_BaseMovedProceedsUnchanged(t *testing.T) {
	e := newEnv(t)
	e.api.SetBranch("acme", "app", "main", strings.Repeat("b", 40))
	res, err := e.pub.Publish(ctx, "run1", "step1", e.prop)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BaseMoved || res.HeadCommit != e.prop.HeadCommit {
		t.Fatalf("result = %+v", res)
	}
	pr := e.api.Repo("acme", "app").PRs[0]
	if !strings.Contains(pr.Body, "has moved since") || !strings.Contains(pr.Body, e.prop.BaseCommit) {
		t.Fatalf("body = %s", pr.Body)
	}
	if ops(t, e)[0].BaseHeadAtPublish != strings.Repeat("b", 40) {
		t.Fatal("observed base at publish not recorded")
	}
}

func TestReconcile_PushOkPRFailed(t *testing.T) {
	e := newEnv(t)
	e.api.FailNextCreatePR = true
	if _, err := e.pub.Publish(ctx, "run1", "step1", e.prop); err == nil {
		t.Fatal("expected PR failure")
	}
	o := ops(t, e)
	if o[0].Status != task.OpDone || o[1].Status != task.OpFailed {
		t.Fatalf("ops = %+v", o)
	}
	before := e.remoteHead(t)
	rec, err := e.pub.Reconcile(ctx, "run1")
	if err != nil || rec.Outcome != "completed" || rec.Result.PRNumber != 1 {
		t.Fatalf("rec = %+v %v", rec, err)
	}
	if e.remoteHead(t) != before {
		t.Fatal("push repeated")
	}
	if len(e.api.Repo("acme", "app").PRs) != 1 || !strings.Contains(rec.Actions[len(rec.Actions)-1], "created") {
		t.Fatalf("prs = %d actions = %v", len(e.api.Repo("acme", "app").PRs), rec.Actions)
	}
}

func TestReconcile_PRCreatedResponseLost(t *testing.T) {
	e := newEnv(t)
	e.api.DropNextCreatePRResponse = true
	if _, err := e.pub.Publish(ctx, "run1", "step1", e.prop); err == nil {
		t.Fatal("expected a transport error")
	}
	if len(e.api.Repo("acme", "app").PRs) != 1 {
		t.Fatal("PR was not created server-side")
	}
	rec, err := e.pub.Reconcile(ctx, "run1")
	if err != nil || rec.Outcome != "completed" || rec.Result.PRNumber != 1 {
		t.Fatalf("rec = %+v %v", rec, err)
	}
	if len(e.api.Repo("acme", "app").PRs) != 1 {
		t.Fatal("PR recreated despite the marker")
	}
	if !strings.Contains(strings.Join(rec.Actions, " "), "found #1 by marker") {
		t.Fatalf("actions = %v", rec.Actions)
	}
}

func TestReconcile_ClosedAndMergedPRsNotRecreated(t *testing.T) {
	for _, state := range []string{"closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			e.api.FailNextCreatePR = true
			e.pub.Publish(ctx, "run1", "step1", e.prop)
			pr := fake.PR{State: "closed", Body: "x " + publish.Marker(e.prop.ID)}
			pr.Head.Ref, pr.Base.Ref = e.prop.HeadRef, "main"
			if state == "merged" {
				m := "2026-09-20T00:00:00Z"
				pr.MergedAt, pr.Merged = &m, true
			}
			e.api.AddPR("acme", "app", pr)
			rec, err := e.pub.Reconcile(ctx, "run1")
			if err != nil || rec.Outcome != "completed" || rec.Result.PRNumber != 1 {
				t.Fatalf("rec = %+v %v", rec, err)
			}
			if len(e.api.Repo("acme", "app").PRs) != 1 {
				t.Fatal("closed PR recreated")
			}
			if !strings.Contains(strings.Join(rec.Actions, " "), state) {
				t.Fatalf("actions = %v", rec.Actions)
			}
		})
	}
}

func TestReconcile_ForeignPRConflicts(t *testing.T) {
	e := newEnv(t)
	e.api.FailNextCreatePR = true
	e.pub.Publish(ctx, "run1", "step1", e.prop)
	pr := fake.PR{State: "open", Body: "someone else's"}
	pr.Head.Ref, pr.Base.Ref = e.prop.HeadRef, "main"
	e.api.AddPR("acme", "app", pr)
	rec, err := e.pub.Reconcile(ctx, "run1")
	if err != nil || rec.Outcome != "conflict" || !strings.Contains(rec.Detail, "without the proposal marker") {
		t.Fatalf("rec = %+v %v", rec, err)
	}
}

func TestReconcile_RefAbsentPushesAgain(t *testing.T) {
	e := newEnv(t)
	// Journal says dispatched but nothing reached the remote.
	op := task.PublicationOp{ID: "op1", RunID: "run1", ProposalID: e.prop.ID, Kind: "push", Status: task.OpDispatched, Marker: "m", DispatchedAt: task.Now()}
	e.store.InsertPublicationOp(ctx, op)
	rec, err := e.pub.Reconcile(ctx, "run1")
	if err != nil || rec.Outcome != "completed" || rec.Result.PRNumber != 1 {
		t.Fatalf("rec = %+v %v", rec, err)
	}
	if e.remoteHead(t) != e.prop.HeadCommit {
		t.Fatal("push not repeated for an absent ref")
	}
	if !strings.Contains(strings.Join(rec.Actions, " "), "ref absent, pushed") || !strings.Contains(strings.Join(rec.Actions, " "), "none found, created") {
		t.Fatalf("actions = %v", rec.Actions)
	}
}

func TestReconcile_RefAtDifferentCommitConflicts(t *testing.T) {
	e := newEnv(t)
	op := task.PublicationOp{ID: "op1", RunID: "run1", ProposalID: e.prop.ID, Kind: "push", Status: task.OpDispatched, Marker: "m", DispatchedAt: task.Now()}
	e.store.InsertPublicationOp(ctx, op)
	e.api.SetBranch("acme", "app", e.prop.HeadRef, strings.Repeat("c", 40))
	rec, err := e.pub.Reconcile(ctx, "run1")
	if err != nil || rec.Outcome != "conflict" {
		t.Fatalf("rec = %+v %v", rec, err)
	}
	if e.remoteHead(t) != "" {
		t.Fatal("pushed over a conflicting ref")
	}
}

func TestPublish_InFlightRefused(t *testing.T) {
	e := newEnv(t)
	e.store.InsertPublicationOp(ctx, task.PublicationOp{ID: "op1", RunID: "run1", ProposalID: e.prop.ID, Kind: "push", Status: task.OpDispatched, Marker: "m"})
	if _, err := e.pub.Publish(ctx, "run1", "s", e.prop); err == nil || !strings.Contains(err.Error(), "reconcile first") {
		t.Fatalf("err = %v", err)
	}
}

func TestPushCommand_TokenOnlyInEnvironment(t *testing.T) {
	args, env := publish.PushCommand("https://github.com/acme/app.git", "abc", "repo-steward/x", "ghp_secret")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ghp_secret") {
		t.Fatal("token in arguments")
	}
	if len(env) != 1 || env[0] != "REPO_STEWARD_PUSH_TOKEN=ghp_secret" {
		t.Fatalf("env = %v", env)
	}
	if !strings.Contains(joined, "credential.helper=!f()") || !strings.Contains(joined, "--no-verify") || !strings.HasSuffix(joined, "abc:refs/heads/repo-steward/x") {
		t.Fatalf("args = %s", joined)
	}
	args, env = publish.PushCommand("file:///tmp/x", "abc", "b", "")
	if len(env) != 0 || strings.Contains(strings.Join(args, " "), "credential") {
		t.Fatal("helper configured without a token")
	}
}

func TestClient_TokenSentAsBearer(t *testing.T) {
	api := fake.New()
	defer api.Close()
	api.RequireToken = "tok"
	api.AddRepo("acme", "app", "main", strings.Repeat("a", 40))
	if _, err := github.New(api.URL(), "wrong").GetRepository(ctx, "acme", "app"); err == nil {
		t.Fatal("bad token accepted")
	}
	if _, err := github.New(api.URL(), "tok").GetRepository(ctx, "acme", "app"); err != nil {
		t.Fatal(err)
	}
}
