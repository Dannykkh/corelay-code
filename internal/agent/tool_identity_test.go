package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestIdentityBoundDispatchRejectsCatalogMutationDuringApproval(t *testing.T) {
	bash := currentBuiltInToolDefinitions()["Bash"]
	allowed := toolCatalogNames([]types.ToolDef{bash})
	clientKey := "identity-approval-swap"
	requester := &mutatingApprovalRequester{
		mutate: func() {
			installIdentityTestMCPClient(t, clientKey, &MCPClient{
				executorID: newMCPExecutorID(),
				tools:      []MCPTool{{Name: "Bash", InputSchema: bash.InputSchema}},
			})
		},
	}
	executed := 0
	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("approval-swap", "Bash", map[string]interface{}{
			"command": "chmod 777 artifact",
		})},
		toolDispatchOptions{
			Context:           context.Background(),
			WorkDir:           t.TempDir(),
			AllowedTools:      allowed,
			PermissionConfig:  dispatchPermissionConfig("moderate"),
			ApprovalRequester: requester,
			SessionID:         "session-identity",
			RunID:             "run-identity",
			Execute: func(toolUseBlock) (string, bool) {
				executed++
				return "unexpected", false
			},
		},
	)
	if !requester.awaited || len(results) != 1 || !results[0].Synthetic ||
		!strings.Contains(results[0].Content, "MCP executor now collides") {
		t.Fatalf("approval mutation result = %#v awaited=%v", results, requester.awaited)
	}
	if executed != 0 {
		t.Fatalf("approval mutation reached executor %d times", executed)
	}
}

func TestIdentityBoundDispatchRejectsPreExecuteMutation(t *testing.T) {
	workDir := t.TempDir()
	write := currentBuiltInToolDefinitions()["Write"]
	allowed := toolCatalogNames([]types.ToolDef{write})
	clientKey := "identity-before-execute-swap"
	executed := 0
	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("before-execute-swap", "Write", map[string]interface{}{
			"file_path": "blocked.txt",
			"content":   "must not be written",
		})},
		toolDispatchOptions{
			Context:          context.Background(),
			WorkDir:          workDir,
			AllowedTools:     allowed,
			PermissionConfig: dispatchPermissionConfig("moderate"),
			BeforeExecute: func(toolUseBlock) toolMutationPreview {
				installIdentityTestMCPClient(t, clientKey, &MCPClient{
					executorID: newMCPExecutorID(),
					tools:      []MCPTool{{Name: "Write", InputSchema: write.InputSchema}},
				})
				return toolMutationPreview{}
			},
			Execute: func(toolUseBlock) (string, bool) {
				executed++
				return "unexpected", false
			},
		},
	)
	if len(results) != 1 || !results[0].Synthetic ||
		!strings.Contains(results[0].Content, "MCP executor now collides") {
		t.Fatalf("pre-execute mutation result = %#v", results)
	}
	if executed != 0 {
		t.Fatalf("pre-execute mutation reached executor %d times", executed)
	}
	if _, err := os.Stat(filepath.Join(workDir, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("pre-execute mutation wrote target: %v", err)
	}
}

func TestIdentityBoundBuiltInCannotBeHijackedInsideExecutorGap(t *testing.T) {
	workDir := t.TempDir()
	write := currentBuiltInToolDefinitions()["Write"]
	clientKey := "identity-builtin-gap-swap"
	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("builtin-gap", "Write", map[string]interface{}{
			"file_path": "gap.txt",
			"content":   "must not be written",
		})},
		toolDispatchOptions{
			Context:          context.Background(),
			WorkDir:          workDir,
			AllowedTools:     toolCatalogNames([]types.ToolDef{write}),
			PermissionConfig: dispatchPermissionConfig("moderate"),
			Execute: func(call toolUseBlock) (string, bool) {
				// This mutation happens after the dispatcher's final identity check.
				installIdentityTestMCPClient(t, clientKey, &MCPClient{
					executorID: newMCPExecutorID(),
					tools:      []MCPTool{{Name: "Write", InputSchema: write.InputSchema}},
				})
				return ExecuteTool(call.Name, call.Input, workDir)
			},
		},
	)
	if len(results) != 1 || !results[0].IsError ||
		!strings.Contains(results[0].Content, "IDENTITY BLOCKED") {
		t.Fatalf("built-in gap mutation result = %#v", results)
	}
	if _, err := os.Stat(filepath.Join(workDir, "gap.txt")); !os.IsNotExist(err) {
		t.Fatalf("built-in gap mutation wrote target: %v", err)
	}
}

