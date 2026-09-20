package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func beginWorkstreamPlanStageForAgentRun(
	store *workstream.Store,
	durableRun *durableAgentRun,
	workstreamID, planID string,
	request workstream.BeginPlanStageRequest,
) (*workstream.Plan, error) {
	started, err := store.BeginPlanStage(workstreamID, planID, request)
	if err != nil && durableRun != nil {
		err = errors.Join(err, durableRun.DiscardPreparedImages())
	}
	return started, err
}

func commitDurableImagesForPlanRun(
	durableRun *durableAgentRun,
	recorder *workstreamPlanStageRecorder,
) error {
	if err := durableRun.CommitImages(); err != nil {
		if recorder != nil {
			recorder.RunFailed("durable session image commit failed before provider execution")
		}
		return err
	}
	return nil
}

func workstreamPlanStageDefinition(plan *workstream.Plan, stageID string) (agent.TeamPlan, agent.TeamPlanStage, []string, error) {
	if plan == nil {
		return agent.TeamPlan{}, agent.TeamPlanStage{}, nil, fmt.Errorf("workstream plan is missing")
	}
	var definition agent.TeamPlan
	if err := json.Unmarshal(plan.Definition, &definition); err != nil {
		return agent.TeamPlan{}, agent.TeamPlanStage{}, nil, fmt.Errorf("stored workstream plan definition is invalid")
	}
	var err error
	definition, err = normalizeWorkstreamPlanDefinition(definition)
	if err != nil || agent.ValidateTeamPlan(definition) != nil {
		return agent.TeamPlan{}, agent.TeamPlanStage{}, nil, fmt.Errorf("stored workstream plan definition is invalid")
	}
	var selected *agent.TeamPlanStage
	for index := range definition.Stages {
		if definition.Stages[index].ID == stageID {
			selected = &definition.Stages[index]
			break
		}
	}
	if selected == nil {
		return agent.TeamPlan{}, agent.TeamPlanStage{}, nil, fmt.Errorf("plan stage does not belong to plan")
	}
	criteria := make([]string, 0)
	for _, task := range definition.Tasks {
		if task.Stage != stageID {
			continue
		}
		criteria = append(criteria, task.AcceptanceCriteria...)
	}
	criteria = normalizedPlanCriteria(criteria)
	if len(criteria) == 0 {
		criteria = []string{fmt.Sprintf("Complete all tasks assigned to the %q stage.", selected.Name)}
	}
	return definition, *selected, criteria, nil
}

func normalizedPlanCriteria(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.Join(strings.Fields(item), " ")
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result
}

