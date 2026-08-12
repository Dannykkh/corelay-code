package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

var (
	_ acp.ResumeSessionBackend = (*Backend)(nil)
	_ acp.DeleteSessionBackend = (*Backend)(nil)
)

func TestACPStdioAdvertisesResumeDeleteAndRejectsStaleLoadedDelete(t *testing.T) {
	stateRoot := t.TempDir()
	workspace := t.TempDir()
	store := agent.NewSessionStore(stateRoot)
	backend := newSessionSurfaceBackend(t, store)
	harness := newStdioHarness(t, backend)
	defer harness.close(t)

	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": acp.MethodInitialize,
		"params": map[string]any{"protocolVersion": 1},
	})
	_, initialized := harness.response(t, "1")
	if initialized.Error != nil {
		t.Fatalf("initialize error = %#v", initialized.Error)
	}
	var initialize acp.InitializeResponse
	if err := json.Unmarshal(initialized.Result, &initialize); err != nil {
		t.Fatal(err)
	}
	caps := initialize.AgentCapabilities.SessionCapabilities
	if !initialize.AgentCapabilities.LoadSession || caps.List == nil || caps.Resume == nil || caps.Delete == nil || caps.Close == nil {
		t.Fatalf("advertised session capabilities = %#v", initialize.AgentCapabilities)
	}

	sessionID := newStdioSession(t, harness, workspace)
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": acp.MethodSessionClose,
		"params": map[string]any{"sessionId": sessionID},
	})
	if _, frame := harness.response(t, "3"); frame.Error != nil {
		t.Fatalf("session/close error = %#v", frame.Error)
	}
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": acp.MethodSessionResume,
		"params": map[string]any{"sessionId": sessionID, "cwd": workspace, "mcpServers": []any{}},
	})
	if _, frame := harness.response(t, "4"); frame.Error != nil {
		t.Fatalf("session/resume error = %#v", frame.Error)
	}

	newer, err := store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	newer.Title = "newer HTTP/Web-style commit"
	if err := store.SaveExpected(newer, newer.Revision); err != nil {
		t.Fatal(err)
	}
	if newer.Revision != 2 {
		t.Fatalf("external revision = %d, want 2", newer.Revision)
	}

	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": acp.MethodSessionDelete,
		"params": map[string]any{"sessionId": sessionID},
	})
	observed, staleDelete := harness.response(t, "5")
	if staleDelete.Error == nil || staleDelete.Error.Code != acp.CodeInvalidRequest {
		t.Fatalf("stale session/delete response = %#v", staleDelete)
	}
	if strings.Contains(string(observed), stateRoot) || strings.Contains(string(observed), "sourcePath") || strings.Contains(string(observed), "quarantinePath") {
		t.Fatalf("stale delete leaked a storage path: %s", observed)
	}
	preserved, err := store.Get(sessionID)
	if err != nil || preserved.Revision != 2 || preserved.Title != newer.Title {
		t.Fatalf("newer session was not preserved: session=%+v err=%v", preserved, err)
	}

	// Detach the stale ACP runtime, then delete the freshly observed revision.
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": acp.MethodSessionClose,
		"params": map[string]any{"sessionId": sessionID},
	})
	if _, frame := harness.response(t, "6"); frame.Error != nil {
		t.Fatalf("second session/close error = %#v", frame.Error)
	}
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": acp.MethodSessionDelete,
		"params": map[string]any{"sessionId": sessionID},
	})
	if _, frame := harness.response(t, "7"); frame.Error != nil {
		t.Fatalf("fresh session/delete error = %#v", frame.Error)
	}
	if _, err := store.Get(sessionID); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("deleted session Get = %v, want ErrSessionNotFound", err)
	}
}

