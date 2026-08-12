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
	"github.com/Dannykkh/corelay-code/internal/types"
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
