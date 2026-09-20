package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func TestWorkstreamAPI_CreateListGetHandoff(t *testing.T) {
	workDir := t.TempDir()
	s := New(nil, "", 0)
	s.SetWorkDir(workDir)

	body := []byte(`{
		"id":"ws_api",
		"title":"API Workstream",
		"summary":"server api test",
		"nextAction":"generate handoff",
		"goal":{"objective":"prove api works","acceptanceCriteria":["created","handoff"]}
	}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/workstreams", bytes.NewReader(body))
	createRec := httptest.NewRecorder()
	s.handleWorkstreamCreate(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/workstreams", nil)
	listRec := httptest.NewRecorder()
	s.handleWorkstreamList(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed struct {
		Workstreams []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"workstreams"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(listed.Workstreams) != 1 || listed.Workstreams[0].ID != "ws_api" {
		t.Fatalf("listed workstreams = %+v", listed.Workstreams)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/workstreams/ws_api", nil)
	getReq.SetPathValue("id", "ws_api")
	getRec := httptest.NewRecorder()
	s.handleWorkstreamGet(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}

	plan := createWorkstreamPlanAPIForTest(t, s, workDir, "ws_api", "plan_api_handoff", "research")
	handoffBody, err := json.Marshal(map[string]any{"workDir": workDir, "planId": plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	handoffReq := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_api/handoff", bytes.NewReader(handoffBody))
	handoffReq.SetPathValue("id", "ws_api")
	handoffRec := httptest.NewRecorder()
	s.handleWorkstreamHandoff(handoffRec, handoffReq)
	if handoffRec.Code != http.StatusOK {
		t.Fatalf("handoff status=%d body=%s", handoffRec.Code, handoffRec.Body.String())
	}
	var handoff struct {
		Path     string `json:"path"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(handoffRec.Body.Bytes(), &handoff); err != nil {
		t.Fatalf("handoff json: %v", err)
	}
	if handoff.Path == "" || handoff.Markdown == "" ||
		!strings.Contains(handoff.Markdown, "Plan ID: "+plan.ID) ||
		!strings.Contains(handoff.Markdown, "Objective: validate durable workflow binding") {
		t.Fatalf("empty handoff response: %+v", handoff)
	}
	if _, err := os.Stat(handoff.Path); err != nil {
		t.Fatalf("handoff file missing: %v", err)
	}
}

func TestWorkstreamPatchStoresAndClearsDecisions(t *testing.T) {
	workspace := t.TempDir()
	if _, err := workstream.NewStore(workspace).Create(workstream.CreateRequest{ID: "ws_decisions_api", Title: "Decision API"}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", 0)
	s.SetWorkDir(workspace)
	patch := httptest.NewRequest(http.MethodPatch, "/api/workstreams/ws_decisions_api", strings.NewReader(`{"decisions":["Use Workstream as the canonical owner.","Retain evidence-bound completion."],"hasDecisions":true}`))
	patch.SetPathValue("id", "ws_decisions_api")
	patchRec := httptest.NewRecorder()
	s.handleWorkstreamPatch(patchRec, patch)
	var patched struct {
		Workstream workstream.Workstream `json:"workstream"`
	}
	if patchRec.Code != http.StatusOK || json.Unmarshal(patchRec.Body.Bytes(), &patched) != nil ||
		len(patched.Workstream.Decisions) != 2 || patched.Workstream.Decisions[0] != "Use Workstream as the canonical owner." {
		t.Fatalf("decision patch status=%d body=%s", patchRec.Code, patchRec.Body.String())
	}
	clear := httptest.NewRequest(http.MethodPatch, "/api/workstreams/ws_decisions_api", strings.NewReader(`{"hasDecisions":true,"decisions":[]}`))
	clear.SetPathValue("id", "ws_decisions_api")
	clearRec := httptest.NewRecorder()
	s.handleWorkstreamPatch(clearRec, clear)
	var cleared struct {
		Workstream workstream.Workstream `json:"workstream"`
	}
	if clearRec.Code != http.StatusOK || json.Unmarshal(clearRec.Body.Bytes(), &cleared) != nil || len(cleared.Workstream.Decisions) != 0 {
		t.Fatalf("decision clear status=%d body=%s", clearRec.Code, clearRec.Body.String())
	}
}

func createWorkstreamPlanAPIForTest(t *testing.T, s *Server, workspace, workstreamID, planID, stageID string) *workstream.Plan {
	t.Helper()
	definition, err := json.Marshal(agent.TeamPlan{
		Version:   1,
		Name:      planID,
		Objective: "validate durable workflow binding",
		Stages:    []agent.TeamPlanStage{{ID: stageID, Name: stageID}},
		Tasks: []agent.AgentTask{{
			ID: "task_" + stageID, Stage: stageID, Goal: "inspect the workflow binding", ReadOnly: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"workDir": workspace, "id": planID, "definition": json.RawMessage(definition),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workstreams/"+workstreamID+"/plans", bytes.NewReader(body))
	req.SetPathValue("id", workstreamID)
	rec := httptest.NewRecorder()
	s.handleWorkstreamPlanCreate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Plan *workstream.Plan `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Plan == nil {
		t.Fatalf("decode plan response=%s err=%v", rec.Body.String(), err)
	}
	return response.Plan
}

func TestWorkstreamPlanAPIAndScopedPlanCompatibilityRoutes(t *testing.T) {
	workspace := t.TempDir()
	store := workstream.NewStore(workspace)
	if _, err := store.Create(workstream.CreateRequest{ID: "ws_plan_api", Title: "Plan API"}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", 0)
	s.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_plan_api", "plan_api", "research")

	listReq := httptest.NewRequest(http.MethodGet, "/api/workstreams/ws_plan_api/plans", nil)
	listReq.SetPathValue("id", "ws_plan_api")
	listRec := httptest.NewRecorder()
	s.handleWorkstreamPlanList(listRec, listReq)
	var listed struct {
		Plans []workstream.Plan `json:"plans"`
	}
	if listRec.Code != http.StatusOK || json.Unmarshal(listRec.Body.Bytes(), &listed) != nil ||
		len(listed.Plans) != 1 || listed.Plans[0].ID != plan.ID {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/workstreams/ws_plan_api/plans/plan_api", nil)
	getReq.SetPathValue("id", "ws_plan_api")
	getReq.SetPathValue("planId", "plan_api")
	getRec := httptest.NewRecorder()
	s.handleWorkstreamPlanGet(getRec, getReq)
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), `"workstreamId":"ws_plan_api"`) {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}

	noScopeReq := httptest.NewRequest(http.MethodGet, "/api/plan", nil)
	noScopeRec := httptest.NewRecorder()
	s.handlePlanGet(noScopeRec, noScopeReq)
	if noScopeRec.Code != http.StatusOK || !strings.Contains(noScopeRec.Body.String(), `"scopeRequired":true`) {
		t.Fatalf("unscoped plan status=%d body=%s", noScopeRec.Code, noScopeRec.Body.String())
	}
	query := url.Values{"workstreamId": {"ws_plan_api"}, "planId": {plan.ID}, "workDir": {workspace}}
	scopedReq := httptest.NewRequest(http.MethodGet, "/api/plan?"+query.Encode(), nil)
	scopedRec := httptest.NewRecorder()
	s.handlePlanGet(scopedRec, scopedReq)
	if scopedRec.Code != http.StatusOK || !strings.Contains(scopedRec.Body.String(), `"id":"plan_api"`) {
		t.Fatalf("scoped plan status=%d body=%s", scopedRec.Code, scopedRec.Body.String())
	}

	approveReq := httptest.NewRequest(http.MethodPost, "/api/plan/approve", strings.NewReader(`{}`))
	approveRec := httptest.NewRecorder()
	s.handlePlanApprove(approveRec, approveReq)
	if approveRec.Code != http.StatusGone || !strings.Contains(approveRec.Body.String(), `"code":"global_plan_approval_removed"`) {
		t.Fatalf("legacy approval status=%d body=%s", approveRec.Code, approveRec.Body.String())
	}
}

func TestWorkstreamPlanAPIApprovalBindsRevisionAndUpdateRevokesIt(t *testing.T) {
	workspace := t.TempDir()
	if _, err := workstream.NewStore(workspace).Create(workstream.CreateRequest{ID: "ws_plan_lifecycle", Title: "Plan lifecycle"}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", 0)
	s.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_plan_lifecycle", "plan_lifecycle", "research")

	approveBody, err := json.Marshal(map[string]any{
		"workDir": workspace, "expectedRevision": plan.Revision, "expectedStateRevision": plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	approveReq := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_plan_lifecycle/plans/plan_lifecycle/approve", bytes.NewReader(approveBody))
	approveReq.SetPathValue("id", "ws_plan_lifecycle")
	approveReq.SetPathValue("planId", "plan_lifecycle")
	approveRec := httptest.NewRecorder()
	s.handleWorkstreamPlanApprove(approveRec, approveReq)
	var approvedResponse struct {
		Plan *workstream.Plan `json:"plan"`
	}
	if approveRec.Code != http.StatusOK || json.Unmarshal(approveRec.Body.Bytes(), &approvedResponse) != nil || approvedResponse.Plan == nil {
		t.Fatalf("approve status=%d body=%s", approveRec.Code, approveRec.Body.String())
	}
	approved := approvedResponse.Plan
	if approved.Status != workstream.PlanStatusApproved || approved.ApprovedRevision != plan.Revision ||
		approved.Revision != plan.Revision || approved.StateRevision != plan.StateRevision+1 {
		t.Fatalf("approval was not bound to the exact definition revision: %+v", approved)
	}

	var definition agent.TeamPlan
	if err := json.Unmarshal(approved.Definition, &definition); err != nil {
		t.Fatal(err)
	}
	definition.Objective = "revise the approved workflow definition"
	updatedDefinition, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	updateBody, err := json.Marshal(map[string]any{
		"workDir": workspace, "expectedRevision": approved.Revision,
		"expectedStateRevision": approved.StateRevision, "definition": json.RawMessage(updatedDefinition),
	})
	if err != nil {
		t.Fatal(err)
	}
	updateReq := httptest.NewRequest(http.MethodPut, "/api/workstreams/ws_plan_lifecycle/plans/plan_lifecycle", bytes.NewReader(updateBody))
	updateReq.SetPathValue("id", "ws_plan_lifecycle")
	updateReq.SetPathValue("planId", "plan_lifecycle")
	updateRec := httptest.NewRecorder()
	s.handleWorkstreamPlanUpdate(updateRec, updateReq)
	var updatedResponse struct {
		Plan *workstream.Plan `json:"plan"`
	}
	if updateRec.Code != http.StatusOK || json.Unmarshal(updateRec.Body.Bytes(), &updatedResponse) != nil || updatedResponse.Plan == nil {
		t.Fatalf("update status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}
	updated := updatedResponse.Plan
	if updated.Status != workstream.PlanStatusDraft || updated.Revision != approved.Revision+1 ||
		updated.StateRevision != approved.StateRevision+1 || updated.ApprovedRevision != 0 {
		t.Fatalf("definition update did not revoke the prior approval: %+v", updated)
	}

	staleBody, err := json.Marshal(map[string]any{
		"workDir": workspace, "expectedRevision": approved.Revision,
		"expectedStateRevision": approved.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleReq := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_plan_lifecycle/plans/plan_lifecycle/approve", bytes.NewReader(staleBody))
	staleReq.SetPathValue("id", "ws_plan_lifecycle")
	staleReq.SetPathValue("planId", "plan_lifecycle")
	staleRec := httptest.NewRecorder()
	s.handleWorkstreamPlanApprove(staleRec, staleReq)
	if staleRec.Code != http.StatusConflict || !strings.Contains(staleRec.Body.String(), `"code":"plan_revision_conflict"`) {
		t.Fatalf("stale approve status=%d body=%s", staleRec.Code, staleRec.Body.String())
	}
}

func TestWorkstreamPlanStageReconcileRequiresStoppedRunAcknowledgement(t *testing.T) {
	workspace := t.TempDir()
	store := workstream.NewStore(workspace)
	if _, err := store.Create(workstream.CreateRequest{ID: "ws_plan_reconcile_api", Title: "Plan reconcile API"}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", 0)
	s.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_plan_reconcile_api", "plan_reconcile_api", "research")
	plan, err := store.ApprovePlan("ws_plan_reconcile_api", plan.ID, workstream.ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.BeginPlanStage("ws_plan_reconcile_api", plan.ID, workstream.BeginPlanStageRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
		StageID: "research", RunID: "run_reconcile_api",
	})
	if err != nil {
		t.Fatal(err)
	}
	makeRequest := func(manualAcknowledged bool) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{
			"workDir": workspace, "expectedRevision": running.Revision,
			"expectedStateRevision": running.StateRevision, "runId": "run_reconcile_api",
			"manualAcknowledged": manualAcknowledged,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_plan_reconcile_api/plans/plan_reconcile_api/stages/research/reconcile", bytes.NewReader(body))
		req.SetPathValue("id", "ws_plan_reconcile_api")
		req.SetPathValue("planId", "plan_reconcile_api")
		req.SetPathValue("stageId", "research")
		rec := httptest.NewRecorder()
		s.handleWorkstreamPlanStageReconcile(rec, req)
		return rec
	}

	activeKey := activeWorkstreamPlanRunKey(workspace, "ws_plan_reconcile_api", plan.ID, "run_reconcile_api")
	s.activePlanRuns.Store(activeKey, struct{}{})
	active := makeRequest(true)
	s.activePlanRuns.Delete(activeKey)
	if active.Code != http.StatusConflict || !strings.Contains(active.Body.String(), `"code":"plan_stage_active"`) {
		t.Fatalf("live stage reconcile status=%d body=%s", active.Code, active.Body.String())
	}
	noAck := makeRequest(false)
	if noAck.Code != http.StatusConflict || !strings.Contains(noAck.Body.String(), `"code":"plan_transition_conflict"`) {
		t.Fatalf("unacknowledged reconcile status=%d body=%s", noAck.Code, noAck.Body.String())
	}
	acknowledged := makeRequest(true)
	var response struct {
		Plan *workstream.Plan `json:"plan"`
	}
	if acknowledged.Code != http.StatusOK || json.Unmarshal(acknowledged.Body.Bytes(), &response) != nil || response.Plan == nil {
		t.Fatalf("acknowledged reconcile status=%d body=%s", acknowledged.Code, acknowledged.Body.String())
	}
	if response.Plan.Status != workstream.PlanStatusFailed || response.Plan.Stages[0].Status != workstream.PlanStageStatusFailed ||
		response.Plan.Stages[0].Attempts[0].VerificationStatus != "not-run" || response.Plan.Stages[0].Attempts[0].Evidence != nil {
		t.Fatalf("reconcile response falsely records completion: %+v", response.Plan)
	}
}

func TestWorkstreamPlanAPIScopesWorkspaceBeforeReadingWorkflowData(t *testing.T) {
	defaultWorkspace := t.TempDir()
	unregisteredWorkspace := t.TempDir()
	configureProjectWorkspaces(t, defaultWorkspace)
	if _, err := workstream.NewStore(unregisteredWorkspace).Create(workstream.CreateRequest{ID: "ws_external", Title: "External"}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", 0)
	s.SetWorkDir(defaultWorkspace)

	definition, err := json.Marshal(agent.TeamPlan{
		Name: "External plan", Objective: "must remain outside the API scope",
		Stages: []agent.TeamPlanStage{{ID: "research", Name: "Research"}},
		Tasks:  []agent.AgentTask{{ID: "task_research", Stage: "research", Goal: "inspect", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	createBody, err := json.Marshal(map[string]any{
		"workDir": unregisteredWorkspace, "id": "plan_external", "definition": json.RawMessage(definition),
	})
	if err != nil {
		t.Fatal(err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_external/plans", bytes.NewReader(createBody))
	createReq.SetPathValue("id", "ws_external")
	createRec := httptest.NewRecorder()
	s.handleWorkstreamPlanCreate(createRec, createReq)
	if createRec.Code != http.StatusForbidden || !strings.Contains(createRec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("unregistered create status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	query := url.Values{"workDir": {unregisteredWorkspace}}
	listReq := httptest.NewRequest(http.MethodGet, "/api/workstreams/ws_external/plans?"+query.Encode(), nil)
	listReq.SetPathValue("id", "ws_external")
	listRec := httptest.NewRecorder()
	s.handleWorkstreamPlanList(listRec, listReq)
	if listRec.Code != http.StatusForbidden || !strings.Contains(listRec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("unregistered list status=%d body=%s", listRec.Code, listRec.Body.String())
	}

	query.Set("workstreamId", "ws_external")
	query.Set("planId", "plan_external")
	scopedReq := httptest.NewRequest(http.MethodGet, "/api/plan?"+query.Encode(), nil)
	scopedRec := httptest.NewRecorder()
	s.handlePlanGet(scopedRec, scopedReq)
	if scopedRec.Code != http.StatusForbidden || !strings.Contains(scopedRec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("unregistered scoped plan status=%d body=%s", scopedRec.Code, scopedRec.Body.String())
	}

	s.SetSessionStore(agent.NewSessionStore(t.TempDir()))
	body, _ := json.Marshal(map[string]any{
		"workspace": unregisteredWorkspace, "workstreamId": "ws_missing",
	})
	sessionRec := httptest.NewRecorder()
	s.handleSessionSave(sessionRec, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body)))
	if sessionRec.Code != http.StatusForbidden || !strings.Contains(sessionRec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("unregistered session workflow status=%d body=%s", sessionRec.Code, sessionRec.Body.String())
	}
}

func TestWorkstreamPlanAPIReportsCorruptStoredPlanAsRecoveryRequired(t *testing.T) {
	workspace := t.TempDir()
	store := workstream.NewStore(workspace)
	if _, err := store.Create(workstream.CreateRequest{ID: "ws_corrupt_plan", Title: "Corrupt plan"}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "", 0)
	s.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_corrupt_plan", "plan_corrupt", "research")
	if err := os.WriteFile(workstream.PlanPath(workspace, "ws_corrupt_plan", plan.ID), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/workstreams/ws_corrupt_plan/plans/plan_corrupt", nil)
	req.SetPathValue("id", "ws_corrupt_plan")
	req.SetPathValue("planId", "plan_corrupt")
	rec := httptest.NewRecorder()
	s.handleWorkstreamPlanGet(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"code":"plan_corrupt"`) {
		t.Fatalf("corrupt stored plan status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionAPIEnforcesRevisionedWorkstreamPlanBinding(t *testing.T) {
	workspace := t.TempDir()
	workstreams := workstream.NewStore(workspace)
	for _, id := range []string{"ws_plan_bound", "ws_plan_other"} {
		if _, err := workstreams.Create(workstream.CreateRequest{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(&agentLoopFakeProvider{}, "default", 0)
	s.SetWorkDir(workspace)
	sessionStore := agent.NewSessionStore(t.TempDir())
	s.SetSessionStore(sessionStore)
	plan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_plan_bound", "plan_bound", "research")
	otherPlan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_plan_bound", "plan_other", "implement")

	createBody, err := json.Marshal(map[string]any{
		"workspace": workspace, "workstreamId": "ws_plan_bound", "planId": plan.ID,
		"planRevision": plan.Revision, "stageId": "research",
		"messages": []map[string]string{{"role": "user", "content": "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", string(createBody))
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create bound session status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created session=%s err=%v", rec.Body.String(), err)
	}

	partial, partialRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":1,"title":"partial"}`)
	s.handleSessionSave(partialRec, partial)
	if partialRec.Code != http.StatusOK {
		t.Fatalf("partial update status=%d body=%s", partialRec.Code, partialRec.Body.String())
	}
	persisted, err := sessionStore.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.PlanID != plan.ID || persisted.PlanRevision != plan.Revision || persisted.StageID != "research" {
		t.Fatalf("partial update lost plan binding: %#v", persisted)
	}
	getBound, getBoundRec := sessionRequest(t, http.MethodGet, "/api/sessions/"+created.ID, "", "")
	getBound.SetPathValue("id", created.ID)
	s.handleSessionGet(getBoundRec, getBound)
	var resumed agent.Session
	if getBoundRec.Code != http.StatusOK || json.Unmarshal(getBoundRec.Body.Bytes(), &resumed) != nil ||
		resumed.WorkstreamID != "ws_plan_bound" || resumed.PlanID != plan.ID || resumed.PlanRevision != plan.Revision || resumed.StageID != "research" {
		t.Fatalf("resumed session lost exact workflow binding: status=%d body=%s", getBoundRec.Code, getBoundRec.Body.String())
	}

	stale, staleRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":2,"planId":"`+plan.ID+`","planRevision":2}`)
	s.handleSessionSave(staleRec, stale)
	if staleRec.Code != http.StatusConflict || !strings.Contains(staleRec.Body.String(), `"code":"plan_revision_conflict"`) {
		t.Fatalf("stale plan association status=%d body=%s", staleRec.Code, staleRec.Body.String())
	}
	wrongStage, wrongStageRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":2,"stageId":"missing"}`)
	s.handleSessionSave(wrongStageRec, wrongStage)
	if wrongStageRec.Code != http.StatusNotFound || !strings.Contains(wrongStageRec.Body.String(), `"code":"plan_stage_not_found"`) {
		t.Fatalf("wrong stage association status=%d body=%s", wrongStageRec.Code, wrongStageRec.Body.String())
	}

	switchPlan, switchPlanRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":2,"planId":"`+otherPlan.ID+`","planRevision":1}`)
	s.handleSessionSave(switchPlanRec, switchPlan)
	if switchPlanRec.Code != http.StatusOK {
		t.Fatalf("switch plan status=%d body=%s", switchPlanRec.Code, switchPlanRec.Body.String())
	}
	persisted, err = sessionStore.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.PlanID != otherPlan.ID || persisted.PlanRevision != otherPlan.Revision || persisted.StageID != "" {
		t.Fatalf("plan switch inherited old stage or lost revision: %#v", persisted)
	}

	otherWorkstreamPlan := createWorkstreamPlanAPIForTest(t, s, workspace, "ws_plan_other", otherPlan.ID, "implement")
	if otherWorkstreamPlan.Revision != otherPlan.Revision || len(otherWorkstreamPlan.Stages) != len(otherPlan.Stages) {
		t.Fatalf("test fixture did not create a colliding plan identity: %+v", otherWorkstreamPlan)
	}
	changeWorkstream, changeWorkstreamRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":3,"workstreamId":"ws_plan_other"}`)
	s.handleSessionSave(changeWorkstreamRec, changeWorkstream)
	if changeWorkstreamRec.Code != http.StatusOK {
		t.Fatalf("change workstream status=%d body=%s", changeWorkstreamRec.Code, changeWorkstreamRec.Body.String())
	}
	persisted, err = sessionStore.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WorkstreamID != "ws_plan_other" || persisted.PlanID != "" || persisted.PlanRevision != 0 || persisted.StageID != "" {
		t.Fatalf("workstream switch rebound plan from prior workstream: %#v", persisted)
	}

	planWithoutWorkstream := `{"workspace":"` + filepath.ToSlash(workspace) + `","planId":"` + plan.ID + `","planRevision":1}`
	invalid, invalidRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", planWithoutWorkstream)
	s.handleSessionSave(invalidRec, invalid)
	if invalidRec.Code != http.StatusBadRequest || !strings.Contains(invalidRec.Body.String(), `"code":"invalid_session_association"`) {
		t.Fatalf("plan without workstream status=%d body=%s", invalidRec.Code, invalidRec.Body.String())
	}

	crossStream, crossStreamRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"workspace":"`+filepath.ToSlash(workspace)+`","workstreamId":"ws_plan_other","planId":"`+plan.ID+`","planRevision":1}`)
	s.handleSessionSave(crossStreamRec, crossStream)
	if crossStreamRec.Code != http.StatusNotFound || !strings.Contains(crossStreamRec.Body.String(), `"code":"plan_not_found"`) {
		t.Fatalf("cross-workstream plan status=%d body=%s", crossStreamRec.Code, crossStreamRec.Body.String())
	}
}

func TestSessionAPIValidatesWorkstreamBindingAndPreservesSessionTargets(t *testing.T) {
	workspace := t.TempDir()
	otherWorkspace := t.TempDir()
	workstreams := workstream.NewStore(workspace)
	ws, err := workstreams.Create(workstream.CreateRequest{ID: "ws_bound", Title: "Bound workstream"})
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewSessionStore(t.TempDir())
	s := New(&agentLoopFakeProvider{}, "server-default-model", 0)
	s.SetWorkDir(workspace)
	s.SetSessionStore(store)

	createBody, err := json.Marshal(map[string]any{
		"workspace": workspace, "workstreamId": ws.ID,
		"provider": "fake", "model": "session-target-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", string(createBody))
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode create response=%s err=%v", rec.Body.String(), err)
	}

	update, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":1,"title":"partial"}`)
	s.handleSessionSave(rec, update)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial update status=%d body=%s", rec.Code, rec.Body.String())
	}
	persisted, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WorkstreamID != ws.ID || persisted.Provider != "fake" || persisted.Model != "session-target-model" {
		t.Fatalf("partial update lost session binding/target: %#v", persisted)
	}

	badCreateBody, _ := json.Marshal(map[string]any{
		"workspace": otherWorkspace, "workstreamId": ws.ID,
		"messages": []map[string]string{{"role": "user", "content": "wrong project"}},
	})
	badCreate, badRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", string(badCreateBody))
	s.handleSessionSave(badRec, badCreate)
	if badRec.Code != http.StatusForbidden || !strings.Contains(badRec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("cross-workspace association status=%d body=%s", badRec.Code, badRec.Body.String())
	}

	badUpdate, badUpdateRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":2,"workstreamId":"ws_missing"}`)
	s.handleSessionSave(badUpdateRec, badUpdate)
	if badUpdateRec.Code != http.StatusNotFound || !strings.Contains(badUpdateRec.Body.String(), `"code":"workstream_not_found"`) {
		t.Fatalf("missing workstream update status=%d body=%s", badUpdateRec.Code, badUpdateRec.Body.String())
	}
	unchanged, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != persisted.Revision || unchanged.WorkstreamID != ws.ID {
		t.Fatalf("rejected workstream update changed session: %#v", unchanged)
	}
	s.durableActive.Store(created.ID, struct{}{})
	activeUpdate, activeRec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"`+created.ID+`","expectedRevision":2,"model":"changed-during-run"}`)
	s.handleSessionSave(activeRec, activeUpdate)
	s.durableActive.Delete(created.ID)
	if activeRec.Code != http.StatusConflict || !strings.Contains(activeRec.Body.String(), `"code":"active_run_conflict"`) {
		t.Fatalf("active session target update status=%d body=%s", activeRec.Code, activeRec.Body.String())
	}
	unchanged, err = store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != persisted.Revision || unchanged.Model != "session-target-model" {
		t.Fatalf("active target update changed session: %#v", unchanged)
	}
}

func TestRunTracesAPI(t *testing.T) {
	tracker := observability.NewTracker(t.TempDir())
	started := time.Now().UTC().Add(-time.Second)
	tracker.RecordRun(observability.RunTrace{
		ID:        "run_api",
		Kind:      "agent",
		StartedAt: started,
		EndedAt:   time.Now().UTC(),
		Provider:  "fake",
		Model:     "fake-model",
		Status:    "ok",
		Spans: []observability.RunSpan{{
			ID:        "maker_01",
			Name:      "maker.llm_call",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "ok",
		}},
	})

	s := New(nil, "", 0)
	s.SetTracker(tracker)

	req := httptest.NewRequest(http.MethodGet, "/api/run-traces?limit=1", nil)
	rec := httptest.NewRecorder()
	s.handleRunTraces(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run traces status=%d body=%s", rec.Code, rec.Body.String())
	}
	var runs []observability.RunTrace
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("run traces json: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run_api" || !hasRunSpan(runs[0], "maker.llm_call") {
		t.Fatalf("unexpected run traces: %+v", runs)
	}
}

func TestRunTraceRegressionAPI(t *testing.T) {
	tracker := observability.NewTracker(t.TempDir())
	started := time.Now().UTC().Add(-time.Second)
	tracker.RecordRun(observability.RunTrace{
		ID:        "run_failed_api",
		Kind:      "chronos",
		StartedAt: started,
		EndedAt:   time.Now().UTC(),
		Provider:  "fake",
		Model:     "fake-model",
		Status:    "failed",
		Error:     "verify failed",
		Metadata: map[string]string{
			"task":          "repair api regression",
			"verifyCommand": "go test ./...",
		},
		Spans: []observability.RunSpan{{
			ID:        "chronos",
			Name:      "chronos.run",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "failed",
		}},
	})

	s := New(nil, "", 0)
	s.SetTracker(tracker)

	createReq := httptest.NewRequest(http.MethodPost, "/api/run-traces/run_failed_api/regression", nil)
	createReq.SetPathValue("id", "run_failed_api")
	createRec := httptest.NewRecorder()
	s.handleCreateRegressionCase(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create regression status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created observability.RegressionCase
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create regression json: %v", err)
	}
	if created.TraceID != "run_failed_api" || !created.Replayable || created.Failure.Error != "verify failed" {
		t.Fatalf("unexpected created regression: %+v", created)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/regressions?limit=1", nil)
	listRec := httptest.NewRecorder()
	s.handleRegressionCases(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list regressions status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed []observability.RegressionCase
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list regressions json: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("unexpected listed regressions: %+v", listed)
	}
}

func TestRunRegressionChronosAPI(t *testing.T) {
	workDir := t.TempDir()
	tracker := observability.NewTracker(t.TempDir())
	started := time.Now().UTC().Add(-time.Second)
	tracker.RecordRun(observability.RunTrace{
		ID:        "run_failed_replay",
		Kind:      "chronos",
		StartedAt: started,
		EndedAt:   time.Now().UTC(),
		Provider:  "fake",
		Model:     "fake-model",
		WorkDir:   workDir,
		Status:    "failed",
		Error:     "verify failed",
		Metadata: map[string]string{
			"task":          "repair replay regression",
			"verifyCommand": "go test ./...",
			"maxCycles":     "1",
		},
		Spans: []observability.RunSpan{{
			ID:        "chronos",
			Name:      "chronos.run",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "failed",
		}},
	})
	c, err := tracker.CreateRegressionCase("run_failed_replay")
	if err != nil {
		t.Fatal(err)
	}

	provider := &agentLoopFakeProvider{text: "[COMPLETE]"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workDir)
	s.SetTracker(tracker)

	req := httptest.NewRequest(http.MethodPost, "/api/regressions/"+c.ID+"/run", nil)
	req.SetPathValue("id", c.ID)
	rec := httptest.NewRecorder()
	s.handleRunRegressionCase(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run regression status=%d body=%s", rec.Code, rec.Body.String())
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	events := agentSSEEventTypes(t, rec.Body.String())
	for _, want := range []string{"regression", "status", "text", "done", "regression_result", "stream_end"} {
		if events[want] == 0 {
			t.Fatalf("missing SSE event %q in %v\nbody=%s", want, events, rec.Body.String())
		}
	}

	regressionRuns := tracker.RegressionRuns(10)
	if len(regressionRuns) != 1 {
		t.Fatalf("regression runs=%d, want 1", len(regressionRuns))
	}
	if regressionRuns[0].CaseID != c.ID || regressionRuns[0].Status != "passed" || regressionRuns[0].RunTraceID == "" {
		t.Fatalf("unexpected regression run: %+v", regressionRuns[0])
	}
	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 2 {
		t.Fatalf("run traces=%d, want original failure + replay", len(runTraces))
	}
	replayTrace := runTraces[len(runTraces)-1]
	if replayTrace.ID != regressionRuns[0].RunTraceID || replayTrace.Status != "ok" || replayTrace.Metadata["source"] != "regression" {
		t.Fatalf("unexpected replay trace: %+v", replayTrace)
	}
}

func TestRunRegressionRecordsUnsupportedWithoutProvider(t *testing.T) {
	workDir := t.TempDir()
	tracker := observability.NewTracker(t.TempDir())
	started := time.Now().UTC().Add(-time.Second)
	tracker.RecordRun(observability.RunTrace{
		ID:        "run_failed_no_provider",
		Kind:      "chronos",
		StartedAt: started,
		EndedAt:   time.Now().UTC(),
		Provider:  "fake",
		Model:     "fake-model",
		WorkDir:   workDir,
		Status:    "failed",
		Error:     "verify failed",
		Metadata: map[string]string{
			"task":      "repair replay regression",
			"maxCycles": "1",
		},
		Spans: []observability.RunSpan{{
			ID:        "chronos",
			Name:      "chronos.run",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "failed",
		}},
	})
	c, err := tracker.CreateRegressionCase("run_failed_no_provider")
	if err != nil {
		t.Fatal(err)
	}

	s := New(nil, "", 0)
	s.SetWorkDir(workDir)
	s.SetTracker(tracker)

	req := httptest.NewRequest(http.MethodPost, "/api/regressions/"+c.ID+"/run", nil)
	req.SetPathValue("id", c.ID)
	rec := httptest.NewRecorder()
	s.handleRunRegressionCase(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run regression status=%d body=%s", rec.Code, rec.Body.String())
	}
	var run observability.RegressionRun
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("run regression json: %v", err)
	}
	if run.Status != "unsupported" || !strings.Contains(run.Error, "No provider configured") {
		t.Fatalf("unexpected unsupported run response: %+v", run)
	}
	regressionRuns := tracker.RegressionRuns(10)
	if len(regressionRuns) != 1 || regressionRuns[0].ID != run.ID || regressionRuns[0].Status != "unsupported" {
		t.Fatalf("unsupported run was not recorded: %+v", regressionRuns)
	}
}

func TestRunRegressionTeamAPI(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")

	workDir := t.TempDir()
	tracker := observability.NewTracker(t.TempDir())
	receiptPath, err := agent.WriteTeamRunReceipt(t.TempDir(), workDir, agent.TeamRunReceipt{
		Kind:          "team-run",
		Status:        "failed",
		TeamName:      "api-team",
		PlanName:      "api-team",
		PlanVersion:   1,
		Objective:     "replay failed team",
		Provider:      "fake",
		Model:         "fake-model",
		Capacity:      agent.CapacityConfig{ModelSlots: 1, MaxParallelTasks: 1, FileScopeLock: true},
		VerifyCommand: "",
		TaskCount:     1,
		Failed:        1,
		Verification:  agent.ReceiptVerification{Status: "failed", Source: "team-verify"},
		Tasks: []agent.TeamTaskReceipt{{
			ID:          "task-1",
			Name:        "Replay one",
			Description: "return team ok",
			Kind:        agent.TaskKindImplement,
			Role:        "implementer",
			Status:      "failed",
			Files:       []string{"**"},
			Resources:   agent.AgentTaskResources{ModelSlots: 1},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Second)
	tracker.RecordRun(observability.RunTrace{
		ID:        "run_team_failed_replay",
		Kind:      "team",
		StartedAt: started,
		EndedAt:   time.Now().UTC(),
		Provider:  "fake",
		Model:     "fake-model",
		WorkDir:   workDir,
		Status:    "failed",
		Error:     "team failed",
		Metadata: map[string]string{
			"receipt":       receiptPath,
			"teamName":      "api-team",
			"objective":     "replay failed team",
			"verifyCommand": "",
		},
		Spans: []observability.RunSpan{{
			ID:        "team-run",
			Name:      "team.run",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "failed",
		}},
	})
	c, err := tracker.CreateRegressionCase("run_team_failed_replay")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Replayable || c.Kind != "team" {
		t.Fatalf("unexpected regression case: %+v", c)
	}

	provider := &agentLoopFakeProvider{text: "team ok"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workDir)
	s.SetTracker(tracker)

	req := httptest.NewRequest(http.MethodPost, "/api/regressions/"+c.ID+"/run", bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", c.ID)
	rec := httptest.NewRecorder()
	s.handleRunRegressionCase(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run team regression status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"\"type\":\"regression\"", "\"type\":\"done\"", "\"type\":\"regression_result\"", "\"type\":\"stream_end\""} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("team regression SSE missing %s:\n%s", want, rec.Body.String())
		}
	}

	regressionRuns := tracker.RegressionRuns(10)
	if len(regressionRuns) != 1 || regressionRuns[0].Status != "passed" || regressionRuns[0].Kind != "team" {
		t.Fatalf("unexpected regression runs: %+v", regressionRuns)
	}
	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 2 {
		t.Fatalf("run traces=%d, want original failure + replay", len(runTraces))
	}
	replayTrace := runTraces[len(runTraces)-1]
	if replayTrace.ID != regressionRuns[0].RunTraceID || replayTrace.Kind != "team" || replayTrace.Status != "ok" || replayTrace.Metadata["source"] != "regression" {
		t.Fatalf("unexpected replay trace: %+v", replayTrace)
	}
	if !hasRunSpan(replayTrace, "team.run") {
		t.Fatalf("team replay trace missing run span: %+v", replayTrace.Spans)
	}
}

func TestAgentLoopRecordsWorkstreamRun(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")

	workDir := t.TempDir()
	provider := &agentLoopFakeProvider{text: "agent ok"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workDir)
	tracker := observability.NewTracker(t.TempDir())
	s.SetTracker(tracker)

	store := workstream.NewStore(workDir)
	if _, err := store.Create(workstream.CreateRequest{
		ID:         "ws_agent",
		Title:      "Agent Workstream",
		Summary:    "background summary",
		NextAction: "run agent",
		Goal: workstream.Goal{
			Objective:          "prove agent workstream recording",
			AcceptanceCriteria: []string{"sse event", "timeline event"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{
		"workstreamId":"ws_agent",
		"messages":[{"role":"user","content":"reply briefly"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAgentLoop(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent status=%d body=%s", rec.Code, rec.Body.String())
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	if !strings.Contains(rec.Body.String(), `"traceId"`) {
		t.Fatalf("SSE response missing traceId:\n%s", rec.Body.String())
	}

	events := agentSSEEventTypes(t, rec.Body.String())
	for _, want := range []string{"session", "workstream", "text", "done", "stream_end"} {
		if events[want] == 0 {
			t.Fatalf("missing SSE event %q in %v\nbody=%s", want, events, rec.Body.String())
		}
	}
	if !strings.Contains(provider.systemPrompt, "## Workstream Context") ||
		!strings.Contains(provider.systemPrompt, "Agent Workstream") ||
		!strings.Contains(provider.systemPrompt, "prove agent workstream recording") {
		t.Fatalf("provider did not receive compact workstream context:\n%s", provider.systemPrompt)
	}

	updated, err := store.Get("ws_agent")
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastVerification.Status != "not-run" || updated.LastVerification.Source != "none" {
		t.Fatalf("unexpected verification: %+v", updated.LastVerification)
	}

	timeline, err := store.Timeline("ws_agent")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, event := range timeline {
		have[event.Type] = true
	}
	for _, want := range []string{"agent_run_started", "verification_updated", "agent_run_completed"} {
		if !have[want] {
			t.Fatalf("missing timeline event %q in %+v", want, timeline)
		}
	}

	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 1 {
		t.Fatalf("run traces=%d, want 1", len(runTraces))
	}
	if runTraces[0].Kind != "agent" || runTraces[0].WorkstreamID != "ws_agent" || runTraces[0].Status != "ok" {
		t.Fatalf("unexpected run trace: %+v", runTraces[0])
	}
	if !hasRunSpan(runTraces[0], "maker.llm_call") {
		t.Fatalf("agent run trace missing maker span: %+v", runTraces[0].Spans)
	}
}

func TestChronosRecordsWorkstreamRun(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")

	workDir := t.TempDir()
	provider := &agentLoopFakeProvider{text: "[COMPLETE]"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workDir)
	tracker := observability.NewTracker(t.TempDir())
	s.SetTracker(tracker)

	store := workstream.NewStore(workDir)
	if _, err := store.Create(workstream.CreateRequest{
		ID:         "ws_chronos",
		Title:      "Chronos Workstream",
		Summary:    "chronos background summary",
		NextAction: "run chronos",
		Goal: workstream.Goal{
			Objective:          "prove chronos workstream recording",
			AcceptanceCriteria: []string{"chronos event", "timeline event"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{
		"task":"finish scoped work",
		"workstreamId":"ws_chronos",
		"maxCycles":1
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chronos", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleChronos(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chronos status=%d body=%s", rec.Code, rec.Body.String())
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	if !strings.Contains(rec.Body.String(), `"traceId"`) {
		t.Fatalf("SSE response missing traceId:\n%s", rec.Body.String())
	}

	events := agentSSEEventTypes(t, rec.Body.String())
	for _, want := range []string{"workstream", "status", "text", "done", "stream_end"} {
		if events[want] == 0 {
			t.Fatalf("missing SSE event %q in %v\nbody=%s", want, events, rec.Body.String())
		}
	}
	if !strings.Contains(provider.systemPrompt, "## Workstream Context") ||
		!strings.Contains(provider.systemPrompt, "Chronos Workstream") ||
		!strings.Contains(provider.systemPrompt, "prove chronos workstream recording") {
		t.Fatalf("provider did not receive compact workstream context:\n%s", provider.systemPrompt)
	}

	updated, err := store.Get("ws_chronos")
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastVerification.Status != "not-run" || updated.LastVerification.Source != "chronos" {
		t.Fatalf("unexpected verification: %+v", updated.LastVerification)
	}

	timeline, err := store.Timeline("ws_chronos")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, event := range timeline {
		have[event.Type] = true
	}
	for _, want := range []string{"chronos_run_started", "verification_updated", "chronos_run_completed"} {
		if !have[want] {
			t.Fatalf("missing timeline event %q in %+v", want, timeline)
		}
	}

	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 1 {
		t.Fatalf("run traces=%d, want 1", len(runTraces))
	}
	if runTraces[0].Kind != "chronos" || runTraces[0].WorkstreamID != "ws_chronos" || runTraces[0].Status != "ok" {
		t.Fatalf("unexpected run trace: %+v", runTraces[0])
	}
	if !hasRunSpan(runTraces[0], "chronos.run") {
		t.Fatalf("chronos run trace missing run span: %+v", runTraces[0].Spans)
	}
}

func TestChronosRejectsUnapprovedPlanBeforeProviderCall(t *testing.T) {
	workspace := t.TempDir()
	store := workstream.NewStore(workspace)
	ws, err := store.Create(workstream.CreateRequest{ID: "ws_chronos_plan", Title: "Chronos plan"})
	if err != nil {
		t.Fatal(err)
	}
	setup := New(nil, "", 0)
	setup.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, setup, workspace, ws.ID, "plan_chronos", "research")
	provider := &agentLoopFakeProvider{text: "must not run"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workspace)
	body, err := json.Marshal(map[string]any{
		"workDir": workspace, "workstreamId": ws.ID, "planId": plan.ID,
		"planRevision": plan.Revision, "stageId": "research",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleChronos(recorder, httptest.NewRequest(http.MethodPost, "/api/chronos", bytes.NewReader(body)))
	if recorder.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(recorder.Body.String(), `"code":"plan_transition_conflict"`) {
		t.Fatalf("unapproved Chronos Plan status=%d provider calls=%d body=%s", recorder.Code, provider.calls, recorder.Body.String())
	}
	unchanged, err := store.GetPlan(ws.ID, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != workstream.PlanStatusDraft || unchanged.Stages[0].Status != workstream.PlanStageStatusPending ||
		len(unchanged.Stages[0].Attempts) != 0 {
		t.Fatalf("rejected Chronos run changed plan state: %+v", unchanged)
	}
}

func TestChronosPlanContextUsesStartedPlanState(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	workspace := t.TempDir()
	store := workstream.NewStore(workspace)
	ws, err := store.Create(workstream.CreateRequest{ID: "ws_chronos_context", Title: "Chronos Plan Context"})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(agent.TeamPlan{
		Version: 1, Name: "chronos-context", Objective: "verify the started Plan context",
		VerifyCommand: "echo chronos-plan-verified",
		Stages:        []agent.TeamPlanStage{{ID: "research", Name: "Research", TaskIDs: []string{"task_research"}}},
		Tasks: []agent.AgentTask{{
			ID: "task_research", Stage: "research", Goal: "Inspect the production code path",
			Files: []string{"**"}, ReadOnly: true,
			AcceptanceCriteria: []string{"The prompt reflects the running attempt"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreatePlan(ws.ID, workstream.CreatePlanRequest{ID: "plan_chronos_context", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = store.ApprovePlan(ws.ID, plan.ID, workstream.ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}

	provider := &agentLoopFakeProvider{text: "[COMPLETE]"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workspace)
	body, err := json.Marshal(map[string]any{
		"workDir": workspace, "workstreamId": ws.ID, "planId": plan.ID,
		"planRevision": plan.Revision, "stageId": "research", "maxCycles": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.handleChronos(response, httptest.NewRequest(http.MethodPost, "/api/chronos", bytes.NewReader(body)))
	if response.Code != http.StatusOK || provider.calls == 0 {
		t.Fatalf("approved Chronos Plan status=%d provider calls=%d body=%s", response.Code, provider.calls, response.Body.String())
	}
	for _, want := range []string{
		"Plan ID: plan_chronos_context (definition revision 1, state revision 3, status executing, approved revision 1)",
		"Current stage: research (running) — Research",
		"Current stage attempt: run run_",
		"Inspect the production code path",
	} {
		if !strings.Contains(provider.systemPrompt, want) {
			t.Fatalf("Plan-bound Chronos prompt is missing started state %q:\n%s", want, provider.systemPrompt)
		}
	}
}

type agentLoopFakeProvider struct {
	text         string
	calls        int
	systemPrompt string
	model        string
}

func (p *agentLoopFakeProvider) Name() string              { return "fake" }
func (p *agentLoopFakeProvider) DisplayName() string       { return "Fake" }
func (p *agentLoopFakeProvider) Models() []types.ModelInfo { return nil }
func (p *agentLoopFakeProvider) Validate() error           { return nil }

func (p *agentLoopFakeProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	p.systemPrompt = string(req.System)
	p.model = req.Model

	ch := make(chan types.SSEEvent, 5)
	go func() {
		defer close(ch)
		block, _ := json.Marshal(map[string]string{"type": "text"})
		delta, _ := json.Marshal(map[string]string{"type": "text_delta", "text": p.text})
		stop, _ := json.Marshal(map[string]string{"stop_reason": "end_turn"})
		ch <- types.SSEEvent{Type: "content_block_start", ContentBlock: block}
		ch <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
		ch <- types.SSEEvent{Type: "content_block_stop"}
		ch <- types.SSEEvent{Type: "message_delta", Delta: stop}
		ch <- types.SSEEvent{Type: "message_stop"}
	}()
	return ch, nil
}

func agentSSEEventTypes(t *testing.T, body string) map[string]int {
	t.Helper()

	events := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode SSE line %q: %v", line, err)
		}
		events[event.Type]++
	}
	return events
}

func hasRunSpan(trace observability.RunTrace, name string) bool {
	for _, span := range trace.Spans {
		if span.Name == name {
			return true
		}
	}
	return false
}
