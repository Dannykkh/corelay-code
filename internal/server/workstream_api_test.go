package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aniclew/aniclew/internal/agent"
	"github.com/aniclew/aniclew/internal/observability"
	"github.com/aniclew/aniclew/internal/types"
	"github.com/aniclew/aniclew/internal/workstream"
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

	handoffReq := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_api/handoff", bytes.NewReader([]byte(`{}`)))
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
	if handoff.Path == "" || handoff.Markdown == "" {
		t.Fatalf("empty handoff response: %+v", handoff)
	}
	if _, err := os.Stat(handoff.Path); err != nil {
		t.Fatalf("handoff file missing: %v", err)
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
	t.Setenv("ANICLEW_MEMORY", "off")
	t.Setenv("ANICLEW_AUTOSKILL", "off")

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
	t.Setenv("ANICLEW_MEMORY", "off")
	t.Setenv("ANICLEW_AUTOSKILL", "off")

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
	t.Setenv("ANICLEW_MEMORY", "off")
	t.Setenv("ANICLEW_AUTOSKILL", "off")

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

type agentLoopFakeProvider struct {
	text         string
	calls        int
	systemPrompt string
}

func (p *agentLoopFakeProvider) Name() string              { return "fake" }
func (p *agentLoopFakeProvider) DisplayName() string       { return "Fake" }
func (p *agentLoopFakeProvider) Models() []types.ModelInfo { return nil }
func (p *agentLoopFakeProvider) Validate() error           { return nil }

func (p *agentLoopFakeProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	p.systemPrompt = string(req.System)

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
