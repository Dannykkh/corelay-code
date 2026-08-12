package server

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestDurableExecutionJournalSameRunParallelIsPersistedAndIdempotent(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "parallel"}}}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"parallel"`),
	}})
	if err != nil {
		t.Fatal(err)
	}

	const tools = 12
	start := make(chan struct{})
	errs := make(chan error, tools)
	var wait sync.WaitGroup
	for index := 0; index < tools; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			errs <- run.JournalToolExecution(agent.ToolExecutionJournalEntry{
				ID: "call_" + string(rune('a'+index)), Name: "Read", RunID: "run_parallel",
				InputDigest: "sha256:" + strings.Repeat(string(rune('a'+index%6)), 64),
			})
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("same-run journal = %v", err)
		}
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != tools+1 || !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.RunID != "run_parallel" ||
		persisted.Interruption.ToolName != "multiple_tools" || persisted.Interruption.ToolCallID != "multiple" ||
		persisted.Interruption.SideEffectState != agent.SessionSideEffectMayHaveApplied ||
		!strings.Contains(persisted.Interruption.Summary, "every side effect") {
		t.Fatalf("persisted run quarantine = %#v", persisted)
	}

	// Same-run idempotency is conditional on the exact marker still being on
	// disk. Clearing it behind this live run makes the next tool fail closed.
	if _, err := store.MarkReconciled(session.ID, persisted.Revision); err != nil {
		t.Fatal(err)
	}
	if err := run.JournalToolExecution(agent.ToolExecutionJournalEntry{
		ID: "call_after_clear", Name: "Read", RunID: "run_parallel",
		InputDigest: "sha256:" + strings.Repeat("f", 64),
	}); !errors.Is(err, agent.ErrSessionReconcileRequired) {
		t.Fatalf("journal after marker clear = %v, want reconcile required", err)
	}
}

func TestDurableExecutionJournalStaleOrDifferentRunNeverAdoptsRevision(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "race"}}}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	request := []types.Message{{Role: "user", Content: json.RawMessage(`"race"`)}}
	winner, err := prepareDurableAgentRun(store, session.ID, 1, workDir, request)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := prepareDurableAgentRun(store, session.ID, 1, workDir, request)
	if err != nil {
		t.Fatal(err)
	}
	winnerEntry := agent.ToolExecutionJournalEntry{
		ID: "winner", Name: "Bash", RunID: "run_winner",
		InputDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := winner.JournalToolExecution(winnerEntry); err != nil {
		t.Fatal(err)
	}
	if err := stale.JournalToolExecution(agent.ToolExecutionJournalEntry{
		ID: "stale", Name: "Bash", RunID: "run_stale",
		InputDigest: "sha256:" + strings.Repeat("b", 64),
	}); !errors.Is(err, agent.ErrSessionRevisionConflict) {
		t.Fatalf("stale journal = %v, want revision conflict", err)
	}
	if stale.expectedRevision != 1 || stale.session.ReconcileRequired {
		t.Fatalf("stale run adopted winner state: revision=%d session=%#v", stale.expectedRevision, stale.session)
	}
	if err := winner.JournalToolExecution(agent.ToolExecutionJournalEntry{
		ID: "different", Name: "Bash", RunID: "run_other",
		InputDigest: "sha256:" + strings.Repeat("c", 64),
	}); !errors.Is(err, agent.ErrSessionReconcileRequired) {
		t.Fatalf("different run journal = %v, want reconcile required", err)
	}
	persisted, err := store.Get(session.ID)
	if err != nil || persisted.Interruption == nil || persisted.Interruption.RunID != winnerEntry.RunID || persisted.Revision != 2 {
		t.Fatalf("winner marker changed: session=%#v err=%v", persisted, err)
	}
}

func TestDurableExecutionJournalFailureDoesNotAdvanceState(t *testing.T) {
	base := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "fail"}}}
	if err := base.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	secret := "C:/private/session/path"
	store := &failingDurableInterruptionStore{SessionStore: base, err: errors.New(secret)}
	run, err := prepareDurableAgentRun(store, session.ID, 1, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"fail"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.JournalToolExecution(agent.ToolExecutionJournalEntry{
		ID: "call", Name: "Bash", RunID: "run_failure",
		InputDigest: "sha256:" + strings.Repeat("d", 64),
	}); err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("journal persistence error = %v", err)
	}
	persisted, err := base.Get(session.ID)
	if err != nil || persisted.Revision != 1 || persisted.ReconcileRequired || persisted.Interruption != nil {
		t.Fatalf("failed journal changed state: session=%#v err=%v", persisted, err)
	}
}

func TestDurableExecutionJournalSuccessfulTerminalClearsFirstRunMarkerAtomically(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "two tools"}}}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, 1, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"two tools"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	entries := []agent.ToolExecutionJournalEntry{
		{ID: "call_a", Name: "Read", RunID: "run_two", InputDigest: "sha256:" + strings.Repeat("a", 64)},
		{ID: "call_b", Name: "Read", RunID: "run_two", InputDigest: "sha256:" + strings.Repeat("b", 64)},
	}
	for _, entry := range entries {
		if err := run.JournalToolExecution(entry); err != nil {
			t.Fatal(err)
		}
		run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": entry.ID, "name": entry.Name, "runId": entry.RunID, "inputDigest": entry.InputDigest,
		}})
		run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{
			"id": entry.ID, "name": entry.Name, "result": "ok", "isError": false, "executed": true,
		}})
	}
	run.Observe(agent.Event{Type: "done", Data: agent.DurableRunTerminalMetadata{
		TerminalState: agent.EvidenceTerminalVerified,
	}})
	committed, err := run.Finalize("provider", "model")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 4 || committed.ReconcileRequired || committed.Interruption != nil ||
		committed.LastRunTerminal == nil || len(committed.Messages) != 3 {
		t.Fatalf("successful atomic clear = %#v", committed)
	}
}

func TestDurableExecutionJournalGracefulFailureEnrichesPersistedAggregate(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "aggregate"}}}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, 1, workDir, []types.Message{{
		Role: "user", Content: json.RawMessage(`"aggregate"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	entries := []agent.ToolExecutionJournalEntry{
		{ID: "call_b", Name: "Read", RunID: "run_enrich", InputDigest: "sha256:" + strings.Repeat("b", 64)},
		{ID: "call_a", Name: "Read", RunID: "run_enrich", InputDigest: "sha256:" + strings.Repeat("a", 64)},
	}
	for _, entry := range entries {
		if err := run.JournalToolExecution(entry); err != nil {
			t.Fatal(err)
		}
		run.Observe(agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": entry.ID, "name": entry.Name, "runId": entry.RunID, "inputDigest": entry.InputDigest,
		}})
		run.Observe(agent.Event{Type: "tool_result", Data: map[string]any{
			"id": entry.ID, "name": entry.Name, "result": "ok", "isError": false, "executed": true,
		}})
	}
	preEnrichment, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preEnrichment.Interruption == nil || preEnrichment.Interruption.SideEffectState != agent.SessionSideEffectMayHaveApplied {
		t.Fatalf("pre-execution aggregate = %#v", preEnrichment)
	}
	want := run.observer.Interruption("graceful cancellation after completed tool results")
	if want == nil {
		t.Fatal("observer did not produce aggregate marker")
	}
	want.At = preEnrichment.Interruption.At
	updated, err := run.markReconciliation(*want)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 4 || updated.Interruption == nil ||
		updated.Interruption.SideEffectState != agent.SessionSideEffectApplied ||
		!sameDurableInterruption(updated.Interruption, want) {
		t.Fatalf("enriched aggregate = %#v want=%#v", updated, want)
	}
}
