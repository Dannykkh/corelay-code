package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func TestWorkstreamCompactionContextProjectsCanonicalPlanState(t *testing.T) {
	ws := &workstream.Workstream{
		ID: "ws-auth",
		Goal: workstream.Goal{
			Objective:   "Preserve authentication behavior.",
			Constraints: []string{"Keep the token boundary unchanged."},
		},
		Decisions: []string{"Use the canonical Workstream Plan."},
		LastVerification: workstream.VerificationResult{
			Status: "passed", Source: "go-test", Summary: "The token verifier tests passed.",
		},
	}
	plan := &workstream.Plan{
		ID: "plan-auth", WorkstreamID: "ws-auth", Revision: 5, StateRevision: 12,
		Stages: make([]workstream.PlanStage, 13),
	}
	for index := range plan.Stages {
		plan.Stages[index] = workstream.PlanStage{ID: fmt.Sprintf("stage-%02d", index), Status: workstream.PlanStageStatusCompleted}
	}
	plan.Stages[0] = workstream.PlanStage{
		ID: "verify", Status: workstream.PlanStageStatusFailed,
		Attempts: []workstream.PlanStageAttempt{
			{
				RunID: "run-accepted", PlanRevision: 5, Status: workstream.PlanStageAttemptComplete,
				Evidence: &workstream.PlanAcceptanceEvidence{
					ReceiptDigest:      "sha256:" + strings.Repeat("a", 64),
					CriteriaDigest:     "sha256:" + strings.Repeat("b", 64),
					VerificationStatus: "passed", CompletionStatus: "completed",
				},
			},
			{RunID: "run-failed", PlanRevision: 5, Status: workstream.PlanStageAttemptFailed},
		},
	}
	plan.Stages[12].Attempts = []workstream.PlanStageAttempt{{
		RunID: "run-accepted", PlanRevision: 5, Status: workstream.PlanStageAttemptComplete,
		Evidence: &workstream.PlanAcceptanceEvidence{
			ReceiptDigest: strings.Repeat("a", 64), CriteriaDigest: strings.Repeat("b", 64),
			VerificationStatus: "passed", CompletionStatus: "completed",
		},
	}}

	context := workstreamCompactionContext(ws, plan, "verify")
	if context.WorkstreamID != "ws-auth" || context.WorkstreamObjective != "Preserve authentication behavior." ||
		context.PlanID != "plan-auth" || context.PlanRevision != 5 || context.PlanStateRevision != 12 || context.StageID != "verify" {
		t.Fatalf("workflow identity/revisions = %+v", context)
	}
	if len(context.Constraints) != 1 || context.Constraints[0] != "Keep the token boundary unchanged." ||
		len(context.Decisions) != 1 || context.Decisions[0] != "Use the canonical Workstream Plan." ||
		context.LastVerificationStatus != "passed" || context.LastVerificationSource != "go-test" ||
		context.LastVerificationSummary != "The token verifier tests passed." {
		t.Fatalf("durable Workstream facts = %+v", context)
	}
	if len(context.PlanEvidence) != len(plan.Stages) {
		t.Fatalf("stage state records = %d, want %d", len(context.PlanEvidence), len(plan.Stages))
	}
	current := context.PlanEvidence[len(context.PlanEvidence)-1]
	if current.StageID != "verify" || current.StageStatus != workstream.PlanStageStatusFailed ||
		current.AttemptStatus != workstream.PlanStageAttemptFailed || current.AttemptPlanRevision != 5 ||
		current.ReceiptDigest != "" || current.CriteriaDigest != "" || current.VerificationStatus != "" || current.CompletionStatus != "" {
		t.Fatalf("selected stage must report its latest failed attempt without stale acceptance evidence: %+v", current)
	}
	accepted := context.PlanEvidence[len(context.PlanEvidence)-2]
	if accepted.StageID != "stage-12" || accepted.ReceiptDigest != strings.Repeat("a", 64) ||
		accepted.CriteriaDigest != strings.Repeat("b", 64) || accepted.VerificationStatus != "passed" || accepted.CompletionStatus != "completed" {
		t.Fatalf("canonical stored acceptance evidence = %+v", accepted)
	}

	message, err := json.Marshal("inspect the current verifier")
	if err != nil {
		t.Fatal(err)
	}
	compacted := agent.BuildDeterministicCompaction([]types.Message{{Role: "user", Content: message}}, agent.CompactionState{Context: context})
	if len(compacted.Snapshot.PlanEvidence) == 0 {
		t.Fatal("bounded structured snapshot lost stage state")
	}
	boundedCurrent := compacted.Snapshot.PlanEvidence[len(compacted.Snapshot.PlanEvidence)-1]
	if boundedCurrent.StageID != "verify" || boundedCurrent.StageStatus != workstream.PlanStageStatusFailed ||
		boundedCurrent.AttemptStatus != workstream.PlanStageAttemptFailed || boundedCurrent.ReceiptDigest != "" {
		t.Fatalf("bounded snapshot lost selected failed stage state: %+v", boundedCurrent)
	}
	boundedAccepted := compacted.Snapshot.PlanEvidence[len(compacted.Snapshot.PlanEvidence)-2]
	if boundedAccepted.StageID != "stage-12" || boundedAccepted.ReceiptDigest != "sha256:"+strings.Repeat("a", 64) ||
		boundedAccepted.CriteriaDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("bounded snapshot did not normalize persisted bare digests: %+v", boundedAccepted)
	}
}
