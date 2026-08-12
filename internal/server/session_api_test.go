package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

func newSessionAPITestServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, "", 0)
	s.SetSessionStore(agent.NewSessionStore(t.TempDir()))
	return s
}

func sessionRequest(t *testing.T, method, target, id, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	return req, httptest.NewRecorder()
}

func TestSessionAPIMapsValidationAndNotFoundErrors(t *testing.T) {
	s := newSessionAPITestServer(t)
	validMissingID := "sess_aaaaaaaaaaaaaaaaaaaaaaaaaa"

	t.Run("get rejects traversal", func(t *testing.T) {
		req, rec := sessionRequest(t, http.MethodGet, "/api/sessions/../outside", "../outside", "")
		s.handleSessionGet(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("get missing", func(t *testing.T) {
		req, rec := sessionRequest(t, http.MethodGet, "/api/sessions/missing", validMissingID, "")
		s.handleSessionGet(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
		}
	})

	t.Run("delete missing", func(t *testing.T) {
		req, rec := sessionRequest(t, http.MethodDelete, "/api/sessions/missing", validMissingID, `{"expectedRevision":1}`)
		s.handleSessionDelete(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
		}
	})

	t.Run("rename rejects invalid JSON", func(t *testing.T) {
		req, rec := sessionRequest(t, http.MethodPatch, "/api/sessions/missing", validMissingID, "not-json")
		s.handleSessionRename(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("rename missing", func(t *testing.T) {
		req, rec := sessionRequest(t, http.MethodPut, "/api/sessions/missing", validMissingID, `{"title":"new","expectedRevision":1}`)
		s.handleSessionRename(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
		}
	})
}

func TestSessionAPISaveConflictAndDeleteResult(t *testing.T) {
	s := newSessionAPITestServer(t)
	id := "sess_" + strings.Repeat("b", 26)
	base := t.TempDir()
	workspaceA := filepath.Join(base, "a")
	workspaceB := filepath.Join(base, "b")

	firstBody := `{"id":"` + id + `","workspace":"` + filepath.ToSlash(workspaceA) + `"}`
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", firstBody)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first save status=%d body=%s", rec.Code, rec.Body.String())
	}

	legacyUpdateBody := `{"id":"` + id + `","workspace":"` + filepath.ToSlash(workspaceA) + `"}`
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions", "", legacyUpdateBody)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("legacy update status=%d body=%s, want 428", rec.Code, rec.Body.String())
	}

	conflictBody := `{"id":"` + id + `","workspace":"` + filepath.ToSlash(workspaceB) + `","expectedRevision":1}`
	req, rec = sessionRequest(t, http.MethodPost, "/api/sessions", "", conflictBody)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting save status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}

	req, rec = sessionRequest(t, http.MethodDelete, "/api/sessions/"+id, id, `{"expectedRevision":1}`)
	s.handleSessionDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	req, rec = sessionRequest(t, http.MethodDelete, "/api/sessions/"+id, id, `{"expectedRevision":1}`)
	s.handleSessionDelete(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestSessionAPISaveRejectsInvalidID(t *testing.T) {
	s := newSessionAPITestServer(t)
	req, rec := sessionRequest(t, http.MethodPost, "/api/sessions", "", `{"id":"../outside"}`)
	s.handleSessionSave(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestSessionAPIListKeepsLegacyCollidingWorkspaceNamesIsolated(t *testing.T) {
	s := New(nil, "", 0)
	store := agent.NewSessionStore(t.TempDir())
	s.SetSessionStore(store)
	workspaceA := `D:\a\b`
	workspaceB := `D:\a--b`
	idA := "sess_" + strings.Repeat("c", 26)
	idB := "sess_" + strings.Repeat("d", 26)
	if err := store.Save(&agent.Session{ID: idA, Workspace: workspaceA}); err != nil {
		t.Fatalf("Save(A) = %v", err)
	}
	if err := store.Save(&agent.Session{ID: idB, Workspace: workspaceB}); err != nil {
		t.Fatalf("Save(B) = %v", err)
	}

	for _, test := range []struct {
		workspace string
		wantID    string
		otherID   string
	}{
		{workspace: workspaceA, wantID: idA, otherID: idB},
		{workspace: workspaceB, wantID: idB, otherID: idA},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/sessions?workspace="+url.QueryEscape(test.workspace), nil)
		rec := httptest.NewRecorder()
		s.handleSessionList(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list(%q) status=%d body=%s", test.workspace, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, test.wantID) || strings.Contains(body, test.otherID) {
			t.Fatalf("list(%q) body=%s, want %s without %s", test.workspace, body, test.wantID, test.otherID)
		}
	}
}
