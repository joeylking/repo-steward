package tools_test

import (
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/testkit"

	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/tools"
)

// TestSpecs_FuzzedArguments puts generated arguments through the runtime's
// own acceptance of a tool call, with agent-runtime's testkit: arguments
// that satisfy a tool's schema must be accepted and reach the tool without
// a panic, and near misses must be refused before the tool is called. The
// session is a fixture workspace with no container engine, publication
// included, so every tool is exercised except run_validation, which needs
// the engine to run at all and is covered by the integration tests.
func TestSpecs_FuzzedArguments(t *testing.T) {
	s, _ := newSession(t, "patch-safe")
	s.Publish = &session.Publication{}
	var fuzzed []agentrt.Tool
	for _, tl := range tools.All(s) {
		if tl.Spec().Name != names.Validate {
			fuzzed = append(fuzzed, tl)
		}
	}
	if len(fuzzed) != 13 {
		t.Fatalf("%d tools fuzzed, want every tool but run_validation", len(fuzzed))
	}
	testkit.Fuzz{Count: 16}.Check(t, fuzzed...)
}
