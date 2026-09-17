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
	if _, code, stderr := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy); code == 0 || !strings.Contains(stderr, "no approved approval") {
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

	res, code, stderr := c.run(c.bin, nil, "resume", runID, "-data-dir", c.data, "-fixture-proxy", c.proxy)
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
