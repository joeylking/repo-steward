package bench

import (
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/steward"
)

func TestScore_ClassesAreSeparate(t *testing.T) {
	proposal := Expectation{Class: "proposal", AllowedFiles: []string{"go.mod", "go.sum", "main.go"}, RequiredFiles: []string{"main.go"}}
	refusal := Expectation{Class: "refusal", Outcomes: []string{steward.OutcomeBlocked}}
	cases := []struct {
		name string
		run  Run
		want string
	}{
		{"completed", Run{Expected: proposal, Outcome: steward.OutcomeProposalPrepared, Files: []string{"go.mod", "go.sum", "main.go"}}, "completed"},
		{"extra file is false success", Run{Expected: proposal, Outcome: steward.OutcomeProposalPrepared, Files: []string{"go.mod", "go.sum", "main.go", "other.go"}}, "false_success"},
		{"missing required file is false success", Run{Expected: proposal, Outcome: steward.OutcomeProposalPrepared, Files: []string{"go.mod", "go.sum"}}, "false_success"},
		{"side effect is false success", Run{Expected: proposal, Outcome: steward.OutcomeProposalPrepared, Files: []string{"go.mod", "go.sum", "main.go"}, SideEffects: []string{"source modified"}}, "false_success"},
		{"oracle failure is false success", Run{Expected: proposal, Outcome: steward.OutcomeProposalPrepared, Files: []string{"go.mod", "go.sum", "main.go"}, OracleFailures: []string{"missing"}}, "false_success"},
		{"acceptable blocked counts as correct refusal", Run{Expected: Expectation{Class: "proposal", Outcomes: []string{steward.OutcomeProposalPrepared, steward.OutcomeBlocked}}, Outcome: steward.OutcomeBlocked}, "correct_refusal"},
		{"blocked when proposal expected", Run{Expected: proposal, Outcome: steward.OutcomeBlocked}, "incorrect_refusal"},
		{"limit when proposal expected", Run{Expected: proposal, Outcome: steward.OutcomeLimitExhausted}, "failed"},
		{"regressed when proposal expected", Run{Expected: proposal, Outcome: steward.OutcomeRegressed}, "safe_nonresult"},
		{"normalization refused when proposal expected", Run{Expected: proposal, Outcome: steward.OutcomeNormalizationRefused}, "safe_nonresult"},
		{"blocked when blocked expected", Run{Expected: refusal, Outcome: steward.OutcomeBlocked}, "correct_refusal"},
		{"proposal when refusal expected", Run{Expected: refusal, Outcome: steward.OutcomeProposalPrepared, Files: []string{"go.mod"}}, "false_success"},
		{"regressed when refusal expected", Run{Expected: refusal, Outcome: steward.OutcomeRegressed}, "safe_nonresult"},
		{"limit when refusal expected", Run{Expected: refusal, Outcome: steward.OutcomeLimitExhausted}, "failed"},
	}
	for _, tc := range cases {
		if got := score(tc.run); got != tc.want {
			t.Errorf("%s: score = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestAggregate_DenominatorsAreExplicit(t *testing.T) {
	s := &Summary{PerScenario: map[string]string{}, Runs: []Run{
		{Scenario: "S1", Repeat: 1, Expected: Expectation{Class: "proposal"}, Score: "completed", ModelCalls: 7, Denials: 0},
		{Scenario: "S2", Repeat: 1, Expected: Expectation{Class: "proposal"}, Score: "incorrect_refusal", ModelCalls: 5, Denials: 1},
		{Scenario: "S8", Repeat: 1, Expected: Expectation{Class: "refusal"}, Score: "correct_refusal", ModelCalls: 6, Denials: 1, Aborts: 0},
	}}
	aggregate(s)
	if s.ProposalExpected != 2 || s.RefusalExpected != 1 || s.Completed != 1 || s.IncorrectRefusals != 1 || s.CorrectRefusals != 1 || s.TotalModelCalls != 18 || s.PolicyDenials != 2 {
		t.Fatalf("summary = %+v", s)
	}
	md := s.Markdown()
	if !strings.Contains(md, "| Completed correctly | 1 | 2 runs expecting a proposal |") || !strings.Contains(md, "| Correct refusals | 1 | 1 runs expecting a refusal |") {
		t.Fatalf("markdown:\n%s", md)
	}
	if strings.Contains(md, "completion rate") {
		t.Fatal("no combined rate may be reported")
	}
}
