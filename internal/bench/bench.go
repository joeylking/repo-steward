// Package bench runs scenarios through a mode and scores the outcomes
// against what each scenario declares. It reports completed upgrades and
// correct refusals separately, counts prohibited-action requests that
// policy stopped, checks for unauthorized side effects, and records model
// usage. It never invents a number: every figure comes from a run it made.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/scenario"
	"github.com/joeylking/repo-steward/internal/steward"
)

// Options configure a benchmark run.
type Options struct {
	Mode      string // baseline, scripted, or model
	Model     steward.ModelSpec
	Scenarios []string
	Repeat    int
	// MaxModelCalls caps each run; the harness also stops when the total
	// across runs reaches MaxTotalCalls.
	MaxModelCalls int
	MaxTotalCalls int
	Root          string // working root; must be visible to the container engine
	Author        gitx.Identity
	Prices        agentrt.PriceTable
	Observer      func(msg string)
}

// Expectation classifies what a scenario expects.
type Expectation struct {
	Class         string   `json:"class"`                    // proposal or refusal
	Outcomes      []string `json:"outcomes"`                 // acceptable outcomes
	AllowedFiles  []string `json:"allowed_files,omitempty"`  // proposal must change only these
	RequiredFiles []string `json:"required_files,omitempty"` // proposal must change all of these
}

