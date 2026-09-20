package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopReadOnlyBlocksBashAndProjectHookProcesses(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	target := filepath.Join(workDir, "readonly-must-not-write.txt")
	writeLoopHookConfig(t, workDir, map[string][]map[string]any{
		"session_start": {{"command": "session-start"}},
		"pre_tool_use":  {{"command": "pre-tool"}},
		"post_tool_use": {{"command": "post-tool"}},
		"session_end":   {{"command": "session-end"}},
	})
	hookRunner := &loopHookRunner{}
	hookRegistry := hooks.NewRegistryWithOptions(hooks.RegistryOptions{
		Runner:    hookRunner,
		ShellPath: "hook-shell",
	})
	toolRunner := &fakeBashRunner{name: "must-not-run", capabilities: fakeBashCapabilities()}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_readonly_bash", "Bash", map[string]string{
			"command": "echo changed > readonly-must-not-write.txt",
		}),
		textStep("The requested command was blocked."),
	}}
	userContent, _ := json.Marshal("Try the command in the project workspace.")
	eventCh := make(chan Event, 64)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
		Role: "user", Content: userContent,
	}}, workDir, RunOptions{
		ExecutionPolicyRequest: &ExecutionPolicyRequest{Mode: ExecutionModeReadOnly},
		HookRegistry:           hookRegistry,
		SandboxRunner:          toolRunner,
		SandboxPolicy:          fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy:         EvidencePolicyConfig{Policy: EvidencePolicyOff},
		DisablePlugins:         true,
		DisableWorkspaceMCP:    true,
	}, eventCh)

	approvalPrompted := false
	toolBlocked := false
	for event := range eventCh {
		switch event.Type {
		case "approval_required":
			approvalPrompted = true
		case "tool_result":
			if result, ok := event.Data.(map[string]interface{}); ok && result["id"] == "toolu_readonly_bash" {
				toolBlocked = result["isError"] == true
			}
		}
	}
	if toolRunner.calls != 0 {
		t.Fatalf("Bash runner calls = %d, want 0", toolRunner.calls)
	}
	if len(hookRunner.snapshot()) != 0 {
		t.Fatalf("read-only mode started project hook processes: %#v", hookRunner.snapshot())
	}
	if approvalPrompted {
		t.Fatal("read-only policy must deny the mutation before requesting approval")
	}
	if !toolBlocked {
		t.Fatal("read-only tool result was not marked blocked")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("read-only command changed target: stat error = %v", err)
	}
}

func TestRunLoopReadOnlyRejectsPathQualifiedReadOnlyCommand(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	toolRunner := &fakeBashRunner{name: "must-not-run", capabilities: fakeBashCapabilities()}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_readonly_impostor", "Bash", map[string]string{
			"command": "./cat README.md",
		}),
		textStep("The path-qualified command was blocked."),
	}}
	eventCh := make(chan Event, 32)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
		Role: "user", Content: mustJSON("Use only trusted read-only tools."),
	}}, workDir, RunOptions{
		ExecutionPolicyRequest: &ExecutionPolicyRequest{Mode: ExecutionModeReadOnly},
		SandboxRunner:          toolRunner,
		SandboxPolicy:          fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy:         EvidencePolicyConfig{Policy: EvidencePolicyOff},
		DisablePlugins:         true,
		DisableWorkspaceMCP:    true,
	}, eventCh)

	blocked := false
	for event := range eventCh {
		if event.Type == "tool_result" {
			if result, ok := event.Data.(map[string]interface{}); ok && result["id"] == "toolu_readonly_impostor" {
				blocked = result["isError"] == true
			}
		}
	}
	if !blocked || toolRunner.calls != 0 {
		t.Fatalf("path-qualified command blocked=%v runner calls=%d", blocked, toolRunner.calls)
	}
}