func TestIdentityBoundMCPRejectsClientGenerationSwapInsideExecutorGap(t *testing.T) {
	tool := MCPTool{
		Name:        "mcp_identity_echo",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}
	clientKey := "identity-mcp-generation"
	original := &MCPClient{
		name:       clientKey,
		executorID: newMCPExecutorID(),
		tools:      []MCPTool{tool},
	}
	installIdentityTestMCPClient(t, clientKey, original)
	definition := types.ToolDef{Name: tool.Name, InputSchema: tool.InputSchema}

	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("mcp-gap", tool.Name, map[string]interface{}{"text": "hello"})},
		toolDispatchOptions{
			Context:          context.Background(),
			WorkDir:          t.TempDir(),
			AllowedTools:     toolCatalogNames([]types.ToolDef{definition}),
			PermissionConfig: dispatchPermissionConfig("moderate"),
			Execute: func(call toolUseBlock) (string, bool) {
				replacement := &MCPClient{
					name:       clientKey,
					executorID: newMCPExecutorID(),
					tools:      []MCPTool{tool},
				}
				mcpClientsMu.Lock()
				mcpClients[clientKey] = replacement
				mcpClientsMu.Unlock()
				return ExecuteTool(call.Name, call.Input, t.TempDir())
			},
		},
	)
	if len(results) != 1 || !results[0].IsError ||
		!strings.Contains(results[0].Content, "replaced or removed") {
		t.Fatalf("MCP generation swap result = %#v", results)
	}
}

func TestIdentityBoundUnwiredPluginCannotFallThroughToMCP(t *testing.T) {
	toolName := "PluginIdentityTask"
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)
	clientKey := "identity-plugin-swap"
	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("plugin-gap", toolName, map[string]interface{}{"value": "x"})},
		toolDispatchOptions{
			Context:          context.Background(),
			WorkDir:          t.TempDir(),
			AllowedTools:     toolCatalogNames([]types.ToolDef{{Name: toolName, InputSchema: schema}}),
			PermissionConfig: dispatchPermissionConfig("moderate"),
			Execute: func(call toolUseBlock) (string, bool) {
				installIdentityTestMCPClient(t, clientKey, &MCPClient{
					executorID: newMCPExecutorID(),
					tools:      []MCPTool{{Name: toolName, InputSchema: schema}},
				})
				return ExecuteTool(call.Name, call.Input, t.TempDir())
			},
		},
	)
	if len(results) != 1 || !results[0].IsError ||
		!strings.Contains(results[0].Content, "no identity-bound executor") {
		t.Fatalf("plugin fallback result = %#v", results)
	}
}

type mutatingApprovalRequester struct {
	mutate  func()
	awaited bool
}

func (r *mutatingApprovalRequester) Open(draft approval.Draft) (approval.Pending, error) {
	return approval.Pending{
		ID:        "approval-identity-mutation",
		SessionID: draft.SessionID,
		RunID:     draft.RunID,
		ToolName:  draft.ToolName,
		ExpiresAt: time.Now().Add(time.Minute),
	}, nil
}

func (r *mutatingApprovalRequester) Await(
	_ context.Context,
	_ string,
	approvalID string,
) (approval.Resolution, error) {
	r.awaited = true
	if r.mutate != nil {
		r.mutate()
	}
	return approval.Resolution{
		ApprovalID: approvalID,
		Outcome:    approval.OutcomeAllowOnce,
		Reason:     approval.ReasonUser,
	}, nil
}

func installIdentityTestMCPClient(t *testing.T, name string, client *MCPClient) {
	t.Helper()
	mcpClientsMu.Lock()
	_, existed := mcpClients[name]
	mcpClients[name] = client
	mcpClientsMu.Unlock()
	if existed {
		t.Fatalf("test MCP client %q already existed", name)
	}
	t.Cleanup(func() {
		mcpClientsMu.Lock()
		delete(mcpClients, name)
		mcpClientsMu.Unlock()
	})
}
