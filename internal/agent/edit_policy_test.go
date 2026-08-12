package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
)

func TestApplyUnifiedPatchMultipleHunks(t *testing.T) {
	content := "alpha\nbeta\ngamma\ndelta\nepsilon\n"
	patch := strings.Join([]string{
		"--- a/state.txt",
		"+++ b/state.txt",
		"@@ -1,2 +1,2 @@",
		" alpha",
		"-beta",
		"+BETA",
		"@@ -4,2 +4,3 @@",
		" delta",
		"+inserted",
		" epsilon",
	}, "\n")
	result, changes, err := applyUnifiedPatch(content, patch)
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\nBETA\ngamma\ndelta\ninserted\nepsilon\n"
	if result != want || changes != 3 {
		t.Fatalf("result=%q changes=%d want=%q/3", result, changes, want)
	}
}

func TestApplyUnifiedPatchRejectsMismatchAndLimits(t *testing.T) {
	if _, _, err := applyUnifiedPatch("alpha\n", "@@ -1 +1 @@\n-wrong\n+beta"); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched patch err=%v", err)
	}
	if _, _, err := applyUnifiedPatch("alpha\n", strings.Repeat("x", maxUnifiedPatchBytes+1)); err == nil ||
		!strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized patch err=%v", err)
	}
}

func TestEditPoliciesHaveDistinctExecutionBehavior(t *testing.T) {
	t.Run("exact rejects fuzzy", func(t *testing.T) {
		workDir := t.TempDir()
		path := filepath.Join(workDir, "state.txt")
		if err := os.WriteFile(path, []byte("\tvalue = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		input, _ := json.Marshal(map[string]any{
			"file_path":  "state.txt",
			"old_string": "    value = 1",
			"new_string": "\tvalue = 2",
		})
		output, isError := executeEditV2WithOptions(input, workDir, ToolExecutionOptions{EditPolicy: harness.EditExact})
		if !isError || !strings.Contains(output, "exact") {
			t.Fatalf("output=%q isError=%v", output, isError)
		}
		content, _ := os.ReadFile(path)
		if string(content) != "\tvalue = 1\n" {
			t.Fatalf("exact policy changed file: %q", content)
		}
	})

	t.Run("patch first applies unified hunk", func(t *testing.T) {
		workDir := t.TempDir()
		path := filepath.Join(workDir, "state.txt")
		if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		input, _ := json.Marshal(map[string]any{
			"file_path": "state.txt",
			"patch":     "@@ -1,2 +1,2 @@\n alpha\n-beta\n+BETA",
		})
		output, isError := executeEditV2WithOptions(input, workDir, ToolExecutionOptions{EditPolicy: harness.EditPatchFirst})
		if isError || !strings.Contains(output, "Edited") {
			t.Fatalf("output=%q isError=%v", output, isError)
		}
		content, _ := os.ReadFile(path)
		if string(content) != "alpha\nBETA\n" {
			t.Fatalf("patch result=%q", content)
		}
	})

	t.Run("whole file replaces existing artifact", func(t *testing.T) {
		workDir := t.TempDir()
		path := filepath.Join(workDir, "state.txt")
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		input, _ := json.Marshal(map[string]any{
			"file_path": "state.txt",
			"content":   "whole replacement",
		})
		output, isError := executeEditV2WithOptions(input, workDir, ToolExecutionOptions{EditPolicy: harness.EditWholeFile})
		if isError || !strings.Contains(output, "Edited") {
			t.Fatalf("output=%q isError=%v", output, isError)
		}
		content, _ := os.ReadFile(path)
		if string(content) != "whole replacement" {
			t.Fatalf("whole-file result=%q", content)
		}
	})
}

func TestEditPolicyCatalogSchemaAndExecutorStayAligned(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "state.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := applyEditPolicyToToolDefs(AllToolDefs(workDir), harness.EditPatchFirst)
	allowed := toolCatalogNames(tools)
	missingPatch := dispatchTestCall("missing-patch", "Edit", map[string]any{
		"file_path":  "state.txt",
		"old_string": "beta",
		"new_string": "BETA",
	})
	validPatch := dispatchTestCall("valid-patch", "Edit", map[string]any{
		"file_path": "state.txt",
		"patch":     "@@ -1,2 +1,2 @@\n alpha\n-beta\n+BETA",
	})
	results := dispatchToolCalls([]toolUseBlock{missingPatch, validPatch}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     allowed,
		PermissionConfig: dispatchPermissionConfig("moderate"),
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(call toolUseBlock) (string, bool) {
			return ExecuteToolWithOptions(call.Name, call.Input, workDir, ToolExecutionOptions{EditPolicy: harness.EditPatchFirst})
		},
	})
	if len(results) != 2 || !results[0].Synthetic || !results[0].IsError || results[1].IsError {
		t.Fatalf("results=%#v", results)
	}
	if !strings.Contains(results[0].Content, "required property") {
		t.Fatalf("schema rejection=%q", results[0].Content)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "alpha\nBETA\n" {
		t.Fatalf("catalog-aligned result=%q", content)
	}
}
