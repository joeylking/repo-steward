package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/replay"

	"github.com/joeylking/repo-steward/internal/agent"
)

type callerOver struct{ m agentrt.Model }

func (c callerOver) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	return c.m.Generate(ctx, req)
}

func step(i int, tool, args string, kind agentrt.ObservationKind, summary, content string) agentrt.Step {
	return agentrt.Step{Index: i, Status: agentrt.StepDone, Decision: &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tool, Args: []byte(args)}, Observation: &agentrt.Observation{Kind: kind, Summary: summary, Content: []byte(content)}}
}

func input(steps []agentrt.Step, m agentrt.Model) agentrt.StepInput {
	return agentrt.StepInput{Run: agentrt.Run{Goal: "upgrade something", StepCount: len(steps) + 1, Limits: agentrt.Limits{MaxSteps: 40}}, Steps: steps, Tools: []agentrt.ToolSpec{{Name: "read_file"}}, Model: callerOver{m}}
}

func TestRender_StepsBecomeToolUseAndResultPairs(t *testing.T) {
	a := &agent.Agent{Facts: agent.Facts{ModulePath: "example.com/app", GoDirective: "1.22", CandidateCount: 1}, RecentResults: 1}
	steps := []agentrt.Step{
		step(0, "list_candidates", "{}", agentrt.ObserveToolResult, "1 candidate", `[{"module":"example.com/lib"}]`),
		step(1, "read_file", `{"path":"main.go"}`, agentrt.ObserveToolResult, "read main.go", `{"content":"package main"}`),
	}
	msgs := a.Render(input(steps, nil))
	if len(msgs) != 5 || msgs[0].Role != "user" || !strings.Contains(msgs[0].Content[0].Text, "example.com/app") {
		t.Fatalf("messages = %+v", msgs)
	}
	if msgs[1].Content[0].Type != "tool_use" || msgs[1].Content[0].Name != "list_candidates" || msgs[2].Content[0].Type != "tool_result" || msgs[2].Content[0].ToolUseID != msgs[1].Content[0].ToolUseID {
		t.Fatalf("pair = %+v %+v", msgs[1], msgs[2])
	}
	// The older result is elided to its summary; the latest is full.
	if !strings.Contains(msgs[2].Content[0].Content, "elided") || strings.Contains(msgs[2].Content[0].Content, "example.com/lib") {
		t.Fatalf("old result not elided: %s", msgs[2].Content[0].Content)
	}
	if !strings.Contains(msgs[4].Content[0].Content, "package main") {
		t.Fatalf("recent result not full: %s", msgs[4].Content[0].Content)
	}
	// The system prompt carries no repository content and marks tool output as data.
	if !strings.Contains(agent.System, "DATA") || strings.Contains(agent.System, "example.com") {
		t.Fatal("system prompt problem")
	}
}

func TestDecide_MapsFirstToolUse(t *testing.T) {
	m := &replay.Scripted{Responses: []agentrt.ModelResponse{{Text: "I will read it.", ToolUses: []agentrt.ToolUse{{ID: "c1", Name: "read_file", Args: []byte(`{"path":"main.go"}`)}, {ID: "c2", Name: "git_diff", Args: []byte(`{}`)}}}}}
	a := &agent.Agent{}
	d, err := a.Decide(context.Background(), input(nil, m))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != agentrt.DecideToolCall || d.Tool != "read_file" || string(d.Args) != `{"path":"main.go"}` || d.Reason != "I will read it." {
		t.Fatalf("decision = %+v", d)
	}
	if m.Requests[0].System != agent.System || len(m.Requests[0].Tools) != 1 || m.Requests[0].MaxOutputTokens == 0 {
		t.Fatalf("request = %+v", m.Requests[0])
	}
}

// A reply with no executable tool call is not retried inside the step: it
// is recorded as an invalid decision, and the next render tells the model
// what was wrong. A refusal fails the run.
func TestDecide_NoToolCallIsRecordedNotRetried(t *testing.T) {
	m := &replay.Scripted{Responses: []agentrt.ModelResponse{{Text: "Let me think."}}}
	d, err := (&agent.Agent{}).Decide(context.Background(), input(nil, m))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != agentrt.KindNoToolCall || d.Reason != "Let me think." || len(m.Requests) != 1 {
		t.Fatalf("d=%+v requests=%d", d, len(m.Requests))
	}
	m2 := &replay.Scripted{Responses: []agentrt.ModelResponse{{StopReason: "max_tokens", ToolUses: []agentrt.ToolUse{{Name: "write_file"}}}}}
	if d, err = (&agent.Agent{}).Decide(context.Background(), input(nil, m2)); err != nil || d.Kind != agentrt.KindTruncated {
		t.Fatalf("truncated: d=%+v err=%v", d, err)
	}
	m3 := &replay.Scripted{Responses: []agentrt.ModelResponse{{StopReason: "refusal", Text: "no"}}}
	if d, err = (&agent.Agent{}).Decide(context.Background(), input(nil, m3)); err != nil || d.Kind != agentrt.DecideFail {
		t.Fatalf("refusal: d=%+v err=%v", d, err)
	}
}

// Such a step is rendered as the model's own text answered by a nudge, not
// as a tool_use with no tool.
func TestRender_InvalidStepIsNudged(t *testing.T) {
	steps := []agentrt.Step{
		{Index: 0, Status: agentrt.StepFailed, Decision: &agentrt.Decision{Kind: agentrt.KindNoToolCall, Reason: "narrating"},
			Observation: &agentrt.Observation{Kind: agentrt.ObserveInvalidDecision, Summary: "invalid decision"}},
		step(1, "read_file", `{"path":"main.go"}`, agentrt.ObserveToolResult, "read main.go", `{"content":"package main"}`),
	}
	msgs := (&agent.Agent{}).Render(input(steps, nil))
	if len(msgs) != 5 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if msgs[1].Content[0].Type != "text" || msgs[1].Content[0].Text != "narrating" {
		t.Fatalf("assistant turn = %+v", msgs[1])
	}
	if msgs[2].Content[0].Type != "text" || !strings.Contains(msgs[2].Content[0].Text, "exactly one tool call") {
		t.Fatalf("nudge = %+v", msgs[2])
	}
	if msgs[3].Content[0].Type != "tool_use" || msgs[4].Content[0].Type != "tool_result" {
		t.Fatalf("tool-call step = %+v %+v", msgs[3], msgs[4])
	}
}

func TestDecide_PropagatesLimitErrors(t *testing.T) {
	m := &replay.Scripted{Errors: []error{agentrt.ErrLimit{Reason: agentrt.ReasonLimitCost, Detail: "cap"}}}
	_, err := (&agent.Agent{}).Decide(context.Background(), input(nil, m))
	var lim agentrt.ErrLimit
	if !errors.As(err, &lim) || lim.Reason != agentrt.ReasonLimitCost {
		t.Fatalf("err = %v", err)
	}
	var d agentrt.Decision
	_ = json.Unmarshal([]byte(`{}`), &d)
}
