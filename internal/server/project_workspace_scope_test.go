package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
)

func configureProjectWorkspaces(t *testing.T, defaultWorkspace string, projects ...string) {
	t.Helper()
	t.Setenv("CORELAY_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	_, err := config.Update(func(cfg *config.Config) error {
		cfg.WorkDir = defaultWorkspace
		cfg.Projects = nil
		for _, project := range projects {
			cfg.Projects = append(cfg.Projects, config.Project{Path: project})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("configure workspaces: %v", err)
	}
}

func TestFileHandlersUseRegisteredClientWorkspace(t *testing.T) {
	defaultWorkspace := t.TempDir()
	selectedWorkspace := t.TempDir()
	unregisteredWorkspace := t.TempDir()
	configureProjectWorkspaces(t, defaultWorkspace, selectedWorkspace)
	if err := os.WriteFile(filepath.Join(defaultWorkspace, "same.txt"), []byte("default workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(selectedWorkspace, "same.txt"), []byte("selected workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unregisteredWorkspace, "secret.txt"), []byte("outside registered projects"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(&agentLoopFakeProvider{}, "test-model", 0)
	s.SetWorkDir(defaultWorkspace)

	query := url.Values{"workDir": []string{selectedWorkspace}}
	treeRec := httptest.NewRecorder()
	s.handleFileTree(treeRec, httptest.NewRequest(http.MethodGet, "/api/tree?"+query.Encode(), nil))
	if treeRec.Code != http.StatusOK || !strings.Contains(treeRec.Body.String(), "same.txt") {
		t.Fatalf("selected tree status=%d body=%s", treeRec.Code, treeRec.Body.String())
	}
	if strings.Contains(treeRec.Body.String(), "secret.txt") {
		t.Fatalf("tree escaped selected workspace: %s", treeRec.Body.String())
	}

	readQuery := url.Values{"workDir": []string{selectedWorkspace}, "path": []string{"same.txt"}}
	readRec := httptest.NewRecorder()
	s.handleReadFile(readRec, httptest.NewRequest(http.MethodGet, "/api/file?"+readQuery.Encode(), nil))
	if readRec.Code != http.StatusOK || !strings.Contains(readRec.Body.String(), "selected workspace") {
		t.Fatalf("selected read status=%d body=%s", readRec.Code, readRec.Body.String())
	}

	traversalQuery := url.Values{"workDir": []string{selectedWorkspace}, "path": []string{"../secret.txt"}}
	traversalRec := httptest.NewRecorder()
	s.handleReadFile(traversalRec, httptest.NewRequest(http.MethodGet, "/api/file?"+traversalQuery.Encode(), nil))
	if traversalRec.Code != http.StatusForbidden {
		t.Fatalf("traversal status=%d body=%s", traversalRec.Code, traversalRec.Body.String())
	}

	unknownQuery := url.Values{"workDir": []string{unregisteredWorkspace}}
	unknownRec := httptest.NewRecorder()
	s.handleFileTree(unknownRec, httptest.NewRequest(http.MethodGet, "/api/tree?"+unknownQuery.Encode(), nil))
	if unknownRec.Code != http.StatusForbidden {
		t.Fatalf("unregistered workspace status=%d body=%s", unknownRec.Code, unknownRec.Body.String())
	}

	writeBody, err := json.Marshal(map[string]string{
		"workDir": selectedWorkspace,
		"path":    "nested/new.txt",
		"content": "written to selected workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeRec := httptest.NewRecorder()
	s.handleWriteFile(writeRec, httptest.NewRequest(http.MethodPost, "/api/file/write", bytes.NewReader(writeBody)))
	if writeRec.Code != http.StatusOK {
		t.Fatalf("selected write status=%d body=%s", writeRec.Code, writeRec.Body.String())
	}
	written, err := os.ReadFile(filepath.Join(selectedWorkspace, "nested", "new.txt"))
	if err != nil || string(written) != "written to selected workspace" {
		t.Fatalf("selected write content=%q err=%v", written, err)
	}
	if _, err := os.Stat(filepath.Join(defaultWorkspace, "nested", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("write changed the server default workspace: err=%v", err)
	}
}

func TestAddProjectDoesNotChangeServerWorkspace(t *testing.T) {
	defaultWorkspace := t.TempDir()
	projectWorkspace := t.TempDir()
	configureProjectWorkspaces(t, defaultWorkspace)
	s := New(&agentLoopFakeProvider{}, "test-model", 0)
	s.SetWorkDir(defaultWorkspace)

	body, err := json.Marshal(map[string]string{"path": projectWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleAddProject(rec, httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("add project status=%d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	gotWorkspace := s.workDir
	s.mu.RUnlock()
	if !sameDurableWorkspace(gotWorkspace, defaultWorkspace) {
		t.Fatalf("adding a project changed server workspace to %q, want %q", gotWorkspace, defaultWorkspace)
	}
}

func TestNewSessionUsesClientWorkspaceAndUpdateKeepsItsSnapshot(t *testing.T) {
	defaultWorkspace := t.TempDir()
	selectedWorkspace := t.TempDir()
	configureProjectWorkspaces(t, defaultWorkspace, selectedWorkspace)
	store := agent.NewSessionStore(t.TempDir())
	s := New(&agentLoopFakeProvider{}, "test-model", 0)
	s.SetWorkDir(defaultWorkspace)
	s.SetSessionStore(store)

	createBody, err := json.Marshal(map[string]any{
		"workspace": selectedWorkspace,
		"messages":  []map[string]string{{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	createRec := httptest.NewRecorder()
	s.handleSessionSave(createRec, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(createBody)))
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &result); err != nil || result.ID == "" {
		t.Fatalf("decode create response=%s err=%v", createRec.Body.String(), err)
	}

	s.SetWorkDir(t.TempDir()) // A later server-default change must not retarget the saved chat.
	updateBody, err := json.Marshal(map[string]any{
		"id":               result.ID,
		"expectedRevision": 1,
		"title":            "updated",
	})
	if err != nil {
		t.Fatal(err)
	}
	updateRec := httptest.NewRecorder()
	s.handleSessionSave(updateRec, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(updateBody)))
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}
	persisted, err := store.Get(result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDurableWorkspace(persisted.Workspace, selectedWorkspace) {
		t.Fatalf("session workspace=%q, want selected snapshot %q", persisted.Workspace, selectedWorkspace)
	}
}

func TestNewSessionRejectsUnregisteredWorkspace(t *testing.T) {
	defaultWorkspace := t.TempDir()
	unregisteredWorkspace := t.TempDir()
	configureProjectWorkspaces(t, defaultWorkspace)
	store := agent.NewSessionStore(t.TempDir())
	s := New(&agentLoopFakeProvider{}, "test-model", 0)
	s.SetWorkDir(defaultWorkspace)
	s.SetSessionStore(store)

	body, err := json.Marshal(map[string]any{
		"workspace": unregisteredWorkspace,
		"messages":  []map[string]string{{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleSessionSave(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("unregistered workspace status=%d body=%s", rec.Code, rec.Body.String())
	}
	if sessions := store.ListAll(); len(sessions) != 0 {
		t.Fatalf("unregistered workspace created a session: %#v", sessions)
	}
}
