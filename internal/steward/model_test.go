package steward

import (
	"errors"
	"testing"

	"github.com/joeylking/agent-runtime/providers"
)

// The adapters' own names are what the price table and the recordings are
// keyed by, so a spec's string must be the name the adapter reports.
func TestModelSpec_LocalModelIsNamedAndFree(t *testing.T) {
	spec := ModelSpec{Provider: "ollama", Name: "qwen3:30b-a3b"}
	m, err := spec.build()
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != spec.String() {
		t.Fatalf("name = %s, want %s", m.Name(), spec.String())
	}
	prices, err := spec.Prices()
	if err != nil {
		t.Fatal(err)
	}
	// Free, not unpriced: the entry exists and costs nothing.
	p, err := providers.PriceFor(prices, spec.String())
	if err != nil {
		t.Fatalf("local model unpriced: %v", err)
	}
	if p.InputPerMTok != 0 || p.OutputPerMTok != 0 {
		t.Fatalf("local model priced: %+v", p)
	}
}

// The paid provider refuses without a key and without a known price, and
// neither refusal reaches the API.
func TestModelSpec_PaidProviderRefusals(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_KEY", "")
	if _, err := (ModelSpec{Provider: "anthropic", Name: "claude-sonnet-5"}).build(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("built a paid model without a key: %v", err)
	}
	if _, err := (ModelSpec{Provider: "anthropic", Name: "no-such-model"}).Prices(); !errors.Is(err, providers.ErrNoPrice) {
		t.Fatalf("an unpriced paid model was accepted: %v", err)
	}
	priced := ModelSpec{Provider: "anthropic", Name: "claude-sonnet-5"}
	prices, err := priced.Prices()
	if err != nil {
		t.Fatal(err)
	}
	p, err := providers.PriceFor(prices, priced.String())
	if err != nil || p.InputPerMTok == 0 || p.OutputPerMTok == 0 {
		t.Fatalf("price for %s = %+v, %v", priced, p, err)
	}
	if _, err := (ModelSpec{Provider: "openai", Name: "x"}).build(); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
