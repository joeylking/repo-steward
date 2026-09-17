package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/model/ollama"
)

func server(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) *ollama.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", 404)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		handler(w, body)
	}))
	t.Cleanup(srv.Close)
	m := ollama.New("test-model")
	m.Host = srv.URL
	return m
}

func TestGenerate_MapsRequestAndToolCalls(t *testing.T) {
	var got map[string]any
	m := server(t, func(w http.ResponseWriter, body map[string]any) {
		got = body
		w.Write([]byte(`{"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":{"path":"main.go"}}}]},"done_reason":"stop","prompt_eval_count":142,"eval_count":30}`))
	})
	req := agentrt.ModelRequest{
		System: "sys",
		Messages: []agentrt.Message{
			{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "goal"}}},
			{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: "step_0", Name: "list_candidates", Input: []byte(`{}`)}}},
			{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "step_0", Name: "list_candidates", Content: "[]"}}},
		},
		Tools:           []agentrt.ToolSpec{{Name: "read_file", Description: "read", InputSchema: []byte(`{"type":"object"}`)}},
		MaxOutputTokens: 512,
	}
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read_file" || string(resp.ToolUses[0].Args) != `{"path":"main.go"}` || resp.ToolUses[0].ID != "call_1" {
		t.Fatalf("tool uses = %+v", resp.ToolUses)
	}
	if resp.Usage.InputTokens != 142 || resp.Usage.OutputTokens != 30 || resp.StopReason != "tool_use" || resp.Text != "thinking" {
		t.Fatalf("resp = %+v", resp)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" || msgs[2].(map[string]any)["role"] != "assistant" || msgs[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("messages = %v", msgs)
	}
	if calls := msgs[2].(map[string]any)["tool_calls"].([]any); calls[0].(map[string]any)["function"].(map[string]any)["name"] != "list_candidates" {
		t.Fatalf("assistant tool_calls = %v", calls)
	}
	if got["model"] != "test-model" || got["stream"] != false || got["think"] != false {
		t.Fatalf("request fields = %v", got)
	}
	opts := got["options"].(map[string]any)
	if opts["temperature"].(float64) != 0 || opts["num_predict"].(float64) != 512 {
		t.Fatalf("options = %v", opts)
	}
	tools := got["tools"].([]any)
	if tools[0].(map[string]any)["function"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("tools = %v", tools)
	}
	if m.Name() != "ollama:test-model" {
		t.Fatalf("name = %s", m.Name())
	}
}

func TestGenerate_TextOnlyAndMissingIDs(t *testing.T) {
	m := server(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Write([]byte(`{"message":{"role":"assistant","content":"just text","tool_calls":[{"function":{"name":"git_diff"}}]},"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
	})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID == "" || string(resp.ToolUses[0].Args) != "{}" {
		t.Fatalf("tool uses = %+v", resp.ToolUses)
	}
}

func TestGenerate_ErrorsClassified(t *testing.T) {
	m := server(t, func(w http.ResponseWriter, _ map[string]any) { http.Error(w, "overloaded", 503) })
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	var tr agentrt.TransientError
	if !errors.As(err, &tr) {
		t.Fatalf("5xx not transient: %v", err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any) { http.Error(w, "model not found", 404) })
	_, err = m.Generate(context.Background(), agentrt.ModelRequest{})
	if err == nil || errors.As(err, &tr) {
		t.Fatalf("4xx should be a plain error: %v", err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any) { w.Write([]byte(`{"error":"context length exceeded"}`)) })
	if _, err = m.Generate(context.Background(), agentrt.ModelRequest{}); err == nil {
		t.Fatal("body error ignored")
	}
	// A server that is down is transient (the caller retries), not fatal.
	down := ollama.New("x")
	down.Host = "http://127.0.0.1:1"
	if _, err = down.Generate(context.Background(), agentrt.ModelRequest{}); !errors.As(err, &tr) {
		t.Fatalf("connection refused not transient: %v", err)
	}
}
