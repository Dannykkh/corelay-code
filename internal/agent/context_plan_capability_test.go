package agent

import (
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestContextPlanConsumesReliableInputCeilingWithoutChangingTotalWindow(t *testing.T) {
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:                  "reliable-context-plan",
		ContextWindow:       64_000,
		OutputReserve:       8_000,
		ReliableInputTokens: 5_000,
	})
	for _, test := range []struct {
		name  string
		input int
		fits  bool
	}{
		{name: "at empirical boundary", input: 5_000, fits: true},
		{name: "over empirical boundary", input: 5_001, fits: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			estimator := &phase2TokenEstimator{estimate: func(ContextEstimateRequest) TokenEstimate {
				return TokenEstimate{
					InputTokens:            test.input,
					ProtocolOverheadTokens: 200,
					Source:                 "exact-capability-fixture",
					Confidence:             "exact",
				}
			}}
			plan, _, err := CalculateContextPlan(ContextPlanningRequest{
				Profile:            profile,
				Model:              "empirical-model",
				System:             ContextSystemSections{CorePrefix: "system"},
				Messages:           []types.Message{{Role: "user", Content: mustJSON("task")}},
				MaxTokens:          2_000,
				Estimator:          estimator,
				SafetyMarginTokens: 500,
			})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Fits != test.fits {
				t.Fatalf("Fits = %v, want %v", plan.Fits, test.fits)
			}
			if plan.ReliableInputLimitTokens != 5_000 || plan.UsableInputTokens != 5_000 {
				t.Fatalf("reliable/usable input = %d/%d, want 5000/5000", plan.ReliableInputLimitTokens, plan.UsableInputTokens)
			}
			if plan.ContextWindowTokens != 64_000 || plan.ModelOutputLimitTokens != 8_000 {
				t.Fatalf("model limits changed: %+v", plan)
			}
			wantRemaining := 64_000 - (test.input + 200 + 2_000 + 500)
			if plan.RemainingTokens != wantRemaining {
				t.Fatalf("RemainingTokens = %d, want context-derived %d", plan.RemainingTokens, wantRemaining)
			}
		})
	}
}
