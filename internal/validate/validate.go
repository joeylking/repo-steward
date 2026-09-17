// Package validate runs build, vet, and test in the execute profile and
// turns their output into structured, fail-closed results. A check is
// conclusive only when it ran to completion, its output was captured whole,
// its output parsed completely, and its exit code agrees with its findings.
package validate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox"
)

// Status is the process outcome of an attempt.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Error   Status = "error"   // the check could not run
	Timeout Status = "timeout" // killed at the deadline
)

// ParseStatus reports how completely the output was understood.
type ParseStatus string

const (
	ParseComplete ParseStatus = "complete"
	ParsePartial  ParseStatus = "partial"
	ParseFailed   ParseStatus = "failed"
)

// Finding is one diagnostic with a stable key for comparison.
type Finding struct {
	Check   string `json:"check"`
	Key     string `json:"key"`
	Package string `json:"package,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// Attempt is one execution of a check. Every attempt is kept.
type Attempt struct {
	Index          int           `json:"index"`
	Status         Status        `json:"status"`
	ExitCode       int           `json:"exit_code"`
	OutputComplete bool          `json:"output_complete"`
	ParseStatus    ParseStatus   `json:"parse_status"`
	Findings       []Finding     `json:"findings"`
	Duration       time.Duration `json:"duration_ns"`
	Stdout         string        `json:"stdout,omitempty"`
	Stderr         string        `json:"stderr,omitempty"`
	Note           string        `json:"note,omitempty"`
}

// CheckResult is a check with its attempts and derived conclusiveness.
type CheckResult struct {
	Name       string    `json:"name"`
	Attempts   []Attempt `json:"attempts"`
	Conclusive bool      `json:"conclusive"`
	Status     Status    `json:"status"`
	Findings   []Finding `json:"findings"`
	// Reason explains why the check is inconclusive.
	Reason string `json:"reason,omitempty"`
}

// Run is a complete validation of one snapshot.
type Run struct {
	Kind            string                 `json:"kind"`
	TreeHash        string                 `json:"tree_hash"`
	ToolchainDigest string                 `json:"toolchain_digest"`
	Packages        []string               `json:"packages"`
	Checks          map[string]CheckResult `json:"checks"`
	StartedAt       time.Time              `json:"started_at"`
	Duration        time.Duration          `json:"duration_ns"`
	// Conclusive is true only when every required check is conclusive.
	Conclusive bool `json:"conclusive"`
	// Clean is true when conclusive and every check passed with no findings.
	Clean bool `json:"clean"`
}

// Required are the checks a run must contain.
var Required = []string{"build", "vet", "test"}

// Options control a run.
type Options struct {
	Kind            string
	TreeHash        string
	ToolchainDigest string
	CheckTimeout    time.Duration
	RunID, StepID   string
}

// Baseline validates the snapshot mounted in the sandbox.
func Baseline(ctx context.Context, sb sandbox.Sandbox, opts Options) (*Run, error) {
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = 10 * time.Minute
	}
	if opts.Kind == "" {
		opts.Kind = "baseline"
	}
	run := &Run{Kind: opts.Kind, TreeHash: opts.TreeHash, ToolchainDigest: opts.ToolchainDigest, Checks: map[string]CheckResult{}, StartedAt: time.Now()}
	exec := func(argv ...string) (sandbox.ExecResult, error) {
		return sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: argv, Timeout: opts.CheckTimeout, RunID: opts.RunID, StepID: opts.StepID})
	}

	// Package enumeration is the precondition for test completeness.
	listRes, err := exec("go", "list", "./...")
	if err != nil {
		return nil, err
	}
	if listRes.TimedOut || listRes.ExitCode != 0 || listRes.StdoutTruncated {
		note := fmt.Sprintf("go list ./... exit %d: %s", listRes.ExitCode, strings.TrimSpace(string(listRes.Stderr)))
		for _, name := range Required {
			run.Checks[name] = CheckResult{Name: name, Status: Error, Reason: "package listing failed: " + note, Attempts: []Attempt{{Status: Error, ExitCode: listRes.ExitCode, ParseStatus: ParseFailed, Note: note, Stderr: string(listRes.Stderr)}}}
		}
		run.Duration = time.Since(run.StartedAt)
		return run, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(listRes.Stdout)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			run.Packages = append(run.Packages, line)
		}
	}
	sort.Strings(run.Packages)

	specs := []struct {
		name  string
		argv  []string
		parse func(res sandbox.ExecResult, packages []string) Attempt
	}{
		// Binaries go to tmpfs; the source mount is read-only.
		{"build", []string{"go", "build", "-o", "/tmp/gobuild/", "./..."}, parseDiagnostics("build")},
		{"vet", []string{"go", "vet", "./..."}, parseDiagnostics("vet")},
		{"test", []string{"go", "test", "-json", "-count=1", "./..."}, parseTestJSON},
	}
	for _, s := range specs {
		res, err := exec(s.argv...)
		if err != nil {
			return nil, err
		}
		att := s.parse(res, run.Packages)
		att.Index = 0
		att.Duration = res.Duration
		run.Checks[s.name] = summarize(s.name, []Attempt{att})
	}
	run.Duration = time.Since(run.StartedAt)
	run.Conclusive = true
	run.Clean = true
	for _, name := range Required {
		c := run.Checks[name]
		if !c.Conclusive {
			run.Conclusive = false
			run.Clean = false
		} else if c.Status != Pass || len(c.Findings) > 0 {
			run.Clean = false
		}
	}
	return run, nil
}

// summarize derives conclusiveness from the deciding (last) attempt.
func summarize(name string, attempts []Attempt) CheckResult {
	last := attempts[len(attempts)-1]
	c := CheckResult{Name: name, Attempts: attempts, Status: last.Status, Findings: last.Findings}
	switch {
	case last.Status == Timeout:
		c.Reason = "check timed out"
	case last.Status == Error:
		c.Reason = "check could not run: " + last.Note
	case !last.OutputComplete:
		c.Reason = "output exceeded the capture limit"
	case last.ParseStatus != ParseComplete:
		c.Reason = "output did not parse completely: " + last.Note
	case last.Status == Pass && len(last.Findings) > 0:
		c.Reason = "exit code 0 but diagnostics were parsed"
	case last.Status == Fail && len(last.Findings) == 0:
		c.Reason = "nonzero exit with no parsed findings"
	default:
		c.Conclusive = true
	}
	return c
}

var (
	diagLine    = regexp.MustCompile(`^(\S+?\.(?:go|s|c|h)):(\d+)(?::(\d+))?: (.+)$`)
	packageLine = regexp.MustCompile(`^# (\S+)`)
	goErrLine   = regexp.MustCompile(`^go: (.+)$`)
)

