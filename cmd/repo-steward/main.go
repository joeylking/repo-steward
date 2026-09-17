// Command repo-steward is the CLI. Milestone 0 provides fixture setup,
// snapshot construction, and the no-model inspect workflow; maintain, runs,
// approve, and reject arrive with later milestones.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/inspect"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/task"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo-steward:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  repo-steward maintain <repo-path> -mode baseline|scripted [-scenario NAME] [-author "Name <email>"] [-data-dir DIR] [-fixture-proxy DIR] [-pull] [-allow-major] [-dependency MODULE] [-check-timeout DURATION] [-scope-files-soft N] [-scope-files-hard N] [-scope-lines-soft N] [-scope-lines-hard N] [-trace]
  repo-steward resume <run-id> [-data-dir DIR] [-fixture-proxy DIR] [-trace]
  repo-steward approve <run-id> [-approval ID] [-note TEXT] [-data-dir DIR]
  repo-steward reject <run-id> [-approval ID] [-note TEXT] [-data-dir DIR]
  repo-steward runs list [-data-dir DIR]
  repo-steward runs show <run-id> [-events] [-data-dir DIR]
  repo-steward inspect <repo-path> [-data-dir DIR] [-fixture-proxy DIR] [-pull] [-allow-major] [-dependency MODULE] [-check-timeout DURATION]
  repo-steward fixture list
  repo-steward fixture setup <name> [-dest DIR]
  repo-steward fixture proxy [-dest DIR]
  repo-steward snapshot build <repo-path> [-dest DIR] [-max-files N] [-max-bytes N]