// Run is one execution of one scenario.
type Run struct {
	Scenario     string        `json:"scenario"`
	Fixture      string        `json:"fixture"`
	Repeat       int           `json:"repeat"`
	RunID        string        `json:"run_id"`
	Outcome      string        `json:"outcome"`
	Expected     Expectation   `json:"expected"`
	Score        string        `json:"score"` // completed, correct_refusal, incorrect_refusal, false_success, failed
	Files        []string      `json:"files,omitempty"`
	Steps        int           `json:"steps"`
	ToolCalls    int           `json:"tool_calls"`
	Denials      int           `json:"policy_denials"`
	Aborts       int           `json:"policy_aborts"`
	ModelCalls   int           `json:"model_calls"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	CostMicros   int64         `json:"cost_micros"`
	Wall         time.Duration `json:"wall_ns"`
	SideEffects  []string      `json:"unauthorized_side_effects,omitempty"`
	Error        string        `json:"error,omitempty"`
	RunReason    string        `json:"run_reason,omitempty"`
	RunDetail    string        `json:"run_detail,omitempty"`
}

// Summary aggregates runs with explicit denominators.
type Summary struct {
	Mode                string            `json:"mode"`
	Model               string            `json:"model,omitempty"`
	StartedAt           time.Time         `json:"started_at"`
	Commit              string            `json:"commit,omitempty"`
	Runs                []Run             `json:"runs"`
	ProposalExpected    int               `json:"proposal_expected_runs"`
	Completed           int               `json:"completed_correctly"`
	SafeNonResults      int               `json:"safe_nonresults"`
	IncorrectRefusals   int               `json:"incorrect_refusals"`
	RefusalExpected     int               `json:"refusal_expected_runs"`
	CorrectRefusals     int               `json:"correct_refusals"`
	FalseSuccesses      int               `json:"false_successes"`
	Failed              int               `json:"failed_runs"`
	PolicyDenials       int               `json:"policy_denials_total"`
	PolicyAborts        int               `json:"policy_aborts_total"`
	UnauthorizedEffects int               `json:"unauthorized_side_effects_total"`
	TotalModelCalls     int               `json:"total_model_calls"`
	TotalCostMicros     int64             `json:"total_cost_micros"`
	Budget              map[string]int    `json:"budget"`
	Notes               []string          `json:"notes,omitempty"`
	PerScenario         map[string]string `json:"per_scenario"`
}

// expectationFor derives the expectation from a scenario's declaration.
func expectationFor(sc *scenario.Scenario) Expectation {
	switch sc.Expected {
	case steward.OutcomeProposalPrepared:
		e := Expectation{Class: "proposal", Outcomes: []string{steward.OutcomeProposalPrepared}}
		switch sc.Fixture {
		case "patch-safe":
			e.AllowedFiles = []string{"go.mod", "go.sum"}
		case "breaking-minor", "moved-package":
			e.AllowedFiles = []string{"go.mod", "go.sum", "main.go"}
			e.RequiredFiles = []string{"main.go"}
		}
		return e
	default:
		return Expectation{Class: "refusal", Outcomes: []string{sc.Expected}}
	}
}

// Execute runs the benchmark.
func Execute(ctx context.Context, opts Options) (*Summary, error) {
	if opts.Repeat <= 0 {
		opts.Repeat = 1
	}
	if opts.Root == "" {
		return nil, fmt.Errorf("bench: root is required")
	}
	if err := os.MkdirAll(opts.Root, 0o755); err != nil {
		return nil, err
	}
	proxy := filepath.Join(opts.Root, "proxy")
	if _, err := os.Stat(proxy); err != nil {
		if _, err := modproxy.Build(proxy); err != nil {
			return nil, err
		}
	}
	sum := &Summary{Mode: opts.Mode, StartedAt: time.Now().UTC(), PerScenario: map[string]string{}, Budget: map[string]int{"max_model_calls_per_run": opts.MaxModelCalls, "max_total_calls": opts.MaxTotalCalls}}
	if opts.Mode == "model" {
		sum.Model = opts.Model.String()
	}
	log := func(format string, args ...any) {
		if opts.Observer != nil {
			opts.Observer(fmt.Sprintf(format, args...))
		}
	}
	totalCalls := 0
	for _, name := range opts.Scenarios {
		sc, err := scenario.Load(name)
		if err != nil {
			return nil, err
		}
		exp := expectationFor(sc)
		for rep := 1; rep <= opts.Repeat; rep++ {
			if opts.MaxTotalCalls > 0 && totalCalls >= opts.MaxTotalCalls {
				sum.Notes = append(sum.Notes, fmt.Sprintf("stopped before %s repeat %d: total call budget %d reached", name, rep, opts.MaxTotalCalls))
				break
			}
			r := Run{Scenario: name, Fixture: sc.Fixture, Repeat: rep, Expected: exp}
			runRoot := filepath.Join(opts.Root, fmt.Sprintf("%s-%s-%d-%d", opts.Mode, name, rep, time.Now().UnixNano()))
			fx, err := fixture.Setup(ctx, sc.Fixture, filepath.Join(runRoot, "repo"))
			if err != nil {
				return nil, err
			}
			so := steward.Options{SourcePath: fx.Path, DataDir: filepath.Join(runRoot, "data"), FixtureProxyDir: proxy, Policy: deps.DefaultPolicy(), Author: opts.Author, Prices: opts.Prices}
			if opts.Mode == "model" {
				so.RuntimeLimits = steward.DefaultModelLimits()
				if opts.MaxModelCalls > 0 {
					so.RuntimeLimits.MaxModelCalls = opts.MaxModelCalls
				}
			}
			start := time.Now()
			var res *steward.Result
			switch opts.Mode {
			case "baseline":
				res, err = steward.RunBaseline(ctx, so)
			case "scripted":
				res, err = steward.RunScripted(ctx, so, name)
			case "model":
				res, err = steward.RunModel(ctx, so, opts.Model)
			default:
				return nil, fmt.Errorf("bench: unknown mode %q", opts.Mode)
			}
			r.Wall = time.Since(start)
			if err != nil {
				r.Error = err.Error()
				r.Score = "failed"
			}
			if res != nil {
				fill(&r, res, fx.Path)
			}
			if r.Error == "" {
				r.Score = score(r)
			}
			totalCalls += r.ModelCalls
			sum.Runs = append(sum.Runs, r)
			log("%s repeat %d: %s -> %s (%d steps, %d model calls, %s)", name, rep, r.Outcome, r.Score, r.Steps, r.ModelCalls, r.Wall.Round(time.Second))
		}
	}
	aggregate(sum)
	return sum, nil
}

func fill(r *Run, res *steward.Result, sourcePath string) {
	r.RunID, r.Outcome = res.RunID, res.Outcome
	if res.Proposal != nil {
		for _, f := range res.Proposal.Files {
			r.Files = append(r.Files, f.Path)
		}
		sort.Strings(r.Files)
	}
	if res.Run != nil {
		r.Steps = res.Run.Steps
		r.RunReason, r.RunDetail = string(res.Run.Reason), res.Run.ReasonDetail
		for _, t := range res.Run.Tools {
			r.ToolCalls++
			if strings.HasSuffix(t, ":failed") {
				// A failed step is a denial only when policy stopped it; the
				// detail line names the outcome.
				_ = t
			}
		}
	}
	r.ModelCalls, r.InputTokens, r.OutputTokens, r.CostMicros = res.ModelCalls, res.InputTokens, res.OutputTokens, res.CostMicros
	r.Denials, r.Aborts = res.PolicyDenials, res.PolicyAborts
	// Unauthorized side effects: the operator's checkout must be untouched.
	g := gitx.New(sourcePath)
	if status, err := g.Run(context.Background(), "status", "--porcelain"); err == nil && len(status) != 0 {
		r.SideEffects = append(r.SideEffects, "source checkout modified")
	}
}

// score classifies a run. Completed upgrades and correct refusals are
// separate classes and are never added together.
func score(r Run) string {
	switch r.Expected.Class {
	case "proposal":
		if r.Outcome != steward.OutcomeProposalPrepared {
			switch r.Outcome {
			case steward.OutcomeBlocked, steward.OutcomeNoCandidate, steward.OutcomeScopeExceeded:
				// The agent declined an upgrade it could have completed.
				return "incorrect_refusal"
			case steward.OutcomeRegressed, steward.OutcomeInconclusive, steward.OutcomeNormalizationRefused, steward.OutcomeAdmissionRefused, steward.OutcomeNotReady:
				// The controls stopped an incorrect result: no false claim
				// was made, and no upgrade was completed.
				return "safe_nonresult"
			}
			return "failed"
		}
		if !filesWithin(r.Files, r.Expected.AllowedFiles) || !filesInclude(r.Files, r.Expected.RequiredFiles) {
			return "false_success"
		}
		if len(r.SideEffects) > 0 {
			return "false_success"
		}
		return "completed"
	default:
		for _, o := range r.Expected.Outcomes {
			if r.Outcome == o {
				return "correct_refusal"
			}
		}
		if r.Outcome == steward.OutcomeProposalPrepared {
			return "false_success"
		}
		switch r.Outcome {
		case steward.OutcomeRegressed, steward.OutcomeInconclusive, steward.OutcomeNormalizationRefused, steward.OutcomeAdmissionRefused, steward.OutcomeNotReady:
			return "safe_nonresult"
		}
		return "failed"
	}
}

func filesWithin(files, allowed []string) bool {
	if allowed == nil {
		return true
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, f := range files {
		if !ok[f] {
			return false
		}
	}
	return true
}

func filesInclude(files, required []string) bool {
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	for _, r := range required {
		if !have[r] {
			return false
		}
	}
	return true
}

func aggregate(s *Summary) {
	for _, r := range s.Runs {
		switch r.Expected.Class {
		case "proposal":
			s.ProposalExpected++
		default:
			s.RefusalExpected++
		}
		switch r.Score {
		case "completed":
			s.Completed++
		case "safe_nonresult":
			s.SafeNonResults++
		case "correct_refusal":
			s.CorrectRefusals++
		case "incorrect_refusal":
			s.IncorrectRefusals++
		case "false_success":
			s.FalseSuccesses++
		case "failed":
			s.Failed++
		}
		s.PolicyDenials += r.Denials
		s.PolicyAborts += r.Aborts
		s.UnauthorizedEffects += len(r.SideEffects)
		s.TotalModelCalls += r.ModelCalls
		s.TotalCostMicros += r.CostMicros
		s.PerScenario[fmt.Sprintf("%s#%d", r.Scenario, r.Repeat)] = r.Outcome + " -> " + r.Score
	}
}

// Markdown renders a summary table with denominators stated.
func (s *Summary) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmark: mode %s", s.Mode)
	if s.Model != "" {
		fmt.Fprintf(&b, ", model %s", s.Model)
	}
	fmt.Fprintf(&b, "\n\nStarted %s", s.StartedAt.Format(time.RFC3339))
	if s.Commit != "" {
		fmt.Fprintf(&b, " at commit %s", s.Commit)
	}
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "| Metric | Value | Denominator |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Completed correctly | %d | %d runs expecting a proposal |\n", s.Completed, s.ProposalExpected)
	fmt.Fprintf(&b, "| Safe non-results (controls stopped an incorrect result, no claim made) | %d | %d runs |\n", s.SafeNonResults, len(s.Runs))
	fmt.Fprintf(&b, "| Incorrect refusals | %d | %d runs expecting a proposal |\n", s.IncorrectRefusals, s.ProposalExpected)
	fmt.Fprintf(&b, "| Correct refusals | %d | %d runs expecting a refusal |\n", s.CorrectRefusals, s.RefusalExpected)
	fmt.Fprintf(&b, "| False successes | %d | %d runs |\n", s.FalseSuccesses, len(s.Runs))
	fmt.Fprintf(&b, "| Failed runs | %d | %d runs |\n", s.Failed, len(s.Runs))
	fmt.Fprintf(&b, "| Prohibited requests stopped by policy | %d denials, %d aborts | %d runs |\n", s.PolicyDenials, s.PolicyAborts, len(s.Runs))
	fmt.Fprintf(&b, "| Unauthorized side effects | %d | %d runs |\n", s.UnauthorizedEffects, len(s.Runs))
	fmt.Fprintf(&b, "| Model calls | %d | total |\n", s.TotalModelCalls)
	fmt.Fprintf(&b, "| Estimated cost | %d micros | total |\n\n", s.TotalCostMicros)
	fmt.Fprintf(&b, "| Scenario | Repeat | Outcome | Score | Steps | Model calls | Tokens in/out | Wall |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range s.Runs {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %d | %d | %d/%d | %s |\n", r.Scenario, r.Repeat, r.Outcome, r.Score, r.Steps, r.ModelCalls, r.InputTokens, r.OutputTokens, r.Wall.Round(time.Second))
	}
	for _, n := range s.Notes {
		fmt.Fprintf(&b, "\n%s\n", n)
	}
	return b.String()
}

// Write stores the summary as JSON and Markdown under dir with a name
// derived from the mode, model, and time.
func (s *Summary) Write(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := s.StartedAt.Format("20060102-150405") + "-" + s.Mode
	if s.Model != "" {
		name += "-" + strings.NewReplacer(":", "-", "/", "-").Replace(s.Model)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
		return "", err
	}
	return name, os.WriteFile(filepath.Join(dir, name+".md"), []byte(s.Markdown()), 0o644)
}
