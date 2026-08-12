package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCommitInterruptedRunAtomicallyCommitsAndClearsExactMarker(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	workspace := t.TempDir()
	session := Session{
		Workspace: workspace,
		Messages:  []SessionMessage{{Role: "user", Content: "run a tool"}},
	}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	marker := SessionInterruption{
		RunID:           "run_exact_commit",
		ToolName:        "Bash",
		ToolCallID:      "call_1",
		InputDigest:     "sha256:" + strings.Repeat("a", 64),
		SideEffectState: SessionSideEffectStarted,
		Summary:         "Authorized tool execution started; marker quarantines the entire run.",
	}
	interrupted, err := store.MarkInterrupted(session.ID, session.Revision, marker)
	if err != nil {
		t.Fatal(err)
	}
	candidate := *interrupted
	candidate.Messages = append(candidate.Messages, SessionMessage{Role: "assistant", Content: "done"})
	candidate.LastRunTerminal = &DurableRunTerminalMetadata{TerminalState: EvidenceTerminalVerified}
	if err := store.CommitInterruptedRun(&candidate, interrupted.Revision, *interrupted.Interruption); err != nil {
		t.Fatal(err)
	}
	if candidate.Revision != interrupted.Revision+1 || candidate.LastCommittedRevision != candidate.Revision ||
		candidate.LifecycleStatus != SessionLifecycleActive || candidate.ReconcileRequired ||
		candidate.Interruption != nil || len(candidate.Messages) != 2 || candidate.LastRunTerminal == nil {
		t.Fatalf("atomic commit = %#v", candidate)
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != candidate.Revision || persisted.ReconcileRequired || persisted.Interruption != nil ||
		len(persisted.Messages) != 2 || persisted.LastRunTerminal == nil {
		t.Fatalf("persisted commit = %#v", persisted)
	}
}

func TestUpdateInterruptedRunBuildsScheduleIndependentAggregate(t *testing.T) {
	entries := []ToolExecutionJournalEntry{
		{ID: "call_b", Name: "Grep", RunID: "run_aggregate", InputDigest: "sha256:" + strings.Repeat("b", 64)},
		{ID: "call_a", Name: "Read", RunID: "run_aggregate", InputDigest: "sha256:" + strings.Repeat("a", 64)},
	}
	at := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	forward, err := AggregateToolExecutionJournal(entries, at, "run quarantine")
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := AggregateToolExecutionJournal([]ToolExecutionJournalEntry{entries[1], entries[0]}, at, "run quarantine")
	if err != nil {
		t.Fatal(err)
	}
	if !sameSessionInterruption(&forward, &reverse) || forward.ToolName != "multiple_tools" ||
		forward.ToolCallID != "multiple" || forward.SideEffectState != SessionSideEffectMayHaveApplied {
		t.Fatalf("schedule-dependent aggregate: forward=%#v reverse=%#v", forward, reverse)
	}
	if _, err := AggregateToolExecutionJournal([]ToolExecutionJournalEntry{{
		Name: "Read", RunID: "run_aggregate", InputDigest: "sha256:" + strings.Repeat("a", 64),
	}}, at, "run quarantine"); !errors.Is(err, ErrSessionLifecycleInvalid) {
		t.Fatalf("empty tool call id = %v", err)
	}

	store := NewSessionStore(t.TempDir())
	session := Session{Workspace: t.TempDir(), Messages: []SessionMessage{{Role: "user", Content: "parallel"}}}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}
	first, err := AggregateToolExecutionJournal(entries[:1], time.Time{}, "run quarantine")
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := store.MarkInterrupted(session.ID, 1, first)
	if err != nil {
		t.Fatal(err)
	}
	forward.At = interrupted.Interruption.At
	updated, err := store.UpdateInterruptedRun(session.ID, 2, *interrupted.Interruption, forward)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 3 || updated.LastCommittedRevision != 1 || updated.Interruption == nil ||
		!sameSessionInterruption(updated.Interruption, &forward) {
		t.Fatalf("aggregate update = %#v", updated)
	}
	wrong := *updated.Interruption
	wrong.InputDigest = "sha256:" + strings.Repeat("f", 64)
	if _, err := store.UpdateInterruptedRun(session.ID, 3, wrong, forward); !errors.Is(err, ErrSessionReconcileRequired) {
		t.Fatalf("wrong expected marker = %v", err)
	}
}

func TestCommitInterruptedRunRejectsStaleWrongMarkerAndIdentityChanges(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Session, *SessionInterruption)
		expected  uint64
		wantError error
	}{
		{
			name: "stale revision", expected: 1, wantError: ErrSessionRevisionConflict,
		},
		{
			name:      "different tool marker",
			mutate:    func(_ *Session, marker *SessionInterruption) { marker.ToolCallID = "call_other" },
			wantError: ErrSessionReconcileRequired,
		},
		{
			name:      "workspace change",
			mutate:    func(candidate *Session, _ *SessionInterruption) { candidate.Workspace = t.TempDir() },
			wantError: ErrSessionConflict,
		},
		{
			name: "parent change",
			mutate: func(candidate *Session, _ *SessionInterruption) {
				candidate.ParentSessionID = "sess_aaaaaaaaaaaaaaaaaaaaaaaaaa"
				candidate.ParentRevision = 1
			},
			wantError: ErrSessionParentInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewSessionStore(t.TempDir())
			session := Session{Workspace: t.TempDir(), Messages: []SessionMessage{{Role: "user", Content: "run"}}}
			if err := store.SaveExpected(&session, 0); err != nil {
				t.Fatal(err)
			}
			interrupted, err := store.MarkInterrupted(session.ID, session.Revision, SessionInterruption{
				RunID: "run_guarded", ToolName: "Bash", ToolCallID: "call_1",
				InputDigest:     "sha256:" + strings.Repeat("b", 64),
				SideEffectState: SessionSideEffectStarted, Summary: "run quarantine",
			})
			if err != nil {
				t.Fatal(err)
			}
			candidate := *interrupted
			candidate.Messages = append(candidate.Messages, SessionMessage{Role: "assistant", Content: "must not commit"})
			marker := *interrupted.Interruption
			if test.mutate != nil {
				test.mutate(&candidate, &marker)
			}
			expected := test.expected
			if expected == 0 {
				expected = interrupted.Revision
			}
			if err := store.CommitInterruptedRun(&candidate, expected, marker); !errors.Is(err, test.wantError) {
				t.Fatalf("CommitInterruptedRun() = %v, want %v", err, test.wantError)
			}
			persisted, err := store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Revision != interrupted.Revision || !persisted.ReconcileRequired ||
				persisted.Interruption == nil || len(persisted.Messages) != 1 {
				t.Fatalf("failed commit changed guarded state: %#v", persisted)
			}
		})
	}
}
