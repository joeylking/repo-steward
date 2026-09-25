package steward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
	"github.com/joeylking/agent-runtime/providers/anthropic"
	"github.com/joeylking/agent-runtime/providers/ollama"
	"github.com/joeylking/agent-runtime/replay"

	"github.com/joeylking/repo-steward/internal/agent"
	"github.com/joeylking/repo-steward/internal/session"
)

// ModelSpec names a model and how its calls are captured.
type ModelSpec struct {
	// Provider and Name, e.g. "ollama" and "qwen3:30b-a3b".
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// RecordDir, when set, stores every response for later replay.
	RecordDir string `json:"record_dir,omitempty"`
	// ReplayDir, when set, serves recorded responses and never calls the
	// provider.
	ReplayDir string `json:"replay_dir,omitempty"`
}

// ParseModelSpec parses "provider:name".
func ParseModelSpec(s string) (ModelSpec, error) {
	i := strings.Index(s, ":")
	if i <= 0 || i == len(s)-1 {
		return ModelSpec{}, fmt.Errorf("model must be provider:name, got %q", s)
	}
	return ModelSpec{Provider: s[:i], Name: s[i+1:]}, nil
}

func (m ModelSpec) String() string { return m.Provider + ":" + m.Name }

// ErrNoKey is returned when a Claude run has no API key configured. The key
// is read here rather than left to the provider SDK so that a run without
// one is refused before anything is recorded.
var ErrNoKey = errors.New("anthropic: no API key in ANTHROPIC_API_KEY or CLAUDE_KEY")

// apiKey reads the Claude credential from the environment.
func apiKey() string {
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return k
	}
	return os.Getenv("CLAUDE_KEY")
}

// Prices returns the price table for the spec's model. A local model is
// recorded as free, which keeps "free" and "unpriced" distinguishable; a
// paid model absent from its provider's table is refused rather than run at
// a price of zero.
func (m ModelSpec) Prices() (agentrt.PriceTable, error) {
	switch m.Provider {
	case "ollama":
		om, err := ollama.New(ollama.Config{Model: m.Name})
		if err != nil {
			return nil, err
		}
		return ollama.Free(om), nil
	case "anthropic":
		table := anthropic.Prices()
		if _, err := providers.PriceFor(table, m.String()); err != nil {
			return nil, err
		}
		return providers.Merge(table), nil
	}
	return nil, nil
}

// build returns the agentrt.Model for the spec. The adapters' default names
// are "ollama:<model>" and "anthropic:<model>", which is what the price
// table and the recordings are keyed by.
func (m ModelSpec) build() (agentrt.Model, error) {
	var inner agentrt.Model
	switch m.Provider {
	case "ollama":
		// Host comes from OLLAMA_HOST by way of the adapter's default;
		// thinking mode stays off and the context window is the default.
		om, err := ollama.New(ollama.Config{Model: m.Name})
		if err != nil {
			return nil, err
		}
		inner = om
	case "anthropic":
		key := apiKey()
		if key == "" {
			return nil, ErrNoKey
		}
		am, err := anthropic.New(anthropic.Config{Model: m.Name, APIKey: key})
		if err != nil {
			return nil, err
		}
		inner = am
	default:
		return nil, fmt.Errorf("unknown model provider %q (available: ollama, anthropic)", m.Provider)
	}
	if m.ReplayDir != "" {
		return &replay.Replayer{ModelName: inner.Name(), Dir: m.ReplayDir}, nil
	}
	if m.RecordDir != "" {
		return &replay.Recorder{Inner: inner, Dir: m.RecordDir}, nil
	}
	return inner, nil
}

// DefaultModelLimits bound a model-driven run. A local model is priced
// free, so the cost limit only matters for a paid provider.
func DefaultModelLimits() agentrt.Limits {
	l := agentrt.DefaultLimits()
	l.MaxSteps = 40
	l.MaxModelCalls = 80
	l.MaxOutputTokensPerCall = 4096
	l.MaxTotalTokens = 2_000_000
	return l
}

// RunModel executes the model-driven agent.
func RunModel(ctx context.Context, opts Options, spec ModelSpec) (*Result, error) {
	if opts.RuntimeLimits.MaxSteps == 0 {
		opts.RuntimeLimits = DefaultModelLimits()
	}
	opts.Model = &spec
	if opts.Prices == nil {
		p, err := spec.Prices()
		if err != nil {
			return nil, err
		}
		opts.Prices = p
	}
	return runAgent(ctx, opts, "model:"+spec.String(), nil)
}

// modelDriverConfig is applied by driver() when the run has a model spec.
func (r *run) modelConfig() (*agentrt.ModelConfig, agentrt.Agent, error) {
	if r.opts.Model == nil {
		return nil, nil, nil
	}
	m, err := r.opts.Model.build()
	if err != nil {
		return nil, nil, err
	}
	sc := r.opts.ScopeConfig
	facts := agent.Facts{ModulePath: r.profile.ModulePath, GoDirective: r.profile.GoDirective, NamedDependency: r.opts.Policy.NamedDependency, CandidateCount: len(r.cands),
		ScopeSummary: fmt.Sprintf("at most %d source files and %d changed lines without approval, hard limits %d files and %d lines", sc.FilesSoft, sc.LinesSoft, sc.FilesHard, sc.LinesHard)}
	return &agentrt.ModelConfig{Model: m, Prices: r.opts.Prices, MaxRetries: 2}, &agent.Agent{Facts: facts}, nil
}

var _ = session.NewID