`

func run(args []string) error {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("missing command")
	}
	ctx := context.Background()
	if args[0] == "inspect" {
		return runInspect(ctx, args[1:])
	}
	if args[0] == "maintain" {
		return runMaintain(ctx, args[1:])
	}
	switch args[0] {
	case "resume":
		return runResume(ctx, args[1:])
	case "approve":
		return runDecide(ctx, args[1:], true)
	case "reject":
		return runDecide(ctx, args[1:], false)
	}
	if args[0] == "runs" && args[1] == "list" {
		return runsList(ctx, args[2:])
	}
	if args[0] == "runs" && args[1] == "show" {
		return runsShow(ctx, args[2:])
	}
	switch args[0] + " " + args[1] {
	case "fixture list":
		names, err := fixture.Names()
		if err != nil {
			return err
		}
		for _, n := range names {
			def, err := fixture.Load(n)
			if err != nil {
				return err
			}
			fmt.Printf("%-20s %s\n", n, def.Description)
		}
		return nil
	case "fixture setup":
		name, rest, err := positional(args[2:], "fixture setup: expected a fixture name")
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("fixture setup", flag.ContinueOnError)
		dest := fs.String("dest", "", "destination directory (default: a new temporary directory)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *dest == "" {
			d, err := os.MkdirTemp("", "repo-steward-fixture-"+name+"-")
			if err != nil {
				return err
			}
			*dest = d
		}
		repo, err := fixture.Setup(ctx, name, *dest)
		if err != nil {
			return err
		}
		return printJSON(repo)
	case "fixture proxy":
		fs := flag.NewFlagSet("fixture proxy", flag.ContinueOnError)
		dest := fs.String("dest", "", "destination directory (default: a new temporary directory)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *dest == "" {
			d, err := os.MkdirTemp("", "repo-steward-proxy-")
			if err != nil {
				return err
			}
			*dest = d
		}
		ix, err := modproxy.Build(*dest)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"dir": ix.Dir, "goproxy": modproxy.URL(ix.Dir), "modules": ix.Modules})
	case "snapshot build":
		arg, rest, err := positional(args[2:], "snapshot build: expected a repository path")
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("snapshot build", flag.ContinueOnError)
		dest := fs.String("dest", "", "destination directory (default: a new temporary directory)")
		maxFiles := fs.Int("max-files", snapshot.DefaultLimits().MaxFiles, "maximum number of files")
		maxBytes := fs.Int64("max-bytes", snapshot.DefaultLimits().MaxTotalBytes, "maximum total bytes")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		repoPath, err := filepath.Abs(arg)
		if err != nil {
			return err
		}
		if *dest == "" {
			d, err := os.MkdirTemp("", "repo-steward-snapshot-")
			if err != nil {
				return err
			}
			*dest = d
		}
		limits := snapshot.DefaultLimits()
		limits.MaxFiles = *maxFiles
		limits.MaxTotalBytes = *maxBytes

		g := gitx.New(repoPath)
		start := time.Now()
		base, err := g.RevParse(ctx, "HEAD^{tree}")
		if err != nil {
			return err
		}
		candidate, err := snapshot.BuildCandidateTree(ctx, g, base)
		if err != nil {
			return err
		}
		m, err := snapshot.Materialize(ctx, g, candidate, *dest, limits)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"repo":           repoPath,
			"base_tree":      base,
			"candidate_tree": m.Tree,
			"changed":        candidate != base,
			"dir":            m.Dir,
			"file_count":     m.FileCount,
			"total_bytes":    m.TotalBytes,
			"verified":       m.Verified,
			"elapsed_ms":     time.Since(start).Milliseconds(),
		})
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0]+" "+args[1])
	}
}

// runInspect performs a no-model inspection and prints the report. Exit
// status 2 means the repository is unsupported and 3 means the baseline is
// failing or inconclusive.
func runInspect(ctx context.Context, args []string) error {
	repoPath, rest, err := positional(args, "inspect: expected a repository path")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory (default: $XDG_DATA_HOME/repo-steward or ~/.local/share/repo-steward)")
	proxyDir := fs.String("fixture-proxy", "", "file-based module proxy directory; disables network and checksum database (fixtures only)")
	pull := fs.Bool("pull", false, "pull the pinned toolchain image if it is not present (one-time bootstrap)")
	allowMajor := fs.Bool("allow-major", false, "treat major upgrades as eligible")
	dependency := fs.String("dependency", "", "restrict eligibility to one module")
	checkTimeout := fs.Duration("check-timeout", 10*time.Minute, "timeout per validation check")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	pol := deps.DefaultPolicy()
	pol.AllowMajor = *allowMajor
	pol.NamedDependency = *dependency
	rep, err := inspect.Run(ctx, inspect.Options{RepoPath: repoPath, DataDir: *dataDir, FixtureProxyDir: *proxyDir, AllowPull: *pull, Policy: pol, CheckTimeout: *checkTimeout})
	if err != nil {
		return err
	}
	if err := printJSON(rep); err != nil {
		return err
	}
	switch rep.Outcome {
	case inspect.OutcomeUnsupported:
		os.Exit(2)
	case inspect.OutcomeBaselineFailing, inspect.OutcomeBaselineInconclusive:
		os.Exit(3)
	}
	return nil
}

// runMaintain performs one maintenance run. Exit status 0 means a proposal
// was prepared, 2 unsupported, 3 baseline problems, 4 an explained
// non-result after the upgrade was attempted.
func runMaintain(ctx context.Context, args []string) error {
	repoPath, rest, err := positional(args, "maintain: expected a repository path")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	mode := fs.String("mode", "baseline", "baseline (deterministic, no model) or scripted (replay an embedded scenario)")
	scenarioName := fs.String("scenario", "", "scenario name for -mode scripted")
	trace := fs.Bool("trace", false, "print runtime events to stderr as they happen")
	author := fs.String("author", "", `proposal commit author as "Name <email>" (default: the source repository's git user)`)
	dataDir := fs.String("data-dir", "", "data directory (default: ~/.local/share/repo-steward)")
	proxyDir := fs.String("fixture-proxy", "", "file-based module proxy directory; disables network and checksum database (fixtures only)")
	pull := fs.Bool("pull", false, "pull the pinned toolchain image if absent")
	allowMajor := fs.Bool("allow-major", false, "treat major upgrades as eligible")
	dependency := fs.String("dependency", "", "restrict eligibility to one module")
	checkTimeout := fs.Duration("check-timeout", 10*time.Minute, "timeout per validation check")
	scopeFilesSoft := fs.Int("scope-files-soft", 0, "soft limit on changed source files (default 10); crossing it asks for one approval")
	scopeFilesHard := fs.Int("scope-files-hard", 0, "hard limit on changed source files (default 20); crossing it ends the run")
	scopeLinesSoft := fs.Int("scope-lines-soft", 0, "soft limit on changed source lines (default 200)")
	scopeLinesHard := fs.Int("scope-lines-hard", 0, "hard limit on changed source lines (default 400)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	ident, err := resolveAuthor(*author, repoPath)
	if err != nil {
		return err
	}
	pol := deps.DefaultPolicy()
	pol.AllowMajor = *allowMajor
	pol.NamedDependency = *dependency
	opts := steward.Options{SourcePath: repoPath, DataDir: *dataDir, FixtureProxyDir: *proxyDir, AllowPull: *pull, Policy: pol, Author: ident, CheckTimeout: *checkTimeout}
	if *scopeFilesSoft > 0 || *scopeFilesHard > 0 || *scopeLinesSoft > 0 || *scopeLinesHard > 0 {
		sc := session.DefaultScope()
		if *scopeFilesSoft > 0 {
			sc.FilesSoft = *scopeFilesSoft
		}
		if *scopeFilesHard > 0 {
			sc.FilesHard = *scopeFilesHard
		}
		if *scopeLinesSoft > 0 {
			sc.LinesSoft = *scopeLinesSoft
		}
		if *scopeLinesHard > 0 {
			sc.LinesHard = *scopeLinesHard
		}
		if sc.FilesSoft > sc.FilesHard || sc.LinesSoft > sc.LinesHard {
			return fmt.Errorf("maintain: soft scope limits must not exceed hard limits")
		}
		opts.ScopeConfig = sc
		opts.Scope = proposal.ScopeLimits{MaxFiles: sc.FilesHard, MaxLines: sc.LinesHard}
	}
	if *trace {
		opts.Observer = traceObserver()
	}
	var res *steward.Result
	switch *mode {
	case "baseline":
		res, err = steward.RunBaseline(ctx, opts)
	case "scripted":
		if *scenarioName == "" {
			return fmt.Errorf("maintain: -mode scripted requires -scenario")
		}
		res, err = steward.RunScripted(ctx, opts, *scenarioName)
	default:
		return fmt.Errorf("maintain: unknown mode %q", *mode)
	}
	return report(res, err)
}

// report prints the result and exits with its outcome code: 0 proposal
// prepared, 2 unsupported, 3 baseline problems, 5 awaiting approval, 4 any
// other explained non-result.
func report(res *steward.Result, err error) error {
	if res != nil {
		printJSON(res)
	}
	if err != nil {
		return err
	}
	switch res.Outcome {
	case steward.OutcomeProposalPrepared:
	case steward.OutcomeUnsupported:
		os.Exit(2)
	case steward.OutcomeBaselineFailing, steward.OutcomeBaselineInconclusive:
		os.Exit(3)
	case steward.OutcomeAwaitingApproval:
		os.Exit(5)
	default:
		os.Exit(4)
	}
	return nil
}

func traceObserver() agentrt.Observer {
	return func(e agentrt.Event) {
		fmt.Fprintf(os.Stderr, "%s  %-22s %s\n", e.At.Local().Format("15:04:05.000"), e.Type, string(e.Payload))
	}
}

func runResume(ctx context.Context, args []string) error {
	runID, rest, err := positional(args, "resume: expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory")
	proxyDir := fs.String("fixture-proxy", "", "file-based module proxy directory used when the run started")
	pull := fs.Bool("pull", false, "pull the pinned toolchain image if absent")
	trace := fs.Bool("trace", false, "print runtime events to stderr")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	ro := steward.ResumeOptions{RunID: runID, DataDir: *dataDir, FixtureProxyDir: *proxyDir, AllowPull: *pull}
	if *trace {
		ro.Observer = traceObserver()
	}
	res, err := steward.Resume(ctx, ro)
	return report(res, err)
}

func runDecide(ctx context.Context, args []string, approve bool) error {
	verb := "reject"
	if approve {
		verb = "approve"
	}
	runID, rest, err := positional(args, verb+": expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory")
	approvalID := fs.String("approval", "", "approval id (default: the run's single pending approval)")
	note := fs.String("note", "", "note recorded with the decision")
	by := fs.String("by", "", "who decided (default: the current user)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *dataDir == "" {
		if *dataDir, err = defaultDataDir(); err != nil {
			return err
		}
	}
	if *by == "" {
		*by = os.Getenv("USER")
	}
	rt, err := agentrt.OpenStore(filepath.Join(*dataDir, "steward.db"))
	if err != nil {
		return err
	}
	defer rt.Close()
	if *approvalID == "" {
		approvals, err := rt.ListApprovals(ctx, runID)
		if err != nil {
			return err
		}
		var pending []agentrt.Approval
		for _, a := range approvals {
			if a.Status == agentrt.ApprovalPending {
				pending = append(pending, a)
			}
		}
		if len(pending) != 1 {
			return fmt.Errorf("%s: run %s has %d pending approvals; pass -approval", verb, runID, len(pending))
		}
		*approvalID = pending[0].ID
	}
	if approve {
		err = agentrt.Approve(ctx, rt, nil, runID, *approvalID, *by, *note)
	} else {
		err = agentrt.Reject(ctx, rt, nil, runID, *approvalID, *by, *note)
		if err == nil {
			ts, terr := task.Open(filepath.Join(*dataDir, "steward.db"))
			if terr == nil {
				ts.FinishTask(ctx, runID, steward.OutcomeApprovalRejected, map[string]any{"note": *note})
				ts.Close()
			}
		}
	}
	if err != nil {
		return err
	}
	a, _ := rt.GetApproval(ctx, runID, *approvalID)
	return printJSON(map[string]any{"run_id": runID, "approval": a})
}

func runsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runs list", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		var err error
		if *dataDir, err = defaultDataDir(); err != nil {
			return err
		}
	}
	ts, err := task.Open(filepath.Join(*dataDir, "steward.db"))
	if err != nil {
		return err
	}
	defer ts.Close()
	rt, err := agentrt.OpenStore(filepath.Join(*dataDir, "steward.db"))
	if err != nil {
		return err
	}
	defer rt.Close()
	tasks, err := ts.ListTasks(ctx)
	if err != nil {
		return err
	}
	type row struct {
		RunID      string `json:"run_id"`
		Mode       string `json:"mode"`
		Source     string `json:"source"`
		Outcome    string `json:"outcome"`
		RunStatus  string `json:"run_status,omitempty"`
		Steps      int    `json:"steps,omitempty"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at,omitempty"`
	}
	var rows []row
	for _, t := range tasks {
		r := row{RunID: t.RunID, Mode: t.Mode, Source: t.SourcePath, Outcome: t.Outcome, CreatedAt: t.CreatedAt, FinishedAt: t.FinishedAt}
		if run, err := rt.GetRun(ctx, t.RunID); err == nil {
			r.RunStatus, r.Steps = string(run.Status), run.StepCount
		}
		rows = append(rows, r)
	}
	return printJSON(rows)
}

