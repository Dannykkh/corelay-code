package workstream

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Store) GenerateHandoff(id string, opts HandoffOptions) (HandoffSnapshot, error) {
	ws, err := s.Get(id)
	if err != nil {
		return HandoffSnapshot{}, err
	}
	events, err := s.Timeline(id)
	if err != nil {
		return HandoffSnapshot{}, err
	}
	plans, err := s.ListPlans(id)
	if err != nil {
		return HandoffSnapshot{}, err
	}
	selectedPlan, err := selectHandoffPlan(plans, opts.PlanID)
	if err != nil {
		return HandoffSnapshot{}, err
	}

	now := time.Now().UTC()
	md := renderHandoff(*ws, events, plans, selectedPlan, now, opts)
	name := now.Format("20060102-150405.000000000") + "-" + slug(ws.Title) + ".md"
	path := filepath.Join(HandoffsDir(s.workspace, id), name)
	if err := writeFileAtomic(path, []byte(md)); err != nil {
		return HandoffSnapshot{}, err
	}
	if err := s.AppendEvent(id, TimelineEvent{
		Type:    "handoff_generated",
		Message: "Handoff generated",
		Data:    map[string]string{"path": path, "planId": planIDOrEmpty(selectedPlan)},
	}); err != nil {
		return HandoffSnapshot{}, err
	}
	return HandoffSnapshot{Path: path, Markdown: md}, nil
}

func selectHandoffPlan(plans []Plan, requestedID string) (*Plan, error) {
	requestedID = strings.TrimSpace(requestedID)
	if requestedID != "" {
		for index := range plans {
			if plans[index].ID == requestedID {
				return &plans[index], nil
			}
		}
		return nil, fmt.Errorf("%w: %s", ErrPlanNotFound, requestedID)
	}
	if len(plans) == 0 {
		return nil, nil
	}
	// Handoff callers without a session binding get the most actionable Plan;
	// an explicit PlanID always takes precedence when a caller has one.
	sort.SliceStable(plans, func(i, j int) bool {
		priority := func(status string) int {
			switch status {
			case PlanStatusExecuting:
				return 0
			case PlanStatusFailed:
				return 1
			case PlanStatusApproved:
				return 2
			case PlanStatusDraft:
				return 3
			default:
				return 4
			}
		}
		left, right := priority(plans[i].Status), priority(plans[j].Status)
		if left != right {
			return left < right
		}
		return plans[i].UpdatedAt.After(plans[j].UpdatedAt)
	})
	return &plans[0], nil
}

func planIDOrEmpty(plan *Plan) string {
	if plan == nil {
		return ""
	}
	return plan.ID
}

