package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

type workstreamPlanCreateRequest struct {
	WorkDir    string          `json:"workDir"`
	ID         string          `json:"id,omitempty"`
	Definition json.RawMessage `json:"definition"`
}

type workstreamPlanUpdateRequest struct {
	WorkDir               string          `json:"workDir"`
	ExpectedRevision      uint64          `json:"expectedRevision"`
	ExpectedStateRevision uint64          `json:"expectedStateRevision"`
	Definition            json.RawMessage `json:"definition"`
}

type workstreamPlanApproveRequest struct {
	WorkDir               string `json:"workDir"`
	ExpectedRevision      uint64 `json:"expectedRevision"`
	ExpectedStateRevision uint64 `json:"expectedStateRevision"`
}

type workstreamPlanStageReconcileRequest struct {
	WorkDir               string `json:"workDir"`
	ExpectedRevision      uint64 `json:"expectedRevision"`
	ExpectedStateRevision uint64 `json:"expectedStateRevision"`
	RunID                 string `json:"runId"`
	ManualAcknowledged    bool   `json:"manualAcknowledged"`
}

func (s *Server) handleWorkstreamPlanCreate(w http.ResponseWriter, r *http.Request) {
	var body workstreamPlanCreateRequest
	if !decodeSessionRequest(w, r, &body) {
		return
	}
	workDir, err := s.requestProjectWorkspace(r, body.WorkDir)
	if err != nil {
		writeProjectWorkspaceScopeError(w, err)
		return
	}
	store := workstream.NewStore(workDir)
	workstreamID := r.PathValue("id")
	if _, err := store.Get(workstreamID); err != nil {
		writeWorkstreamNotFound(w)
		return
	}
	var definition agent.TeamPlan
	if err := json.Unmarshal(body.Definition, &definition); err != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition is invalid", nil, nil)
		return
	}
	definition, err = normalizeWorkstreamPlanDefinition(definition)
	if err != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition failed validation", nil, nil)
		return
	}
	if err := agent.ValidateTeamPlan(definition); err != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition failed validation", nil, nil)
		return
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition could not be encoded", nil, nil)
		return
	}
	plan, err := store.CreatePlan(workstreamID, workstream.CreatePlanRequest{ID: body.ID, Definition: encoded})
	if err != nil {
		writeWorkstreamPlanError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "plan": plan})
}

func (s *Server) handleWorkstreamPlanUpdate(w http.ResponseWriter, r *http.Request) {
	var body workstreamPlanUpdateRequest
	if !decodeSessionRequest(w, r, &body) {
		return
	}
	workDir, err := s.requestProjectWorkspace(r, body.WorkDir)
	if err != nil {
		writeProjectWorkspaceScopeError(w, err)
		return
	}
	store := workstream.NewStore(workDir)
	workstreamID := r.PathValue("id")
	if _, err := store.Get(workstreamID); err != nil {
		writeWorkstreamNotFound(w)
		return
	}
	var definition agent.TeamPlan
	if err := json.Unmarshal(body.Definition, &definition); err != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition is invalid", nil, nil)
		return
	}
	definition, err = normalizeWorkstreamPlanDefinition(definition)
	if err != nil || agent.ValidateTeamPlan(definition) != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition failed validation", nil, nil)
		return
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan definition could not be encoded", nil, nil)
		return
	}
	plan, err := store.UpdatePlan(workstreamID, r.PathValue("planId"), workstream.UpdatePlanRequest{
		ExpectedRevision: body.ExpectedRevision, ExpectedStateRevision: body.ExpectedStateRevision,
		Definition: encoded,
	})
	if err != nil {
		writeWorkstreamPlanError(w, err)
		return
	}
	writeJSON(w, map[string]any{"plan": plan})
}

