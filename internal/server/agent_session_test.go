package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

type failingDurableInterruptionStore struct {
	*agent.SessionStore
	err error
}

func (s *failingDurableInterruptionStore) MarkInterrupted(
	string,
	uint64,
	agent.SessionInterruption,
) (*agent.Session, error) {
	return nil, s.err
}

func TestDurableAgentRunCommitsBoundedTranscriptAtExpectedRevision(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{
		Workspace: workDir,
		Provider:  "old-provider",
		Model:     "old-model",
		Messages: []agent.SessionMessage{{
			Role: "user", Content: "ship it",
		}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"ship it"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	run.SetRuntimeRunID("run_test")
	run.Observe(agent.Event{Type: "text", Data: "working"})
	run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "tool_1", "name": "Read", "inputDigest": "sha256:" + strings.Repeat("a", 64), "runId": "run_kernel",
	}})
	run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{
		"id": "tool_1", "name": "Read", "result": "ok", "isError": false, "executed": true,
	}})
	run.Observe(agent.Event{Type: "text", Data: "done"})
	run.Observe(agent.Event{Type: "done", Data: map[string]any{}})

	committed, err := run.Finalize("new-provider", "new-model")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 2 || committed.Provider != "new-provider" || committed.Model != "new-model" {
		t.Fatalf("committed = %#v", committed)
	}
	if len(committed.Messages) != 4 || committed.Messages[1].Content != "working" ||
		committed.Messages[2].Role != "tool" || committed.Messages[3].Content != "done" {
		t.Fatalf("messages = %#v", committed.Messages)
	}
	reference, ok := committed.Messages[2].ToolInput.(map[string]interface{})
	if !ok || reference["inputDigest"] == "" {
		t.Fatalf("tool reference = %#v", committed.Messages[2].ToolInput)
	}
}

func TestDurableAgentRunCommitsTerminalMetadataWithTranscriptRevision(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		terminal agent.DurableRunTerminalMetadata
	}{
		{
			name: "complete",
			text: "completed answer",
			terminal: agent.DurableRunTerminalMetadata{
				TerminalState: agent.EvidenceTerminalVerified, CompletionStatus: agent.CompletionStatusComplete,
				CompletionRevision: 3, CompletionCriteria: 2, CompletionSatisfied: 2,
			},
		},
		{
			name: "incomplete without transcript delta",
			terminal: agent.DurableRunTerminalMetadata{
				TerminalState: agent.EvidenceTerminalBlocked, CompletionStatus: agent.CompletionStatusIncomplete,
				CompletionCriteria: 2, CompletionSatisfied: 1,
			},
		},
		{
			name: "blocked",
			text: "bounded partial answer",
			terminal: agent.DurableRunTerminalMetadata{
				TerminalState: agent.EvidenceTerminalBlocked, CompletionStatus: agent.CompletionStatusBlocked,
				CompletionRevision: 1, CompletionCriteria: 2, CompletionSatisfied: 1, CompletionBlocked: 1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := agent.NewSessionStore(t.TempDir())
			workDir := t.TempDir()
			session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "run"}}}
			if err := store.Save(&session); err != nil {
				t.Fatal(err)
			}
			run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
				Role: "user", Content: json.RawMessage(`"run"`),
			}})
			if err != nil {
				t.Fatal(err)
			}
			if test.text != "" {
				run.Observe(agent.Event{Type: "text", Data: test.text})
			}
			run.Observe(agent.Event{Type: "done", Data: test.terminal})

			committed, err := run.Finalize("provider", "model")
			if err != nil {
				t.Fatal(err)
			}
			if committed.Revision != 2 || committed.LastCommittedRevision != 2 ||
				committed.LastRunTerminal == nil || *committed.LastRunTerminal != test.terminal {
				t.Fatalf("committed terminal = %#v at r%d/%d", committed.LastRunTerminal, committed.Revision, committed.LastCommittedRevision)
			}
			if got := len(committed.Messages); got != 1+boolInt(test.text != "") {
				t.Fatalf("committed messages = %d, want %d", got, 1+boolInt(test.text != ""))
			}
			committed.LastRunTerminal.CompletionSatisfied = 99
			loaded, err := store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.LastRunTerminal == nil || *loaded.LastRunTerminal != test.terminal {
				t.Fatalf("loaded terminal = %#v, want %#v", loaded.LastRunTerminal, test.terminal)
			}
			resume, err := store.ResumeState(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if resume.LastRunTerminal == nil || *resume.LastRunTerminal != test.terminal {
				t.Fatalf("resume terminal = %#v, want %#v", resume.LastRunTerminal, test.terminal)
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestDurableAgentRunRejectsStaleWorkspaceAndHistory(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "original"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}

	_, err := prepareDurableAgentRun(store, session.ID, session.Revision+1, workDir, []types.Message{{Role: "user", Content: json.RawMessage(`"original"`)}})
	var conflict *agent.SessionRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale error = %v", err)
	}
	_, err = prepareDurableAgentRun(store, session.ID, session.Revision, t.TempDir(), []types.Message{{Role: "user", Content: json.RawMessage(`"original"`)}})
	if !errors.Is(err, agent.ErrSessionConflict) {
		t.Fatalf("workspace error = %v", err)
	}
	_, err = prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{Role: "user", Content: json.RawMessage(`"rewritten"`)}, {Role: "assistant", Content: json.RawMessage(`"extra"`)}})
	if !errors.Is(err, errDurableTranscriptMismatch) {
		t.Fatalf("history error = %v", err)
	}
}

