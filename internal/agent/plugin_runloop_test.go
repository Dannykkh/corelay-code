package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type pluginDecisionRequester struct {
	mu      sync.Mutex
	drafts  []approval.Draft
	outcome approval.Outcome
	mutate  func()
}

func (requester *pluginDecisionRequester) Open(draft approval.Draft) (approval.Pending, error) {
	requester.mu.Lock()
	requester.drafts = append(requester.drafts, draft)
	index := len(requester.drafts)
	requester.mu.Unlock()
	return approval.Pending{
		ID:              "plugin-approval-" + string(rune('a'+index-1)),
		SessionID:       draft.SessionID,
		SessionRevision: draft.SessionRevision,
		RunID:           draft.RunID,
		ToolCallID:      draft.ToolCallID,
		ToolName:        draft.ToolName,
		RedactedInput:   draft.RedactedInput,
		InputDigest:     draft.InputDigest,
		DangerLevel:     draft.DangerLevel,
		Scope:           draft.Scope,
		RememberAllowed: draft.RememberAllowed,
		ExpiresAt:       time.Now().Add(time.Minute),
	}, nil
}

func (requester *pluginDecisionRequester) Await(_ context.Context, _ string, approvalID string) (approval.Resolution, error) {
	if requester.mutate != nil {
		requester.mutate()
	}
	outcome := requester.outcome
	if outcome == "" {
		outcome = approval.OutcomeAllowOnce
	}
	return approval.Resolution{ApprovalID: approvalID, Outcome: outcome, Reason: approval.ReasonUser}, nil
}

func (requester *pluginDecisionRequester) snapshot() []approval.Draft {
	requester.mu.Lock()
	defer requester.mu.Unlock()
	return append([]approval.Draft(nil), requester.drafts...)
}

func securePluginPolicy(workspace string) sandbox.Policy {
	return sandbox.Policy{
		Enforcement:     sandbox.EnforcementRequired,
		Required:        securePluginCapabilities(),
		Workspace:       workspace,
		WorkspaceAccess: sandbox.WorkspaceReadWrite,
		Network:         sandbox.NetworkDenied,
	}
}