func (s *Server) handleWorkstreamPlanApprove(w http.ResponseWriter, r *http.Request) {
	var body workstreamPlanApproveRequest
	if !decodeSessionRequest(w, r, &body) {
		return
	}
	workDir, err := s.requestProjectWorkspace(r, body.WorkDir)
	if err != nil {
		writeProjectWorkspaceScopeError(w, err)
		return
	}
	store := workstream.NewStore(workDir)
	workstreamID := r.PathValue("id")
	if _, err := store.Get(workstreamID); err != nil {
		writeWorkstreamNotFound(w)
		return
	}
	plan, err := store.ApprovePlan(workstreamID, r.PathValue("planId"), workstream.ApprovePlanRequest{
		ExpectedRevision: body.ExpectedRevision, ExpectedStateRevision: body.ExpectedStateRevision,
	})
	if err != nil {
		writeWorkstreamPlanError(w, err)
		return
	}
	writeJSON(w, map[string]any{"plan": plan})
}

func (s *Server) handleWorkstreamPlanStageReconcile(w http.ResponseWriter, r *http.Request) {
	var body workstreamPlanStageReconcileRequest
	if !decodeSessionRequest(w, r, &body) {
		return
	}
	workDir, err := s.requestProjectWorkspace(r, body.WorkDir)
	if err != nil {
		writeProjectWorkspaceScopeError(w, err)
		return
	}
	workstreamID := r.PathValue("id")
	planID := r.PathValue("planId")
	stageID := r.PathValue("stageId")
	if _, err := workstream.NewStore(workDir).Get(workstreamID); err != nil {
		writeWorkstreamNotFound(w)
		return
	}
	key := activeWorkstreamPlanRunKey(workDir, workstreamID, planID, body.RunID)
	if _, active := s.activePlanRuns.Load(key); active {
		writeWorkstreamPlanError(w, workstream.ErrPlanStageActive)
		return
	}
	plan, err := workstream.NewStore(workDir).ReconcilePlanStage(workstreamID, planID, workstream.ReconcilePlanStageRequest{
		ExpectedRevision: body.ExpectedRevision, ExpectedStateRevision: body.ExpectedStateRevision,
		StageID: stageID, RunID: body.RunID, ManualAcknowledged: body.ManualAcknowledged,
	})
	if err != nil {
		writeWorkstreamPlanError(w, err)
		return
	}
	writeJSON(w, map[string]any{"plan": plan})
}

func (s *Server) handleWorkstreamPlanList(w http.ResponseWriter, r *http.Request) {
	workDir, err := s.requestProjectWorkspace(r, "")
	if err != nil {
		writeProjectWorkspaceScopeError(w, err)
		return
	}
	store := workstream.NewStore(workDir)
	workstreamID := r.PathValue("id")
	if _, err := store.Get(workstreamID); err != nil {
		writeWorkstreamNotFound(w)
		return
	}
	plans, err := store.ListPlans(workstreamID)
	if err != nil {
		writeWorkstreamPlanError(w, err)
		return
	}
	writeJSON(w, map[string]any{"workstreamId": workstreamID, "plans": plans})
}

func (s *Server) handleWorkstreamPlanGet(w http.ResponseWriter, r *http.Request) {
	workDir, err := s.requestProjectWorkspace(r, "")
	if err != nil {
		writeProjectWorkspaceScopeError(w, err)
		return
	}
	store := workstream.NewStore(workDir)
	workstreamID := r.PathValue("id")
	if _, err := store.Get(workstreamID); err != nil {
		writeWorkstreamNotFound(w)
		return
	}
	plan, err := store.GetPlan(workstreamID, r.PathValue("planId"))
	if err != nil {
		writeWorkstreamPlanError(w, err)
		return
	}
	writeJSON(w, map[string]any{"plan": plan})
}

func writeWorkstreamPlanError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workstream.ErrPlanNotFound):
		writeSessionAPIError(w, http.StatusNotFound, "plan_not_found", "plan not found", nil, nil)
	case errors.Is(err, workstream.ErrPlanCorrupt):
		writeSessionAPIError(w, http.StatusUnprocessableEntity, "plan_corrupt", "stored plan requires recovery", nil, nil)
	case errors.Is(err, workstream.ErrPlanInvalid):
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_plan", "plan identifier or stored plan is invalid", nil, nil)
	case errors.Is(err, workstream.ErrPlanRevisionConflict):
		writeSessionAPIError(w, http.StatusConflict, "plan_revision_conflict", "plan revision or state changed", nil, nil)
	case errors.Is(err, workstream.ErrPlanConflict):
		writeSessionAPIError(w, http.StatusConflict, "plan_conflict", "plan already exists", nil, nil)
	case errors.Is(err, workstream.ErrPlanTransition):
		writeSessionAPIError(w, http.StatusConflict, "plan_transition_conflict", "plan state does not allow this transition", nil, nil)
	case errors.Is(err, workstream.ErrPlanEvidence):
		writeSessionAPIError(w, http.StatusUnprocessableEntity, "plan_evidence_invalid", "plan transition requires verified acceptance evidence", nil, nil)
	case errors.Is(err, workstream.ErrPlanStageActive):
		writeSessionAPIError(w, http.StatusConflict, "plan_stage_active", "the Plan stage still has an active same-process execution", nil, nil)
	default:
		writeError(w, http.StatusInternalServerError, "workstream plan operation failed")
	}
}

