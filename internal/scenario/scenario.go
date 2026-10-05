// Package scenario provides a scripted agent that replays a fixed sequence
// of tool calls from an embedded scenario file. It demonstrates
// orchestration and control behaviour deterministically; it says nothing
// about a real model's repair quality, which is measured separately.
package scenario

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	agentrt "github.com/joeylking/agent-runtime"
)

//go:embed testdata/scenarios
var embedded embed.FS

// Step is one scripted decision.
type Step struct {
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Reason string          `json:"reason,omitempty"`
}

// Scenario is a fixture plus the decisions an agent would make on it and
// the expectations a benchmark scores it against.
type Scenario struct {
	Name        string `json:"name"`
	Fixture     string `json:"fixture"`
	Description string `json:"description"`
	// Expected is the outcome the scenario should reach; Acceptable lists
	// other outcomes that also count as correct (for example a justified
	// blocked outcome where a proposal was possible).
	Expected   string   `json:"expected"`
	Acceptable []string `json:"acceptable,omitempty"`
	// AllowedFiles and RequiredFiles constrain a proposal's changed files.
	AllowedFiles  []string `json:"allowed_files,omitempty"`
	RequiredFiles []string `json:"required_files,omitempty"`
	// Oracles are hidden checks run on the proposal tree; the agent never
	// sees them.
	Oracles []Oracle `json:"oracles,omitempty"`
	// ForbiddenInProposal lists strings that must not appear in the
	// proposal's title or body, for injection scenarios.
	ForbiddenInProposal []string `json:"forbidden_in_proposal,omitempty"`
	// Scope and Policy override the run configuration when set.
	Scope  *ScopeOverride  `json:"scope,omitempty"`
	Policy *PolicyOverride `json:"policy,omitempty"`
	// MaxModelCalls, when set, asserts an upper bound (zero means none).
	MaxModelCalls *int   `json:"max_model_calls,omitempty"`
	Steps         []Step `json:"steps"`
}

// Oracle is a hidden check on one file of the proposal tree.
type Oracle struct {
	File           string   `json:"file"`
	MustContain    []string `json:"must_contain,omitempty"`
	MustNotContain []string `json:"must_not_contain,omitempty"`
	// MustMatch and MustNotMatch are regular expressions (RE2 syntax) that
	// must each match, or must not match, the file.
	MustMatch    []string `json:"must_match,omitempty"`
	MustNotMatch []string `json:"must_not_match,omitempty"`
	// NoUncheckedAssertion lists types, written as gofmt prints them, that
	// the Go file must never assert to in the single-value form, which
	// panics when the value has another type. The comma-ok form and type
	// switches pass.
	NoUncheckedAssertion []string `json:"no_unchecked_assertion,omitempty"`
}

// Check applies the oracle to the file's content and returns one line per
// failure; an invalid expression is a failure too.
func (o Oracle) Check(content []byte) []string {
	var out []string
	for _, m := range o.MustContain {
		if !strings.Contains(string(content), m) {
			out = append(out, fmt.Sprintf("missing %q", m))
		}
	}
	for _, m := range o.MustNotContain {
		if strings.Contains(string(content), m) {
			out = append(out, fmt.Sprintf("contains %q", m))
		}
	}
	for _, expr := range o.MustMatch {
		re, err := regexp.Compile(expr)
		if err != nil {
			out = append(out, fmt.Sprintf("invalid expression %q: %v", expr, err))
		} else if !re.Match(content) {
			out = append(out, fmt.Sprintf("does not match %q", expr))
		}
	}
	for _, expr := range o.MustNotMatch {
		re, err := regexp.Compile(expr)
		if err != nil {
			out = append(out, fmt.Sprintf("invalid expression %q: %v", expr, err))
		} else if loc := re.Find(content); loc != nil {
			out = append(out, fmt.Sprintf("matches %q at %q", expr, loc))
		}
	}
	if len(o.NoUncheckedAssertion) > 0 {
		out = append(out, uncheckedAssertions(content, o.NoUncheckedAssertion)...)
	}
	return out
}

// uncheckedAssertions finds single-value type assertions to any of types.
func uncheckedAssertions(src []byte, types []string) []string {
	norm := func(s string) string { return strings.Join(strings.Fields(s), "") }
	want := map[string]bool{}
	for _, t := range types {
		want[norm(t)] = true
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"does not parse as Go: " + err.Error()}
	}
	var out []string
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if ta, ok := n.(*ast.TypeAssertExpr); ok && ta.Type != nil && want[norm(gotypes.ExprString(ta.Type))] && !commaOK(stack, ta) {
			out = append(out, fmt.Sprintf("unchecked type assertion to %s at line %d", gotypes.ExprString(ta.Type), fset.Position(ta.Pos()).Line))
		}
		stack = append(stack, n)
		return true
	})
	return out
}

// commaOK reports whether the assertion, under any parentheses, is the one
// value assigned to two names.
func commaOK(stack []ast.Node, ta *ast.TypeAssertExpr) bool {
	var child ast.Expr = ta
	for i := len(stack) - 1; i >= 0; i-- {
		switch p := stack[i].(type) {
		case *ast.ParenExpr:
			child = p
			continue
		case *ast.AssignStmt:
			return len(p.Lhs) == 2 && len(p.Rhs) == 1 && p.Rhs[0] == child
		case *ast.ValueSpec:
			return len(p.Names) == 2 && len(p.Values) == 1 && p.Values[0] == child
		}
		return false
	}
	return false
}

// ScopeOverride sets scope limits for the run.
type ScopeOverride struct {
	FilesSoft int `json:"files_soft"`
	FilesHard int `json:"files_hard"`
	LinesSoft int `json:"lines_soft,omitempty"`
	LinesHard int `json:"lines_hard,omitempty"`
}

// PolicyOverride sets dependency policy for the run.
type PolicyOverride struct {
	AllowMajor      bool   `json:"allow_major,omitempty"`
	NamedDependency string `json:"named_dependency,omitempty"`
}

// Names lists the embedded scenarios.
func Names() ([]string, error) {
	entries, err := fs.ReadDir(embedded, "testdata/scenarios")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// Load reads one scenario.
func Load(name string) (*Scenario, error) {
	raw, err := fs.ReadFile(embedded, "testdata/scenarios/"+name+".json")
	if err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	var s Scenario
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	if s.Name != name {
		return nil, fmt.Errorf("scenario %q declares name %q", name, s.Name)
	}
	return &s, nil
}

// Agent replays a scenario by step index.
type Agent struct {
	Scenario *Scenario
}

// Decide implements agentrt.Agent.
func (a *Agent) Decide(_ context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	i := len(in.Steps)
	if i >= len(a.Scenario.Steps) {
		return agentrt.Decision{}, fmt.Errorf("scenario %s: no decision for step %d", a.Scenario.Name, i)
	}
	st := a.Scenario.Steps[i]
	args := st.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: st.Tool, Args: args, Reason: st.Reason}, nil
}