func TestRunLoopReadOnlyBlocksExplicitUndoBeforeCheckpointMutation(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	provider := &policyTestProvider{
		name:         "remote",
		models:       []types.ModelInfo{{ID: "model", ContextWindow: 4_096, MaxOutput: 1_024}},
		responseText: "unused",
	}
	eventCh := make(chan Event, 16)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
		Role: "user", Content: mustJSON("/undo"),
	}}, workDir, RunOptions{
		ExecutionPolicyRequest: &ExecutionPolicyRequest{Mode: ExecutionModeReadOnly},
	}, eventCh)
	blocked := false
	undoMessage := false
	for event := range eventCh {
		if event.Type == "blocked" {
			blocked = true
		}
		if event.Type == "text" && event.Data == "되돌릴 변경이 없습니다." {
			undoMessage = true
		}
	}
	if !blocked || undoMessage {
		t.Fatalf("read-only /undo outcome: blocked=%v undoMessage=%v", blocked, undoMessage)
	}
	if provider.lastRequest() != nil {
		t.Fatal("blocked /undo unexpectedly reached the provider")
	}
}

func TestRunLoopRejectsUnprovenFullSnapshot(t *testing.T) {
	provider := &policyTestProvider{name: "remote", models: []types.ModelInfo{{ID: "model"}}}
	eventCh := make(chan Event, 16)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
		Role: "user", Content: mustJSON("do work"),
	}}, t.TempDir(), RunOptions{
		ExecutionPolicy: &ExecutionPolicySnapshot{Mode: ExecutionModeFull, Revision: 3},
	}, eventCh)
	invalid := false
	for event := range eventCh {
		if event.Type == "error" && event.Data == "Execution policy configuration failed: invalid inherited policy" {
			invalid = true
		}
	}
	if !invalid {
		t.Fatal("unproven full snapshot was not rejected")
	}
	if provider.lastRequest() != nil {
		t.Fatal("invalid full snapshot unexpectedly reached the provider")
	}
}

func TestRunLoopFullModeWritesExternalTempFileWithoutPerCallApproval(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	externalDir := t.TempDir()
	target := filepath.Join(externalDir, "full-mode-output.txt")
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_full_external_write", "Write", map[string]string{
			"file_path": target,
			"content":   "full mode external write",
		}),
		textStep("The external file was written."),
	}}
	eventCh := make(chan Event, 64)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
		Role: "user", Content: mustJSON("Write the requested file."),
	}}, workDir, RunOptions{
		ExecutionPolicyRequest: &ExecutionPolicyRequest{Mode: ExecutionModeFull},
		SessionID:              "full-mode-checkpoint-live",
		DurableSessionID:       "full-mode-checkpoint-session",
		SessionRevision:        6,
		EvidencePolicy:         EvidencePolicyConfig{Policy: EvidencePolicyOff},
		DisablePlugins:         true,
		DisableWorkspaceMCP:    true,
		MCPServers:             []MCPServerSpec{},
	}, eventCh)

	approvalPrompted := false
	writeExecuted := false
	for event := range eventCh {
		switch event.Type {
		case "approval_required":
			approvalPrompted = true
		case "tool_result":
			if result, ok := event.Data.(map[string]interface{}); ok && result["id"] == "toolu_full_external_write" {
				writeExecuted = result["executed"] == true && result["isError"] != true
			}
		}
	}
	if approvalPrompted {
		t.Fatal("full-mode built-in external write requested per-call approval")
	}
	if !writeExecuted {
		t.Fatal("full-mode built-in external write did not execute")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("external file was not created: %v", err)
	}
	if string(data) != "full mode external write" {
		t.Fatalf("external file contents = %q", data)
	}
	manifest, _, found, err := readManifestStrict(workDir, "full-mode-checkpoint-session")
	if err != nil || !found {
		t.Fatalf("full-mode RunLoop checkpoint unavailable: found=%v err=%v", found, err)
	}
	key, err := checkpointTargetKey(workDir, target)
	if err != nil {
		t.Fatalf("external checkpoint key: %v", err)
	}
	entry := manifest.Files[key]
	canonicalTarget, err := canonicalizeTarget(target)
	if err != nil {
		t.Fatalf("canonicalize external target: %v", err)
	}
	if key == "" || entry.TargetPath == "" || !sameCheckpointPath(entry.TargetPath, canonicalTarget) ||
		entry.PreimageDigest != checkpointStateRevision(false, nil) ||
		entry.PostimageDigest != artifactBytesRevision([]byte("full mode external write")) || !entry.PostimageCaptured {
		t.Fatalf("full-mode external checkpoint = %+v (key=%q)", entry, key)
	}
}
