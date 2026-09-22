//go:build integration

package steward_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/github/fake"
	"github.com/joeylking/repo-steward/internal/gitx"
)

// publication drives the built binary against an in-process fake GitHub
// API and a bare repository standing in for the destination's git side.
type publication struct {
	*cli
	api  *fake.Server
	bare string
	base string
}

func newPublication(t *testing.T) *publication {
	t.Helper()
	c := newCLI(t, "patch-safe")
	bare := filepath.Join(c.root, "dest.git")
	if _, err := gitx.New(c.root).Run(ctx, "clone", "-q", "--bare", c.repo, bare); err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.New(c.repo).RevParse(ctx, "HEAD")
	api := fake.New()
	t.Cleanup(api.Close)
	api.AddRepo("acme", "app", "main", head).GitDir = bare
	return &publication{cli: c, api: api, bare: bare, base: head}
}

func (p *publication) maintainPublish(bin string, env []string, extra ...string) (map[string]any, int, string) {
	args := append([]string{"-mode", "scripted", "-scenario", "S1P", "-publish", "-destination", "acme/app", "-github-api", p.api.URL(), "-push-url", "file://" + p.bare}, extra...)
	return p.maintain(bin, env, args...)
}

func (p *publication) remoteHead(t *testing.T, branch string) string {
	out, err := gitx.New(p.bare).Run(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestCLI_PublicationAcrossProcesses(t *testing.T) {
	p := newPublication(t)
	res, code, stderr := p.maintainPublish(p.bin, nil)
	if code != 5 || res["outcome"] != "awaiting_approval" {
		t.Fatalf("code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	runID := res["run_id"].(string)
	ts := tools(res)
	if ts[len(ts)-2] != "prepare_proposal:done" || ts[len(ts)-1] != "publish_proposal:awaiting_approval" {
		t.Fatalf("tools = %v", ts)
	}
	if p.remoteHead(t, "repo-steward/lib-v1.2.4") != "" || len(p.api.Repo("acme", "app").PRs) != 0 {
		t.Fatal("something reached the destination before approval")
	}
	show, _, _ := p.run(p.bin, nil, "runs", "show", runID, "-data-dir", p.data)
	approvals := show["approvals"].([]any)
	a := approvals[0].(map[string]any)
	if a["kind"] != "publication" || a["status"] != "pending" {
		t.Fatalf("approval = %v", a)
	}
	pres := a["presentation"].(map[string]any)
	if pres["head_ref"] != "repo-steward/lib-v1.2.4" || pres["base_ref"] != "main" {
		t.Fatalf("presentation = %v", pres)
	}
	proposals := show["proposals"].([]any)
	if len(proposals) != 1 || proposals[0].(map[string]any)["status"] != "frozen" {
		t.Fatalf("proposals = %v", proposals)
	}
	headCommit := proposals[0].(map[string]any)["head_commit"].(string)

	if _, code, stderr := p.run(p.bin, nil, "approve", runID, "-data-dir", p.data, "-note", "ship it"); code != 0 {
		t.Fatalf("approve: %s", stderr)
	}
	res, code, stderr = p.run(p.bin, nil, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy)
	if code != 0 || res["outcome"] != "proposal_published" {
		t.Fatalf("resume: code %d outcome %v detail %v\n%s", code, res["outcome"], res["detail"], stderr)
	}
	detail := res["detail"].(map[string]any)
	if detail["pr_number"].(float64) != 1 || detail["branch"] != "repo-steward/lib-v1.2.4" || detail["head_commit"] != headCommit {
		t.Fatalf("detail = %v", detail)
	}
	if p.remoteHead(t, "repo-steward/lib-v1.2.4") != headCommit {
		t.Fatalf("remote branch = %s, want %s", p.remoteHead(t, "repo-steward/lib-v1.2.4"), headCommit)
	}
	prs := p.api.Repo("acme", "app").PRs
	if len(prs) != 1 || prs[0].Head.Ref != "repo-steward/lib-v1.2.4" || prs[0].Base.Ref != "main" || !strings.Contains(prs[0].Body, "repo-steward:proposal:") || strings.Contains(strings.ToLower(prs[0].Title+prs[0].Body), "assistant") {
		t.Fatalf("prs = %+v", prs)
	}
	// The base branch on the destination is untouched, and the source too.
	if p.remoteHead(t, "main") != p.base {
		t.Fatal("destination base branch moved")
	}
	show, _, _ = p.run(p.bin, nil, "runs", "show", runID, "-data-dir", p.data)
	if show["task"].(map[string]any)["outcome"] != "proposal_published" || show["proposals"].([]any)[0].(map[string]any)["status"] != "published" {
		t.Fatalf("after publish: %v", show["task"])
	}
}

func TestCLI_PublicationRefusedWhenBaseMismatch(t *testing.T) {
	p := newPublication(t)
	p.api.SetBranch("acme", "app", "main", strings.Repeat("d", 40))
	_, code, stderr := p.maintainPublish(p.bin, nil)
	if code != 1 || !strings.Contains(stderr, "differs from the local base commit") {
		t.Fatalf("code %d\n%s", code, stderr)
	}
	// Nothing was created: no run directory, no data.
	if entries, _ := os.ReadDir(filepath.Join(p.data, "runs")); len(entries) != 0 {
		t.Fatalf("run directories created despite refusal: %d", len(entries))
	}
}

func TestCLI_PublicationInterruptedIsReconciled(t *testing.T) {
	p := newPublication(t)
	res, _, _ := p.maintainPublish(p.bin, nil)
	runID := res["run_id"].(string)
	p.run(p.bin, nil, "approve", runID, "-data-dir", p.data)
	// Crash after the pull request was created but before it was recorded.
	_, code, stderr := p.run(p.fault, []string{"REPO_STEWARD_FAULT=publish.pr.done_unrecorded"}, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy)
	if code != 3 || !strings.Contains(stderr, "crashing at publish.pr.done_unrecorded") {
		t.Fatalf("fault resume: code %d\n%s", code, stderr)
	}
	if len(p.api.Repo("acme", "app").PRs) != 1 {
		t.Fatal("PR was not created before the crash")
	}
	res, code, stderr = p.run(p.bin, nil, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy)
	if code != 0 || res["outcome"] != "proposal_published" {
		t.Fatalf("recovery resume: code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	detail := res["detail"].(map[string]any)
	if detail["recovered"] != true || detail["pr_number"].(float64) != 1 {
		t.Fatalf("detail = %v", detail)
	}
	if len(p.api.Repo("acme", "app").PRs) != 1 {
		t.Fatal("PR recreated during reconciliation")
	}
	// Reconciliation completed the run; no further agent decision was made.
	ts := tools(res)
	if ts[len(ts)-1] != "publish_proposal:interrupted" {
		t.Fatalf("tools = %v", ts)
	}
	if res["run"].(map[string]any)["status"] != "COMPLETED" {
		t.Fatalf("run = %v", res["run"])
	}
}

func TestCLI_PublicationInterruptedBeforePRIsFinished(t *testing.T) {
	p := newPublication(t)
	res, _, _ := p.maintainPublish(p.bin, nil)
	runID := res["run_id"].(string)
	p.run(p.bin, nil, "approve", runID, "-data-dir", p.data)
	_, code, _ := p.run(p.fault, []string{"REPO_STEWARD_FAULT=publish.push.done_unrecorded"}, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy)
	if code != 3 {
		t.Fatalf("fault resume code %d", code)
	}
	if p.remoteHead(t, "repo-steward/lib-v1.2.4") == "" || len(p.api.Repo("acme", "app").PRs) != 0 {
		t.Fatal("unexpected remote state after the crash")
	}
	res, code, stderr := p.run(p.bin, nil, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy)
	if code != 0 || res["outcome"] != "proposal_published" {
		t.Fatalf("recovery resume: code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	if len(p.api.Repo("acme", "app").PRs) != 1 {
		t.Fatalf("prs = %d", len(p.api.Repo("acme", "app").PRs))
	}
	actions := res["detail"].(map[string]any)["actions"].([]any)
	joined := ""
	for _, a := range actions {
		joined += a.(string) + " "
	}
	if !strings.Contains(joined, "ref already at") || !strings.Contains(joined, "none found, created") {
		t.Fatalf("actions = %v", actions)
	}
}

func TestCLI_PublicationRejected(t *testing.T) {
	p := newPublication(t)
	res, _, _ := p.maintainPublish(p.bin, nil)
	runID := res["run_id"].(string)
	if _, code, _ := p.run(p.bin, nil, "reject", runID, "-data-dir", p.data, "-note", "not today"); code != 0 {
		t.Fatal("reject failed")
	}
	if p.remoteHead(t, "repo-steward/lib-v1.2.4") != "" || len(p.api.Repo("acme", "app").PRs) != 0 {
		t.Fatal("rejected publication reached the destination")
	}
	show, _, _ := p.run(p.bin, nil, "runs", "show", runID, "-data-dir", p.data)
	if show["task"].(map[string]any)["outcome"] != "approval_rejected" {
		t.Fatalf("task = %v", show["task"])
	}
}

// TestCLI_PublicationDestinationFromOriginAcrossProcesses covers the path a
// real run takes: no -destination, so the destination is parsed from the
// source's origin remote. It must be persisted with the run, or a resume in
// a new process cannot register the publish tool and the approved request
// cannot execute.
func TestCLI_PublicationDestinationFromOriginAcrossProcesses(t *testing.T) {
	p := newPublication(t)
	if _, err := gitx.New(p.repo).Run(ctx, "remote", "add", "origin", "https://github.com/acme/app.git"); err != nil {
		t.Fatal(err)
	}
	res, code, stderr := p.maintain(p.bin, nil, "-mode", "scripted", "-scenario", "S1P", "-publish", "-github-api", p.api.URL(), "-push-url", "file://"+p.bare)
	if code != 5 || res["outcome"] != "awaiting_approval" {
		t.Fatalf("code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	runID := res["run_id"].(string)
	dest := res["destination"].(map[string]any)
	if dest["owner"] != "acme" || dest["repo"] != "app" || dest["push_url"] != "file://"+p.bare {
		t.Fatalf("destination = %v", dest)
	}
	if _, code, stderr := p.run(p.bin, nil, "approve", runID, "-data-dir", p.data); code != 0 {
		t.Fatalf("approve: %s", stderr)
	}
	res, code, stderr = p.run(p.bin, nil, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy)
	if code != 0 || res["outcome"] != "proposal_published" {
		t.Fatalf("resume: code %d outcome %v detail %v\n%s", code, res["outcome"], res["detail"], stderr)
	}
	if p.remoteHead(t, "repo-steward/lib-v1.2.4") == "" || len(p.api.Repo("acme", "app").PRs) != 1 {
		t.Fatal("publication did not reach the destination")
	}
}

// TestCLI_PublicationCancelledAfterApproval closes a run whose approval was
// granted but which was never resumed. Reject cannot apply to a decided
// approval; cancel ends the run, and a later resume is refused.
func TestCLI_PublicationCancelledAfterApproval(t *testing.T) {
	p := newPublication(t)
	res, _, _ := p.maintainPublish(p.bin, nil)
	runID := res["run_id"].(string)
	if _, code, stderr := p.run(p.bin, nil, "approve", runID, "-data-dir", p.data); code != 0 {
		t.Fatalf("approve: %s", stderr)
	}
	if _, code, _ := p.run(p.bin, nil, "reject", runID, "-data-dir", p.data); code == 0 {
		t.Fatal("reject after approval must fail")
	}
	out, code, stderr := p.run(p.bin, nil, "cancel", runID, "-data-dir", p.data, "-note", "changed my mind")
	if code != 0 || out["outcome"] != "cancelled" {
		t.Fatalf("cancel: code %d %v\n%s", code, out, stderr)
	}
	if _, code, _ := p.run(p.bin, nil, "resume", runID, "-data-dir", p.data, "-fixture-proxy", p.proxy); code == 0 {
		t.Fatal("resume of a cancelled run must fail")
	}
	if p.remoteHead(t, "repo-steward/lib-v1.2.4") != "" || len(p.api.Repo("acme", "app").PRs) != 0 {
		t.Fatal("cancelled publication reached the destination")
	}
	show, _, _ := p.run(p.bin, nil, "runs", "show", runID, "-data-dir", p.data)
	if show["task"].(map[string]any)["outcome"] != "cancelled" || show["run"].(map[string]any)["Reason"] != "operator_cancelled" {
		t.Fatalf("after cancel: task %v run %v", show["task"], show["run"])
	}
}
