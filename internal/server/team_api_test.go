package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aniclew/aniclew/internal/observability"
	"github.com/aniclew/aniclew/internal/types"
	"github.com/aniclew/aniclew/internal/workstream"
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
	t.Setenv("ANICLEW_MEMORY", "off")
	t.Setenv("ANICLEW_AUTOSKILL", "off")

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
	if runTraces[0].Kind != "team" || runTraces[0].WorkstreamID != "ws_team" || runTraces[0].Status != "ok" {
		t.Fatalf("unexpected run trace: %+v", runTraces[0])
	}
	if runTraces[0].Metadata["source"] != "api.team" || runTraces[0].Metadata["receipt"] == "" {
		t.Fatalf("unexpected run metadata: %+v", runTraces[0].Metadata)
	}
	if !hasRunSpan(runTraces[0], "team.run") {
		t.Fatalf("team run trace missing run span: %+v", runTraces[0].Spans)
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
