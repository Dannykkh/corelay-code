package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionStoreForkCreatesIndependentImmutableSnapshot(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	parent := &Session{
		Workspace: workspace,
		Title:     "parent title",
		Provider:  "ollama",
		Model:     "qwen",
		Messages: []SessionMessage{{
			Role:    "tool",
			Content: "tool call",
			ToolInput: map[string]any{
				"path":   "main.go",
				"nested": map[string]any{"value": "original"},
			},
		}},
	}
	if err := store.Save(parent); err != nil {
		t.Fatal(err)
	}
	parentBefore, err := store.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.Fork(parent.ID, parent.Revision)
	if err != nil {
		t.Fatalf("Fork(first) = %v", err)
	}
	second, err := store.Fork(parent.ID, parent.Revision)
	if err != nil {
		t.Fatalf("Fork(second) = %v", err)
	}
	if first.ID == parent.ID || first.ID == second.ID {
		t.Fatalf("fork IDs are not unique: parent=%s first=%s second=%s", parent.ID, first.ID, second.ID)
	}
	for _, fork := range []*Session{first, second} {
		if fork.Version != currentSessionVersion || fork.Revision != 1 || fork.LastCommittedRevision != 1 {
			t.Fatalf("fork version metadata = v%d/r%d/committed%d", fork.Version, fork.Revision, fork.LastCommittedRevision)
		}
		if fork.ParentSessionID != parent.ID || fork.ParentRevision != parent.Revision {
			t.Fatalf("fork lineage = %s@%d", fork.ParentSessionID, fork.ParentRevision)
		}
		if fork.LifecycleStatus != SessionLifecycleActive || fork.ReconcileRequired || fork.Interruption != nil {
			t.Fatalf("fork lifecycle = %+v", sessionResumeState(*fork))
		}
		if fork.Workspace != parent.Workspace || fork.Provider != parent.Provider || fork.Model != parent.Model {
			t.Fatalf("fork identity snapshot = %+v", fork)
		}
	}

	first.Messages[0].Content = "changed child"
	firstInput := first.Messages[0].ToolInput.(map[string]any)
	firstInput["nested"].(map[string]any)["value"] = "changed child"
	if err := store.SaveExpected(first, 1); err != nil {
		t.Fatalf("SaveExpected(fork) = %v", err)
	}

	parentAfter, err := store.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondAfter, err := store.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter.Revision != parentBefore.Revision || parentAfter.Messages[0].Content != parentBefore.Messages[0].Content {
		t.Fatalf("fork mutated parent: before=%+v after=%+v", parentBefore, parentAfter)
	}
	parentNested := parentAfter.Messages[0].ToolInput.(map[string]any)["nested"].(map[string]any)["value"]
	secondNested := secondAfter.Messages[0].ToolInput.(map[string]any)["nested"].(map[string]any)["value"]
	if parentNested != "original" || secondNested != "original" {
		t.Fatalf("fork snapshots share nested tool input: parent=%v second=%v", parentNested, secondNested)
	}
}

