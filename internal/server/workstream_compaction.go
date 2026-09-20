package server

import (
	"strings"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

// workstreamCompactionContext projects typed durable workflow facts into the
// agent's bounded compaction contract. Prompt-rendered context is deliberately
// not parsed back into state.
func workstreamCompactionContext(
	ws *workstream.Workstream,
	plan *workstream.Plan,
	stageID string,
) agent.CompactionContext {
	var result agent.CompactionContext
	if ws != nil {
		result.WorkstreamID = ws.ID
		result.WorkstreamObjective = ws.Goal.Objective
		result.Constraints = append([]string(nil), ws.Goal.Constraints...)
		result.Decisions = append([]string(nil), ws.Decisions...)
		result.LastVerificationStatus = ws.LastVerification.Status
		result.LastVerificationSource = ws.LastVerification.Source
		result.LastVerificationSummary = ws.LastVerification.Summary
	}
	if plan == nil {
		return result
	}
	result.WorkstreamID = plan.WorkstreamID
	result.PlanID = plan.ID
	result.PlanRevision = plan.Revision
	result.PlanStateRevision = plan.StateRevision
	result.StageID = strings.TrimSpace(stageID)
	for _, stage := range plan.Stages {
		item := agent.CompactionPlanEvidence{StageID: stage.ID, StageStatus: stage.Status}
		if count := len(stage.Attempts); count > 0 {
			attempt := stage.Attempts[count-1]
			item.AttemptStatus = attempt.Status
			item.AttemptPlanRevision = attempt.PlanRevision
			if attempt.Evidence != nil {
				item.ReceiptDigest = attempt.Evidence.ReceiptDigest
				item.CriteriaDigest = attempt.Evidence.CriteriaDigest
				item.VerificationStatus = attempt.Evidence.VerificationStatus
				item.CompletionStatus = attempt.Evidence.CompletionStatus
			}
		}
		result.PlanEvidence = append(result.PlanEvidence, item)
	}
	// Keep evidence for the currently selected stage inside the bounded tail.
	for index, item := range result.PlanEvidence {
		if item.StageID == result.StageID && index != len(result.PlanEvidence)-1 {
			result.PlanEvidence = append(append(result.PlanEvidence[:index], result.PlanEvidence[index+1:]...), item)
			break
		}
	}
	return result
}
