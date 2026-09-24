package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func TestServerTeamTaskFilesDefaultsLegacyScope(t *testing.T) {
	files := serverTeamTaskFiles("", false, nil)
	if len(files) != 1 || files[0] != "**" {
		t.Fatalf("legacy empty file scope = %+v, want **", files)
	}

	files = serverTeamTaskFiles("implement", false, nil)
	if len(files) != 0 {
		t.Fatalf("explicit implement task should keep empty scope for validation, got %+v", files)
	}

	files = serverTeamTaskFiles("", true, []string{"", " internal/agent/** "})
	if len(files) != 1 || files[0] != "internal/agent/**" {
		t.Fatalf("file scope cleanup = %+v", files)
	}
}

func TestHandleTeamExecuteRejectsInvalidPlan(t *testing.T) {
	s := New(serverTestProvider{}, "qwen3:8b", 0)
	s.workDir = t.TempDir()

	body := `{
		"name": "bad-team",
		"objective": "reject malformed plan",
		"tasks": [
			{"id": "dup", "name": "One", "description": "first", "files": ["**"]},
			{"id": "dup", "name": "Two", "description": "second", "files": ["**"]}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/team", strings.NewReader(body))
	rec := httptest.NewRecorder()

	s.handleTeamExecute(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "duplicate task id") {
		t.Fatalf("body should mention duplicate task id, got %s", rec.Body.String())
	}
}

func TestHandleTeamExecuteRecordsTraceAndWorkstream(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")

	workDir := t.TempDir()
	provider := &agentLoopFakeProvider{text: "team ok"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workDir)
	tracker := observability.NewTracker(t.TempDir())
	s.SetTracker(tracker)

	store := workstream.NewStore(workDir)
	if _, err := store.Create(workstream.CreateRequest{
		ID:         "ws_team",
		Title:      "Team Workstream",
		Summary:    "team background summary",
		NextAction: "run team",
		Goal: workstream.Goal{
			Objective:          "prove team workstream recording",
			AcceptanceCriteria: []string{"team trace", "timeline event"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{
		"name": "api-team",
		"objective": "record team run",
		"executionPolicy": {"mode": "full", "revision": 999},
		"workstreamId": "ws_team",
		"capacity": {"modelSlots": 1, "maxParallelTasks": 1},
		"tasks": [
			{"id": "task-1", "name": "One", "description": "say ok", "files": ["**"]}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/team", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleTeamExecute(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("team status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"traceId"`) {
		t.Fatalf("SSE response missing traceId:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"executionPolicy":{"mode":"full","revision":`) ||
		strings.Contains(rec.Body.String(), `"revision":999`) {
		t.Fatalf("SSE session did not report the server-resolved execution policy:\n%s", rec.Body.String())
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	if !strings.Contains(provider.systemPrompt, "## Workstream Context") ||
		!strings.Contains(provider.systemPrompt, "Team Workstream") ||
		!strings.Contains(provider.systemPrompt, "prove team workstream recording") {
		t.Fatalf("provider did not receive compact workstream context:\n%s", provider.systemPrompt)
	}
	events := agentSSEEventTypes(t, rec.Body.String())
	for _, want := range []string{"session", "workstream", "status", "done", "stream_end"} {
		if events[want] == 0 {
			t.Fatalf("missing SSE event %q in %v\nbody=%s", want, events, rec.Body.String())
		}
	}

	updated, err := store.Get("ws_team")
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastVerification.Status != "not-run" || updated.LastVerification.Source != "team" {
		t.Fatalf("unexpected verification: %+v", updated.LastVerification)
	}

	timeline, err := store.Timeline("ws_team")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, event := range timeline {
		have[event.Type] = true
	}
	for _, want := range []string{"team_run_started", "verification_updated", "team_run_completed"} {
		if !have[want] {
			t.Fatalf("missing timeline event %q in %+v", want, timeline)
		}
	}

	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 1 {
		t.Fatalf("run traces=%d, want 1", len(runTraces))
	}
	if runTraces[0].Kind != "team" || runTraces[0].WorkstreamID != "ws_team" || runTraces[0].Status != "ok" || runTraces[0].WorkDir != workDir {
		t.Fatalf("unexpected run trace: %+v", runTraces[0])
	}
	if runTraces[0].Metadata["source"] != "api.team" || runTraces[0].Metadata["receipt"] == "" {
		t.Fatalf("unexpected run metadata: %+v", runTraces[0].Metadata)
	}
	if !hasRunSpan(runTraces[0], "team.run") {
		t.Fatalf("team run trace missing run span: %+v", runTraces[0].Spans)
	}
}

func TestHandleTeamExecuteUsesApprovedWorkstreamPlanStageAndEvidence(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	workDir := t.TempDir()
	store := workstream.NewStore(workDir)
	ws, err := store.Create(workstream.CreateRequest{ID: "ws_team_plan", Title: "Team plan"})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(agent.TeamPlan{
		Version: 1, Name: "stored-plan", Objective: "execute the approved stage", VerifyCommand: "echo plan-verification-passed",
		Stages: []agent.TeamPlanStage{{ID: "execute", Name: "Execute", TaskIDs: []string{"task_execute"}}},
		Tasks: []agent.AgentTask{{
			ID: "task_execute", Stage: "execute", Goal: "Return a short completion", Files: []string{"**"},
			AcceptanceCriteria: []string{"The stored task finishes successfully"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreatePlan(ws.ID, workstream.CreatePlanRequest{ID: "plan_team", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "team task completed"}
	s := New(provider, "fake-model", 0)
	s.SetWorkDir(workDir)
	requestBody, err := json.Marshal(map[string]any{
		"workDir": workDir, "workstreamId": ws.ID, "planId": plan.ID,
		"planRevision": plan.Revision, "stageId": "execute",
		"executionPolicy": map[string]string{"mode": "full"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		s.handleTeamExecute(recorder, httptest.NewRequest(http.MethodPost, "/api/team", bytes.NewReader(requestBody)))
		return recorder
	}
	if unapproved := request(); unapproved.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(unapproved.Body.String(), `"code":"plan_transition_conflict"`) {
		t.Fatalf("unapproved Team plan status=%d provider calls=%d body=%s", unapproved.Code, provider.calls, unapproved.Body.String())
	}
	plan, err = store.ApprovePlan(ws.ID, plan.ID, workstream.ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleBody, err := json.Marshal(map[string]any{
		"workDir": workDir, "workstreamId": ws.ID, "planId": plan.ID,
		"planRevision": plan.Revision + 1, "stageId": "execute",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := httptest.NewRecorder()
	s.handleTeamExecute(stale, httptest.NewRequest(http.MethodPost, "/api/team", bytes.NewReader(staleBody)))
	if stale.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(stale.Body.String(), `"code":"plan_revision_conflict"`) {
		t.Fatalf("stale Team plan status=%d provider calls=%d body=%s", stale.Code, provider.calls, stale.Body.String())
	}

	response := request()
	if response.Code != http.StatusOK || provider.calls == 0 {
		t.Fatalf("approved Team plan status=%d provider calls=%d body=%s", response.Code, provider.calls, response.Body.String())
	}
	for _, want := range []string{
		"Plan ID: plan_team (definition revision 1, state revision 3, status executing, approved revision 1)",
		"Current stage: execute (running) — Execute",
		"Current stage attempt: run run_",
	} {
		if !strings.Contains(provider.systemPrompt, want) {
			t.Fatalf("Plan-bound Team prompt is missing started state %q:\n%s", want, provider.systemPrompt)
		}
	}
	updated, err := store.GetPlan(ws.ID, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Stages[0].Status != workstream.PlanStageStatusCompleted || updated.Status != workstream.PlanStatusCompleted ||
		len(updated.Stages[0].Attempts) != 1 || updated.Stages[0].Attempts[0].Evidence == nil ||
		updated.Stages[0].Attempts[0].Evidence.Source != "team-receipt" ||
		updated.Stages[0].Attempts[0].Evidence.VerificationStatus != "passed" {
		t.Fatalf("verified stored Team plan stage was not completed from its receipt: %+v", updated)
	}
}

type serverTestProvider struct{}

func (serverTestProvider) Name() string              { return "ollama" }
func (serverTestProvider) DisplayName() string       { return "Ollama" }
func (serverTestProvider) Models() []types.ModelInfo { return nil }
func (serverTestProvider) Validate() error           { return nil }
func (serverTestProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	ch := make(chan types.SSEEvent)
	close(ch)
	return ch, nil
}