func TestSessionStoreForkTypedParentFailures(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	parent := &Session{Workspace: workspace, Title: "parent"}
	if err := store.Save(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fork(parent.ID, parent.Revision+1); !errors.Is(err, ErrSessionRevisionConflict) {
		t.Fatalf("Fork(stale) = %v, want ErrSessionRevisionConflict", err)
	} else {
		var conflict *SessionRevisionConflictError
		if !errors.As(err, &conflict) || conflict.Expected != 2 || conflict.Current != 1 {
			t.Fatalf("stale fork conflict = %#v", conflict)
		}
	}

	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	legacyID := opaqueTestSessionID("d")
	writeLegacySession(t, dir, Session{ID: legacyID, Workspace: workspace, Title: "legacy"})
	if _, err := store.Fork(legacyID, 0); !errors.Is(err, ErrSessionVersionUnsupported) {
		t.Fatalf("Fork(legacy) = %v, want ErrSessionVersionUnsupported", err)
	}

	unsupportedID := opaqueTestSessionID("e")
	unsupported, err := json.Marshal(Session{
		Version:   currentSessionVersion + 1,
		Revision:  1,
		ID:        unsupportedID,
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, unsupportedID+".json"), unsupported, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fork(unsupportedID, 1); !errors.Is(err, ErrSessionVersionUnsupported) {
		t.Fatalf("Fork(unsupported) = %v, want ErrSessionVersionUnsupported", err)
	}

	corruptID := opaqueTestSessionID("f")
	if err := os.WriteFile(filepath.Join(dir, corruptID+".json"), []byte(`{"version":1`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fork(corruptID, 1); !errors.Is(err, ErrSessionCorrupt) {
		t.Fatalf("Fork(corrupt) = %v, want ErrSessionCorrupt", err)
	}
}

func TestSessionStoreConcurrentForksAreUnique(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	stores := []*SessionStore{NewSessionStore(base), NewSessionStore(base)}
	parent := &Session{Workspace: workspace, Title: "parent"}
	if err := stores[0].Save(parent); err != nil {
		t.Fatal(err)
	}

	const forks = 32
	start := make(chan struct{})
	results := make(chan *Session, forks)
	errs := make(chan error, forks)
	var wg sync.WaitGroup
	for i := 0; i < forks; i++ {
		wg.Add(1)
		go func(store *SessionStore) {
			defer wg.Done()
			<-start
			fork, err := store.Fork(parent.ID, 1)
			if err != nil {
				errs <- err
				return
			}
			results <- fork
		}(stores[i%len(stores)])
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Fork = %v", err)
	}
	ids := make(map[string]struct{}, forks)
	for fork := range results {
		if _, duplicate := ids[fork.ID]; duplicate {
			t.Fatalf("duplicate fork ID %s", fork.ID)
		}
		ids[fork.ID] = struct{}{}
	}
	if len(ids) != forks {
		t.Fatalf("fork count = %d, want %d", len(ids), forks)
	}
	gotParent, err := stores[1].Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.Revision != 1 || gotParent.Title != "parent" {
		t.Fatalf("concurrent forks mutated parent: %+v", gotParent)
	}
}

func TestSessionStoreForkAndParentSaveAreLinearizable(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		base := t.TempDir()
		workspace := filepath.Join(base, "workspace")
		forkStore := NewSessionStore(base)
		saveStore := NewSessionStore(base)
		parent := &Session{Workspace: workspace, Title: "before"}
		if err := forkStore.Save(parent); err != nil {
			t.Fatal(err)
		}
		update, err := saveStore.Get(parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		update.Title = "after"

		start := make(chan struct{})
		var fork *Session
		var forkErr, saveErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			fork, forkErr = forkStore.Fork(parent.ID, 1)
		}()
		go func() {
			defer wg.Done()
			<-start
			saveErr = saveStore.SaveExpected(update, 1)
		}()
		close(start)
		wg.Wait()
		if saveErr != nil {
			t.Fatalf("iteration %d SaveExpected = %v", iteration, saveErr)
		}
		switch {
		case forkErr == nil:
			if fork.ParentRevision != 1 || fork.Title != "before" {
				t.Fatalf("iteration %d fork snapshot = %+v", iteration, fork)
			}
		case errors.Is(forkErr, ErrSessionRevisionConflict):
			var conflict *SessionRevisionConflictError
			if !errors.As(forkErr, &conflict) || conflict.Current != 2 {
				t.Fatalf("iteration %d conflict = %#v", iteration, conflict)
			}
		default:
			t.Fatalf("iteration %d Fork = %v", iteration, forkErr)
		}
		persisted, err := forkStore.Get(parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Revision != 2 || persisted.Title != "after" {
			t.Fatalf("iteration %d parent = %+v", iteration, persisted)
		}
	}
}

func TestSessionStoreInterruptionRequiresExplicitReconciliation(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	sess := &Session{Workspace: workspace, Title: "active"}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if sess.LifecycleStatus != SessionLifecycleActive || sess.LastCommittedRevision != 1 {
		t.Fatalf("new lifecycle = %+v", sessionResumeState(*sess))
	}

	marker := SessionInterruption{
		At:         time.Date(2026, 8, 12, 3, 4, 5, 0, time.FixedZone("test", 9*60*60)),
		Reason:     "client disconnected after dispatch",
		ToolName:   "bash_exec",
		ToolCallID: "tool-1",
	}
	interrupted, err := store.MarkInterrupted(sess.ID, 1, marker)
	if err != nil {
		t.Fatalf("MarkInterrupted = %v", err)
	}
	if interrupted.Revision != 2 || interrupted.LastCommittedRevision != 1 ||
		interrupted.LifecycleStatus != SessionLifecycleInterrupted || !interrupted.ReconcileRequired ||
		interrupted.Interruption == nil || interrupted.Interruption.ToolCallID != "tool-1" ||
		interrupted.Interruption.At.Location() != time.UTC {
		t.Fatalf("interrupted lifecycle = %+v", interrupted)
	}

	blocked := *interrupted
	blocked.Title = "must not save"
	if err := store.SaveExpected(&blocked, 2); !errors.Is(err, ErrSessionReconcileRequired) {
		t.Fatalf("SaveExpected(interrupted) = %v", err)
	}
	if _, err := store.Fork(sess.ID, 2); !errors.Is(err, ErrSessionReconcileRequired) {
		t.Fatalf("Fork(interrupted) = %v", err)
	}
	state, err := store.ResumeState(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ReconcileRequired || state.LastCommittedRevision != 1 || state.Interruption == nil {
		t.Fatalf("ResumeState = %+v", state)
	}
	if _, err := store.MarkReconciled(sess.ID, 1); !errors.Is(err, ErrSessionRevisionConflict) {
		t.Fatalf("MarkReconciled(stale) = %v", err)
	}

	reconciled, err := store.MarkReconciled(sess.ID, 2)
	if err != nil {
		t.Fatalf("MarkReconciled = %v", err)
	}
	if reconciled.Revision != 3 || reconciled.LastCommittedRevision != 3 ||
		reconciled.LifecycleStatus != SessionLifecycleActive || reconciled.ReconcileRequired || reconciled.Interruption != nil {
		t.Fatalf("reconciled lifecycle = %+v", reconciled)
	}
	closed, err := store.Close(sess.ID, 3)
	if err != nil {
		t.Fatalf("Close = %v", err)
	}
	if closed.Revision != 4 || closed.LastCommittedRevision != 4 || closed.LifecycleStatus != SessionLifecycleClosed {
		t.Fatalf("closed lifecycle = %+v", closed)
	}
	if _, err := store.MarkInterrupted(sess.ID, 4, marker); !errors.Is(err, ErrSessionLifecycleInvalid) {
		t.Fatalf("MarkInterrupted(closed) = %v", err)
	}
	if err := store.SaveExpected(closed, 4); !errors.Is(err, ErrSessionLifecycleInvalid) {
		t.Fatalf("SaveExpected(closed) = %v", err)
	}
}

func TestSessionStoreLifecycleBackwardCompatibilityAndRecoveryMetadata(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	legacyVersioned := Session{
		Version:   currentSessionVersion,
		Revision:  1,
		ID:        opaqueTestSessionID("g"),
		Workspace: workspace,
		Title:     "pre-lifecycle",
	}
	writeLegacySession(t, dir, legacyVersioned)
	loaded, err := store.Get(legacyVersioned.ID)
	if err != nil {
		t.Fatalf("Get(pre-lifecycle) = %v", err)
	}
	if loaded.LifecycleStatus != SessionLifecycleActive || loaded.LastCommittedRevision != 1 || loaded.ReconcileRequired {
		t.Fatalf("normalized lifecycle = %+v", loaded)
	}

	corruptID := opaqueTestSessionID("h")
	corrupt := Session{
		Version:               currentSessionVersion,
		Revision:              2,
		ID:                    corruptID,
		Workspace:             workspace,
		LifecycleStatus:       SessionLifecycleInterrupted,
		LastCommittedRevision: 1,
		ReconcileRequired:     true,
	}
	data, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, corruptID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Get(corruptID)
	if !errors.Is(err, ErrSessionCorrupt) {
		t.Fatalf("Get(invalid interrupted metadata) = %v", err)
	}
	recovery, ok := SessionRecoveryFromError(err)
	if !ok || recovery.LifecycleStatus != SessionLifecycleRecoveryNeeded || recovery.LastCommittedRevision != 1 {
		t.Fatalf("recovery metadata = %+v, ok=%v", recovery, ok)
	}
}

func TestSessionInterruptionStructuredMetadataValidationAndNoMutation(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	sess := &Session{Workspace: filepath.Join(base, "workspace")}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	marker := SessionInterruption{
		RunID:           "run-1",
		ToolName:        "bash_exec",
		ToolCallID:      "tool-1",
		InputDigest:     "sha256:" + strings.Repeat("a", 64),
		SideEffectState: SessionSideEffectMayHaveApplied,
		Summary:         "process dispatched; completion unknown",
	}
	original := marker
	updated, err := store.MarkInterrupted(sess.ID, sess.Revision, marker)
	if err != nil {
		t.Fatalf("MarkInterrupted = %v", err)
	}
	if marker != original {
		t.Fatalf("MarkInterrupted mutated input: got=%+v want=%+v", marker, original)
	}
	if updated.Interruption == nil || updated.Interruption.At.IsZero() ||
		updated.Interruption.SideEffectState != SessionSideEffectMayHaveApplied {
		t.Fatalf("persisted marker = %+v", updated.Interruption)
	}
	updated.Interruption.Summary = "mutated caller copy"
	state, err := store.ResumeState(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Interruption == nil || state.Interruption.Summary != original.Summary {
		t.Fatalf("stored marker mutated through result: %+v", state.Interruption)
	}

	validDigest := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name   string
		marker SessionInterruption
	}{
		{
			name: "incomplete structured metadata",
			marker: SessionInterruption{
				RunID: "run", ToolName: "bash_exec",
			},
		},
		{
			name: "invalid digest",
			marker: SessionInterruption{
				RunID: "run", ToolName: "bash_exec", InputDigest: "sha256:ABC",
				SideEffectState: SessionSideEffectUnknown, Summary: "redacted",
			},
		},
		{
			name: "invalid side effect state",
			marker: SessionInterruption{
				RunID: "run", ToolName: "bash_exec", InputDigest: validDigest,
				SideEffectState: "completed_without_evidence", Summary: "redacted",
			},
		},
		{
			name: "oversized summary",
			marker: SessionInterruption{
				RunID: "run", ToolName: "bash_exec", InputDigest: validDigest,
				SideEffectState: SessionSideEffectStarted, Summary: strings.Repeat("x", 1025),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateSessionInterruption(test.marker); !errors.Is(err, ErrSessionLifecycleInvalid) {
				t.Fatalf("ValidateSessionInterruption() = %v", err)
			}
		})
	}

	corrupt := *updated
	corrupt.Interruption = cloneSessionInterruption(updated.Interruption)
	corrupt.Interruption.InputDigest = "sha256:invalid"
	data, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.workspaceDir(corrupt.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.sessionPath(dir, corrupt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(corrupt.ID); !errors.Is(err, ErrSessionCorrupt) {
		t.Fatalf("Get(invalid persisted marker) = %v", err)
	}
}
