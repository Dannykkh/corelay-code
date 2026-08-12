package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type capturingDurableResultProvider struct {
	mu           sync.Mutex
	systemPrompt string
	tools        []types.ToolDef
}

func (*capturingDurableResultProvider) Name() string              { return "durable-result" }
func (*capturingDurableResultProvider) DisplayName() string       { return "Durable Result" }
func (*capturingDurableResultProvider) Models() []types.ModelInfo { return nil }
func (*capturingDurableResultProvider) Validate() error           { return nil }

func (p *capturingDurableResultProvider) StreamMessage(
	_ context.Context,
	request *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.systemPrompt = string(request.System)
	p.tools = append([]types.ToolDef(nil), request.Tools...)
	p.mu.Unlock()
	return textProviderEvents("done"), nil
}

func (p *capturingDurableResultProvider) snapshot() (string, []types.ToolDef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.systemPrompt, append([]types.ToolDef(nil), p.tools...)
}

type blockingDurableProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (*blockingDurableProvider) Name() string              { return "blocking-durable" }
func (*blockingDurableProvider) DisplayName() string       { return "Blocking Durable" }
func (*blockingDurableProvider) Models() []types.ModelInfo { return nil }
func (*blockingDurableProvider) Validate() error           { return nil }

func (p *blockingDurableProvider) StreamMessage(
	ctx context.Context,
	_ *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return textProviderEvents("done"), nil
}

func textProviderEvents(text string) <-chan types.SSEEvent {
	events := make(chan types.SSEEvent, 5)
	block, _ := json.Marshal(map[string]string{"type": "text"})
	delta, _ := json.Marshal(map[string]string{"type": "text_delta", "text": text})
	stop, _ := json.Marshal(map[string]string{"stop_reason": "end_turn"})
	events <- types.SSEEvent{Type: "content_block_start", ContentBlock: block}
	events <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
	events <- types.SSEEvent{Type: "content_block_stop"}
	events <- types.SSEEvent{Type: "message_delta", Delta: stop}
	events <- types.SSEEvent{Type: "message_stop"}
	close(events)
	return events
}

func TestDurableHTTPRunInjectsOnlyCommittedVerifiedResultInventory(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	base := t.TempDir()
	workspace := t.TempDir()
	store := agent.NewSessionStore(base)
	session := agent.Session{Workspace: workspace, Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	memory, _, err := store.OpenToolResultMemory(session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	committedContent := "Authorization: Bearer " + secret + "\n" + strings.Repeat("committed/", 500)
	orphanContent := strings.Repeat("CAS_ORPHAN_MUST_NOT_REPLAY/", 300)
	reference, replaced := memory.StoreResult("Read", committedContent)
	if !replaced {
		t.Fatal("failed to store committed result")
	}
	if _, replaced := memory.StoreResult("Read", orphanContent); !replaced {
		t.Fatal("failed to seed CAS orphan")
	}
	session.Messages = append(session.Messages, agent.SessionMessage{Role: "tool", Content: reference, ToolResult: reference})
	if err := store.SaveExpected(&session, session.Revision); err != nil {
		t.Fatal(err)
	}

	provider := &capturingDurableResultProvider{}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workspace)
	server.SetSessionStore(store)
	missingRevisionDelete := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+session.ID, nil)
	missingRevisionDelete.SetPathValue("id", session.ID)
	missingRevisionRecorder := httptest.NewRecorder()
	server.handleSessionDelete(missingRevisionRecorder, missingRevisionDelete)
	if missingRevisionRecorder.Code != http.StatusPreconditionRequired ||
		!strings.Contains(missingRevisionRecorder.Body.String(), `"code":"expected_revision_required"`) {
		t.Fatalf("delete without revision status=%d body=%s", missingRevisionRecorder.Code, missingRevisionRecorder.Body.String())
	}
	staleDelete := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+session.ID, strings.NewReader(`{"expectedRevision":0}`))
	staleDelete.SetPathValue("id", session.ID)
	staleDeleteRecorder := httptest.NewRecorder()
	server.handleSessionDelete(staleDeleteRecorder, staleDelete)
	if staleDeleteRecorder.Code != http.StatusConflict ||
		!strings.Contains(staleDeleteRecorder.Body.String(), `"code":"session_revision_conflict"`) {
		t.Fatalf("stale delete status=%d body=%s", staleDeleteRecorder.Code, staleDeleteRecorder.Body.String())
	}
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
	systemPrompt, tools := provider.snapshot()
	committedID := strings.TrimPrefix(agentResultID(committedContent), "result_")
	orphanID := strings.TrimPrefix(agentResultID(orphanContent), "result_")
	if !strings.Contains(systemPrompt, committedID) || strings.Contains(systemPrompt, orphanID) ||
		strings.Contains(systemPrompt, secret) || strings.Contains(systemPrompt, "CAS_ORPHAN_MUST_NOT_REPLAY") {
		t.Fatalf("unsafe committed inventory: %s", systemPrompt)
	}
	if !serverToolCatalogContains(tools, "LoadToolResult") {
		t.Fatal("durable HTTP run did not advertise LoadToolResult")
	}
	if strings.Contains(recorder.Body.String(), secret) || strings.Contains(recorder.Body.String(), "CAS_ORPHAN_MUST_NOT_REPLAY") {
		t.Fatal("raw result crossed HTTP SSE boundary")
	}
}

func TestDurableHTTPActiveRunSingleWinnerAndDeleteGate(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workspace := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{Workspace: workspace, Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &blockingDurableProvider{started: make(chan struct{}), release: make(chan struct{})}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workspace)
	server.SetSessionStore(store)
	body, _ := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "hello"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
		firstDone <- recorder
	}()
	<-provider.started

	second := httptest.NewRecorder()
	server.handleAgentLoop(second, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), `"code":"session_run_active"`) || provider.calls.Load() != 1 {
		t.Fatalf("second run status=%d calls=%d body=%s", second.Code, provider.calls.Load(), second.Body.String())
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+session.ID, strings.NewReader(`{"expectedRevision":1}`))
	deleteRequest.SetPathValue("id", session.ID)
	deleteRecorder := httptest.NewRecorder()
	server.handleSessionDelete(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusConflict || !strings.Contains(deleteRecorder.Body.String(), `"code":"session_run_active"`) {
		t.Fatalf("active delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	close(provider.release)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first run status=%d body=%s", first.Code, first.Body.String())
	}
	committed, err := store.Get(session.ID)
	if err != nil || committed.Revision != 2 {
		t.Fatalf("committed session=%#v err=%v", committed, err)
	}
	deleteRequest = httptest.NewRequest(http.MethodDelete, "/api/sessions/"+session.ID, strings.NewReader(`{"expectedRevision":2}`))
	deleteRequest.SetPathValue("id", session.ID)
	deleteRecorder = httptest.NewRecorder()
	server.handleSessionDelete(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("idle delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
}

func serverToolCatalogContains(tools []types.ToolDef, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func agentResultID(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "result_" + hex.EncodeToString(digest[:])
}