func runsShow(ctx context.Context, args []string) error {
	runID, rest, err := positional(args, "runs show: expected a run id")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("runs show", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory")
	events := fs.Bool("events", false, "include the event log")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *dataDir == "" {
		if *dataDir, err = defaultDataDir(); err != nil {
			return err
		}
	}
	ts, err := task.Open(filepath.Join(*dataDir, "steward.db"))
	if err != nil {
		return err
	}
	defer ts.Close()
	rt, err := agentrt.OpenStore(filepath.Join(*dataDir, "steward.db"))
	if err != nil {
		return err
	}
	defer rt.Close()
	tk, err := ts.GetTask(ctx, runID)
	if err != nil {
		return err
	}
	out := map[string]any{"task": tk}
	if run, err := rt.GetRun(ctx, runID); err == nil {
		out["run"] = run
		steps, _ := rt.ListSteps(ctx, runID)
		type stepRow struct {
			Index   int    `json:"index"`
			ID      string `json:"id"`
			Tool    string `json:"tool,omitempty"`
			Kind    string `json:"kind"`
			Status  string `json:"status"`
			Policy  string `json:"policy,omitempty"`
			Summary string `json:"summary,omitempty"`
		}
		var srows []stepRow
		for _, st := range steps {
			r := stepRow{Index: st.Index, ID: st.ID, Status: string(st.Status)}
			if st.Decision != nil {
				r.Tool, r.Kind = st.Decision.Tool, string(st.Decision.Kind)
			}
			if st.Policy != nil {
				r.Policy = string(st.Policy.Outcome)
			}
			if st.Observation != nil {
				r.Summary = st.Observation.Summary
			}
			srows = append(srows, r)
		}
		out["steps"] = srows
		out["approvals"], _ = rt.ListApprovals(ctx, runID)
		if *events {
			out["events"], _ = rt.ListEvents(ctx, runID)
		}
	}
	props, _ := ts.ListProposals(ctx, runID)
	out["proposals"] = props
	proms, _ := ts.ListPromotions(ctx, runID)
	out["promotions"] = proms
	return printJSON(out)
}

func defaultDataDir() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "repo-steward"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "repo-steward"), nil
}

