package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestSubAgentAccumulatesPartialJSONBeforeScopeAndExecution(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	workDir := t.TempDir()
	provider := &alternateLoopTestProvider{
		name: "remote",
		models: []types.ModelInfo{{
			ID:            "test-model",
			ContextWindow: 16_384,
			MaxOutput:     2_048,
		}},
		steps: []alternateLoopStep{
			{
				toolName: "Write",
				toolID:   "write-1",
				inputChunks: []string{
					`{"file_path":"created.txt",`,
					`"content":"hello from chunks"}`,
				},
			},
			{text: "done"},
		},
	}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", workDir, SubAgentManagerOptions{
		SessionID:         "subagent-partial-json-test",
		ApprovalRequester: &allowingApprovalRequester{},
		// This regression targets fragmented native JSON. Keep the new <=16K
		// selector policy out of the scripted two-request fixture.
		HarnessProfile: directAlternateLoopHarness(t),
	})
	task := &SubAgentTask{
		ID:          "sub-test",
		Name:        "partial-json",
		Instruction: "Create the assigned file",
		Files:       []string{"created.txt"},
		Status:      "pending",
	}

	manager.run(task)

	content, err := os.ReadFile(filepath.Join(workDir, "created.txt"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if got, want := string(content), "hello from chunks"; got != want {
		t.Fatalf("created content = %q, want %q", got, want)
	}
	if task.Status != "completed" || task.ToolCalls != 1 || task.Result != "done" {
		t.Fatalf("task result = %#v", task)
	}
	if provider.modelCallCount() != 0 {
		t.Fatalf("SubAgent Models() calls = %d, want zero with an explicit HarnessProfile", provider.modelCallCount())
	}
	requests := provider.requestsSnapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	if got := requests[0].MaxTokens; got != 2_048 {
		t.Fatalf("SubAgent MaxTokens = %d, want model reserve 2048", got)
	}
	history := marshalMessagesForTest(t, requests[1].Messages)
	if !strings.Contains(history, `hello from chunks`) ||
		!strings.Contains(history, `"is_error":false`) {
		t.Fatalf("partial input/result missing from protocol history: %s", history)
	}
}

func TestSubAgentUnknownAndInvalidCallsRemainErrors(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	tests := []struct {
		name        string
		step        alternateLoopStep
		files       []string
		wantMessage string
	}{
		{
			name: "unknown tool",
			step: alternateLoopStep{
				toolName:    "NotInCatalog",
				toolID:      "unknown-1",
				inputChunks: []string{`{}`},
			},
			wantMessage: "Tool is not present in the immutable catalog",
		},
		{
			name: "invalid partial JSON",
			step: alternateLoopStep{
				toolName:    "Write",
				toolID:      "invalid-1",
				inputChunks: []string{`{"file_path":"bad.txt"`},
			},
			files:       []string{"bad.txt"},
			wantMessage: "Invalid tool input",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			provider := &alternateLoopTestProvider{
				name:   "remote",
				models: []types.ModelInfo{{ID: "test-model"}},
				steps:  []alternateLoopStep{test.step, {text: "done"}},
			}
			manager := NewSubAgentManager(provider, "test-model", workDir)
			task := &SubAgentTask{
				ID:          "sub-error",
				Name:        test.name,
				Instruction: "Do not execute malformed calls",
				Files:       test.files,
				Status:      "pending",
			}

			manager.run(task)

			requests := provider.requestsSnapshot()
			if len(requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(requests))
			}
			history := marshalMessagesForTest(t, requests[1].Messages)
			if !strings.Contains(history, test.wantMessage) ||
				!strings.Contains(history, `"is_error":true`) {
				t.Fatalf("synthetic error missing from history: %s", history)
			}
			if _, err := os.Stat(filepath.Join(workDir, "bad.txt")); !os.IsNotExist(err) {
				t.Fatalf("invalid call reached executor: %v", err)
			}
		})
	}
}

func TestSubAgentOwnershipUsesPathBoundaries(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	workDir := t.TempDir()
	manager := NewSubAgentManager(nil, "", workDir)
	task := &SubAgentTask{Files: []string{"src/allowed.go"}}

	if !manager.isAllowedFile(task, "Edit", json.RawMessage(`{"file_path":"src/allowed.go"}`)) {
		t.Fatal("exact owned path was rejected")
	}
	if manager.isAllowedFile(task, "Edit", json.RawMessage(`{"file_path":"src/allowed.go.bak"}`)) {
		t.Fatal("substring path escaped exact ownership boundary")
	}

	task.Files = []string{"src/**"}
	if !manager.isAllowedFile(task, "Write", json.RawMessage(`{"file_path":"src/nested/result.go"}`)) {
		t.Fatal("recursive owned scope rejected a nested path")
	}
	if manager.isAllowedFile(task, "Write", json.RawMessage(`{"file_path":"src-elsewhere/result.go"}`)) {
		t.Fatal("recursive scope accepted a sibling prefix")
	}
}