func TestDurableAgentRunMarksOnlyStartedToolInterrupted(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	newRun := func(t *testing.T) (*durableAgentRun, agent.Session) {
		t.Helper()
		session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "run"}}}
		if err := store.Save(&session); err != nil {
			t.Fatal(err)
		}
		run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{Role: "user", Content: json.RawMessage(`"run"`)}})
		if err != nil {
			t.Fatal(err)
		}
		run.SetRuntimeRunID("runtime_run")
		return run, session
	}

	run, session := newRun(t)
	updated, err := run.MarkInterrupted("client disconnected")
	if err != nil || updated != nil {
		t.Fatalf("no-tool interrupt = (%#v, %v)", updated, err)
	}
	unchanged, _ := store.Get(session.ID)
	if unchanged.Revision != session.Revision || unchanged.ReconcileRequired {
		t.Fatalf("unchanged = %#v", unchanged)
	}

	run, _ = newRun(t)
	run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "tool_2", "name": "Write", "inputDigest": "sha256:" + strings.Repeat("b", 64),
	}})
	updated, err = run.MarkInterrupted("client disconnected")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ReconcileRequired || updated.Interruption == nil ||
		updated.Interruption.RunID != "runtime_run" || updated.Interruption.SideEffectState != agent.SessionSideEffectStarted {
		t.Fatalf("interrupted = %#v", updated)
	}
}

func TestDurableAgentRunCommitConflictMarksCompletedToolForReconciliation(t *testing.T) {
	baseDir := t.TempDir()
	workDir := t.TempDir()
	store := agent.NewSessionStore(baseDir)
	session := agent.Session{
		Workspace: workDir,
		Messages:  []agent.SessionMessage{{Role: "user", Content: "mutate"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"mutate"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	run.SetRuntimeRunID("runtime-run")
	run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "call-commit", "name": "Write", "runId": "kernel-run",
		"inputDigest": "sha256:" + strings.Repeat("c", 64),
	}})
	run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{
		"id": "call-commit", "name": "Write", "result": "ok", "executed": true,
	}})
	run.Observe(agent.Event{Type: "done"})

	concurrent, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	concurrent.Title = "concurrent update"
	if err := store.SaveExpected(concurrent, concurrent.Revision); err != nil {
		t.Fatal(err)
	}

	if _, err := run.Finalize("provider", "model"); !errors.Is(err, agent.ErrSessionRevisionConflict) {
		t.Fatalf("Finalize() error = %v, want revision conflict", err)
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.ToolCallID != "call-commit" ||
		persisted.Interruption.SideEffectState != agent.SessionSideEffectApplied {
		t.Fatalf("commit conflict did not checkpoint ambiguous side effect: %#v", persisted)
	}
}

