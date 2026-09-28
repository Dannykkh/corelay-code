package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
)

func TestKairosGitStatusRejectsUnregisteredWorkspace(t *testing.T) {
	defaultWorkspace := t.TempDir()
	registeredWorkspace := t.TempDir()
	unregisteredWorkspace := t.TempDir()
	configureProjectWorkspaces(t, defaultWorkspace, registeredWorkspace)
	// Best effort: a real repository proves the handler refuses before Git
	// runs. The scope check itself does not depend on Git being installed.
	_ = exec.Command("git", "-C", unregisteredWorkspace, "init", "-q").Run()

	s := New(&agentLoopFakeProvider{}, "test-model", 0)
	s.SetWorkDir(defaultWorkspace)

	query := url.Values{"workDir": []string{unregisteredWorkspace}}
	rec := httptest.NewRecorder()
	s.handleKairosGitStatus(rec, httptest.NewRequest(http.MethodGet, "/api/kairos/git?"+query.Encode(), nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"workspace_not_registered"`) {
		t.Fatalf("unregistered workspace status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"branch"`) {
		t.Fatalf("unregistered workspace leaked git status: %s", rec.Body.String())
	}

	missing := url.Values{"workDir": []string{unregisteredWorkspace + "-missing"}}
	missingRec := httptest.NewRecorder()
	s.handleKairosGitStatus(missingRec, httptest.NewRequest(http.MethodGet, "/api/kairos/git?"+missing.Encode(), nil))
	if missingRec.Code != http.StatusBadRequest || !strings.Contains(missingRec.Body.String(), `"code":"invalid_workspace"`) {
		t.Fatalf("missing workspace status=%d body=%s", missingRec.Code, missingRec.Body.String())
	}

	for name, target := range map[string]string{
		"default":    "/api/kairos/git",
		"registered": "/api/kairos/git?" + url.Values{"workDir": []string{registeredWorkspace}}.Encode(),
	} {
		allowedRec := httptest.NewRecorder()
		s.handleKairosGitStatus(allowedRec, httptest.NewRequest(http.MethodGet, target, nil))
		if allowedRec.Code != http.StatusOK || strings.Contains(allowedRec.Body.String(), "workspace_not_registered") {
			t.Fatalf("%s workspace status=%d body=%s", name, allowedRec.Code, allowedRec.Body.String())
		}
	}
}
