// Package ollama adapts a local Ollama server to agentrt.Model through its
// native chat API with tool calling. It is the free, local provider the
// agent is developed against; nothing here reaches beyond the configured
// host.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// Model is one Ollama model on one server.
type Model struct {
	Host  string // e.g. http://127.0.0.1:11434
	Name_ string
	// NumCtx is the context window requested from the server.
	NumCtx int
	// Think disables the model's thinking mode when false; tool-calling
	// models are steadier without it.
	Think  bool
	Client *http.Client
}

// New returns a model for name on the host named by OLLAMA_HOST or the
// default local address.
func New(name string) *Model {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "127.0.0.1:11434"
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	return &Model{Host: strings.TrimRight(host, "/"), Name_: name, NumCtx: 32768, Client: &http.Client{}}
}

// Name implements agentrt.Model.
func (m *Model) Name() string { return "ollama:" + m.Name_ }

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string         `json:"model"`
	Messages []message      `json:"messages"`
	Tools    []tool         `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Think    bool           `json:"think"`
	Options  map[string]any `json:"options,omitempty"`
}

type chatResponse struct {
	Message         message `json:"message"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	Error           string  `json:"error"`
}

// Generate implements agentrt.Model.
func (m *Model) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	cr := chatRequest{Model: m.Name_, Stream: false, Think: m.Think, Options: map[string]any{"temperature": 0, "num_ctx": m.NumCtx}}
	if req.MaxOutputTokens > 0 {
		cr.Options["num_predict"] = req.MaxOutputTokens
	}
	if req.System != "" {
		cr.Messages = append(cr.Messages, message{Role: "system", Content: req.System})
	}
	for _, msg := range req.Messages {
		cr.Messages = append(cr.Messages, convert(msg)...)
	}
	for _, ts := range req.Tools {
		var t tool
		t.Type = "function"
		t.Function.Name, t.Function.Description, t.Function.Parameters = ts.Name, ts.Description, ts.InputSchema
		cr.Tools = append(cr.Tools, t)
	}
	body, err := json.Marshal(cr)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := m.Client.Do(hreq)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return agentrt.ModelResponse{}, err // ambiguous: the caller classifies timeouts
		}
		return agentrt.ModelResponse{}, agentrt.TransientError{Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	if resp.StatusCode >= 500 {
		return agentrt.ModelResponse{}, agentrt.TransientError{Err: fmt.Errorf("ollama: %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	if resp.StatusCode != http.StatusOK {
		return agentrt.ModelResponse{}, fmt.Errorf("ollama: %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return agentrt.ModelResponse{}, fmt.Errorf("ollama: decode: %w", err)
	}
	if out.Error != "" {
		return agentrt.ModelResponse{}, fmt.Errorf("ollama: %s", out.Error)
	}
	mr := agentrt.ModelResponse{Text: out.Message.Content, StopReason: out.DoneReason, Usage: agentrt.Usage{InputTokens: out.PromptEvalCount, OutputTokens: out.EvalCount}, Raw: raw}
	for i, tc := range out.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
		}
		args := tc.Function.Arguments
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		}
		mr.ToolUses = append(mr.ToolUses, agentrt.ToolUse{ID: id, Name: tc.Function.Name, Args: args})
	}
	if len(mr.ToolUses) > 0 {
		mr.StopReason = "tool_use"
	}
	return mr, nil
}

// convert maps one runtime message to Ollama messages. Tool results become
// role "tool" messages that follow the assistant message carrying the call.
func convert(msg agentrt.Message) []message {
	var out []message
	var text strings.Builder
	var calls []toolCall
	var results []message
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			var tc toolCall
			tc.ID = b.ToolUseID
			tc.Function.Name, tc.Function.Arguments = b.Name, b.Input
			calls = append(calls, tc)
		case "tool_result":
			results = append(results, message{Role: "tool", Content: b.Content, ToolName: b.Name})
		}
	}
	if msg.Role == "assistant" {
		out = append(out, message{Role: "assistant", Content: text.String(), ToolCalls: calls})
		return out
	}
	if text.Len() > 0 {
		out = append(out, message{Role: "user", Content: text.String()})
	}
	return append(out, results...)
}
