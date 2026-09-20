package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
)

func TestSkillListingEndpointsUsePersistedSourceAndCustomRoots(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	homeDir := t.TempDir()
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("HOME", homeDir)
	workDir := t.TempDir()
	extraDir := filepath.Join(t.TempDir(), "custom skills 한글")
	writeServerSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "project-skill")
	writeServerSkillFixture(t, extraDir, "custom-skill")
	cfg := config.DefaultConfig()
	cfg.SkillDirs = []string{extraDir}
	cfg.SkillSource = "none"
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save test skill config: %v", err)
	}

	s := New(nil, "", 0)
	if got := requestSkillJSON(t, s.handleSkillsList, "/api/skills", workDir); len(got) != 0 {
		t.Fatalf("/api/skills ignored source none: %v", got)
	}
	if got := requestCommandNames(t, s.handleCommandsList, "/api/commands", workDir); len(got) != 0 {
		t.Fatalf("/api/commands advertised disabled skills: %v", got)
	}
	contextResponse := requestJSONMap(t, s.handleProjectContext, "/api/context", workDir)
	if got := int(contextResponse["skills"].(float64)); got != 0 {
		t.Fatalf("/api/context skill count = %d, want 0 with source none", got)
	}
	ragServer := New(nil, workDir, 0)
	for _, query := range []string{"project-skill", "custom-skill"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/rag?q="+url.QueryEscape(query), nil)
		ragServer.handleRAGSearch(recorder, request)
		if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "Instructions for ") {
			t.Fatalf("/api/rag exposed disabled %q skill: status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
	}

	cfg.SkillSource = "all"
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save enabled skill config: %v", err)
	}
	listed := requestSkillJSON(t, s.handleSkillsList, "/api/skills", workDir)
	commands := requestCommandNames(t, s.handleCommandsList, "/api/commands", workDir)
	contextResponse = requestJSONMap(t, s.handleProjectContext, "/api/context", workDir)
	if !reflect.DeepEqual(listed, []string{"custom-skill", "project-skill"}) {
		t.Fatalf("/api/skills names = %v", listed)
	}
	if !reflect.DeepEqual(commands, []string{"custom-skill", "project-skill"}) {
		t.Fatalf("/api/commands names = %v", commands)
	}
	if got := int(contextResponse["skills"].(float64)); got != len(listed) {
		t.Fatalf("/api/context skill count = %d, /api/skills count = %d", got, len(listed))
	}
}

func TestInvalidPersistedSkillSourceFailsClosedAtListingBoundary(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.SkillSource = "unexpected"
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save test skill config: %v", err)
	}
	s := New(nil, "", 0)
	request := httptest.NewRequest(http.MethodGet, "/api/skills", nil)
	response := httptest.NewRecorder()
	s.handleSkillsList(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid source status = %d body=%s, want 503", response.Code, response.Body.String())
	}
}

