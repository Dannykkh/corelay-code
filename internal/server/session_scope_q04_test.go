package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type q04BlockingModelProvider struct {
	entered     chan string
	release     chan struct{}
	releaseOnce sync.Once
}

func (*q04BlockingModelProvider) Name() string              { return "fake" }
func (*q04BlockingModelProvider) DisplayName() string       { return "Q04 blocking fake" }
func (*q04BlockingModelProvider) Models() []types.ModelInfo { return nil }
func (*q04BlockingModelProvider) Validate() error           { return nil }

func (p *q04BlockingModelProvider) StreamMessage(
	ctx context.Context,
	request *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	select {
	case p.entered <- request.Model:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return q04ModelProviderEvents("answer"), nil
}

func (p *q04BlockingModelProvider) unblock() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func q04ModelProviderEvents(text string) <-chan types.SSEEvent {
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

func TestConcurrentDurableSessionsUseTheirPersistedModels(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")

	base := t.TempDir()
	store := agent.NewSessionStore(filepath.Join(base, "state"))
	sessions := []*agent.Session{
		{Workspace: filepath.Join(base, "project-a"), Provider: "fake", Model: "model-a", Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}},
		{Workspace: filepath.Join(base, "project-b"), Provider: "fake", Model: "model-b", Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}},
	}
	for _, session := range sessions {
		if err := store.Save(session); err != nil {
			t.Fatalf("Save(%s) = %v", session.Model, err)
		}
	}

	provider := &q04BlockingModelProvider{entered: make(chan string, len(sessions)), release: make(chan struct{})}
	t.Cleanup(provider.unblock)
	server := New(provider, "server-default-model", 0)
	t.Cleanup(server.ShutdownApprovals)
	server.SetSessionStore(store)

	type result struct {
		model    string
		recorder *httptest.ResponseRecorder
	}
	results := make(chan result, len(sessions))
	for _, session := range sessions {
		body, err := json.Marshal(map[string]any{
			"messages":         []map[string]string{{"role": "user", "content": "hello"}},
			"durableSessionId": session.ID,
			"expectedRevision": session.Revision,
		})
		if err != nil {
			t.Fatalf("Marshal(%s request) = %v", session.Model, err)
		}
		recorder := httptest.NewRecorder()
		go func(model string, requestBody []byte, rec *httptest.ResponseRecorder) {
			request := httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(requestBody))
			server.handleAgentLoop(rec, request)
			results <- result{model: model, recorder: rec}
		}(session.Model, body, recorder)
	}

	started := make(map[string]bool, len(sessions))
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(started) < len(sessions) {
		select {
		case model := <-provider.entered:
			started[model] = true
		case <-deadline.C:
			provider.unblock()
			t.Fatalf("provider did not receive both persisted models before release: got %#v", started)
		}
	}
	for _, session := range sessions {
		if !started[session.Model] {
			provider.unblock()
			t.Fatalf("provider models = %#v, missing persisted model %q", started, session.Model)
		}
	}
	provider.unblock()

	completed := make(map[string]bool, len(sessions))
	for range sessions {
		select {
		case got := <-results:
			if got.recorder.Code != http.StatusOK {
				t.Errorf("durable run for %q status=%d body=%s", got.model, got.recorder.Code, got.recorder.Body.String())
			} else if !bytes.Contains(got.recorder.Body.Bytes(), []byte("answer")) {
				t.Errorf("durable run for %q omitted provider answer: %s", got.model, got.recorder.Body.String())
			}
			completed[got.model] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for durable runs to finish; completed=%#v", completed)
		}
	}
	for _, session := range sessions {
		if !completed[session.Model] {
			t.Errorf("durable run for %q did not complete", session.Model)
		}
	}
}

func TestApprovalCannotBeResolvedFromAnotherWorkspaceSession(t *testing.T) {
	base := t.TempDir()
	workspaceA := filepath.Join(base, "project-a")
	workspaceB := filepath.Join(base, "project-b")
	store := agent.NewSessionStore(filepath.Join(base, "state"))
	sessionA := &agent.Session{Workspace: workspaceA, Model: "model-a"}
	sessionB := &agent.Session{Workspace: workspaceB, Model: "model-b"}
	for _, session := range []*agent.Session{sessionA, sessionB} {
		if err := store.Save(session); err != nil {
			t.Fatalf("Save session for %q = %v", session.Workspace, err)
		}
	}
	if sessionA.ID == sessionB.ID || sessionA.Workspace == sessionB.Workspace {
		t.Fatalf("test sessions are not isolated: A=%+v B=%+v", sessionA, sessionB)
	}

	s := New(nil, "", 0)
	t.Cleanup(s.ShutdownApprovals)
	s.SetSessionStore(store)
	pending := openServerApproval(t, s, sessionA.ID)
	resolve := func(sessionID string) *httptest.ResponseRecorder {
		t.Helper()
		request, recorder := approvalAPIRequest(
			t,
			http.MethodPost,
			"/api/approvals/"+pending.ID+"/resolve",
			pending.ID,
			`{"sessionId":"`+sessionID+`","decision":"allow_once"}`,
		)
		s.handleApprovalResolve(recorder, request)
		return recorder
	}

	if response := resolve(sessionB.ID); response.Code != http.StatusNotFound {
		t.Fatalf("project B resolved project A approval: status=%d body=%s, want 404", response.Code, response.Body.String())
	}
	if response := resolve(sessionA.ID); response.Code != http.StatusOK {
		t.Fatalf("project A could not resolve its approval after B rejection: status=%d body=%s", response.Code, response.Body.String())
	}
}
