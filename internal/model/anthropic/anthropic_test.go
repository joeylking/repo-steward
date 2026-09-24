package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
)

// The request the adapter builds must be one the API accepts: no empty
// text blocks, no empty messages, tool schemas passed through. Usage maps
// cache reads to the cached column and charges cache writes as input at
// their higher rate.
func TestGenerate_RequestShapeAndUsage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("headers = %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"a"}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":40,"cache_read_input_tokens":200}}`)
	}))
	defer srv.Close()
	m := &Model{Name_: "claude-x", APIKey: "k", Base: srv.URL, Client: srv.Client()}
	req := agentrt.ModelRequest{System: "s", MaxOutputTokens: 50, Tools: []agentrt.ToolSpec{{Name: "read_file", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Messages: []agentrt.Message{
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "text", Text: ""}}},
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "reply with a tool call"}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: "t0", Name: "read_file", Input: nil}}},
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "t0", Content: "x", IsError: true}}},
	}}
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages = %d", len(msgs))
	}
	for i, mm := range msgs {
		content := mm.(map[string]any)["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("message %d has no blocks", i)
		}
		for _, c := range content {
			b := c.(map[string]any)
			if b["type"] == "text" && b["text"] == "" {
				t.Fatalf("message %d carries an empty text block", i)
			}
			if b["type"] == "tool_use" && b["input"] == nil {
				t.Fatalf("message %d tool_use without input", i)
			}
		}
	}
	if got["max_tokens"].(float64) != 50 || got["system"] != "s" || len(got["tools"].([]any)) != 1 {
		t.Fatalf("request = %v", got)
	}
	if resp.Text != "ok" || len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read_file" || resp.StopReason != "tool_use" {
		t.Fatalf("resp = %+v", resp)
	}
	// 100 base + 200 cached + 40 cache-write at 1.25x = 350 input; 200 cached.
	if resp.Usage.InputTokens != 350 || resp.Usage.CachedInputTokens != 200 || resp.Usage.OutputTokens != 10 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if Prices()[(&Model{Name_: "claude-sonnet-5"}).Name()].InputPerMTok == 0 {
		t.Fatal("sonnet price missing")
	}
}

func TestNew_RefusesWithoutKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_KEY", "")
	if _, err := New("x"); err != ErrNoKey {
		t.Fatalf("err = %v", err)
	}
}