func TestProjectSkillSettingsAreWorkspaceScopedAndConfigurable(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	homeDir := t.TempDir()
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("HOME", homeDir)
	projectA := t.TempDir()
	projectB := t.TempDir()
	globalSkills := filepath.Join(t.TempDir(), "global skills")
	projectSkills := filepath.Join(t.TempDir(), "project skills 한글")
	writeServerSkillFixture(t, filepath.Join(projectA, ".claude", "skills"), "project-a-skill")
	writeServerSkillFixture(t, filepath.Join(projectB, ".claude", "skills"), "project-b-skill")
	writeServerSkillFixture(t, globalSkills, "global-skill")
	writeServerSkillFixture(t, projectSkills, "project-custom-skill")

	cfg := config.DefaultConfig()
	cfg.SkillSource = "all"
	cfg.SkillDirs = []string{globalSkills}
	cfg.Projects = []config.Project{
		{Path: projectA, Name: "A", SkillSource: "none"},
		{Path: projectB, Name: "B"},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save test project skill config: %v", err)
	}
	s := New(nil, projectA, 0)
	if got := requestSkillJSON(t, s.handleSkillsList, "/api/skills", projectA); len(got) != 0 {
		t.Fatalf("project A none policy loaded skills: %v", got)
	}
	if got, want := requestSkillJSON(t, s.handleSkillsList, "/api/skills", projectB), []string{"global-skill", "project-b-skill"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project B skills = %v, want %v", got, want)
	}

	body, err := json.Marshal(map[string]any{
		"source":    "claude",
		"workDir":   projectA,
		"skillDirs": []string{projectSkills},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/skill-source", strings.NewReader(string(body)))
	response := httptest.NewRecorder()
	s.handleSetSkillSource(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save project skill settings status=%d body=%s", response.Code, response.Body.String())
	}
	if got, want := requestSkillJSON(t, s.handleSkillsList, "/api/skills", projectA), []string{"global-skill", "project-a-skill", "project-custom-skill"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated project A skills = %v, want %v", got, want)
	}
	if got, want := requestSkillJSON(t, s.handleSkillsList, "/api/skills", projectB), []string{"global-skill", "project-b-skill"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project B changed after project A update: got %v, want %v", got, want)
	}
}

func TestSkillListPreservesArrayAndReportsNamespacedProjectCollisions(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	homeDir := t.TempDir()
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("HOME", homeDir)
	workDir := t.TempDir()
	projectCustom := filepath.Join(t.TempDir(), "project custom")
	globalCustom := filepath.Join(t.TempDir(), ".claude-custom-root")
	writeServerSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "shared")
	writeServerSkillFixture(t, filepath.Join(workDir, ".codex", "skills"), "Shared")
	registerServerSkillAliases(t, filepath.Join(workDir, ".claude", "skills"), "shared", "shared-flow")
	registerServerSkillAliases(t, filepath.Join(workDir, ".codex", "skills"), "Shared", "shared-flow")
	writeServerSkillFixture(t, filepath.Join(workDir, ".agents", "skills"), "agents-only")
	writeServerSkillFixture(t, projectCustom, "shared")
	writeServerSkillFixture(t, globalCustom, "shared")
	cfg := config.DefaultConfig()
	cfg.SkillSource = "all"
	cfg.SkillDirs = []string{globalCustom}
	cfg.Projects = []config.Project{{
		Path: workDir, Name: "collision fixture", SkillSource: "all", SkillDirs: []string{projectCustom},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save test collision config: %v", err)
	}
	s := New(nil, workDir, 0)
	request := httptest.NewRequest(http.MethodGet, "/api/skills?workDir="+url.QueryEscape(workDir), nil)
	response := httptest.NewRecorder()
	s.handleSkillsList(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("skill list status=%d body=%s", response.Code, response.Body.String())
	}
	var listed []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Digest      string   `json:"digest"`
		Size        int64    `json:"size"`
		ModuleRoot  string   `json:"module_root"`
		Aliases     []string `json:"aliases"`
		Source      string   `json:"source"`
		Namespace   string   `json:"namespace"`
		Shadowed    []struct {
			ID          string   `json:"id"`
			Description string   `json:"description"`
			Digest      string   `json:"digest"`
			Source      string   `json:"source"`
			Namespace   string   `json:"namespace"`
			Path        string   `json:"path"`
			ModuleRoot  string   `json:"module_root"`
			Aliases     []string `json:"aliases"`
			Content     string   `json:"content"`
		} `json:"shadowed"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("skills API stopped returning a bare array: %v; body=%s", err, response.Body.String())
	}
	if len(listed) != 2 {
		t.Fatalf("skills API entries = %+v, want two winners", listed)
	}
	shared := listed[0]
	if shared.Name != "shared" || shared.Source != "claude" || shared.Namespace != "project-claude" ||
		shared.ID == "" || shared.Description != "shared" || len(shared.Digest) != len("sha256:")+64 ||
		shared.Size <= 0 || shared.ModuleRoot == "" || !reflect.DeepEqual(shared.Aliases, []string{"shared-flow"}) {
		t.Fatalf("winner metadata = %+v", shared)
	}
	if len(shared.Shadowed) != 3 {
		t.Fatalf("collision warnings = %+v, want three shadowed roots", shared.Shadowed)
	}
	wantNamespaces := []string{"project-codex", "project-custom-1", "custom-1"}
	for index, want := range wantNamespaces {
		if shared.Shadowed[index].Namespace != want {
			t.Fatalf("shadowed[%d] namespace = %q, want %q", index, shared.Shadowed[index].Namespace, want)
		}
		if shared.Shadowed[index].Path == "" || shared.Shadowed[index].Content != "" ||
			shared.Shadowed[index].ID == "" || shared.Shadowed[index].Digest == "" || shared.Shadowed[index].Description == "" {
			t.Fatalf("shadowed metadata leaked content or omitted origin: %+v", shared.Shadowed[index])
		}
	}
	if strings.Contains(response.Body.String(), "Instructions for shared") {
		t.Fatal("skill metadata API exposed skill bodies")
	}

	commandsRequest := httptest.NewRequest(http.MethodGet, "/api/commands?workDir="+url.QueryEscape(workDir), nil)
	commandsResponse := httptest.NewRecorder()
	s.handleCommandsList(commandsResponse, commandsRequest)
	var commands []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(commandsResponse.Body.Bytes(), &commands); err != nil {
		t.Fatalf("decode namespaced slash commands: %v; body=%s", err, commandsResponse.Body.String())
	}
	commandNames := make([]string, 0, len(commands))
	for _, command := range commands {
		commandNames = append(commandNames, command.Name)
	}
	for _, want := range []string{"shared", "project-claude/shared", "project-codex/Shared", "project-custom-1/shared", "custom-1/shared", "shared-flow", "project-codex/shared-flow"} {
		if !skillTestContains(commandNames, want) {
			t.Errorf("namespaced command %q missing from %v", want, commandNames)
		}
	}
}

type skillListHandler func(http.ResponseWriter, *http.Request)

func requestSkillJSON(t *testing.T, handler skillListHandler, path, workDir string) []string {
	t.Helper()
	query := url.Values{"workDir": []string{workDir}}
	request := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
	}
	var values []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &values); err != nil {
		t.Fatalf("decode %s: %v; body=%s", path, err, response.Body.String())
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Name)
	}
	sort.Strings(result)
	return result
}

func requestCommandNames(t *testing.T, handler skillListHandler, path, workDir string) []string {
	return requestSkillJSON(t, handler, path, workDir)
}

func requestJSONMap(t *testing.T, handler skillListHandler, path, workDir string) map[string]any {
	t.Helper()
	query := url.Values{"workDir": []string{workDir}}
	request := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v; body=%s", path, err, response.Body.String())
	}
	return value
}

func writeServerSkillFixture(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("---\ndescription: %s\n---\n\nInstructions for %s.\n", name, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func registerServerSkillAliases(t *testing.T, root, name string, aliases ...string) {
	t.Helper()
	path := filepath.Join(root, name, "SKILL.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	list := "aliases:\n"
	for _, alias := range aliases {
		list += "  - " + alias + "\n"
	}
	updated := strings.Replace(string(content), "\n---\n", "\n"+list+"---\n", 1)
	if updated == string(content) {
		t.Fatalf("skill fixture %q has no frontmatter boundary", path)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func skillTestContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
