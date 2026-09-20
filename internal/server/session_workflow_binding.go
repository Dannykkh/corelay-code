package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

var (
	errSessionWorkflowWorkstreamNotFound = errors.New("session workflow workstream not found")
	errSessionWorkflowPlanRevisionNeeded = errors.New("session workflow plan revision required")
	errSessionWorkflowPlanRevisionStale  = errors.New("session workflow plan revision stale")
	errSessionWorkflowStageNotFound      = errors.New("session workflow stage not found")
	errSessionWorkflowPlanApprovalNeeded = errors.New("session workflow plan approval required")
	errSessionWorkflowStageRequired      = errors.New("session workflow execution requires a stage")
	errSessionWorkflowStageUnavailable   = errors.New("session workflow stage is not eligible for execution")
)

type sessionWorkflowBinding struct {
	Workstream *workstream.Workstream
	Plan       *workstream.Plan
}

func validateSessionPlanExecution(sess *agent.Session, binding *sessionWorkflowBinding) error {
	if sess == nil || binding == nil || binding.Plan == nil {
		return nil
	}
	plan := binding.Plan
	if plan.ApprovedRevision != plan.Revision ||
		(plan.Status != workstream.PlanStatusApproved && plan.Status != workstream.PlanStatusExecuting && plan.Status != workstream.PlanStatusFailed) {
		return errSessionWorkflowPlanApprovalNeeded
	}
	if sess.StageID == "" {
		return errSessionWorkflowStageRequired
	}
	if planHasRunningStage(plan.Stages) {
		return errSessionWorkflowStageUnavailable
	}
	for index, stage := range plan.Stages {
		if stage.ID != sess.StageID {
			continue
		}
		if stage.Status != workstream.PlanStageStatusPending && stage.Status != workstream.PlanStageStatusFailed {
			return errSessionWorkflowStageUnavailable
		}
		for previous := 0; previous < index; previous++ {
			if plan.Stages[previous].Status != workstream.PlanStageStatusCompleted {
				return errSessionWorkflowStageUnavailable
			}
		}
		return nil
	}
	return errSessionWorkflowStageNotFound
}

func planHasRunningStage(stages []workstream.PlanStage) bool {
	for _, stage := range stages {
		if stage.Status == workstream.PlanStageStatusRunning {
			return true
		}
	}
	return false
}

// resolveSessionWorkflowBinding verifies the full workspace → Workstream →
// Plan revision → Stage chain. A caller must not infer or upgrade a stale
// revision while loading a durable session.
func resolveSessionWorkflowBinding(sess *agent.Session) (*sessionWorkflowBinding, error) {
	if sess == nil {
		return nil, fmt.Errorf("%w: session is missing", agent.ErrSessionAssociationInvalid)
	}
	if err := agent.ValidateSessionAssociations(*sess); err != nil {
		return nil, err
	}
	if sess.PlanID != "" && sess.WorkstreamID == "" {
		return nil, fmt.Errorf("%w: plan requires workstream", agent.ErrSessionAssociationInvalid)
	}
	binding := &sessionWorkflowBinding{}
	if sess.WorkstreamID == "" {
		return binding, nil
	}
	store := workstream.NewStore(sess.Workspace)
	loadedWorkstream, err := store.Get(sess.WorkstreamID)
	if err != nil {
		return nil, errSessionWorkflowWorkstreamNotFound
	}
	binding.Workstream = loadedWorkstream
	if sess.PlanID == "" {
		return binding, nil
	}
	if sess.PlanRevision == 0 {
		return nil, errSessionWorkflowPlanRevisionNeeded
	}
	plan, err := store.GetPlan(sess.WorkstreamID, sess.PlanID)
	if err != nil {
		return nil, err
	}
	if plan.Revision != sess.PlanRevision {
		return nil, errSessionWorkflowPlanRevisionStale
	}
	if sess.StageID != "" {
		found := false
		for _, stage := range plan.Stages {
			if stage.ID == sess.StageID {
				found = true
				break
			}
		}
		if !found {
			return nil, errSessionWorkflowStageNotFound
		}
	}
	binding.Plan = plan
	return binding, nil
}

func writeSessionWorkflowBindingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSessionWorkflowWorkstreamNotFound):
		writeSessionAPIError(w, http.StatusNotFound, "workstream_not_found", "workstream not found", nil, nil)
	case errors.Is(err, workstream.ErrPlanNotFound):
		writeSessionAPIError(w, http.StatusNotFound, "plan_not_found", "plan not found", nil, nil)
	case errors.Is(err, errSessionWorkflowStageNotFound):
		writeSessionAPIError(w, http.StatusNotFound, "plan_stage_not_found", "plan stage not found", nil, nil)
	case errors.Is(err, errSessionWorkflowPlanRevisionNeeded), errors.Is(err, agent.ErrSessionAssociationInvalid):
		writeSessionAPIError(w, http.StatusBadRequest, "invalid_session_association", "session plan association is incomplete", nil, nil)
	case errors.Is(err, errSessionWorkflowPlanRevisionStale):
		writeSessionAPIError(w, http.StatusConflict, "plan_revision_conflict", "session refers to a stale plan revision", nil, nil)
	case errors.Is(err, errSessionWorkflowPlanApprovalNeeded):
		writeSessionAPIError(w, http.StatusConflict, "plan_approval_required", "the current plan revision must be approved before execution", nil, nil)
	case errors.Is(err, errSessionWorkflowStageRequired):
		writeSessionAPIError(w, http.StatusConflict, "plan_stage_required", "a plan-bound execution must select one stage", nil, nil)
	case errors.Is(err, errSessionWorkflowStageUnavailable):
		writeSessionAPIError(w, http.StatusConflict, "plan_stage_unavailable", "the selected plan stage is already running or not ready", nil, nil)
	case errors.Is(err, workstream.ErrPlanInvalid), errors.Is(err, workstream.ErrPlanCorrupt):
		writeSessionAPIError(w, http.StatusUnprocessableEntity, "plan_invalid", "stored plan requires recovery", nil, nil)
	default:
		writeSessionAPIError(w, http.StatusInternalServerError, "workflow_reference_failed", "session workflow reference could not be verified", nil, nil)
	}
}