func planCriteriaDigest(criteria []string) string {
	encoded, _ := json.Marshal(criteria)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func planAnchorForStage(plan *workstream.Plan, stageID string) (*agent.PlanAnchor, string, error) {
	definition, stage, criteria, err := workstreamPlanStageDefinition(plan, stageID)
	if err != nil {
		return nil, "", err
	}
	currentStep := strings.TrimSpace(stage.Name)
	if currentStep == "" {
		currentStep = stage.ID
	}
	remaining := make([]string, 0, len(definition.Stages))
	foundCurrent := false
	for _, candidate := range definition.Stages {
		if candidate.ID == stageID {
			foundCurrent = true
			continue
		}
		if foundCurrent {
			name := strings.TrimSpace(candidate.Name)
			if name == "" {
				name = candidate.ID
			}
			remaining = append(remaining, name)
		}
	}
	if uint64(int(plan.Revision)) != plan.Revision {
		return nil, "", fmt.Errorf("plan revision cannot be represented by the completion contract")
	}
	anchor, err := agent.NewPlanAnchor(agent.PlanAnchorSpec{
		Objective:        definition.Objective,
		CurrentStep:      currentStep,
		RemainingSteps:   remaining,
		DefinitionOfDone: criteria,
		Revision:         int(plan.Revision),
	})
	if err != nil {
		return nil, "", err
	}
	return &anchor, planCriteriaDigest(anchor.DefinitionOfDone()), nil
}

func teamPlanForStage(definition agent.TeamPlan, stageID string) (agent.TeamPlan, error) {
	stageIndex := -1
	for index, stage := range definition.Stages {
		if stage.ID == stageID {
			stageIndex = index
			break
		}
	}
	if stageIndex < 0 {
		return agent.TeamPlan{}, fmt.Errorf("plan stage does not belong to plan")
	}
	selectedIDs := make(map[string]struct{}, len(definition.Stages[stageIndex].TaskIDs))
	for _, taskID := range definition.Stages[stageIndex].TaskIDs {
		selectedIDs[taskID] = struct{}{}
	}
	selected := definition
	selected.Stages = []agent.TeamPlanStage{{
		ID: stageID, Name: definition.Stages[stageIndex].Name,
		Kind: definition.Stages[stageIndex].Kind,
	}}
	selected.Tasks = make([]agent.AgentTask, 0, len(selectedIDs))
	for _, task := range definition.Tasks {
		if _, ok := selectedIDs[task.ID]; !ok {
			continue
		}
		copyTask := task
		copyTask.Stage = stageID
		dependencies := make([]string, 0, len(task.DependsOn))
		for _, dependency := range task.DependsOn {
			if _, included := selectedIDs[dependency]; included {
				dependencies = append(dependencies, dependency)
				continue
			}
			dependencyStageIndex := -1
			for _, candidate := range definition.Tasks {
				if candidate.ID == dependency {
					dependencyStageIndex = -1
					for planStageIndex, candidateStage := range definition.Stages {
						if candidateStage.ID == candidate.Stage {
							dependencyStageIndex = planStageIndex
							break
						}
					}
					break
				}
			}
			if dependencyStageIndex < 0 || dependencyStageIndex >= stageIndex {
				return agent.TeamPlan{}, fmt.Errorf("plan task dependency does not precede the selected stage")
			}
			// Earlier stages have already been accepted before this stage can start.
		}
		copyTask.DependsOn = dependencies
		selected.Tasks = append(selected.Tasks, copyTask)
	}
	selected.Stages[0].TaskIDs = make([]string, 0, len(selected.Tasks))
	for _, task := range selected.Tasks {
		selected.Stages[0].TaskIDs = append(selected.Stages[0].TaskIDs, task.ID)
	}
	selected = selected.Normalized()
	if err := agent.ValidateTeamPlan(selected); err != nil {
		return agent.TeamPlan{}, fmt.Errorf("selected plan stage is invalid")
	}
	return selected, nil
}

func workstreamPlanStageTask(definition agent.TeamPlan, stage agent.TeamPlanStage) string {
	var builder strings.Builder
	objective := strings.TrimSpace(definition.Objective)
	name := strings.TrimSpace(stage.Name)
	if name == "" {
		name = stage.ID
	}
	builder.WriteString("Execute the approved Workstream Plan stage.\nObjective: ")
	builder.WriteString(objective)
	builder.WriteString("\nStage: ")
	builder.WriteString(name)
	builder.WriteString("\nStage tasks:\n")
	for _, task := range definition.Tasks {
		if task.Stage != stage.ID {
			continue
		}
		description := strings.TrimSpace(task.Goal)
		if description == "" {
			description = strings.TrimSpace(task.Description)
		}
		if description == "" {
			description = strings.TrimSpace(task.Name)
		}
		line := "- " + strings.TrimSpace(task.ID) + ": " + description + "\n"
		if builder.Len()+len(line) > 16<<10 {
			break
		}
		builder.WriteString(line)
	}
	return strings.TrimSpace(builder.String())
}

func isPlanModeRequest(messages []types.Message) bool {
	if len(messages) == 0 {
		return false
	}
	var last string
	if json.Unmarshal(messages[len(messages)-1].Content, &last) != nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(last)), "/plan")
}

type workstreamPlanStageRecorder struct {
	store               *workstream.Store
	workstreamID        string
	planID              string
	stageID             string
	revision            uint64
	stateRevision       uint64
	runID               string
	criteriaDigest      string
	mu                  sync.Mutex
	receiptDigest       string
	receiptVerification string
	receiptCompletion   string
	receiptCriteria     string
	finished            bool
}

func (r *workstreamPlanStageRecorder) RunStarted() {}

func (r *workstreamPlanStageRecorder) ReceiptWritten(path string, receipt agent.AgentReceipt) {
	if r == nil || r.store == nil || !samePlanExecutionBinding(receipt.PlanBinding, r.binding()) {
		return
	}
	digest, err := planReceiptFileDigest(path)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.receiptDigest = digest
	r.receiptVerification = receipt.Verification.Status
	if receipt.Completion != nil {
		r.receiptCompletion = string(receipt.Completion.Status)
		criteria := make([]string, 0, len(receipt.Completion.Criteria))
		for _, criterion := range receipt.Completion.Criteria {
			criteria = append(criteria, criterion.Text)
		}
		r.receiptCriteria = planCriteriaDigest(normalizedPlanCriteria(criteria))
	}
	r.mu.Unlock()
}