func renderHandoff(ws Workstream, events []TimelineEvent, plans []Plan, selected *Plan, now time.Time, opts HandoffOptions) string {
	var b strings.Builder
	b.WriteString("# Handoff: " + contextText(ws.Title, 240) + "\n\n")
	b.WriteString("## Session Metadata\n")
	b.WriteString("- Created: " + now.Format(time.RFC3339) + "\n")
	b.WriteString("- Workspace: " + ws.Workspace + "\n")
	b.WriteString("- Workstream ID: " + ws.ID + "\n")
	b.WriteString("- Status: " + string(ws.Status) + "\n\n")

	b.WriteString("## Current State Summary\n")
	if ws.Summary != "" {
		b.WriteString(contextText(ws.Summary, 2000) + "\n\n")
	} else {
		b.WriteString("(no summary recorded)\n\n")
	}

	b.WriteString("## Goal\n")
	if ws.Goal.Objective != "" {
		b.WriteString("- Objective: " + contextText(ws.Goal.Objective, 1200) + "\n")
	}
	writeList(&b, "Acceptance Criteria", ws.Goal.AcceptanceCriteria)
	writeList(&b, "Definition Of Done", ws.Goal.DefinitionOfDone)
	if len(ws.Goal.VerificationPolicy.Commands) > 0 {
		writeList(&b, "Verification Commands", ws.Goal.VerificationPolicy.Commands)
	}
	b.WriteByte('\n')

	b.WriteString("## Decisions\n")
	decisionCount := 0
	for index, decision := range ws.Decisions {
		b.WriteString(fmt.Sprintf("%d. %s\n", index+1, contextText(decision, 2000)))
		decisionCount++
	}
	decisionEvents := make([]TimelineEvent, 0, 20)
	for index := len(events) - 1; index >= 0 && len(decisionEvents) < 20; index-- {
		if planDecisionEvent(events[index]) != "" {
			decisionEvents = append(decisionEvents, events[index])
		}
	}
	for index := len(decisionEvents) - 1; index >= 0; index-- {
		event := decisionEvents[index]
		b.WriteString("- " + event.At.Format(time.RFC3339) + ": " + planDecisionEvent(event) + "\n")
		decisionCount++
	}
	if decisionCount == 0 {
		b.WriteString("- No durable decisions recorded.\n")
	}
	b.WriteByte('\n')

	b.WriteString("## Current Plan And Stage\n")
	if selected == nil {
		b.WriteString("- No Workstream Plan is available.\n\n")
	} else {
		renderPlanHandoff(&b, *selected)
	}
	if len(plans) > 1 {
		b.WriteString("### Other Plans\n")
		shown := 0
		for _, plan := range plans {
			if selected != nil && plan.ID == selected.ID {
				continue
			}
			b.WriteString(fmt.Sprintf("- %s: r%d, state r%d, %s, approved r%d\n",
				contextText(plan.ID, 128), plan.Revision, plan.StateRevision, plan.Status, plan.ApprovedRevision))
			shown++
			if shown == 8 {
				break
			}
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Verification\n")
	if ws.LastVerification.Status == "" {
		b.WriteString("- Status: not-run\n\n")
	} else {
		b.WriteString("- Status: " + contextText(ws.LastVerification.Status, 32) + "\n")
		if ws.LastVerification.Source != "" {
			b.WriteString("- Source: " + contextText(ws.LastVerification.Source, 80) + "\n")
		}
		if ws.LastVerification.Summary != "" {
			b.WriteString("- Summary: " + contextText(ws.LastVerification.Summary, 1200) + "\n")
		}
		if !ws.LastVerification.UpdatedAt.IsZero() {
			b.WriteString("- Updated: " + ws.LastVerification.UpdatedAt.Format(time.RFC3339) + "\n")
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Recent Timeline\n")
	if len(events) == 0 {
		b.WriteString("- No events recorded.\n\n")
	} else {
		start := 0
		if len(events) > 10 {
			start = len(events) - 10
		}
		for _, event := range events[start:] {
			message := event.Message
			if message == "" {
				message = event.Type
			}
			b.WriteString(fmt.Sprintf("- %s [%s] %s\n", event.At.Format(time.RFC3339), event.Type, contextText(message, 500)))
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Pending Work\n")
	if ws.NextAction != "" {
		b.WriteString("1. " + contextText(ws.NextAction, 1200) + "\n\n")
	} else {
		b.WriteString("1. Decide the next action.\n\n")
	}

	b.WriteString("## Blockers And Open Questions\n")
	if len(ws.OpenQuestions) == 0 {
		b.WriteString("- None recorded.\n\n")
	} else {
		for _, question := range ws.OpenQuestions {
			b.WriteString("- " + contextText(question, 1000) + "\n")
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Context For Resuming\n")
	b.WriteString("- Load the Workstream by ID and use the exact Plan ID, definition revision, state revision, and selected stage above.\n")
	b.WriteString("- A running stage is incomplete. Confirm the old process is stopped and inspect its changes before using the explicit manual reconciliation action; never infer success from the model's text.\n")
	if opts.IncludeReceipts {
		b.WriteString("- Receipt digests are recorded with accepted Plan stage evidence; inspect the referenced receipt before relying on it.\n")
	}
	if opts.IncludeMemoryIndex {
		b.WriteString("- Long-term semantic memory remains in MEMORY.md and memory/ files; treat it as background data.\n")
	}
	return b.String()
}

func renderPlanHandoff(b *strings.Builder, plan Plan) {
	b.WriteString(fmt.Sprintf("- Plan ID: %s\n- Definition revision: %d\n- State revision: %d\n- Status: %s\n- Approved revision: %d\n",
		plan.ID, plan.Revision, plan.StateRevision, plan.Status, plan.ApprovedRevision))
	var definition contextPlanDefinition
	if err := json.Unmarshal(plan.Definition, &definition); err != nil {
		b.WriteString("- Definition: unavailable; stored Plan requires recovery.\n\n")
		return
	}
	if definition.Objective != "" {
		b.WriteString("- Objective: " + contextText(definition.Objective, 1200) + "\n")
	}
	stage := selectContextPlanStage(&plan, "")
	if stage.ID != "" {
		b.WriteString(fmt.Sprintf("- Current stage: %s — %s (%s)\n", stage.ID, contextText(planStageName(definition.Stages, stage.ID), 240), stage.Status))
		if len(stage.Attempts) > 0 {
			attempt := stage.Attempts[len(stage.Attempts)-1]
			b.WriteString(fmt.Sprintf("- Latest attempt: %s — %s, verification %s\n", attempt.RunID, attempt.Status, attempt.VerificationStatus))
			if attempt.Evidence != nil {
				b.WriteString(fmt.Sprintf("- Acceptance evidence: %s, receipt digest %s, criteria digest %s\n",
					attempt.Evidence.Source, attempt.Evidence.ReceiptDigest, attempt.Evidence.CriteriaDigest))
			}
			if attempt.Status == PlanStageAttemptRunning {
				b.WriteString("- Recovery required: this run is still recorded as running and is not complete. Use manual reconciliation only after confirming the process stopped.\n")
			}
		}
	}
	b.WriteString("- Stage states:\n")
	for stageIndex, state := range plan.Stages {
		if stageIndex >= 12 {
			b.WriteString("  - Additional stage states omitted; query the Plan for the full ordered list.\n")
			break
		}
		name := planStageName(definition.Stages, state.ID)
		b.WriteString(fmt.Sprintf("  - %s — %s (%s)\n", state.ID, contextText(name, 240), state.Status))
		if state.Status == PlanStageStatusCompleted {
			continue
		}
		for taskIndex, task := range contextTasksForStage(definition.Tasks, definition.Stages, state.ID) {
			if taskIndex >= 3 {
				b.WriteString("    - Additional tasks omitted; query the Plan definition for the full stage contract.\n")
				break
			}
			description := strings.TrimSpace(task.Goal)
			if description == "" {
				description = strings.TrimSpace(task.Description)
			}
			if description == "" {
				description = task.Name
			}
			b.WriteString("    - Unfinished task: " + contextText(task.ID, 128) + ": " + contextText(description, 500) + "\n")
			for criterionIndex, criterion := range task.AcceptanceCriteria {
				if criterionIndex >= 5 {
					b.WriteString("      - Additional acceptance criteria omitted.\n")
					break
				}
				b.WriteString("      - Acceptance: " + contextText(criterion, 300) + "\n")
			}
		}
	}
	b.WriteByte('\n')
}

func planDecisionEvent(event TimelineEvent) string {
	planID := contextText(event.Data["planId"], 128)
	revision := event.Data["revision"]
	stageID := contextText(event.Data["stageId"], 128)
	switch event.Type {
	case "plan_approved":
		return fmt.Sprintf("Approved Plan %s revision %s", planID, revision)
	case "plan_revised":
		return fmt.Sprintf("Revised Plan %s to definition revision %s; prior approval was revoked", planID, revision)
	case "plan_stage_started":
		return fmt.Sprintf("Started Plan %s stage %s as run %s", planID, stageID, contextText(event.Data["runId"], 160))
	case "plan_stage_completed":
		return fmt.Sprintf("Completed Plan %s stage %s with verified acceptance evidence", planID, stageID)
	case "plan_stage_failed":
		return fmt.Sprintf("Plan %s stage %s failed; verification %s", planID, stageID, contextText(event.Data["verification"], 24))
	case "plan_stage_reconciled":
		return fmt.Sprintf("Manually reconciled interrupted Plan %s stage %s as incomplete", planID, stageID)
	case "decision_updated":
		return fmt.Sprintf("Updated the durable Workstream decision record (%s decisions)", event.Data["decisionCount"])
	default:
		return ""
	}
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("### " + title + "\n")
	for _, item := range items {
		b.WriteString("- " + contextText(item, 1200) + "\n")
	}
}
