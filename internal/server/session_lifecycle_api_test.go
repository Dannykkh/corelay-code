package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

type sessionMutationResponse struct {
	OK       bool           `json:"ok"`
	ID       string         `json:"id"`
	Version  int            `json:"version"`
	Revision uint64         `json:"revision"`
	Session  *agent.Session `json:"session,omitempty"`
}

type sessionErrorResponse struct {
	Error struct {
		Code             string `json:"code"`
		SessionID        string `json:"sessionId"`
		ExpectedRevision uint64 `json:"expectedRevision"`
		CurrentRevision  uint64 `json:"currentRevision"`
		Recovery         *struct {
			SessionID             string                       `json:"sessionId"`
			Kind                  string                       `json:"kind"`
			LifecycleStatus       agent.SessionLifecycleStatus `json:"lifecycleStatus"`
			LastCommittedRevision uint64                       `json:"lastCommittedRevision"`
		} `json:"recovery"`
	} `json:"error"`
}

func decodeSessionMutation(t *testing.T, rec *httptest.ResponseRecorder) sessionMutationResponse {
	t.Helper()
	var response sessionMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode mutation response: %v body=%s", err, rec.Body.String())
	}
	return response
}

func decodeSessionAPIError(t *testing.T, rec *httptest.ResponseRecorder) sessionErrorResponse {
	t.Helper()
	var response sessionErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, rec.Body.String())
	}
	return response
}