func TestACPResumeForkPreservesLineageAndChildIsolation(t *testing.T) {
	stateRoot := t.TempDir()
	workspace := t.TempDir()
	store := agent.NewSessionStore(stateRoot)
	parent := &agent.Session{
		Workspace: workspace,
		Title:     "parent",
		Provider:  "fake",
		Model:     "model-a",
		Messages: []agent.SessionMessage{{
			Role: "user", Content: "parent message",
			ToolInput: map[string]any{"nested": map[string]any{"value": "parent"}},
		}},
	}
	if err := store.SaveExpected(parent, 0); err != nil {
		t.Fatal(err)
	}
	child, err := store.Fork(parent.ID, parent.Revision)
	if err != nil {
		t.Fatal(err)
	}

	backend := newSessionSurfaceBackend(t, store)
	client := &recordingClient{}
	if _, err := backend.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionID: child.ID, CWD: workspace, MCPServers: []acp.MCPServer{},
	}, client); err != nil {
		t.Fatalf("ResumeSession(fork) = %v", err)
	}
	backend.mu.Lock()
	loaded := cloneSession(backend.sessions[child.ID].persisted)
	backend.mu.Unlock()
	if loaded.ParentSessionID != parent.ID || loaded.ParentRevision != parent.Revision || loaded.Revision != 1 {
		t.Fatalf("resumed fork lineage = %+v", loaded)
	}
	updates, _ := client.snapshot()
	if len(updates) == 0 {
		t.Fatal("resumed fork did not replay its committed history")
	}

	childCopy, err := store.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	childCopy.Messages[0].Content = "child-only change"
	if err := store.SaveExpected(childCopy, childCopy.Revision); err != nil {
		t.Fatal(err)
	}
	parentAfter, err := store.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter.Revision != 1 || parentAfter.Messages[0].Content != "parent message" ||
		parentAfter.ParentSessionID != "" || parentAfter.ParentRevision != 0 {
		t.Fatalf("child mutation changed immutable parent: %+v", parentAfter)
	}
	childAfter, err := store.Get(child.ID)
	if err != nil || childAfter.ParentSessionID != parent.ID || childAfter.ParentRevision != 1 || childAfter.Revision != 2 {
		t.Fatalf("child lineage after mutation = %+v err=%v", childAfter, err)
	}
}

func TestACPDeleteAndExternalSaveExpectedHaveSingleWinner(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	backend := newSessionSurfaceBackend(t, store)
	for iteration := 0; iteration < 16; iteration++ {
		workspace := t.TempDir()
		created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
			CWD: workspace, MCPServers: []acp.MCPServer{},
		}, &recordingClient{})
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := store.Get(created.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Title = "external winner"

		start := make(chan struct{})
		var wait sync.WaitGroup
		var deleteErr, saveErr error
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_, deleteErr = backend.DeleteSession(context.Background(), acp.DeleteSessionRequest{
				SessionID: created.SessionID,
			}, &recordingClient{})
		}()
		go func() {
			defer wait.Done()
			<-start
			saveErr = store.SaveExpected(candidate, 1)
		}()
		close(start)
		wait.Wait()

		winners := 0
		if deleteErr == nil {
			winners++
		}
		if saveErr == nil {
			winners++
		}
		if winners != 1 {
			t.Fatalf("iteration %d winners=%d deleteErr=%v saveErr=%v", iteration, winners, deleteErr, saveErr)
		}
		if deleteErr == nil {
			if !errors.Is(saveErr, agent.ErrSessionRevisionConflict) {
				t.Fatalf("iteration %d losing save error = %v", iteration, saveErr)
			}
			if _, err := store.Get(created.SessionID); !errors.Is(err, agent.ErrSessionNotFound) {
				t.Fatalf("iteration %d delete winner left session: %v", iteration, err)
			}
			continue
		}
		var rpcErr *acp.RPCError
		if !errors.As(deleteErr, &rpcErr) || rpcErr.Code != acp.CodeInvalidRequest {
			t.Fatalf("iteration %d losing delete error = %v", iteration, deleteErr)
		}
		persisted, err := store.Get(created.SessionID)
		if err != nil || persisted.Revision != 2 || persisted.Title != candidate.Title {
			t.Fatalf("iteration %d save winner = %+v err=%v", iteration, persisted, err)
		}
	}
}