func normalizeWorkstreamPlanDefinition(plan agent.TeamPlan) (agent.TeamPlan, error) {
	plan = plan.Normalized()
	if len(plan.Tasks) == 0 {
		return agent.TeamPlan{}, errors.New("workstream: plan has no tasks")
	}
	if len(plan.Stages) == 0 {
		stage := agent.TeamPlanStage{ID: "execute", Name: "Execution", TaskIDs: make([]string, 0, len(plan.Tasks))}
		for index := range plan.Tasks {
			plan.Tasks[index].Stage = stage.ID
			stage.TaskIDs = append(stage.TaskIDs, plan.Tasks[index].ID)
		}
		plan.Stages = []agent.TeamPlanStage{stage}
	}

	stageByID := make(map[string]int, len(plan.Stages))
	for index, stage := range plan.Stages {
		if stage.ID == "" {
			return agent.TeamPlan{}, errors.New("workstream: plan stage id is required")
		}
		if _, exists := stageByID[stage.ID]; exists {
			return agent.TeamPlan{}, errors.New("workstream: duplicate plan stage")
		}
		stageByID[stage.ID] = index
	}
	taskByID := make(map[string]int, len(plan.Tasks))
	for index, task := range plan.Tasks {
		if task.ID == "" {
			return agent.TeamPlan{}, errors.New("workstream: plan task id is required")
		}
		if _, exists := taskByID[task.ID]; exists {
			return agent.TeamPlan{}, errors.New("workstream: duplicate plan task")
		}
		taskByID[task.ID] = index
	}
	memberStage := make(map[string]string, len(plan.Tasks))
	for _, stage := range plan.Stages {
		for _, taskID := range stage.TaskIDs {
			if _, exists := taskByID[taskID]; !exists {
				return agent.TeamPlan{}, errors.New("workstream: stage references unknown task")
			}
			if previous := memberStage[taskID]; previous != "" && previous != stage.ID {
				return agent.TeamPlan{}, errors.New("workstream: task belongs to multiple stages")
			}
			memberStage[taskID] = stage.ID
		}
	}
	for index := range plan.Tasks {
		task := &plan.Tasks[index]
		if task.Stage != "" {
			if _, exists := stageByID[task.Stage]; !exists {
				return agent.TeamPlan{}, errors.New("workstream: task references unknown stage")
			}
			if previous := memberStage[task.ID]; previous != "" && previous != task.Stage {
				return agent.TeamPlan{}, errors.New("workstream: task stage assignment conflicts")
			}
			memberStage[task.ID] = task.Stage
		} else if memberStage[task.ID] == "" {
			if len(plan.Stages) != 1 {
				return agent.TeamPlan{}, errors.New("workstream: each task must belong to one stage")
			}
			memberStage[task.ID] = plan.Stages[0].ID
			task.Stage = plan.Stages[0].ID
		} else {
			task.Stage = memberStage[task.ID]
		}
	}
	for index := range plan.Stages {
		stage := &plan.Stages[index]
		stage.TaskIDs = stage.TaskIDs[:0]
		for _, task := range plan.Tasks {
			if memberStage[task.ID] == stage.ID {
				stage.TaskIDs = append(stage.TaskIDs, task.ID)
			}
		}
		if len(stage.TaskIDs) == 0 {
			return agent.TeamPlan{}, errors.New("workstream: every plan stage must include a task")
		}
	}
	return plan.Normalized(), nil
}
