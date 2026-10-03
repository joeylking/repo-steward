package steward

import (
	"testing"

	agentrt "github.com/joeylking/agent-runtime"

	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/session"
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