// parseDiagnostics handles the text output of go build and go vet.
func parseDiagnostics(check string) func(res sandbox.ExecResult, packages []string) Attempt {
	return func(res sandbox.ExecResult, _ []string) Attempt {
		att := Attempt{Status: statusOf(res), ExitCode: res.ExitCode, OutputComplete: !res.StdoutTruncated && !res.StderrTruncated, Stdout: string(res.Stdout), Stderr: string(res.Stderr)}
		att.ParseStatus = ParseComplete
		pkg := ""
		unparsed := 0
		for _, raw := range strings.Split(string(res.Stderr)+"\n"+string(res.Stdout), "\n") {
			line := strings.TrimRight(raw, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			if m := packageLine.FindStringSubmatch(line); m != nil {
				pkg = strings.TrimSuffix(strings.TrimPrefix(m[1], "["), "]")
				continue
			}
			// go vet prefixes type-check errors with "vet: ".
			if m := diagLine.FindStringSubmatch(strings.TrimPrefix(strings.TrimSpace(line), "vet: ")); m != nil {
				lineNo := 0
				fmt.Sscanf(m[2], "%d", &lineNo)
				msg := m[4]
				att.Findings = append(att.Findings, Finding{Check: check, Key: check + ":" + m[1] + ":" + normalize(msg), Package: pkg, File: m[1], Line: lineNo, Message: msg})
				continue
			}
			if m := goErrLine.FindStringSubmatch(line); m != nil {
				key := "manifest:" + normalize(m[1])
				att.Findings = append(att.Findings, Finding{Check: check, Key: check + ":" + key, Message: m[1]})
				continue
			}
			if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "note: ") || strings.HasPrefix(line, "ok ") {
				continue // continuation and informational lines
			}
			unparsed++
		}
		if unparsed > 0 && att.Status == Fail && len(att.Findings) == 0 {
			att.ParseStatus = ParseFailed
			att.Note = fmt.Sprintf("%d unrecognised output lines", unparsed)
		} else if unparsed > 0 {
			att.Note = fmt.Sprintf("%d unrecognised output lines", unparsed)
		}
		return att
	}
}

