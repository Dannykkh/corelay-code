package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestDispatchMutationPreconditionBlocksApprovalExecutionGap(t *testing.T) {
	for _, test := range []struct {
		name    string
		tool    string
		input   map[string]any
		catalog types.ToolDef
	}{
		{
			name:    "write",
			tool:    "Write",
			input:   map[string]any{"file_path": "state.txt", "content": "agent"},
			catalog: currentBuiltInToolDefinitions()["Write"],
		},
		{
			name:    "edit",
			tool:    "Edit",
			input:   map[string]any{"file_path": "state.txt", "old_string": "before", "new_string": "agent"},
			catalog: currentBuiltInToolDefinitions()["Edit"],
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			path := filepath.Join(workDir, "state.txt")
			if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			ledger := NewReadLedger(workDir)
			if err := ledger.RecordRead(path); err != nil {
				t.Fatal(err)
			}
			call := dispatchTestCall("mutation-gap", test.tool, test.input)
			results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
				Context:          context.Background(),
				WorkDir:          workDir,
				AllowedTools:     toolCatalogNames([]types.ToolDef{test.catalog}),
				PermissionConfig: dispatchPermissionConfig("moderate"),
				ReadBeforeWrite:  true,
				ReadLedger:       ledger,
				SnapshotDecision: func(toolUseBlock) string { return "allow" },
				Execute: func(call toolUseBlock) (string, bool) {
					if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
						t.Fatal(err)
					}
					return ExecuteTool(call.Name, call.Input, workDir)
				},
			})
			if len(results) != 1 || !results[0].IsError || !results[0].Executed ||
				!strings.Contains(results[0].Content, "MUTATION BLOCKED") {
				t.Fatalf("dispatch result=%#v", results)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != "external" {
				t.Fatalf("external content was overwritten: content=%q err=%v", content, err)
			}
		})
	}
}

func TestDispatchNewWritePreconditionNeverOverwritesAppearedTarget(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "new.txt")
	ledger := NewReadLedger(workDir)
	call := dispatchTestCall("new-race", "Write", map[string]any{
		"file_path": "new.txt",
		"content":   "agent",
	})
	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames([]types.ToolDef{currentBuiltInToolDefinitions()["Write"]}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       ledger,
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(call toolUseBlock) (string, bool) {
			if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
				t.Fatal(err)
			}
			return ExecuteTool(call.Name, call.Input, workDir)
		},
	})
	if len(results) != 1 || !results[0].IsError ||
		!strings.Contains(results[0].Content, "MUTATION BLOCKED") {
		t.Fatalf("dispatch result=%#v", results)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "external" {
		t.Fatalf("appeared target was overwritten: content=%q err=%v", content, err)
	}
}

func TestFileMutationExecutionTokenIsOneShot(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "state.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(path); err != nil {
		t.Fatal(err)
	}
	call := dispatchTestCall("one-shot", "Write", map[string]any{
		"file_path": "state.txt",
		"content":   "after",
	})
	var replayOutput string
	var replayError bool
	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames([]types.ToolDef{currentBuiltInToolDefinitions()["Write"]}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       ledger,
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(call toolUseBlock) (string, bool) {
			firstOutput, firstError := ExecuteTool(call.Name, call.Input, workDir)
			replayOutput, replayError = ExecuteTool(call.Name, call.Input, workDir)
			return firstOutput, firstError
		},
	})
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("first execution result=%#v", results)
	}
	if !replayError || !strings.Contains(replayOutput, "already consumed") {
		t.Fatalf("replay output=%q isError=%v", replayOutput, replayError)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "after" {
		t.Fatalf("committed content=%q err=%v", content, err)
	}
}

