package steward

import (
	"encoding/json"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
	"github.com/joeylking/repo-steward/internal/tools"
)

// A proxy that cannot be reached while staging the upgrade ends the run
// acquisition_failed, not admission_refused, in baseline mode and through
// apply_upgrade's abort in the agent modes.
func TestStageOutcome_AcquisitionFailureIsNotARefusal(t *testing.T) {
	unreachable := `go: example.com/lib@v1.2.4: Get "https://proxy.golang.org/example.com/lib/@v/v1.2.4.info": dial tcp: lookup proxy.golang.org on 192.168.5.1:53: no such host`
	cases := map[string]string{
		unreachable: OutcomeAcquisitionFailed,
		"go: example.com/lib@v2.0.0 requires go >= 1.25 (running go 1.22.12; GOTOOLCHAIN=local)":                     OutcomeRequiresNewerToolchain,
		"go: example.com/lib@v1.2.4: reading https://proxy.golang.org/example.com/lib/@v/v1.2.4.info: 404 Not Found": OutcomeAdmissionRefused,
	}
	for stderr, want := range cases {
		if got := stageOutcome(&manifest.OpError{Op: manifest.Op{Kind: "upgrade"}, ExitCode: 1, Stderr: stderr}); got != want {
			t.Errorf("%q: outcome %s, want %s", stderr, got, want)
		}
	}
	rr := agentrt.Run{Status: agentrt.StatusFailed, Reason: agentrt.ReasonToolAbort, ReasonDetail: "acquisition_failed: " + unreachable}
	got, detail := outcomeOf(rr, &session.Session{})
	if got != OutcomeAcquisitionFailed || detail["run_detail"] != rr.ReasonDetail {
		t.Fatalf("agent outcome %s detail %v", got, detail)
	}
	rr.ReasonDetail = "something else"
	if got, _ := outcomeOf(rr, &session.Session{}); got != OutcomeFailed {
		t.Fatalf("other tool abort mapped to %s", got)
	}
}

// A run that ends without a proposal because readiness refused its repair
// only for want of test coverage, with the tree unchanged since, is named
// repair_not_exercised; any other last refusal, a later edit, or an outcome
// that is not a give-up keeps the runtime's outcome.
func TestNotExercised(t *testing.T) {
	step := func(tool string, status agentrt.StepStatus, errText string) agentrt.Step {
		st := agentrt.Step{Status: status, Decision: &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tool}}
		if errText != "" {
			b, _ := json.Marshal(map[string]string{"tool": tool, "error": errText})
			st.Observation = &agentrt.Observation{Kind: agentrt.ObserveToolError, Content: b}
		} else {
			st.Observation = &agentrt.Observation{Kind: agentrt.ObserveToolResult, Content: []byte(`{}`)}
		}
		return st
	}
	notExercisedErr := tools.NotReadyPrefix + `[{"code":"repair_not_exercised","detail":"main.go:16 (no test executes it)"}]`
	otherErr := tools.NotReadyPrefix + `[{"code":"repair_not_exercised","detail":"x"},{"code":"scope_exceeded","detail":"y"}]`
	edit := step(names.EditFile, agentrt.StepDone, "")
	refused := step(names.Prepare, agentrt.StepFailed, notExercisedErr)
	blocked := step(names.Blocked, agentrt.StepDone, "")
	cases := []struct {
		name    string
		outcome string
		steps   []agentrt.Step
		want    string
	}{
		{"gave up", OutcomeBlocked, []agentrt.Step{edit, refused, blocked}, OutcomeRepairNotExercised},
		{"limit", OutcomeLimitExhausted, []agentrt.Step{edit, refused, refused, refused}, OutcomeRepairNotExercised},
		{"edited after", OutcomeBlocked, []agentrt.Step{edit, refused, edit, blocked}, OutcomeBlocked},
		{"other failures too", OutcomeBlocked, []agentrt.Step{edit, step(names.Prepare, agentrt.StepFailed, otherErr), blocked}, OutcomeBlocked},
		{"never prepared", OutcomeBlocked, []agentrt.Step{edit, blocked}, OutcomeBlocked},
		{"rejected", OutcomeApprovalRejected, []agentrt.Step{edit, refused}, OutcomeApprovalRejected},
		{"scope", OutcomeScopeExceeded, []agentrt.Step{edit, refused}, OutcomeScopeExceeded},
	}
	for _, c := range cases {
		got, detail := notExercised(c.outcome, map[string]any{}, c.steps)
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		if got == OutcomeRepairNotExercised && (detail["run_outcome"] != c.outcome || detail["not_exercised"] != "main.go:16 (no test executes it)") {
			t.Errorf("%s: detail %v", c.name, detail)
		}
	}
}
