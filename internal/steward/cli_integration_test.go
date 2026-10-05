//go:build integration

package steward_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/testtmp"
)

// These tests drive the built command line across separate processes:
// pause for approval in one, decide in another, resume in a third. The
// fault-injected binary crashes mid-step to prove interrupted-run resume.

type cli struct {
	t      *testing.T
	bin    string
	fault  string
	root   string
	proxy  string
	repo   string
	data   string
	author string
}

func newCLI(t *testing.T, fixtureName string) *cli {
	t.Helper()
	root := testtmp.Dir(t)
	c := &cli{t: t, root: root, proxy: filepath.Join(root, "proxy"), data: filepath.Join(root, "data"), author: "Fixture Operator <op@example.invalid>"}
	c.bin = filepath.Join(root, "repo-steward")
	c.fault = filepath.Join(root, "repo-steward-fault")
	for _, b := range []struct{ out, tags string }{{c.bin, ""}, {c.fault, "faultinject"}} {
		args := []string{"build", "-o", b.out}
		if b.tags != "" {
			args = append(args, "-tags", b.tags)
		}
		args = append(args, "../../cmd/repo-steward")
		if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	r, err := fixture.Setup(ctx, fixtureName, filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	c.repo = r.Path
	if _, err := modproxy.Build(c.proxy); err != nil {
		t.Fatal(err)
	}
	return c
}

// run executes the CLI and returns stdout JSON (when any), the exit code,
// and stderr.
func (c *cli) run(bin string, env []string, args ...string) (map[string]any, int, string) {
	c.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		c.t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
	var parsed map[string]any
	if len(out) > 0 {
		if err := json.Unmarshal(out, &parsed); err != nil {
			// runs list prints an array; leave parsed nil.
			parsed = nil
		}
	}
	return parsed, code, stderr.String()
}

func (c *cli) maintain(bin string, env []string, extra ...string) (map[string]any, int, string) {
	args := append([]string{"maintain", c.repo, "-author", c.author, "-data-dir", c.data, "-fixture-proxy", c.proxy}, extra...)
	return c.run(bin, env, args...)
}

// resumeAfterCrash retries a resume after a fault-injected process exit,
// which leaves the run's lease stamped for a now-dead owner: agent-runtime
// v0.3.0 refuses to resume a leased run until that lease expires
// (agentrt.DefaultLeaseTTL, 30 seconds by default), the same wait a real
// crash recovery meets. It retries on exactly that refusal, the wording
// cmd/repo-steward's own resumeError prints, up to a generous bound so the
// test is not tied to the exact default.
func (c *cli) resumeAfterCrash(bin string, env []string, args ...string) (map[string]any, int, string) {
	deadline := time.Now().Add(45 * time.Second)
	for {
		res, code, stderr := c.run(bin, env, args...)
		if !strings.Contains(stderr, "is still being executed by") || time.Now().After(deadline) {
			return res, code, stderr
		}
		time.Sleep(time.Second)
	}
}

func tools(res map[string]any) []string {
	var out []string
	if run, ok := res["run"].(map[string]any); ok {
		for _, t := range run["tools"].([]any) {
			out = append(out, t.(string))
		}
	}
	return out
}

func files(res map[string]any) []string {
	var out []string
	if p, ok := res["proposal"].(map[string]any); ok {
		for _, f := range p["files"].([]any) {
			out = append(out, f.(map[string]any)["path"].(string))
		}
	}
	return out
}

func TestCLI_ScopeExpansionAcrossProcesses(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, code, stderr := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S10", "-scope-files-soft", "1", "-scope-files-hard", "3")
	if code != 5 || res["outcome"] != "awaiting_approval" {
		t.Fatalf("code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	runID := res["run_id"].(string)
	last := tools(res)[len(tools(res))-1]
	if last != "write_file:awaiting_approval" {
		t.Fatalf("last step = %s", last)
	}

	show, code, _ := c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	if code != 0 {
		t.Fatal("runs show failed")
	}
	approvals := show["approvals"].([]any)
	if len(approvals) != 1 || approvals[0].(map[string]any)["kind"] != "scope_expansion" || approvals[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("approvals = %v", approvals)
	}
	if show["run"].(map[string]any)["Status"] != "WAITING_FOR_APPROVAL" {
		t.Fatalf("run = %v", show["run"])
	}

	// Resuming before a decision must fail and change nothing.
	if _, code, stderr := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy); code == 0 || !strings.Contains(stderr, "not approved") {
		t.Fatalf("resume before approval: code %d\n%s", code, stderr)
	}
	// Resuming without the fixture proxy the run started with is refused.
	if _, code, stderr := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data); code == 0 || !strings.Contains(stderr, "fixture proxy") {
		t.Fatalf("resume without proxy: code %d\n%s", code, stderr)
	}

	dec, code, stderr := c.run(c.bin, nil, "approve", runID, "-data-dir", c.data, "-note", "small file", "-by", "tester")
	if code != 0 {
		t.Fatalf("approve: %s", stderr)
	}
	if a := dec["approval"].(map[string]any); a["status"] != "approved" || a["decided_by"] != "tester" {
		t.Fatalf("approval = %v", a)
	}
	// Approving twice is refused.
	if _, code, _ := c.run(c.bin, nil, "approve", runID, "-data-dir", c.data); code == 0 {
		t.Fatal("second approve succeeded")
	}

	res, code, stderr = c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy)
	if code != 0 || res["outcome"] != "proposal_prepared" {
		t.Fatalf("resume: code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	if got := strings.Join(files(res), ","); got != "go.mod,go.sum,main.go,notes.go" {
		t.Fatalf("files = %s", got)
	}
	ts := tools(res)
	if ts[6] != "write_file:done" || ts[len(ts)-1] != "prepare_proposal:done" {
		t.Fatalf("tools = %v", ts)
	}
	// The resumed run is terminal; a further resume is refused.
	if _, code, _ := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy); code == 0 {
		t.Fatal("resume of a finished run succeeded")
	}
}

func TestCLI_HardScopeLimitAborts(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, code, stderr := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S10", "-scope-files-soft", "1", "-scope-files-hard", "1")
	if code != 4 || res["outcome"] != "scope_exceeded" {
		t.Fatalf("code %d outcome %v detail %v\n%s", code, res["outcome"], res["detail"], stderr)
	}
	if _, ok := res["proposal"]; ok {
		t.Fatal("proposal present")
	}
	show, _, _ := c.run(c.bin, nil, "runs", "show", res["run_id"].(string), "-data-dir", c.data)
	if approvals, _ := show["approvals"].([]any); len(approvals) != 0 {
		t.Fatal("a hard limit must not ask for approval")
	}
}

func TestCLI_RejectCancels(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, _, _ := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S10", "-scope-files-soft", "1", "-scope-files-hard", "3")
	runID := res["run_id"].(string)
	if _, code, stderr := c.run(c.bin, nil, "reject", runID, "-data-dir", c.data, "-note", "no"); code != 0 {
		t.Fatalf("reject: %s", stderr)
	}
	show, _, _ := c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	if show["task"].(map[string]any)["outcome"] != "approval_rejected" || show["run"].(map[string]any)["Status"] != "CANCELLED" {
		t.Fatalf("after reject: %v %v", show["task"], show["run"])
	}
	if _, code, stderr := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy); code == 0 || !strings.Contains(stderr, "already ended") {
		t.Fatalf("resume after reject: code %d\n%s", code, stderr)
	}
	// The pending write never landed.
	if _, err := os.Stat(filepath.Join(show["task"].(map[string]any)["workspace_dir"].(string), "notes.go")); err == nil {
		t.Fatal("rejected write landed in the workspace")
	}
}