func TestDurableAgentRunStaleTerminalCASDoesNotOverwriteAndFreshRetryCommits(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "run"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	request := []types.Message{{Role: "user", Content: json.RawMessage(`"run"`)}}
	staleRun, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, request)
	if err != nil {
		t.Fatal(err)
	}
	staleRun.Observe(agent.Event{Type: "text", Data: "must not commit"})
	staleRun.Observe(agent.Event{Type: "done", Data: agent.DurableRunTerminalMetadata{
		TerminalState: agent.EvidenceTerminalBlocked, CompletionStatus: agent.CompletionStatusBlocked,
		CompletionRevision: 1, CompletionCriteria: 1, CompletionBlocked: 1,
	}})

	concurrent, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	concurrent.Title = "concurrent winner"
	if err := store.SaveExpected(concurrent, concurrent.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := staleRun.Finalize("provider", "model"); !errors.Is(err, agent.ErrSessionRevisionConflict) {
		t.Fatalf("stale Finalize() = %v, want revision conflict", err)
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != 2 || persisted.Title != "concurrent winner" || persisted.LastRunTerminal != nil || len(persisted.Messages) != 1 {
		t.Fatalf("stale terminal changed persisted session: %#v", persisted)
	}

	retry, err := prepareDurableAgentRun(store, session.ID, persisted.Revision, workDir, request)
	if err != nil {
		t.Fatal(err)
	}
	retry.Observe(agent.Event{Type: "text", Data: "fresh completion"})
	want := agent.DurableRunTerminalMetadata{
		TerminalState: agent.EvidenceTerminalVerified, CompletionStatus: agent.CompletionStatusComplete,
		CompletionRevision: 2, CompletionCriteria: 1, CompletionSatisfied: 1,
	}
	retry.Observe(agent.Event{Type: "done", Data: want})
	committed, err := retry.Finalize("provider", "model")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 3 || committed.LastRunTerminal == nil || *committed.LastRunTerminal != want ||
		len(committed.Messages) != 2 || committed.Messages[1].Content != "fresh completion" {
		t.Fatalf("fresh retry = %#v", committed)
	}
}

func TestDurableAgentRunInterruptedConflictRetriesLatestRevision(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "run"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"run"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	run.SetRuntimeRunID("runtime-run")
	run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "call-interrupt", "name": "Write", "runId": "kernel-run",
		"inputDigest": "sha256:" + strings.Repeat("d", 64),
	}})
	concurrent, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	concurrent.Title = "advanced revision"
	if err := store.SaveExpected(concurrent, concurrent.Revision); err != nil {
		t.Fatal(err)
	}
	updated, err := run.MarkInterrupted("disconnect")
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil || !updated.ReconcileRequired || updated.Revision != concurrent.Revision+1 ||
		updated.Interruption == nil || updated.Interruption.ToolCallID != "call-interrupt" {
		t.Fatalf("revision-raced interruption = %#v", updated)
	}
}

func TestDurableAgentRunPersistenceFailureInvokesRuntimeQuarantine(t *testing.T) {
	baseStore := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "run"}}}
	if err := baseStore.Save(&session); err != nil {
		t.Fatal(err)
	}
	store := &failingDurableInterruptionStore{SessionStore: baseStore, err: errors.New("disk unavailable")}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"run"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	run.SetRuntimeRunID("runtime-run")
	run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "call-failed-marker", "name": "Write", "runId": "kernel-run",
		"inputDigest": "sha256:" + strings.Repeat("e", 64),
	}})
	quarantined := ""
	run.quarantine = func(sessionID string) { quarantined = sessionID }
	if _, err := run.MarkInterrupted("disconnect"); err == nil {
		t.Fatal("MarkInterrupted() unexpectedly succeeded")
	}
	if quarantined != session.ID {
		t.Fatalf("quarantined session = %q, want %q", quarantined, session.ID)
	}
}

func TestDurableAgentRunAmbiguousReusedIDRequiresReconciliation(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "run"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"run"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	run.SetRuntimeRunID("runtime-run")
	for _, digestRune := range []string{"a", "b"} {
		run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-reused", "name": "Write", "runId": "kernel-run",
			"inputDigest": "sha256:" + strings.Repeat(digestRune, 64),
		}})
	}
	for range 2 {
		run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{
			"id": "call-reused", "name": "Write", "result": "ok", "executed": true,
		}})
	}
	run.Observe(agent.Event{Type: "done"})
	if _, err := run.Finalize("provider", "model"); !errors.Is(err, agent.ErrSessionReconcileRequired) {
		t.Fatalf("Finalize() error = %v, want reconcile required", err)
	}
	persisted, err := store.Get(session.ID)
	if err != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.ToolName != "multiple_tools" {
		t.Fatalf("ambiguous reused ID checkpoint = (%#v, %v)", persisted, err)
	}
}