func TestPluginDispatchAlwaysRequiresPerCallApproval(t *testing.T) {
	tests := []struct {
		name          string
		requester     *pluginDecisionRequester
		wantExecuted  bool
		wantSubstring string
	}{
		{
			name:          "nil broker",
			wantSubstring: "no approval requester",
		},
		{
			name:          "deny",
			requester:     &pluginDecisionRequester{outcome: approval.OutcomeDeny},
			wantSubstring: "denied or became unavailable",
		},
		{
			name:         "allow once",
			requester:    &pluginDecisionRequester{outcome: approval.OutcomeAllowOnce},
			wantExecuted: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var approvalRequester approval.Requester
			if test.requester != nil {
				approvalRequester = test.requester
			}
			manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_dispatch"))
			runner := &recordingPluginRunner{
				caps:   securePluginCapabilities(),
				result: sandbox.Result{Started: true, ExitCode: 0, Stdout: []byte("approved")},
			}
			definitions, err := manager.ExecutableToolDefs(PluginExecutionOptions{Runner: runner, Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			executorCalls := 0
			results := dispatchToolCalls(
				[]toolUseBlock{dispatchTestCall("plugin-call", "plugin_dispatch", map[string]any{"value": "sensitive-value"})},
				toolDispatchOptions{
					Context:           context.Background(),
					WorkDir:           workspace,
					AllowedTools:      toolCatalogNames(definitions),
					PermissionConfig:  PermissionConfig{AutoApprove: "all"},
					SnapshotDecision:  func(toolUseBlock) string { return "allow" },
					ApprovalRequester: approvalRequester,
					SessionID:         "plugin-session",
					RunID:             "plugin-run",
					Execute: func(call toolUseBlock) (string, bool) {
						executorCalls++
						return ExecuteToolWithOptions(call.Name, call.Input, workspace, ToolExecutionOptions{
							Context:           context.Background(),
							ExpectedSessionID: "plugin-session",
							ExpectedRunID:     "plugin-run",
						})
					},
				},
			)
			if len(results) != 1 {
				t.Fatalf("results = %#v", results)
			}
			if test.wantExecuted {
				if results[0].IsError || !results[0].Executed || executorCalls != 1 || len(runner.calls) != 1 {
					t.Fatalf("allowed result=%#v executor=%d runner=%d", results[0], executorCalls, len(runner.calls))
				}
				drafts := test.requester.snapshot()
				if len(drafts) != 1 || drafts[0].RememberAllowed || drafts[0].ToolName != "plugin_dispatch" ||
					strings.Contains(drafts[0].RedactedInput, "sensitive-value") {
					t.Fatalf("approval draft = %#v", drafts)
				}
				return
			}
			if !results[0].IsError || !results[0].Synthetic || !strings.Contains(results[0].Content, test.wantSubstring) {
				t.Fatalf("blocked result = %#v, want %q", results[0], test.wantSubstring)
			}
			if executorCalls != 0 || len(runner.calls) != 0 {
				t.Fatalf("blocked call reached execution: executor=%d runner=%d", executorCalls, len(runner.calls))
			}
		})
	}
}

func TestPluginDispatchRejectsTamperAfterApprovalBeforeExecutor(t *testing.T) {
	manager, workspace, executable := executablePluginFixture(t, pluginFixtureTool("plugin_tamper"))
	runner := &recordingPluginRunner{
		caps:   securePluginCapabilities(),
		result: sandbox.Result{Started: true, ExitCode: 0},
	}
	definitions, err := manager.ExecutableToolDefs(PluginExecutionOptions{Runner: runner, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	requester := &pluginDecisionRequester{
		outcome: approval.OutcomeAllowOnce,
		mutate: func() {
			if err := os.WriteFile(executable, []byte("tampered after approval"), 0o755); err != nil {
				t.Errorf("tamper executable: %v", err)
			}
		},
	}
	executorCalls := 0
	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("plugin-tamper", "plugin_tamper", map[string]any{"value": "x"})},
		toolDispatchOptions{
			Context:           context.Background(),
			WorkDir:           workspace,
			AllowedTools:      toolCatalogNames(definitions),
			PermissionConfig:  PermissionConfig{AutoApprove: "all"},
			ApprovalRequester: requester,
			SessionID:         "tamper-session",
			RunID:             "tamper-run",
			Execute: func(toolUseBlock) (string, bool) {
				executorCalls++
				return "unexpected", false
			},
		},
	)
	if len(results) != 1 || !results[0].IsError || !results[0].Synthetic ||
		!strings.Contains(results[0].Content, "changed after catalog binding") {
		t.Fatalf("tamper result = %#v", results)
	}
	if executorCalls != 0 || len(runner.calls) != 0 || len(requester.snapshot()) != 1 {
		t.Fatalf("tamper boundaries executor=%d runner=%d approvals=%d", executorCalls, len(runner.calls), len(requester.snapshot()))
	}
}

func TestPluginIdentityBoundExecutionWithoutApprovalProofFailsClosed(t *testing.T) {
	manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_direct"))
	runner := &recordingPluginRunner{
		caps: securePluginCapabilities(), result: sandbox.Result{Started: true, ExitCode: 0},
	}
	definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, nil)
	bound, err := bindToolExecutionInput(json.RawMessage(`{"value":"x"}`), identity)
	if err != nil {
		t.Fatal(err)
	}
	result, isError := ExecuteToolWithOptions(definition.Name, bound, workspace, ToolExecutionOptions{
		Context: context.Background(), ExpectedSessionID: "direct-session", ExpectedRunID: "direct-run",
	})
	if !isError || !strings.Contains(result, "per-call approval") || len(runner.calls) != 0 {
		t.Fatalf("direct execution result=%q error=%v calls=%d", result, isError, len(runner.calls))
	}
}

func TestPluginApprovalProofIsOneShot(t *testing.T) {
	manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_one_shot"))
	runner := &recordingPluginRunner{
		caps: securePluginCapabilities(), result: sandbox.Result{Started: true, ExitCode: 0},
	}
	definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, nil)
	approved := approvedPluginExecutionInput(
		t,
		json.RawMessage(`{"value":"x"}`),
		identity,
		"one-shot-session",
		"one-shot-run",
	)
	options := ToolExecutionOptions{
		Context: context.Background(), ExpectedSessionID: "one-shot-session", ExpectedRunID: "one-shot-run",
	}
	if result, isError := ExecuteToolWithOptions(definition.Name, approved, workspace, options); isError {
		t.Fatalf("first execution failed: %s", result)
	}
	result, isError := ExecuteToolWithOptions(definition.Name, approved, workspace, options)
	if !isError || !strings.Contains(result, "already consumed") || len(runner.calls) != 1 {
		t.Fatalf("replay result=%q error=%v calls=%d", result, isError, len(runner.calls))
	}
}