func TestCLI_InterruptedRunResumes(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	_, code, stderr := c.maintain(c.fault, []string{"REPO_STEWARD_FAULT=tools.write_file.after_write"}, "-mode", "scripted", "-scenario", "S2")
	if code != 3 || !strings.Contains(stderr, "crashing at tools.write_file.after_write") {
		t.Fatalf("fault run: code %d\n%s", code, stderr)
	}
	// Find the run and confirm what the crash left behind.
	cmd := exec.Command(c.bin, "runs", "list", "-data-dir", c.data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	json.Unmarshal(out, &rows)
	if len(rows) != 1 || rows[0]["run_status"] != "RUNNING" || rows[0]["outcome"] != "" {
		t.Fatalf("rows = %v", rows)
	}
	runID := rows[0]["run_id"].(string)
	show, _, _ := c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	steps := show["steps"].([]any)
	lastStep := steps[len(steps)-1].(map[string]any)
	if lastStep["tool"] != "write_file" || lastStep["status"] != "executing" {
		t.Fatalf("last step before resume = %v", lastStep)
	}

	res, code, stderr := c.resumeAfterCrash(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy)
	if code != 0 || res["outcome"] != "proposal_prepared" {
		t.Fatalf("resume: code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	ts := tools(res)
	if ts[5] != "write_file:interrupted" {
		t.Fatalf("interrupted step not recorded: %v", ts)
	}
	if got := strings.Join(files(res), ","); got != "go.mod,go.sum,main.go" {
		t.Fatalf("files = %s", got)
	}
}

// Without -ask-unexercised a repair no test exercises ends the run as an
// explained non-result, exit 4, with no proposal.
func TestCLI_UnexercisedRepairEndsTheRun(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, code, stderr := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S2U")
	if code != 4 || res["outcome"] != "repair_not_exercised" {
		t.Fatalf("code %d outcome %v detail %v\n%s", code, res["outcome"], res["detail"], stderr)
	}
	if _, ok := res["proposal"]; ok {
		t.Fatal("proposal present")
	}
	if d := res["detail"].(map[string]any); !strings.Contains(d["not_exercised"].(string), "main.go:16 (no test executes it)") {
		t.Fatalf("detail %v", d)
	}
	show, _, _ := c.run(c.bin, nil, "runs", "show", res["run_id"].(string), "-data-dir", c.data)
	if approvals, _ := show["approvals"].([]any); len(approvals) != 0 {
		t.Fatal("an approval was asked for without -ask-unexercised")
	}
}

// With -ask-unexercised the same prepare_proposal pauses for an approval
// of kind unexercised_repair bound to the candidate tree. approve prints
// the unexercised line and the patch before deciding; after resume the
// proposal is frozen and its body opens with the statement that it was
// approved without test coverage, naming the approval.
func TestCLI_UnexercisedApprovalAcrossProcesses(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, code, stderr := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S2U", "-ask-unexercised")
	if code != 5 || res["outcome"] != "awaiting_approval" {
		t.Fatalf("code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	runID := res["run_id"].(string)
	if ts := tools(res); ts[len(ts)-1] != "prepare_proposal:awaiting_approval" {
		t.Fatalf("tools = %v", ts)
	}
	show, _, _ := c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	approvals := show["approvals"].([]any)
	a := approvals[0].(map[string]any)
	if len(approvals) != 1 || a["kind"] != "unexercised_repair" || a["status"] != "pending" {
		t.Fatalf("approvals = %v", approvals)
	}
	tree := a["capability"].(map[string]any)["tree"].(string)
	if tree == "" {
		t.Fatalf("capability = %v", a["capability"])
	}
	if rows, _ := show["proposals"].([]any); len(rows) != 0 {
		t.Fatal("a proposal exists before the approval")
	}

	dec, code, stderr := c.run(c.bin, nil, "approve", runID, "-data-dir", c.data, "-note", "main only prints", "-by", "tester")
	if code != 0 || dec["approval"].(map[string]any)["status"] != "approved" {
		t.Fatalf("approve: code %d\n%s", code, stderr)
	}
	for _, want := range []string{"unexercised_repair", "main.go:16 (no test executes it)", `greeting(\"repo-steward\")`, tree} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("approve did not show %q:\n%s", want, stderr)
		}
	}

	res, code, stderr = c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy)
	if code != 0 || res["outcome"] != "proposal_prepared" {
		t.Fatalf("resume: code %d outcome %v detail %v\n%s", code, res["outcome"], res["detail"], stderr)
	}
	if got := strings.Join(files(res), ","); got != "go.mod,go.sum,main.go" {
		t.Fatalf("files = %s", got)
	}
	p := res["proposal"].(map[string]any)
	body := p["body"].(string)
	want := "WARNING: NO TEST EXERCISES PART OF THIS REPAIR. It was approved without test coverage by tester in approval " + a["id"].(string) + ", for tree " + tree
	if !strings.HasPrefix(body, want) || !strings.Contains(body, "Not exercised by any test: main.go:16 (no test executes it).") || !strings.Contains(body, "Changed source executed by tests: 1 of 2 required coverage blocks") {
		t.Fatalf("body:\n%s", body)
	}
	cov := res["readiness"].(map[string]any)["coverage"].(map[string]any)
	if cov["verified"] != false || cov["approval"].(map[string]any)["id"] != a["id"] || p["tree_hash"] != tree {
		t.Fatalf("coverage %v tree %v", cov, p["tree_hash"])
	}
}

// Rejecting the approval ends the run with no proposal.
func TestCLI_UnexercisedRejected(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, code, _ := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S2U", "-ask-unexercised")
	if code != 5 {
		t.Fatalf("code %d", code)
	}
	runID := res["run_id"].(string)
	if _, code, stderr := c.run(c.bin, nil, "reject", runID, "-data-dir", c.data, "-note", "needs a test"); code != 0 {
		t.Fatalf("reject: %s", stderr)
	}
	show, _, _ := c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	if show["task"].(map[string]any)["outcome"] != "approval_rejected" || show["run"].(map[string]any)["Status"] != "CANCELLED" {
		t.Fatalf("after reject: %v %v", show["task"], show["run"])
	}
	if rows, _ := show["proposals"].([]any); len(rows) != 0 {
		t.Fatalf("proposals after reject: %v", rows)
	}
	if _, code, _ := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy); code == 0 {
		t.Fatal("resume after reject succeeded")
	}
}

// The approval is bound to the tree it was asked for: when the candidate
// tree changes between the decision and the resume, the approved request
// runs against a tree no approval names and no validation is bound to,
// readiness refuses it, and nothing is frozen.
func TestCLI_UnexercisedApprovalBoundToTree(t *testing.T) {
	c := newCLI(t, "breaking-minor")
	res, code, _ := c.maintain(c.bin, nil, "-mode", "scripted", "-scenario", "S2U", "-ask-unexercised")
	if code != 5 {
		t.Fatalf("code %d", code)
	}
	runID := res["run_id"].(string)
	if _, code, stderr := c.run(c.bin, nil, "approve", runID, "-data-dir", c.data); code != 0 {
		t.Fatalf("approve: %s", stderr)
	}
	show, _, _ := c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	main := filepath.Join(show["task"].(map[string]any)["workspace_dir"].(string), "main.go")
	b, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte(strings.Replace(string(b), "repo-steward", "someone else", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, code, stderr := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy)
	if code == 0 || res["outcome"] == "proposal_prepared" {
		t.Fatalf("resume after the tree changed: code %d outcome %v\n%s", code, res["outcome"], stderr)
	}
	if _, ok := res["proposal"]; ok {
		t.Fatal("proposal present")
	}
	show, _, _ = c.run(c.bin, nil, "runs", "show", runID, "-data-dir", c.data)
	if rows, _ := show["proposals"].([]any); len(rows) != 0 {
		t.Fatalf("proposals: %v", rows)
	}
}
