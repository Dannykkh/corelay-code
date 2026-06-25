package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aniclew/aniclew/internal/agent"
)

func TestHandleAgentTypesIncludesCustomAgents(t *testing.T) {
	workDir := t.TempDir()
	agentsDir := filepath.Join(workDir, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
name: local-reviewer
description: Project-specific reviewer
tools: Read, Grep
readOnly: true
---
Review project-specific constraints.
`
	if err := os.WriteFile(filepath.Join(agentsDir, "local-reviewer.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(serverTestProvider{}, "fake-model", 0)
	s.SetWorkDir(workDir)

	req := httptest.NewRequest(http.MethodGet, "/api/agent-types", nil)
	rec := httptest.NewRecorder()
	s.handleAgentTypes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]agent.AgentType
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("agent types json: %v", err)
	}
	if _, ok := got["coder"]; !ok {
		t.Fatalf("builtin coder missing: %+v", got)
	}
	custom, ok := got["local-reviewer"]
	if !ok {
		t.Fatalf("custom agent missing: %+v", got)
	}
	if custom.Description != "Project-specific reviewer" || !custom.ReadOnly {
		t.Fatalf("unexpected custom agent: %+v", custom)
	}
}
