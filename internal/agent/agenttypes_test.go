package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadCustomAgentTypesParsesMarkdownFrontmatter(t *testing.T) {
	workDir := t.TempDir()
	agentsDir := filepath.Join(workDir, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
name: qa-custom
description: |
  Runs focused QA checks.
  Reports actionable failures.
tools:
  - Read
  - Grep
model: qwen3-coder:30b
readOnly: true
---

# QA Custom

Review changed files and report issues.
`
	if err := os.WriteFile(filepath.Join(agentsDir, "qa-custom.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	types := LoadCustomAgentTypes(workDir)
	got, ok := types["qa-custom"]
	if !ok {
		t.Fatalf("custom agent not loaded: %+v", types)
	}
	if got.Name != "qa-custom" {
		t.Fatalf("Name = %q", got.Name)
	}
	if got.Description != "Runs focused QA checks. Reports actionable failures." {
		t.Fatalf("Description = %q", got.Description)
	}
	if !reflect.DeepEqual(got.Tools, []string{"Read", "Grep"}) {
		t.Fatalf("Tools = %+v", got.Tools)
	}
	if got.Model != "qwen3-coder:30b" || !got.ReadOnly {
		t.Fatalf("runtime fields = model %q readOnly %v", got.Model, got.ReadOnly)
	}
	if got.SystemPrompt != "# QA Custom\n\nReview changed files and report issues." {
		t.Fatalf("SystemPrompt = %q", got.SystemPrompt)
	}
}

func TestLoadCustomAgentTypesUsesFilenameFallbackAndInlineTools(t *testing.T) {
	workDir := t.TempDir()
	agentsDir := filepath.Join(workDir, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
description: Inline tools custom agent
tools: [Read, Grep, Bash]
---
Prompt body.
`
	if err := os.WriteFile(filepath.Join(agentsDir, "file-agent.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	types := LoadCustomAgentTypes(workDir)
	got, ok := types["file-agent"]
	if !ok {
		t.Fatalf("filename fallback agent not loaded: %+v", types)
	}
	if got.Name != "file-agent" {
		t.Fatalf("Name = %q", got.Name)
	}
	if !reflect.DeepEqual(got.Tools, []string{"Read", "Grep", "Bash"}) {
		t.Fatalf("Tools = %+v", got.Tools)
	}
}