type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// parseTestJSON handles go test -json and requires a terminal action for
// every enumerated package.
func parseTestJSON(res sandbox.ExecResult, packages []string) Attempt {
	att := Attempt{Status: statusOf(res), ExitCode: res.ExitCode, OutputComplete: !res.StdoutTruncated && !res.StderrTruncated, Stdout: string(res.Stdout), Stderr: string(res.Stderr)}
	terminal := map[string]string{}
	failedTests := map[string][]string{}
	output := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	badLines := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev testEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			badLines++
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			if ev.Test == "" {
				terminal[ev.Package] = ev.Action
			} else if ev.Action == "fail" {
				failedTests[ev.Package] = append(failedTests[ev.Package], ev.Test)
			}
		case "output":
			if ev.Test == "" && len(output[ev.Package]) < 50 {
				output[ev.Package] = append(output[ev.Package], strings.TrimRight(ev.Output, "\n"))
			}
		}
	}
	att.ParseStatus = ParseComplete
	var missing []string
	for _, p := range packages {
		if _, ok := terminal[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		att.ParseStatus = ParsePartial
		att.Note = fmt.Sprintf("no terminal result for %d package(s): %s", len(missing), strings.Join(missing, ", "))
	}
	if badLines > 0 {
		att.ParseStatus = ParseFailed
		att.Note = strings.TrimSpace(att.Note + fmt.Sprintf(" %d non-JSON lines", badLines))
	}
	pkgs := make([]string, 0, len(terminal))
	for p := range terminal {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	failingPackages := 0
	for _, p := range pkgs {
		if terminal[p] != "fail" {
			continue
		}
		failingPackages++
		tests := failedTests[p]
		sort.Strings(tests)
		if len(tests) == 0 {
			msg := strings.Join(output[p], " | ")
			if len(msg) > 500 {
				msg = msg[:500]
			}
			att.Findings = append(att.Findings, Finding{Check: "test", Key: "test:" + p + ":package", Package: p, Message: "package failed without test-level failures: " + msg})
			continue
		}
		for _, t := range tests {
			att.Findings = append(att.Findings, Finding{Check: "test", Key: "test:" + p + ":" + t, Package: p, Message: "test failed: " + t})
		}
	}
	// Exit code agreement.
	if att.Status == Pass && failingPackages > 0 {
		att.ParseStatus = ParseFailed
		att.Note = strings.TrimSpace(att.Note + " exit 0 with failing packages")
	}
	if att.Status == Fail && failingPackages == 0 && att.ParseStatus == ParseComplete {
		// go test exits nonzero for build failures reported only on stderr.
		att.ParseStatus = ParseFailed
		att.Note = "nonzero exit with no failing package in the JSON stream: " + firstLine(res.Stderr)
	}
	return att
}

func statusOf(res sandbox.ExecResult) Status {
	switch {
	case res.TimedOut:
		return Timeout
	case res.ExitCode == 0:
		return Pass
	default:
		return Fail
	}
}

func normalize(msg string) string {
	return strings.Join(strings.Fields(msg), " ")
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