func TestStagedArtifactCommitIsConditionalAndCleansTemporaryFiles(t *testing.T) {
	workDir := t.TempDir()
	target := filepath.Join(workDir, "state.txt")
	if err := os.WriteFile(target, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := stageArtifact(target, []byte("agent"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stage.cleanup)
	expected := artifactBytesRevision([]byte("before"))
	if err := os.WriteFile(target, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.commit(expected, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("conditional commit err=%v", err)
	}
	stage.cleanup()
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "external" {
		t.Fatalf("conditional commit overwrote target: content=%q err=%v", content, err)
	}
	temps, err := filepath.Glob(filepath.Join(workDir, ".corelay-mutation-*.txt"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary artifacts=%v err=%v", temps, err)
	}
}

func TestMutationEnvelopeRejectsForgedToken(t *testing.T) {
	raw, err := json.Marshal(fileMutationExecutionEnvelope{
		Protocol: fileMutationExecutionProtocol,
		Token:    strings.Repeat("0", 48),
		Input:    json.RawMessage(`{"file_path":"x","content":"y"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	output, isError := ExecuteTool("Write", raw, t.TempDir())
	if !isError || !strings.Contains(output, "unknown or already consumed") {
		t.Fatalf("forged envelope output=%q isError=%v", output, isError)
	}
}

func TestFileMutationBatchPreflightPreventsPartialStart(t *testing.T) {
	workDir := t.TempDir()
	existing := filepath.Join(workDir, "existing.txt")
	if err := os.WriteFile(existing, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := NewReadLedger(workDir)
	calls := []toolUseBlock{
		dispatchTestCall("new-first", "Write", map[string]any{
			"file_path": "new.txt",
			"content":   "must not start",
		}),
		dispatchTestCall("stale-second", "Write", map[string]any{
			"file_path": "existing.txt",
			"content":   "must not apply",
		}),
	}
	executed := 0
	results := dispatchToolCalls(calls, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames([]types.ToolDef{currentBuiltInToolDefinitions()["Write"]}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       ledger,
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
	})
	if len(results) != 2 || !results[0].Synthetic || !results[1].Synthetic || executed != 0 {
		t.Fatalf("preflight results=%#v executed=%d", results, executed)
	}
	if _, err := os.Stat(filepath.Join(workDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("batch began before full preflight: %v", err)
	}
	content, err := os.ReadFile(existing)
	if err != nil || string(content) != "before" {
		t.Fatalf("existing target changed: content=%q err=%v", content, err)
	}
}

func TestFileMutationBatchRollsBackEarlierCommit(t *testing.T) {
	workDir := t.TempDir()
	firstPath := filepath.Join(workDir, "first.txt")
	if err := os.WriteFile(firstPath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(firstPath); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "batch-lint", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		return sandbox.Result{Started: true, ExitCode: 2, Err: errors.New("syntax")}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
			Failure:              sandbox.FailureExecutionFailed,
		}
	}
	calls := []toolUseBlock{
		dispatchTestCall("first", "Write", map[string]any{
			"file_path": "first.txt",
			"content":   "committed then rolled back",
		}),
		dispatchTestCall("second", "Write", map[string]any{
			"file_path": "second.go",
			"content":   "package broken\nfunc {",
		}),
		dispatchTestCall("third", "Write", map[string]any{
			"file_path": "third.txt",
			"content":   "must be aborted",
		}),
	}
	opts := secureToolProcessOptions(runner)
	results := dispatchToolCalls(calls, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames([]types.ToolDef{currentBuiltInToolDefinitions()["Write"]}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       ledger,
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(call toolUseBlock) (string, bool) {
			return ExecuteToolWithOptions(call.Name, call.Input, workDir, opts)
		},
	})
	if len(results) != 3 || !results[0].IsError || !results[1].IsError || !results[2].Synthetic {
		t.Fatalf("transaction results=%#v", results)
	}
	if !strings.Contains(results[0].Content, "TRANSACTION ROLLED BACK") ||
		!strings.Contains(results[2].Content, "batch was aborted") {
		t.Fatalf("transaction messages=%q / %q", results[0].Content, results[2].Content)
	}
	content, err := os.ReadFile(firstPath)
	if err != nil || string(content) != "before" {
		t.Fatalf("first target was not restored: content=%q err=%v", content, err)
	}
	for _, name := range []string{"second.go", "third.txt"} {
		if _, err := os.Stat(filepath.Join(workDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived aborted transaction: %v", name, err)
		}
	}
	if decision := ledger.CheckWrite(firstPath); !decision.Allowed || decision.Code != ReadLedgerAllowed {
		t.Fatalf("ledger was not reconciled after rollback: %#v", decision)
	}
}

func TestFileMutationBatchRollbackPreservesLaterExternalChange(t *testing.T) {
	workDir := t.TempDir()
	firstPath := filepath.Join(workDir, "first.txt")
	if err := os.WriteFile(firstPath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(firstPath); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "batch-external", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		if err := os.WriteFile(firstPath, []byte("external"), 0o600); err != nil {
			t.Fatal(err)
		}
		return sandbox.Result{Started: true, ExitCode: 2, Err: errors.New("syntax")}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
			Failure:              sandbox.FailureExecutionFailed,
		}
	}
	opts := secureToolProcessOptions(runner)
	results := dispatchToolCalls([]toolUseBlock{
		dispatchTestCall("first", "Write", map[string]any{
			"file_path": "first.txt",
			"content":   "agent",
		}),
		dispatchTestCall("second", "Write", map[string]any{
			"file_path": "second.go",
			"content":   "package broken\nfunc {",
		}),
	}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames([]types.ToolDef{currentBuiltInToolDefinitions()["Write"]}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       ledger,
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(call toolUseBlock) (string, bool) {
			return ExecuteToolWithOptions(call.Name, call.Input, workDir, opts)
		},
	})
	if len(results) != 2 || !results[0].IsError ||
		!strings.Contains(results[0].Content, "TRANSACTION INCOMPLETE") {
		t.Fatalf("transaction results=%#v", results)
	}
	content, err := os.ReadFile(firstPath)
	if err != nil || string(content) != "external" {
		t.Fatalf("external content was overwritten: content=%q err=%v", content, err)
	}
}