func TestACPStoreErrorMappingAndCorruptResumeDoNotLeakPaths(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "private", "session.json")
	for _, test := range []struct {
		name string
		err  error
		code int
	}{
		{name: "invalid id", err: agent.ErrInvalidSessionID, code: acp.CodeInvalidParams},
		{name: "not found", err: agent.ErrSessionNotFound, code: acp.CodeResourceNotFound},
		{name: "revision", err: &agent.SessionRevisionConflictError{SessionID: "sess_aaaaaaaaaaaaaaaaaaaaaaaaaa", Expected: 1, Current: 2}, code: acp.CodeInvalidRequest},
		{name: "corrupt", err: fmt.Errorf("load %s: %w", secretPath, agent.ErrSessionCorrupt), code: acp.CodeInvalidRequest},
		{name: "internal", err: fmt.Errorf("open %s", secretPath), code: acp.CodeInternalError},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rpcErr *acp.RPCError
			if err := mapStoreError(test.err); !errors.As(err, &rpcErr) || rpcErr.Code != test.code {
				t.Fatalf("mapStoreError(%v) = %#v", test.err, err)
			}
			if strings.Contains(rpcErr.Message, secretPath) || strings.Contains(rpcErr.Message, "sourcePath") || strings.Contains(rpcErr.Message, "quarantinePath") {
				t.Fatalf("mapped error leaked a path: %#v", rpcErr)
			}
		})
	}

	stateRoot := t.TempDir()
	workspace := t.TempDir()
	store := agent.NewSessionStore(stateRoot)
	session := &agent.Session{Workspace: workspace, Provider: "fake", Model: "model-a"}
	if err := store.SaveExpected(session, 0); err != nil {
		t.Fatal(err)
	}
	path := findACPStoredSessionPath(t, stateRoot, session.ID)
	secret := "corrupt-session-secret"
	corrupt := []byte(`{"id":"` + session.ID + `","workspace":"` + filepath.ToSlash(workspace) + `","secret":"` + secret + `"`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	backend := newSessionSurfaceBackend(t, store)
	harness := newStdioHarness(t, backend)
	defer harness.close(t)
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": acp.MethodInitialize,
		"params": map[string]any{"protocolVersion": 1},
	})
	if _, initialized := harness.response(t, "1"); initialized.Error != nil {
		t.Fatalf("initialize error = %#v", initialized.Error)
	}
	wireCases := []struct {
		id     int
		method string
		params map[string]any
		code   int
	}{
		{id: 2, method: acp.MethodSessionDelete, params: map[string]any{"sessionId": "../outside"}, code: acp.CodeInvalidParams},
		{id: 3, method: acp.MethodSessionDelete, params: map[string]any{"sessionId": "sess_aaaaaaaaaaaaaaaaaaaaaaaaaa"}, code: acp.CodeResourceNotFound},
		{id: 4, method: acp.MethodSessionResume, params: map[string]any{
			"sessionId": session.ID, "cwd": workspace, "mcpServers": []any{},
		}, code: acp.CodeInvalidRequest},
	}
	for _, test := range wireCases {
		harness.send(t, map[string]any{
			"jsonrpc": "2.0", "id": test.id, "method": test.method, "params": test.params,
		})
		observed, frame := harness.response(t, fmt.Sprint(test.id))
		if frame.Error == nil || frame.Error.Code != test.code {
			t.Fatalf("%s response = %#v", test.method, frame)
		}
		if strings.Contains(string(observed), secret) || strings.Contains(string(observed), stateRoot) ||
			strings.Contains(string(observed), path) || strings.Contains(string(observed), "sourcePath") ||
			strings.Contains(string(observed), "quarantinePath") {
			t.Fatalf("%s leaked persisted data: %s", test.method, observed)
		}
	}
}

func newSessionSurfaceBackend(t *testing.T, store SessionStore) *Backend {
	t.Helper()
	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a", DisplayName: "Model A"}}},
		DefaultModel: "model-a",
		Store:        store,
		Version:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func findACPStoredSessionPath(t *testing.T, root, id string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(filepath.Join(root, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == id+".json" {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("persisted session %s not found", id)
	}
	return found
}
