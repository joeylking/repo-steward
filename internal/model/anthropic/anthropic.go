// Package anthropic adapts the Claude Messages API to agentrt.Model. It is
// the paid provider, used for explicit budgeted measurements only: nothing
// in development, tests, or CI constructs it, and every run that does is
// bounded by the runtime's cost limit with the price table below.
package anthropic

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

// Prices per million tokens in USD micros, from the published price list
// on 2026-09-24. Cache writes cost 1.25 times base input and have no
// column in the runtime's Usage, so Generate folds them into InputTokens
// at that multiple; the accounting therefore never understates a bill.
func Prices() agentrt.PriceTable {
	return agentrt.PriceTable{
		"anthropic:claude-sonnet-5":           {InputPerMTok: 2_000_000, OutputPerMTok: 10_000_000, CachedInputPerMTok: 200_000},
		"anthropic:claude-haiku-4-5-20251001": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000, CachedInputPerMTok: 100_000},
		"anthropic:claude-opus-5":             {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CachedInputPerMTok: 500_000},
	}
}

// Model is one Claude model.
type Model struct {
	Name_  string
	APIKey string
	Base   string
	Client *http.Client
}

// ErrNoKey is returned when no API key is configured.
var ErrNoKey = errors.New("anthropic: no API key in ANTHROPIC_API_KEY or CLAUDE_KEY")

// New returns a model reading its key from ANTHROPIC_API_KEY or CLAUDE_KEY.
func New(name string) (*Model, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		key = os.Getenv("CLAUDE_KEY")
	}
	if key == "" {
		return nil, ErrNoKey
	}
	return &Model{Name_: name, APIKey: key, Base: "https://api.anthropic.com", Client: &http.Client{}}, nil
}

// Name implements agentrt.Model.
func (m *Model) Name() string { return "anthropic:" + m.Name_ }

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type request struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	System       string         `json:"system,omitempty"`
	Messages     []message      `json:"messages"`
	Tools        []tool         `json:"tools,omitempty"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type response struct {
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Generate implements agentrt.Model.
func (m *Model) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	r := request{Model: m.Name_, MaxTokens: req.MaxOutputTokens, System: req.System, CacheControl: map[string]any{"type": "ephemeral"}}
	if r.MaxTokens <= 0 {
		r.MaxTokens = 4096
	}
	for _, msg := range req.Messages {
		var blocks []block
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				// The API rejects an empty text block; the agent's nudge can
				// produce one when the model returned nothing at all.
				if strings.TrimSpace(b.Text) != "" {
					blocks = append(blocks, block{Type: "text", Text: b.Text})
				}
			case "tool_use":
				in := b.Input
				if len(bytes.TrimSpace(in)) == 0 {
					in = json.RawMessage("{}")
				}
				blocks = append(blocks, block{Type: "tool_use", ID: b.ToolUseID, Name: b.Name, Input: in})
			case "tool_result":
				blocks = append(blocks, block{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
			}
		}
		if len(blocks) == 0 {
			// A message must carry at least one block.
			blocks = []block{{Type: "text", Text: "(no output)"}}
		}
		r.Messages = append(r.Messages, message{Role: msg.Role, Content: blocks})
	}
	for _, ts := range req.Tools {
		r.Tools = append(r.Tools, tool{Name: ts.Name, Description: ts.Description, InputSchema: ts.InputSchema})
	}
	body, err := json.Marshal(r)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.Base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-api-key", m.APIKey)
	hreq.Header.Set("anthropic-version", "2023-06-01")
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
	if resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode >= 500 {
		return agentrt.ModelResponse{}, agentrt.TransientError{Err: fmt.Errorf("anthropic: %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	if resp.StatusCode != http.StatusOK {
		return agentrt.ModelResponse{}, fmt.Errorf("anthropic: %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return agentrt.ModelResponse{}, fmt.Errorf("anthropic: decode: %w", err)
	}
	if out.Error != nil {
		return agentrt.ModelResponse{}, fmt.Errorf("anthropic: %s: %s", out.Error.Type, out.Error.Message)
	}
	u := out.Usage
	mr := agentrt.ModelResponse{StopReason: out.StopReason, Raw: raw, Usage: agentrt.Usage{
		// Cache writes are billed at 1.25x base input; charge them as
		// input at that multiple, rounded up, so the cap never undercounts.
		InputTokens:       u.InputTokens + u.CacheReadInputTokens + (u.CacheCreationInputTokens*5+3)/4,
		CachedInputTokens: u.CacheReadInputTokens,
		OutputTokens:      u.OutputTokens,
	}}
	var text strings.Builder
	for i, b := range out.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			id := b.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
			}
			in := b.Input
			if len(bytes.TrimSpace(in)) == 0 {
				in = json.RawMessage("{}")
			}
			mr.ToolUses = append(mr.ToolUses, agentrt.ToolUse{ID: id, Name: b.Name, Args: in})
		}
	}
	mr.Text = text.String()
	return mr, nil
}