type capturingPluginLoopProvider struct {
	*scriptedLoopProvider
	mu       sync.Mutex
	requests [][]types.ToolDef
}

func (provider *capturingPluginLoopProvider) StreamMessage(
	ctx context.Context,
	request *types.MessagesRequest,
	options *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	tools := append([]types.ToolDef(nil), request.Tools...)
	provider.mu.Lock()
	provider.requests = append(provider.requests, tools)
	provider.mu.Unlock()
	return provider.scriptedLoopProvider.StreamMessage(ctx, request, options)
}

func (provider *capturingPluginLoopProvider) firstTools() []types.ToolDef {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) == 0 {
		return nil
	}
	return append([]types.ToolDef(nil), provider.requests[0]...)
}

func TestRunLoopDiscoversPluginBeforeCatalogAndExecutesAfterApproval(t *testing.T) {
	isolateEvidenceLoopTest(t)
	t.Setenv("CORELAY_MAX_TOOLS", "")
	manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_loop_run"))
	pluginRoot := manager.plugins[0].rootDir
	discoveryDir := strings.TrimSuffix(pluginRoot, string(os.PathSeparator)+"fixture")
	runner := &recordingPluginRunner{
		caps: securePluginCapabilities(), result: sandbox.Result{Started: true, ExitCode: 0, Stdout: []byte("loop-ok")},
	}
	provider := &capturingPluginLoopProvider{scriptedLoopProvider: &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("plugin-loop-call", "plugin_loop_run", map[string]string{"value": "loop"}),
		textStep("done"),
	}}}
	requester := &pluginDecisionRequester{outcome: approval.OutcomeAllowOnce}
	userContent, _ := json.Marshal("Run plugin_loop_run once.")
	eventCh := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{
		Role: "user", Content: userContent,
	}}, workspace, RunOptions{
		SessionID:           "plugin-loop-session",
		ApprovalRequester:   requester,
		SandboxRunner:       runner,
		SandboxPolicy:       securePluginPolicy(workspace),
		PluginDirs:          []string{discoveryDir},
		DisableWorkspaceMCP: true,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, eventCh)
	var sawResult bool
	for event := range eventCh {
		if event.Type == "tool_result" && strings.Contains(toEventString(event.Data), "loop-ok") {
			sawResult = true
		}
	}
	tools := provider.firstTools()
	foundPlugin := false
	foundLegacy := false
	for _, tool := range tools {
		if tool.Name == "plugin_loop_run" {
			foundPlugin = true
		}
		if tool.Name == "legacy-only" {
			foundLegacy = true
		}
	}
	if !foundPlugin || foundLegacy {
		t.Fatalf("first request plugin catalog found=%v legacy=%v", foundPlugin, foundLegacy)
	}
	if !sawResult || len(runner.calls) != 1 || len(requester.snapshot()) != 1 {
		t.Fatalf("loop result=%v runner=%d approvals=%d", sawResult, len(runner.calls), len(requester.snapshot()))
	}
}

func TestRunLoopExplicitPluginSandboxUnavailableFailsBeforeModel(t *testing.T) {
	isolateEvidenceLoopTest(t)
	manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_unavailable"))
	discoveryDir := strings.TrimSuffix(manager.plugins[0].rootDir, string(os.PathSeparator)+"fixture")
	runner := &recordingPluginRunner{caps: sandbox.Capabilities{
		FilesystemIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
	}}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{textStep("must not run")}}
	eventCh := make(chan Event, 32)
	userContent, _ := json.Marshal("hello")
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workspace, RunOptions{
		SandboxRunner: runner,
		SandboxPolicy: sandbox.Policy{
			Enforcement:     sandbox.EnforcementRequired,
			Required:        runner.caps,
			Workspace:       workspace,
			WorkspaceAccess: sandbox.WorkspaceReadWrite,
		},
		PluginDirs:          []string{discoveryDir},
		DisableWorkspaceMCP: true,
	}, eventCh)
	var errorText string
	for event := range eventCh {
		if event.Type == "error" {
			errorText = toEventString(event.Data)
		}
	}
	if provider.callCount() != 0 || !strings.Contains(errorText, "plugin configuration failed") || len(runner.calls) != 0 {
		t.Fatalf("provider=%d error=%q runner=%d", provider.callCount(), errorText, len(runner.calls))
	}
}