func TestDurableAgentRunAggregatesParallelToolInterruption(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "parallel"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{Role: "user", Content: json.RawMessage(`"parallel"`)}})
	if err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"Read", "Bash"} {
		run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id":          "tool_" + name,
			"name":        name,
			"inputDigest": "sha256:" + strings.Repeat(string(rune('d'+index)), 64),
			"runId":       "kernel_run",
		}})
	}
	run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{
		"id": "tool_Read", "name": "Read", "result": "ok", "executed": true,
	}})
	updated, err := run.MarkInterrupted("disconnect")
	if err != nil {
		t.Fatal(err)
	}
	marker := updated.Interruption
	if marker == nil || marker.ToolName != "multiple_tools" || marker.ToolCallID != "multiple" ||
		marker.RunID != "kernel_run" || marker.SideEffectState != agent.SessionSideEffectMayHaveApplied ||
		!strings.HasPrefix(marker.InputDigest, "sha256:") || !strings.Contains(marker.Summary, "2 tool executions") {
		t.Fatalf("aggregate marker = %#v", marker)
	}
}

func TestDurableAgentRunDoesNotPersistReasoningOrRawToolInput(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "safe"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{Role: "user", Content: json.RawMessage(`"safe"`)}})
	if err != nil {
		t.Fatal(err)
	}
	run.Observe(agent.Event{Type: "thinking", Data: "private reasoning"})
	run.Observe(agent.Event{Type: "tool_input", Data: map[string]any{"id": "tool", "name": "Bash", "input": map[string]string{"command": "secret-command"}}})
	run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "tool", "name": "Bash", "inputDigest": "sha256:" + strings.Repeat("c", 64), "runId": "run",
	}})
	run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{"id": "tool", "name": "Bash", "result": "safe result", "executed": true}})
	run.Observe(agent.Event{Type: "done"})
	committed, err := run.Finalize("provider", "model")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(committed.Messages)
	if strings.Contains(string(encoded), "private reasoning") || strings.Contains(string(encoded), "secret-command") {
		t.Fatalf("sensitive event data persisted: %s", encoded)
	}
}

