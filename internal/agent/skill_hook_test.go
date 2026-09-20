package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestAutoSkillCreationIsDisabledByDefaultForEligibleConversation(t *testing.T) {
	t.Setenv("CORELAY_AUTOSKILL", "")
	t.Setenv("ANICLEW_AUTOSKILL", "")
	provider := &mockProvider{responses: []string{`{"create":true,"name":"repeatable-web-check","description":"Check a web workflow.","when_to_use":"Use when validating a web release.","body":"## Goal\nCheck the release."}`}}
	workDir := t.TempDir()

	if stats := analyzeTaskStats(autoSkillEligibleMessages()); !stats.Eligible() {
		t.Fatalf("fixture must pass the complexity gate: %+v", stats)
	}
	CreateSkillAsync(context.Background(), provider, "fixture-model", workDir, autoSkillEligibleMessages())
	if !WaitForMemoryTasks(10 * time.Second) {
		t.Fatal("background memory/skill tasks did not finish")
	}
	if provider.calls != 0 {
		t.Fatalf("default-off auto-skill called provider %d times", provider.calls)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatalf("default-off auto-skill created skill storage: stat error = %v", err)
	}
}

func TestAutoSkillCreationRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("CORELAY_AUTOSKILL", "on")
	t.Setenv("ANICLEW_AUTOSKILL", "off")
	provider := &mockProvider{responses: []string{`{"create":true,"name":"repeatable-web-check","description":"Check a web workflow.","when_to_use":"Use when validating a web release.","body":"## Goal\nCheck the release."}`}}
	workDir := t.TempDir()

	CreateSkillAsync(context.Background(), provider, "fixture-model", workDir, autoSkillEligibleMessages())
	if !WaitForMemoryTasks(10 * time.Second) {
		t.Fatal("background memory/skill tasks did not finish")
	}
	if provider.calls != 1 {
		t.Fatalf("explicit opt-in provider calls = %d, want 1", provider.calls)
	}
	path := filepath.Join(workDir, ".claude", "skills", "repeatable-web-check", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("explicit opt-in did not save the skill: %v", err)
	}
	if !strings.Contains(string(data), "Check the release.") {
		t.Fatalf("saved skill body missing from %s: %q", path, data)
	}
}

func autoSkillEligibleMessages() []types.Message {
	toolUses := []map[string]string{
		{"type": "tool_use", "name": "Read"},
		{"type": "tool_use", "name": "Grep"},
		{"type": "tool_use", "name": "Bash"},
		{"type": "tool_use", "name": "Read"},
		{"type": "tool_use", "name": "Bash"},
	}
	toolResults := []map[string]any{{"type": "tool_result", "is_error": true}}
	return []types.Message{
		{Role: "assistant", Content: mustJSON(toolUses)},
		{Role: "user", Content: mustJSON(toolResults)},
	}
}