func (r *workstreamPlanStageRecorder) RunCompleted(summary agent.RunSummary) {
	if r == nil {
		return
	}
	r.mu.Lock()
	receiptDigest := r.receiptDigest
	receiptVerification := r.receiptVerification
	receiptCompletion := r.receiptCompletion
	receiptCriteria := r.receiptCriteria
	r.mu.Unlock()
	completionStatus := "incomplete"
	if summary.Completion != nil && summary.Completion.Status == agent.CompletionStatusComplete &&
		summary.Completion.PlanRevision == int(r.revision) &&
		receiptCompletion == string(agent.CompletionStatusComplete) &&
		receiptCriteria == r.criteriaDigest && receiptVerification == "passed" &&
		summary.Verification.Status == "passed" && summary.ReceiptPath != "" {
		completionStatus = "complete"
	}
	verification := summary.Verification.Status
	if receiptVerification != "" && receiptVerification != verification {
		completionStatus = "incomplete"
	}
	if completionStatus == "complete" {
		r.finish(workstream.PlanStageStatusCompleted, "passed", receiptDigest, "complete")
		return
	}
	if verification != "passed" && verification != "failed" {
		verification = "not-run"
	}
	r.finish(workstream.PlanStageStatusFailed, verification, receiptDigest, "incomplete")
}

func (r *workstreamPlanStageRecorder) RunFailed(_ string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	verification := r.receiptVerification
	receiptDigest := r.receiptDigest
	r.mu.Unlock()
	if verification != "passed" && verification != "failed" {
		verification = "not-run"
	}
	r.finish(workstream.PlanStageStatusFailed, verification, receiptDigest, "incomplete")
}

func (r *workstreamPlanStageRecorder) binding() *agent.PlanExecutionBinding {
	return &agent.PlanExecutionBinding{
		WorkstreamID:      r.workstreamID,
		PlanID:            r.planID,
		PlanRevision:      r.revision,
		PlanStateRevision: r.stateRevision,
		StageID:           r.stageID,
		RunID:             r.runID,
	}
}

func samePlanExecutionBinding(left, right *agent.PlanExecutionBinding) bool {
	return left != nil && right != nil && *left == *right
}

func activeWorkstreamPlanRunKey(workDir, workstreamID, planID, runID string) string {
	workspace := workstream.NewStore(workDir).Workspace()
	return strings.Join([]string{workspace, workstreamID, planID, runID}, "\x00")
}

func (r *workstreamPlanStageRecorder) finish(status, verification, receiptDigest, completion string) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.mu.Unlock()
	if r.store == nil {
		return
	}
	_, err := r.store.FinishPlanStage(r.workstreamID, r.planID, workstream.FinishPlanStageRequest{
		ExpectedRevision: r.revision, ExpectedStateRevision: r.stateRevision,
		StageID: r.stageID, RunID: r.runID, Status: status,
		VerificationStatus: verification, ReceiptDigest: receiptDigest,
		CriteriaDigest: r.criteriaDigest, EvidenceSource: "agent-receipt", CompletionStatus: completion,
	})
	if err != nil {
		log.Printf("[workstream] record plan stage result failed: %v", err)
	}
}

func finishServerPlanStageFromTeam(
	store *workstream.Store,
	binding *agent.PlanExecutionBinding,
	stateRevision uint64,
	criteriaDigest string,
	receipt agent.TeamRunReceipt,
	receiptPath string,
) {
	if store == nil || binding == nil || !samePlanExecutionBinding(receipt.PlanBinding, binding) {
		return
	}
	verification := receipt.Verification.Status
	if verification != "passed" && verification != "failed" {
		verification = "not-run"
	}
	receiptDigest, _ := planReceiptFileDigest(receiptPath)
	completeTasks := receipt.Status == "completed" && receipt.TaskCount > 0 &&
		receipt.Completed == receipt.TaskCount && receipt.Failed == 0 &&
		len(receipt.Tasks) == receipt.TaskCount
	if completeTasks {
		for _, task := range receipt.Tasks {
			if task.Status != agent.AgentTaskCompleted {
				completeTasks = false
				break
			}
		}
	}
	status := workstream.PlanStageStatusFailed
	completion := "incomplete"
	if completeTasks && verification == "passed" && receiptDigest != "" {
		status = workstream.PlanStageStatusCompleted
		completion = "complete"
	}
	_, err := store.FinishPlanStage(binding.WorkstreamID, binding.PlanID, workstream.FinishPlanStageRequest{
		ExpectedRevision: binding.PlanRevision, ExpectedStateRevision: stateRevision,
		StageID: binding.StageID, RunID: binding.RunID, Status: status,
		VerificationStatus: verification, ReceiptDigest: receiptDigest,
		CriteriaDigest: criteriaDigest, EvidenceSource: "team-receipt", CompletionStatus: completion,
	})
	if err != nil {
		log.Printf("[workstream] record Team plan stage result failed: %v", err)
	}
}

func planReceiptFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 2<<20 {
		return "", fmt.Errorf("receipt is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return "", fmt.Errorf("receipt is unavailable")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