func TestAgentLoopBindsAndCommitsDurableSessionBeforeDone(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workDir := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{
		Workspace: workDir,
		Messages: []agent.SessionMessage{{
			Role: "user", Content: "hello",
		}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "durable answer"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	body, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body))
	recorder := httptest.NewRecorder()

	server.handleAgentLoop(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := recorder.Body.String()
	durableIndex := strings.Index(response, `"type":"durable_session"`)
	doneIndex := strings.Index(response, `"type":"done"`)
	if durableIndex < 0 || doneIndex < 0 || durableIndex > doneIndex {
		t.Fatalf("durable commit must precede done:\n%s", response)
	}
	committed, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 2 || len(committed.Messages) != 2 || committed.Messages[1].Content != "durable answer" {
		t.Fatalf("committed = %#v", committed)
	}
	if !strings.Contains(response, `"durableSessionId":"`+session.ID+`"`) || !strings.Contains(response, `"durableRevision":1`) {
		t.Fatalf("initial session binding missing:\n%s", response)
	}
}

func TestAgentLoopReplaysCommittedRequestWithoutRedispatch(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workDir := t.TempDir()
	storeDir := t.TempDir()
	store := agent.NewSessionStore(storeDir)
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "once only"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	request := map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID, "expectedRevision": session.Revision,
		"requestId": "turn_123",
	}
	post := func(target *Server, payload map[string]any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(payload)
		recorder := httptest.NewRecorder()
		target.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(encoded)))
		return recorder
	}
	first := post(server, request)
	if first.Code != http.StatusOK || provider.calls != 1 {
		t.Fatalf("first status=%d calls=%d body=%s", first.Code, provider.calls, first.Body.String())
	}
	committed, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.LastAgentRequest == nil || committed.LastAgentRequest.CommittedRevision != committed.Revision {
		t.Fatalf("missing committed request receipt: %#v", committed.LastAgentRequest)
	}
	restarted := New(provider, "fake-model", 0)
	restarted.SetWorkDir(workDir)
	restarted.SetSessionStore(agent.NewSessionStore(storeDir))
	replay := post(restarted, request)
	if replay.Code != http.StatusOK || provider.calls != 1 ||
		!strings.Contains(replay.Body.String(), `"type":"text","data":"once only"`) ||
		!strings.Contains(replay.Body.String(), `"type":"done"`) {
		t.Fatalf("replay status=%d calls=%d body=%s", replay.Code, provider.calls, replay.Body.String())
	}
	changed := map[string]any{}
	for key, value := range request {
		changed[key] = value
	}
	changed["responseLang"] = "ko"
	conflict := post(restarted, changed)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"agent_request_conflict"`) || provider.calls != 1 {
		t.Fatalf("conflict status=%d calls=%d body=%s", conflict.Code, provider.calls, conflict.Body.String())
	}
	forged := *committed
	forged.LastAgentRequest = &agent.AgentRequestReceipt{ID: "forged"}
	if err := store.SaveExpected(&forged, committed.Revision); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Get(session.ID)
	if err != nil || reloaded.LastAgentRequest == nil || reloaded.LastAgentRequest.ID != "turn_123" {
		t.Fatalf("public session save changed server receipt: %#v err=%v", reloaded, err)
	}
}

func TestAgentRequestReplayPreservesTerminalFailure(t *testing.T) {
	session := &agent.Session{
		ID: "sess_20260924-000000", Revision: 3,
		Messages: []agent.SessionMessage{
			{Role: "user", Content: "prompt"},
			{Role: "assistant", Content: "partial result"},
		},
		LastAgentRequest: &agent.AgentRequestReceipt{
			ID: "failed_turn", MessageCount: 1,
			Kind:     agent.RunTerminalFailed,
			Terminal: &agent.DurableRunTerminalMetadata{TerminalState: agent.EvidenceTerminalBlocked},
		},
	}
	recorder := httptest.NewRecorder()
	writeAgentRequestReplay(recorder, session)
	response := recorder.Body.String()
	if !strings.Contains(response, `"type":"text","data":"partial result"`) ||
		!strings.Contains(response, `"kind":"failed"`) ||
		!strings.Contains(response, `"terminalState":"blocked"`) {
		t.Fatalf("replay lost failure semantics: %s", response)
	}
}

func TestAgentLoopUsesDurableSessionWorkspaceWorkstreamAndModel(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	projectWorkspace := t.TempDir()
	serverWorkspace := t.TempDir()
	workstreams := workstream.NewStore(projectWorkspace)
	ws, err := workstreams.Create(workstream.CreateRequest{ID: "ws_saved", Title: "Saved project workstream"})
	if err != nil {
		t.Fatal(err)
	}
	otherWorkstream, err := workstreams.Create(workstream.CreateRequest{ID: "ws_other", Title: "Other workstream"})
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewSessionStore(t.TempDir())
	policy, err := agent.ResolveExecutionPolicy(agent.ExecutionPolicyRequest{Mode: agent.ExecutionModeReadOnly}, "", sandbox.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	session := agent.Session{
		Workspace: projectWorkspace, WorkstreamID: ws.ID,
		Provider: "fake", Model: "session-target-model",
		ExecutionPolicy: &policy,
		Messages:        []agent.SessionMessage{{Role: "user", Content: "hello"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "durable answer"}
	server := New(provider, "server-default-model", 0)
	server.SetWorkDir(serverWorkspace)
	server.SetSessionStore(store)
	body, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.calls != 1 || provider.model != "session-target-model" {
		t.Fatalf("provider calls=%d model=%q, want one call to persisted model", provider.calls, provider.model)
	}
	var sessionEventWorkDir string
	workstreamEventFound := false
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data: "))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode SSE event %q: %v", line, err)
		}
		if event.Type == "session" {
			var data struct {
				WorkDir string `json:"workDir"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatalf("decode session event data: %v", err)
			}
			sessionEventWorkDir = data.WorkDir
		}
		if event.Type == "workstream" {
			var data struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatalf("decode workstream event data: %v", err)
			}
			workstreamEventFound = data.ID == ws.ID
		}
	}
	if !sameDurableWorkspace(sessionEventWorkDir, projectWorkspace) || workstreamEventFound != true {
		t.Fatalf("resolved workspace=%q workstream event=%v response=%s", sessionEventWorkDir, workstreamEventFound, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"executionPolicy":{"mode":"read-only"`) {
		t.Fatalf("durable session policy was not used as the run default: %s", recorder.Body.String())
	}
	if !strings.Contains(provider.systemPrompt, "Saved project workstream") {
		t.Fatalf("persisted workstream context missing from provider prompt: %q", provider.systemPrompt)
	}
	committed, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 2 || committed.Workspace != session.Workspace || committed.WorkstreamID != ws.ID ||
		committed.Provider != session.Provider || committed.Model != session.Model {
		t.Fatalf("durable session target changed after run: %#v", committed)
	}

	boundSession := agent.Session{
		Workspace: projectWorkspace, WorkstreamID: ws.ID, Provider: "fake", Model: "session-target-model",
		Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}},
	}
	if err := store.Save(&boundSession); err != nil {
		t.Fatal(err)
	}
	mismatchBody, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"workstreamId":     otherWorkstream.ID,
		"durableSessionId": boundSession.ID,
		"expectedRevision": boundSession.Revision,
	})
	mismatchRecorder := httptest.NewRecorder()
	server.handleAgentLoop(mismatchRecorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(mismatchBody)))
	if mismatchRecorder.Code != http.StatusConflict || provider.calls != 1 ||
		!strings.Contains(mismatchRecorder.Body.String(), `"code":"session_workstream_conflict"`) {
		t.Fatalf("workstream mismatch status=%d provider calls=%d body=%s", mismatchRecorder.Code, provider.calls, mismatchRecorder.Body.String())
	}
}

func TestAgentLoopRejectsStaleWorkstreamPlanBeforeProviderCall(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workspace := t.TempDir()
	workstreams := workstream.NewStore(workspace)
	ws, err := workstreams.Create(workstream.CreateRequest{ID: "ws_stale_plan", Title: "Stale plan"})
	if err != nil {
		t.Fatal(err)
	}
	planServer := New(nil, "", 0)
	planServer.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, planServer, workspace, ws.ID, "plan_stale", "research")
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{
		Workspace: workspace, WorkstreamID: ws.ID, PlanID: plan.ID,
		PlanRevision: plan.Revision + 1, StageID: "research", Provider: "fake", Model: "fake-model",
		Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "must not run"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workspace)
	server.SetSessionStore(store)
	body, err := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(recorder.Body.String(), `"code":"plan_revision_conflict"`) {
		t.Fatalf("stale workflow plan status=%d provider calls=%d body=%s", recorder.Code, provider.calls, recorder.Body.String())
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != session.Revision || persisted.PlanRevision != plan.Revision+1 {
		t.Fatalf("rejected run changed session: %#v", persisted)
	}
}

func TestAgentLoopRequiresPlanApprovalAndReceiptBeforeCompletingStage(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workspace := t.TempDir()
	workstreams := workstream.NewStore(workspace)
	ws, err := workstreams.Create(workstream.CreateRequest{ID: "ws_plan_approval", Title: "Plan approval"})
	if err != nil {
		t.Fatal(err)
	}
	setup := New(nil, "", 0)
	setup.SetWorkDir(workspace)
	plan := createWorkstreamPlanAPIForTest(t, setup, workspace, ws.ID, "plan_approval", "research")
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{
		Workspace: workspace, WorkstreamID: ws.ID, PlanID: plan.ID,
		PlanRevision: plan.Revision, StageID: "research", Provider: "fake", Model: "fake-model",
		Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "an answer without completion evidence"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workspace)
	server.SetSessionStore(store)
	body, err := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	unapproved := httptest.NewRecorder()
	server.handleAgentLoop(unapproved, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if unapproved.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(unapproved.Body.String(), `"code":"plan_approval_required"`) {
		t.Fatalf("unapproved plan status=%d provider calls=%d body=%s", unapproved.Code, provider.calls, unapproved.Body.String())
	}
	unchanged, err := workstreams.GetPlan(ws.ID, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != workstream.PlanStatusDraft || unchanged.StateRevision != plan.StateRevision {
		t.Fatalf("rejected unapproved run changed plan: %+v", unchanged)
	}

	plan, err = workstreams.ApprovePlan(ws.ID, plan.ID, workstream.ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	approvedRun := httptest.NewRecorder()
	server.handleAgentLoop(approvedRun, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if approvedRun.Code != http.StatusOK || provider.calls == 0 {
		t.Fatalf("approved plan status=%d provider calls=%d body=%s", approvedRun.Code, provider.calls, approvedRun.Body.String())
	}
	for _, want := range []string{
		"Plan ID: plan_approval (definition revision 1, state revision 3, status executing, approved revision 1)",
		"Current stage: research (running)",
		"Current stage attempt: run run_",
	} {
		if !strings.Contains(provider.systemPrompt, want) {
			t.Fatalf("Plan-bound Agent prompt is missing started state %q:\n%s", want, provider.systemPrompt)
		}
	}
	afterRun, err := workstreams.GetPlan(ws.ID, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRun.Status == workstream.PlanStatusCompleted || afterRun.Stages[0].Status == workstream.PlanStageStatusCompleted ||
		len(afterRun.Stages[0].Attempts) != 1 || afterRun.Stages[0].Attempts[0].Evidence != nil {
		t.Fatalf("plain provider response completed stage without receipt acceptance evidence: %+v", afterRun)
	}
}

func TestAgentLoopRejectsExplicitDurableWorkspaceMismatchBeforeDispatch(t *testing.T) {
	projectWorkspace := t.TempDir()
	otherWorkspace := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{
		Workspace: projectWorkspace, Provider: "fake", Model: "session-target-model",
		Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "must not run"}
	server := New(provider, "server-default-model", 0)
	server.SetWorkDir(projectWorkspace)
	server.SetSessionStore(store)
	body, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"workDir":          otherWorkspace,
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(recorder.Body.String(), `"code":"session_workspace_conflict"`) ||
		strings.Contains(recorder.Body.String(), `"type":"session"`) {
		t.Fatalf("status=%d provider calls=%d body=%s", recorder.Code, provider.calls, recorder.Body.String())
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != session.Revision || len(persisted.Messages) != 1 || persisted.Messages[0].Content != "hello" {
		t.Fatalf("rejected workspace request changed session: %#v", persisted)
	}
}

func TestAgentLoopReportsServerResolvedExecutionPolicy(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workDir := t.TempDir()
	provider := &agentLoopFakeProvider{text: "full mode answer"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	body := []byte(`{"messages":[{"role":"user","content":"hello"}],"executionPolicy":{"mode":"full","revision":999,"runtimeCapabilities":{"filesystemIsolation":true}}}`)
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := recorder.Body.String()
	if !strings.Contains(response, `"executionPolicy":{"mode":"full","revision":`) {
		t.Fatalf("initial session event omitted effective execution policy:\n%s", response)
	}
	if strings.Contains(response, `"revision":999`) {
		t.Fatalf("server trusted caller-supplied policy revision:\n%s", response)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
}

func TestAgentLoopRejectsUnknownExecutionPolicyMode(t *testing.T) {
	provider := &agentLoopFakeProvider{text: "must not run"}
	server := New(provider, "fake-model", 0)
	body := []byte(`{"messages":[{"role":"user","content":"hello"}],"executionPolicy":{"mode":"unrestricted"}}`)
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest || provider.calls != 0 ||
		!strings.Contains(recorder.Body.String(), `"code":"invalid_execution_policy"`) {
		t.Fatalf("status=%d provider calls=%d body=%s", recorder.Code, provider.calls, recorder.Body.String())
	}
}

func TestAgentLoopRejectsStaleDurableRevisionBeforeProviderCall(t *testing.T) {
	workDir := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "must not run"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	body, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision + 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body))
	recorder := httptest.NewRecorder()

	server.handleAgentLoop(recorder, req)
	if recorder.Code != http.StatusConflict || provider.calls != 0 {
		t.Fatalf("status=%d provider calls=%d body=%s", recorder.Code, provider.calls, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"session_revision_conflict"`) {
		t.Fatalf("typed conflict missing: %s", recorder.Body.String())
	}
}

func TestAgentLoopRejectsRuntimeQuarantinedSessionBeforeProviderCall(t *testing.T) {
	workDir := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &agentLoopFakeProvider{text: "must not run"}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	server.durableQuarantine.Store(session.ID, struct{}{})
	body, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusConflict || provider.calls != 0 ||
		!strings.Contains(recorder.Body.String(), `"code":"session_runtime_quarantined"`) {
		t.Fatalf("status=%d provider calls=%d body=%s", recorder.Code, provider.calls, recorder.Body.String())
	}
}