func createSessionThroughAPI(t *testing.T, s *Server, workspace string, messages []agent.SessionMessage) *agent.Session {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"workspace": workspace,
		"messages":  messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", string(body))
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	response := decodeSessionMutation(t, rec)
	if response.Version != 1 || response.Revision != 1 || response.ID == "" {
		t.Fatalf("create response=%+v, want v1/r1", response)
	}
	created, err := s.sessions.Get(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestSessionAPICreateUpdateAndRenameRequireExpectedRevision(t *testing.T) {
	s := newSessionAPITestServer(t)
	created := createSessionThroughAPI(t, s, t.TempDir(), []agent.SessionMessage{{
		Role: "user", Content: "first", Timestamp: sessionAPITestTime(),
	}})

	update := fmt.Sprintf(`{"id":%q,"messages":[]}`, created.ID)
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", update)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusPreconditionRequired || decodeSessionAPIError(t, rec).Error.Code != "expected_revision_required" {
		t.Fatalf("legacy update status=%d body=%s", rec.Code, rec.Body.String())
	}

	stale := fmt.Sprintf(`{"id":%q,"expectedRevision":0,"messages":[]}`, created.ID)
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions", "", stale)
	s.handleSessionSave(rec, req)
	conflict := decodeSessionAPIError(t, rec)
	if rec.Code != http.StatusConflict || conflict.Error.Code != "session_revision_conflict" ||
		conflict.Error.ExpectedRevision != 0 || conflict.Error.CurrentRevision != 1 {
		t.Fatalf("stale update status=%d response=%+v body=%s", rec.Code, conflict, rec.Body.String())
	}

	valid := fmt.Sprintf(`{"id":%q,"expectedRevision":1,"messages":[]}`, created.ID)
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions", "", valid)
	s.handleSessionSave(rec, req)
	if response := decodeSessionMutation(t, rec); rec.Code != http.StatusOK || response.Revision != 2 {
		t.Fatalf("update status=%d response=%+v", rec.Code, response)
	}

	req, rec = sessionRequest(t, http.MethodPut, "/api/sessions/"+created.ID, created.ID, `{"title":"renamed"}`)
	s.handleSessionRename(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("rename without revision status=%d body=%s", rec.Code, rec.Body.String())
	}

	req, rec = sessionRequest(t, http.MethodPut, "/api/sessions/"+created.ID, created.ID, `{"title":"renamed","expectedRevision":2}`)
	s.handleSessionRename(rec, req)
	if response := decodeSessionMutation(t, rec); rec.Code != http.StatusOK || response.Revision != 3 {
		t.Fatalf("rename status=%d response=%+v", rec.Code, response)
	}
	persisted, err := s.sessions.Get(created.ID)
	if err != nil || persisted.Title != "renamed" || persisted.Revision != 3 {
		t.Fatalf("renamed session=%+v err=%v", persisted, err)
	}
	req, rec = sessionRequest(t, http.MethodPut, "/api/sessions/"+created.ID, created.ID, `{"title":"stale","expectedRevision":2}`)
	s.handleSessionRename(rec, req)
	if rec.Code != http.StatusConflict || decodeSessionAPIError(t, rec).Error.Code != "session_revision_conflict" {
		t.Fatalf("stale rename status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionAPIConcurrentStaleUpdatesHaveSingleWinner(t *testing.T) {
	s := newSessionAPITestServer(t)
	created := createSessionThroughAPI(t, s, t.TempDir(), nil)

	const writers = 24
	start := make(chan struct{})
	statuses := make(chan int, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"id":%q,"title":%q,"expectedRevision":1}`, created.ID, fmt.Sprintf("writer-%d", index))
			req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
			rec := httptest.NewRecorder()
			s.handleSessionSave(rec, req)
			statuses <- rec.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(statuses)

	wins, conflicts := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent update status %d", status)
		}
	}
	if wins != 1 || conflicts != writers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1/%d", wins, conflicts, writers-1)
	}
	persisted, err := s.sessions.Get(created.ID)
	if err != nil || persisted.Revision != 2 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestSessionAPIForkIsIsolatedAndRevisionChecked(t *testing.T) {
	s := newSessionAPITestServer(t)
	parent := createSessionThroughAPI(t, s, t.TempDir(), []agent.SessionMessage{{
		Role: "user", Content: "parent", ToolInput: map[string]any{"nested": map[string]any{"value": "original"}}, Timestamp: sessionAPITestTime(),
	}})

	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions/"+parent.ID+"/fork", parent.ID, `{"expectedRevision":1}`)
	s.handleSessionFork(rec, req)
	response := decodeSessionMutation(t, rec)
	if rec.Code != http.StatusOK || response.Session == nil || response.Revision != 1 ||
		response.Session.ParentSessionID != parent.ID || response.Session.ParentRevision != 1 {
		t.Fatalf("fork status=%d response=%+v body=%s", rec.Code, response, rec.Body.String())
	}
	child := response.Session
	req, rec = sessionRequest(t, http.MethodGet, "/api/sessions/"+child.ID+"/resume-state", child.ID, "")
	s.handleSessionResumeState(rec, req)
	var childState agent.SessionResumeState
	if err := json.Unmarshal(rec.Body.Bytes(), &childState); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || childState.ParentSessionID != parent.ID || childState.ParentRevision != 1 {
		t.Fatalf("fork resume lineage status=%d state=%+v", rec.Code, childState)
	}
	childUpdate := fmt.Sprintf(`{"id":%q,"expectedRevision":1,"messages":[{"role":"user","content":"child changed","timestamp":"2026-08-12T00:00:00Z"}]}`, child.ID)
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions", "", childUpdate)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("child update status=%d body=%s", rec.Code, rec.Body.String())
	}
	persistedParent, err := s.sessions.Get(parent.ID)
	if err != nil || persistedParent.Revision != 1 || persistedParent.Messages[0].Content != "parent" {
		t.Fatalf("parent changed=%+v err=%v", persistedParent, err)
	}

	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions/"+parent.ID+"/fork", parent.ID, `{"expectedRevision":0}`)
	s.handleSessionFork(rec, req)
	if rec.Code != http.StatusConflict || decodeSessionAPIError(t, rec).Error.Code != "session_revision_conflict" {
		t.Fatalf("stale fork status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionAPIInterruptResumeReconcileAndClose(t *testing.T) {
	s := newSessionAPITestServer(t)
	created := createSessionThroughAPI(t, s, t.TempDir(), nil)
	digest := "sha256:" + strings.Repeat("a", 64)
	interruptBody := fmt.Sprintf(
		`{"expectedRevision":1,"runId":"run-1","toolName":"bash_exec","toolCallId":"tool-1","inputDigest":%q,"sideEffectState":"may_have_applied","summary":"process dispatched; completion unknown"}`,
		digest,
	)
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+"/interrupt", created.ID, interruptBody)
	s.handleSessionInterrupt(rec, req)
	response := decodeSessionMutation(t, rec)
	if rec.Code != http.StatusOK || response.Revision != 2 || response.Session == nil ||
		!response.Session.ReconcileRequired || response.Session.Interruption == nil ||
		response.Session.Interruption.InputDigest != digest {
		t.Fatalf("interrupt status=%d response=%+v body=%s", rec.Code, response, rec.Body.String())
	}

	req, rec = sessionRequest(t, http.MethodGet, "/api/sessions/"+created.ID+"/resume-state", created.ID, "")
	s.handleSessionResumeState(rec, req)
	var state agent.SessionResumeState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || state.Revision != 2 || state.LastCommittedRevision != 1 ||
		!state.ReconcileRequired || state.Interruption == nil || state.Interruption.RunID != "run-1" {
		t.Fatalf("resume state=%+v status=%d", state, rec.Code)
	}

	blockedSave := fmt.Sprintf(`{"id":%q,"expectedRevision":2,"messages":[]}`, created.ID)
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions", "", blockedSave)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusConflict || decodeSessionAPIError(t, rec).Error.Code != "session_reconcile_required" {
		t.Fatalf("blocked save status=%d body=%s", rec.Code, rec.Body.String())
	}

	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+"/reconcile", created.ID, `{"expectedRevision":2}`)
	s.handleSessionReconcile(rec, req)
	response = decodeSessionMutation(t, rec)
	if rec.Code != http.StatusOK || response.Revision != 3 || response.Session == nil || response.Session.ReconcileRequired {
		t.Fatalf("reconcile status=%d response=%+v", rec.Code, response)
	}

	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+"/close", created.ID, `{"expectedRevision":3}`)
	s.handleSessionClose(rec, req)
	response = decodeSessionMutation(t, rec)
	if rec.Code != http.StatusOK || response.Revision != 4 || response.Session == nil ||
		response.Session.LifecycleStatus != agent.SessionLifecycleClosed {
		t.Fatalf("close status=%d response=%+v", rec.Code, response)
	}

	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+"/interrupt", created.ID,
		strings.Replace(interruptBody, `"expectedRevision":1`, `"expectedRevision":4`, 1))
	s.handleSessionInterrupt(rec, req)
	if rec.Code != http.StatusConflict || decodeSessionAPIError(t, rec).Error.Code != "session_lifecycle_conflict" {
		t.Fatalf("closed interrupt status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionAPIInterruptRejectsRawOrInvalidMetadata(t *testing.T) {
	s := newSessionAPITestServer(t)
	created := createSessionThroughAPI(t, s, t.TempDir(), nil)
	digest := "sha256:" + strings.Repeat("b", 64)
	rawBody := fmt.Sprintf(
		`{"expectedRevision":1,"runId":"run","toolName":"bash_exec","inputDigest":%q,"sideEffectState":"unknown","summary":"redacted","rawToolInput":"secret command"}`,
		digest,
	)
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+"/interrupt", created.ID, rawBody)
	s.handleSessionInterrupt(rec, req)
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "secret command") {
		t.Fatalf("raw metadata status=%d body=%s", rec.Code, rec.Body.String())
	}

	invalidDigest := `{"expectedRevision":1,"runId":"run","toolName":"bash_exec","inputDigest":"sha256:not-a-digest","sideEffectState":"unknown","summary":"redacted"}`
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+"/interrupt", created.ID, invalidDigest)
	s.handleSessionInterrupt(rec, req)
	if rec.Code != http.StatusBadRequest || decodeSessionAPIError(t, rec).Error.Code != "invalid_interruption" {
		t.Fatalf("invalid digest status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionAPILifecycleMutationsRequireRevision(t *testing.T) {
	s := newSessionAPITestServer(t)
	created := createSessionThroughAPI(t, s, t.TempDir(), nil)
	tests := []struct {
		name    string
		target  string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{name: "fork", target: "/fork", handler: s.handleSessionFork},
		{name: "interrupt", target: "/interrupt", handler: s.handleSessionInterrupt},
		{name: "reconcile", target: "/reconcile", handler: s.handleSessionReconcile},
		{name: "close", target: "/close", handler: s.handleSessionClose},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, rec := sessionRequest(t, http.MethodPost, "/api/sessions/"+created.ID+test.target, created.ID, `{}`)
			test.handler(rec, req)
			if rec.Code != http.StatusPreconditionRequired || decodeSessionAPIError(t, rec).Error.Code != "expected_revision_required" {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSessionAPILegacyMigrationAndRecoveryNoLeak(t *testing.T) {
	base := t.TempDir()
	store := agent.NewSessionStore(base)
	s := New(nil, "", 0)
	s.SetSessionStore(store)
	created := createSessionThroughAPI(t, s, filepath.Join(base, "workspace"), nil)
	path := persistedSessionPath(t, base, created.ID)

	legacy := map[string]any{
		"id": created.ID, "title": "legacy", "workspace": created.Workspace,
		"messages": []any{}, "provider": "test", "model": "legacy",
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacyData, 0o644); err != nil {
		t.Fatal(err)
	}
	update := fmt.Sprintf(`{"id":%q,"expectedRevision":0,"title":"migrated","messages":[]}`, created.ID)
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", update)
	s.handleSessionSave(rec, req)
	if response := decodeSessionMutation(t, rec); rec.Code != http.StatusOK || response.Version != 1 || response.Revision != 1 {
		t.Fatalf("legacy migration status=%d response=%+v body=%s", rec.Code, response, rec.Body.String())
	}

	secret := "do-not-expose-this-secret"
	if _, err := store.MarkInterrupted(created.ID, 1, agent.SessionInterruption{
		Reason: secret, ToolName: "legacy-tool",
	}); err != nil {
		t.Fatal(err)
	}
	req, rec = sessionRequest(t, http.MethodGet, "/api/sessions/"+created.ID+"/resume-state", created.ID, "")
	s.handleSessionResumeState(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), `"reason"`) {
		t.Fatalf("resume state leaked legacy reason: status=%d body=%s", rec.Code, rec.Body.String())
	}

	if err := os.WriteFile(path, []byte(`{"id":"`+created.ID+`","secret":"`+secret), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []func(http.ResponseWriter, *http.Request){s.handleSessionGet, s.handleSessionResumeState} {
		req, rec = sessionRequest(t, http.MethodGet, "/api/sessions/"+created.ID, created.ID, "")
		handler(rec, req)
		response := decodeSessionAPIError(t, rec)
		if rec.Code != http.StatusUnprocessableEntity || response.Error.Code != "session_corrupt" || response.Error.Recovery == nil {
			t.Fatalf("corrupt status=%d response=%+v body=%s", rec.Code, response, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, secret) || strings.Contains(body, base) || strings.Contains(body, "sourcePath") || strings.Contains(body, "quarantinePath") {
			t.Fatalf("recovery response leaked private data: %s", body)
		}
	}

	unsupported := map[string]any{
		"version": 999, "revision": 1, "id": created.ID, "workspace": created.Workspace,
		"messages": []map[string]string{{"role": "user", "content": secret}},
	}
	unsupportedData, err := json.Marshal(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, unsupportedData, 0o644); err != nil {
		t.Fatal(err)
	}
	req, rec = sessionRequest(t, http.MethodGet, "/api/sessions/"+created.ID, created.ID, "")
	s.handleSessionGet(rec, req)
	response := decodeSessionAPIError(t, rec)
	if rec.Code != http.StatusConflict || response.Error.Code != "session_version_unsupported" ||
		strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), base) {
		t.Fatalf("unsupported status=%d response=%+v body=%s", rec.Code, response, rec.Body.String())
	}
}

func TestSessionAPIErrorMappingTable(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "invalid id", err: agent.ErrInvalidSessionID, status: 400, code: "invalid_session_id"},
		{name: "not found", err: agent.ErrSessionNotFound, status: 404, code: "session_not_found"},
		{name: "revision", err: &agent.SessionRevisionConflictError{SessionID: "sess_test", Expected: 1, Current: 2}, status: 409, code: "session_revision_conflict"},
		{name: "conflict", err: agent.ErrSessionConflict, status: 409, code: "session_conflict"},
		{name: "unsupported", err: agent.ErrSessionVersionUnsupported, status: 409, code: "session_version_unsupported"},
		{name: "corrupt", err: agent.ErrSessionCorrupt, status: 422, code: "session_corrupt"},
		{name: "reconcile", err: agent.ErrSessionReconcileRequired, status: 409, code: "session_reconcile_required"},
		{name: "lifecycle", err: agent.ErrSessionLifecycleInvalid, status: 409, code: "session_lifecycle_conflict"},
		{name: "overflow", err: agent.ErrSessionRevisionOverflow, status: 409, code: "session_revision_overflow"},
		{name: "unknown", err: errors.New("secret internal failure"), status: 500, code: "session_operation_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeSessionError(rec, test.err)
			response := decodeSessionAPIError(t, rec)
			if rec.Code != test.status || response.Error.Code != test.code || strings.Contains(rec.Body.String(), "secret internal failure") {
				t.Fatalf("status=%d response=%+v body=%s", rec.Code, response, rec.Body.String())
			}
		})
	}
}

func persistedSessionPath(t *testing.T, base, id string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(filepath.Join(base, "sessions"), func(path string, entry os.DirEntry, err error) error {
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

func sessionAPITestTime() time.Time {
	return time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
}