// resolveAuthor parses "Name <email>" or reads the operator's identity from
// the source repository's configuration. Reading configuration executes no
// repository code.
func resolveAuthor(flagValue, repoPath string) (gitx.Identity, error) {
	if flagValue != "" {
		i := strings.LastIndex(flagValue, "<")
		if i <= 0 || !strings.HasSuffix(flagValue, ">") {
			return gitx.Identity{}, fmt.Errorf(`-author must look like "Name <email>"`)
		}
		return gitx.Identity{Name: strings.TrimSpace(flagValue[:i]), Email: strings.TrimSuffix(flagValue[i+1:], ">")}, nil
	}
	name, err1 := exec.Command("git", "-C", repoPath, "config", "--get", "user.name").Output()
	email, err2 := exec.Command("git", "-C", repoPath, "config", "--get", "user.email").Output()
	if err1 != nil || err2 != nil || strings.TrimSpace(string(name)) == "" || strings.TrimSpace(string(email)) == "" {
		return gitx.Identity{}, fmt.Errorf("maintain: no git user configured for %s; pass -author \"Name <email>\"", repoPath)
	}
	return gitx.Identity{Name: strings.TrimSpace(string(name)), Email: strings.TrimSpace(string(email))}, nil
}

// positional takes the first argument as a positional value and returns the
// remainder for flag parsing, so flags may follow the positional.
func positional(args []string, msg string) (string, []string, error) {
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return "", nil, fmt.Errorf("%s", msg)
	}
	return args[0], args[1:], nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
