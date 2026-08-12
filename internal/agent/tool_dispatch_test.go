package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestDispatchToolCallsDeniesBeforeParallelAndPreservesSlots(t *testing.T) {
	workDir := t.TempDir()
	writeDispatchFixture(t, filepath.Join(workDir, "a.txt"), "a")
	writeDispatchFixture(t, filepath.Join(workDir, "c.txt"), "c")

	calls := []toolUseBlock{
		dispatchTestCall("call-a", "Read", map[string]interface{}{"file_path": "a.txt"}),
		dispatchTestCall("call-deny", "Grep", map[string]interface{}{"pattern": "secret"}),
		dispatchTestCall("call-c", "Read", map[string]interface{}{"file_path": "c.txt"}),
	}

	var mu sync.Mutex
	preCalls := make(map[string]int)
	postCalls := make(map[string]int)
	executed := make(map[string]int)
	results := dispatchToolCalls(calls, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     dispatchAllowedTools("Read", "Grep"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       NewReadLedger(workDir),
		RunGuard:         NewRunGuard(10),
		SnapshotDecision: func(call toolUseBlock) string {
			if call.ID == "call-deny" {
				return "deny"
			}
			return "allow"
		},
		PreHook: func(call toolUseBlock) (bool, string) {
			preCalls[call.ID]++
			return false, ""
		},
		PostHook: func(call toolUseBlock, _ string, _ bool) {
			mu.Lock()
			postCalls[call.ID]++
			mu.Unlock()
		},
		Execute: func(call toolUseBlock) (string, bool) {
			mu.Lock()
			executed[call.ID]++
			mu.Unlock()
			return call.ID, false
		},
	})

	if len(results) != len(calls) {
		t.Fatalf("result count = %d, want %d", len(results), len(calls))
	}
	for index, call := range calls {
		if results[index].Tool.ID != call.ID {
			t.Fatalf("result slot %d ID = %q, want %q", index, results[index].Tool.ID, call.ID)
		}
	}
	if preCalls["call-a"] != 1 || preCalls["call-c"] != 1 {
		t.Fatalf("allowed pre-hook counts = %#v, want one each", preCalls)
	}
	if preCalls["call-deny"] != 0 {
		t.Fatalf("denied call ran pre-hook %d times", preCalls["call-deny"])
	}
	if !results[1].Synthetic || !results[1].IsError || results[1].Executed {
		t.Fatalf("denied result = %#v", results[1])
	}
	if executed["call-deny"] != 0 || postCalls["call-deny"] != 0 {
		t.Fatalf(
			"denied call executed=%d post-hooks=%d",
			executed["call-deny"],
			postCalls["call-deny"],
		)
	}
	for _, id := range []string{"call-a", "call-c"} {
		if executed[id] != 1 || postCalls[id] != 1 {
			t.Fatalf("%s executed=%d post-hooks=%d", id, executed[id], postCalls[id])
		}
	}
}

func TestDispatchToolCallsRunsConcurrentReadBeforeSerialEdit(t *testing.T) {
	workDir := t.TempDir()
	filePath := filepath.Join(workDir, "state.txt")
	writeDispatchFixture(t, filePath, "before")

	calls := []toolUseBlock{
		dispatchTestCall("call-edit", "Edit", map[string]interface{}{
			"file_path":  "state.txt",
			"old_string": "before",
			"new_string": "after",
		}),
		dispatchTestCall("call-read", "Read", map[string]interface{}{
			"file_path": "state.txt",
		}),
	}

	var mu sync.Mutex
	var executionOrder []string
	results := dispatchToolCalls(calls, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     dispatchAllowedTools("Read", "Edit"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       NewReadLedger(workDir),
		RunGuard:         NewRunGuard(10),
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(call toolUseBlock) (string, bool) {
			mu.Lock()
			executionOrder = append(executionOrder, call.Name)
			mu.Unlock()
			if call.Name == "Edit" {
				if err := os.WriteFile(filePath, []byte("after"), 0o600); err != nil {
					return err.Error(), true
				}
			}
			return "ok", false
		},
	})

	if got, want := results[0].Tool.ID, "call-edit"; got != want {
		t.Fatalf("first result ID = %q, want %q", got, want)
	}
	if got, want := results[1].Tool.ID, "call-read"; got != want {
		t.Fatalf("second result ID = %q, want %q", got, want)
	}
	if results[0].IsError || results[1].IsError {
		t.Fatalf("dispatch results = %#v", results)
	}
	if got := strings.Join(executionOrder, ","); got != "Read,Edit" {
		t.Fatalf("execution order = %q, want %q", got, "Read,Edit")
	}
	content, err := os.ReadFile(filePath)
	if err != nil || string(content) != "after" {
		t.Fatalf("edited content = %q, %v", content, err)
	}
}

func TestDispatchToolCallsBlocksEditWithoutReadEvidence(t *testing.T) {
	workDir := t.TempDir()
	writeDispatchFixture(t, filepath.Join(workDir, "existing.txt"), "before")
	call := dispatchTestCall("call-edit", "Edit", map[string]interface{}{
		"file_path":  "existing.txt",
		"old_string": "before",
		"new_string": "after",
	})

	executed := 0
	postHooks := 0
	beforeExecute := 0
	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     dispatchAllowedTools("Edit"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       NewReadLedger(workDir),
		RunGuard:         NewRunGuard(10),
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		BeforeExecute: func(toolUseBlock) toolMutationPreview {
			beforeExecute++
			return toolMutationPreview{}
		},
		PostHook: func(toolUseBlock, string, bool) {
			postHooks++
		},
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
	})

	if len(results) != 1 || !results[0].Synthetic || !results[0].IsError {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(results[0].Content, "read the existing file") {
		t.Fatalf("denial content = %q", results[0].Content)
	}
	if executed != 0 || postHooks != 0 || beforeExecute != 0 {
		t.Fatalf(
			"denied edit executed=%d post-hooks=%d before-execute=%d",
			executed,
			postHooks,
			beforeExecute,
		)
	}
}

func TestDispatchToolCallsApprovalRoundTripAndNilFailClosed(t *testing.T) {
	call := dispatchTestCall("call-danger", "Bash", map[string]interface{}{
		"command": "chmod 777 artifact",
	})
	base := toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          t.TempDir(),
		AllowedTools:     dispatchAllowedTools("Bash"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		SessionID:        "session-test",
		RunID:            "run-test",
		ReadLedger:       NewReadLedger(t.TempDir()),
		RunGuard:         NewRunGuard(10),
	}

	executed := 0
	base.Execute = func(toolUseBlock) (string, bool) {
		executed++
		return "ok", false
	}
	denied := dispatchToolCalls([]toolUseBlock{call}, base)
	if len(denied) != 1 || !denied[0].Synthetic || executed != 0 {
		t.Fatalf("nil-requester result = %#v executed=%d", denied, executed)
	}
	if !strings.Contains(denied[0].Content, "no approval requester") {
		t.Fatalf("nil-requester content = %q", denied[0].Content)
	}

	requester := &allowingApprovalRequester{}
	var events []Event
	base.ApprovalRequester = requester
	base.Emit = func(event Event) {
		events = append(events, event)
	}
	allowed := dispatchToolCalls([]toolUseBlock{call}, base)
	if len(allowed) != 1 || allowed[0].IsError || !allowed[0].Executed || executed != 1 {
		t.Fatalf("approved result = %#v executed=%d", allowed, executed)
	}
	if requester.opened.ToolName != "Bash" ||
		!strings.HasPrefix(requester.opened.InputDigest, "sha256:") {
		t.Fatalf("approval draft = %#v", requester.opened)
	}
	if strings.Contains(requester.opened.RedactedInput, "chmod") {
		t.Fatalf("approval input was not redacted: %q", requester.opened.RedactedInput)
	}
	if len(events) != 3 || events[0].Type != "tool_input" || events[1].Type != "approval_required" ||
		events[2].Type != "tool_execution_start" {
		t.Fatalf("approval events = %#v", events)
	}
	execution, ok := events[2].Data.(map[string]string)
	if !ok || execution["id"] != call.ID || execution["name"] != call.Name ||
		execution["inputDigest"] != toolInputDigest(call) || execution["runId"] != "run-test" {
		t.Fatalf("execution lifecycle event = %#v", events[2])
	}
}

func TestDispatchGitMutationIgnoresConfirmAndFailsClosedWithoutBroker(t *testing.T) {
	workDir := t.TempDir()
	mutation := dispatchTestCall("git-branch-create", "Git", map[string]interface{}{
		"command": "branch",
		"args":    "feature/new",
		"confirm": true,
	})
	executed := 0
	base := toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     dispatchAllowedTools("Git"),
		PermissionConfig: dispatchPermissionConfig("safe"),
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
	}

	denied := dispatchToolCalls([]toolUseBlock{mutation}, base)
	if len(denied) != 1 || !denied[0].Synthetic || !denied[0].IsError || denied[0].Executed {
		t.Fatalf("denied result=%#v", denied)
	}
	if !strings.Contains(denied[0].Content, "no approval requester") || executed != 0 {
		t.Fatalf("denied content=%q executed=%d", denied[0].Content, executed)
	}

	readOnly := dispatchTestCall("git-branch-list", "Git", map[string]interface{}{
		"command": "branch",
		"args":    "--list feature/*",
	})
	allowed := dispatchToolCalls([]toolUseBlock{readOnly}, base)
	if len(allowed) != 1 || allowed[0].IsError || !allowed[0].Executed || executed != 1 {
		t.Fatalf("read-only result=%#v executed=%d", allowed, executed)
	}
}

func TestDispatchToolCallsUsesRunGuardAcrossBatches(t *testing.T) {
	workDir := t.TempDir()
	writeDispatchFixture(t, filepath.Join(workDir, "repeat.txt"), "same")
	call := dispatchTestCall("call-repeat", "Read", map[string]interface{}{"file_path": "repeat.txt"})
	executed := 0
	options := toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     dispatchAllowedTools("Read"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       NewReadLedger(workDir),
		RunGuard:         NewRunGuard(2),
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "same", false
		},
	}

	// The first successful result is progress and starts a fresh budget. The
	// next two identical results are not progress; the following call blocks.
	for attempt := 0; attempt < 3; attempt++ {
		result := dispatchToolCalls([]toolUseBlock{call}, options)
		if len(result) != 1 || result[0].IsError {
			t.Fatalf("attempt %d result = %#v", attempt+1, result)
		}
	}
	blocked := dispatchToolCalls([]toolUseBlock{call}, options)
	if len(blocked) != 1 || !blocked[0].Synthetic ||
		!strings.Contains(blocked[0].Content, "Repeated action") {
		t.Fatalf("blocked result = %#v", blocked)
	}
	if executed != 3 {
		t.Fatalf("executor calls = %d, want 3", executed)
	}
}

func TestDispatchToolCallsExplicitApprovalResetsRunGuard(t *testing.T) {
	call := dispatchTestCall("call-approved-repeat", "Bash", map[string]interface{}{
		"command": "chmod 777 artifact",
	})
	guard := NewRunGuard(1)
	options := toolDispatchOptions{
		Context:           context.Background(),
		WorkDir:           t.TempDir(),
		AllowedTools:      dispatchAllowedTools("Bash"),
		PermissionConfig:  dispatchPermissionConfig("moderate"),
		ApprovalRequester: &allowingApprovalRequester{},
		SessionID:         "session-approved-repeat",
		RunID:             "run-approved-repeat",
		RunGuard:          guard,
		Execute: func(toolUseBlock) (string, bool) {
			return "same failure", true
		},
	}
	for attempt := 0; attempt < 3; attempt++ {
		result := dispatchToolCalls([]toolUseBlock{call}, options)
		if len(result) != 1 || !result[0].Executed || !result[0].IsError || result[0].Synthetic {
			t.Fatalf("approved attempt %d result = %#v", attempt+1, result)
		}
	}
}

func TestDispatchToolCallsValidatesSchemaBeforeHooksApprovalAndExecution(t *testing.T) {
	definitions := []types.ToolDef{currentBuiltInToolDefinitions()["Bash"]}
	call := dispatchTestCall("schema-invalid", "Bash", map[string]interface{}{
		"command": 42,
	})
	requester := &allowingApprovalRequester{}
	preHooks := 0
	postHooks := 0
	executed := 0
	var events []Event

	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:           context.Background(),
		WorkDir:           t.TempDir(),
		AllowedTools:      toolCatalogNames(definitions),
		PermissionConfig:  dispatchPermissionConfig("moderate"),
		ApprovalRequester: requester,
		SessionID:         "session-schema",
		RunID:             "run-schema",
		PreHook: func(toolUseBlock) (bool, string) {
			preHooks++
			return false, ""
		},
		PostHook: func(toolUseBlock, string, bool) {
			postHooks++
		},
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
		Emit: func(event Event) {
			events = append(events, event)
		},
	})

	if len(results) != 1 || results[0].Tool.ID != call.ID ||
		!results[0].Synthetic || !results[0].IsError || results[0].Executed {
		t.Fatalf("schema result = %#v", results)
	}
	if !strings.Contains(results[0].Content, "$.command: expected string") {
		t.Fatalf("schema result content = %q", results[0].Content)
	}
	if preHooks != 0 || postHooks != 0 || executed != 0 || len(events) != 0 {
		t.Fatalf(
			"schema-invalid call reached side effects: pre=%d post=%d execute=%d events=%d",
			preHooks,
			postHooks,
			executed,
			len(events),
		)
	}
	if requester.opened.ToolName != "" {
		t.Fatalf("schema-invalid call opened approval: %#v", requester.opened)
	}
}

func TestDispatchToolCallsValidatesRecoveredDynamicCatalogCalls(t *testing.T) {
	definition := types.ToolDef{
		Name: "PluginDeploy",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"target":{
					"type":"object",
					"properties":{"region":{"type":"string","enum":["local","ci"]}},
					"required":["region"],
					"additionalProperties":false
				}
			},
			"required":["target"],
			"additionalProperties":false
		}`),
	}
	recovered, _ := recoverToolCallsForCatalog(
		`{"name":"PluginDeploy","arguments":{"target":{"region":2}}}`,
		[]types.ToolDef{definition},
	)
	if len(recovered) != 1 {
		t.Fatalf("recovered calls = %#v", recovered)
	}
	executed := 0
	options := toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          t.TempDir(),
		AllowedTools:     toolCatalogNames([]types.ToolDef{definition}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "ok", false
		},
	}
	invalid := dispatchToolCalls(recovered, options)
	if len(invalid) != 1 || !invalid[0].Synthetic ||
		!strings.Contains(invalid[0].Content, "$.target.region: value is not in the allowed enum") {
		t.Fatalf("invalid recovered result = %#v", invalid)
	}
	if executed != 0 {
		t.Fatalf("invalid recovered call executed %d times", executed)
	}

	validCall := dispatchTestCall("plugin-valid", "PluginDeploy", map[string]interface{}{
		"target": map[string]interface{}{"region": "ci"},
	})
	valid := dispatchToolCalls([]toolUseBlock{validCall}, options)
	if len(valid) != 1 || valid[0].IsError || !valid[0].Executed || executed != 1 {
		t.Fatalf("valid dynamic result = %#v executed=%d", valid, executed)
	}
}

func TestDispatchToolCallsRejectsLateMCPReservedNameCollision(t *testing.T) {
	bash := currentBuiltInToolDefinitions()["Bash"]
	allowed := toolCatalogNames([]types.ToolDef{bash})

	clientName := "dispatcher-reserved-collision-test"
	mcpClientsMu.Lock()
	if _, exists := mcpClients[clientName]; exists {
		mcpClientsMu.Unlock()
		t.Fatalf("test MCP client %q already exists", clientName)
	}
	mcpClients[clientName] = &MCPClient{tools: []MCPTool{{
		Name:        "Bash",
		InputSchema: bash.InputSchema,
	}}}
	mcpClientsMu.Unlock()
	t.Cleanup(func() {
		mcpClientsMu.Lock()
		delete(mcpClients, clientName)
		mcpClientsMu.Unlock()
	})

	executed := 0
	results := dispatchToolCalls(
		[]toolUseBlock{dispatchTestCall("late-collision", "Bash", map[string]interface{}{
			"command": "echo safe",
		})},
		toolDispatchOptions{
			Context:          context.Background(),
			WorkDir:          t.TempDir(),
			AllowedTools:     allowed,
			PermissionConfig: dispatchPermissionConfig("moderate"),
			Execute: func(toolUseBlock) (string, bool) {
				executed++
				return "unexpected", false
			},
		},
	)
	if len(results) != 1 || !results[0].Synthetic ||
		!strings.Contains(results[0].Content, "MCP executor now collides") {
		t.Fatalf("late collision result = %#v", results)
	}
	if executed != 0 {
		t.Fatalf("late collision reached executor %d times", executed)
	}
}

func TestDispatchToolCallsRechecksCancellationAfterApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requester := &cancelOnApprovalRequester{cancel: cancel}
	call := dispatchTestCall("cancel-after-approval", "Bash", map[string]interface{}{
		"command": "chmod 777 artifact",
	})
	executed := 0
	beforeExecute := 0
	postHooks := 0
	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:           ctx,
		WorkDir:           t.TempDir(),
		AllowedTools:      dispatchAllowedTools("Bash"),
		PermissionConfig:  dispatchPermissionConfig("moderate"),
		ApprovalRequester: requester,
		SessionID:         "session-cancel",
		RunID:             "run-cancel",
		BeforeExecute: func(toolUseBlock) toolMutationPreview {
			beforeExecute++
			return toolMutationPreview{}
		},
		PostHook: func(toolUseBlock, string, bool) {
			postHooks++
		},
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
	})
	if len(results) != 1 || !results[0].Synthetic ||
		!strings.Contains(results[0].Content, "context was canceled") {
		t.Fatalf("cancel-after-approval result = %#v", results)
	}
	if !requester.awaited || executed != 0 || beforeExecute != 0 || postHooks != 0 {
		t.Fatalf(
			"cancel race escaped: awaited=%v execute=%d before=%d post=%d",
			requester.awaited,
			executed,
			beforeExecute,
			postHooks,
		)
	}
}

func TestRecoverToolCallsForCatalogAppliesAliasesAndAllowlist(t *testing.T) {
	fence := strings.Repeat(string(rune(96)), 3)
	text := fence + "json\n{\"tool\":\"read_file\",\"args\":{\"file_path\":\"a.txt\"}}\n" + fence
	calls, cleaned := recoverToolCallsForCatalog(text, []types.ToolDef{{Name: "Read"}})
	if cleaned != "" || len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("allowed recovery calls=%#v cleaned=%q", calls, cleaned)
	}

	calls, cleaned = recoverToolCallsForCatalog(text, []types.ToolDef{{Name: "Write"}})
	if len(calls) != 0 || cleaned != text {
		t.Fatalf("disallowed recovery calls=%#v cleaned=%q", calls, cleaned)
	}
}

func TestDispatchToolCallsRejectsScopeBeforeApprovalOrHooks(t *testing.T) {
	requester := &allowingApprovalRequester{}
	preHooks := 0
	executed := 0
	call := dispatchTestCall("scope-denied", "Bash", map[string]interface{}{"command": "echo denied"})

	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:           context.Background(),
		WorkDir:           t.TempDir(),
		AllowedTools:      dispatchAllowedTools("Bash"),
		PermissionConfig:  dispatchPermissionConfig("moderate"),
		ApprovalRequester: requester,
		SessionID:         "session-scope",
		RunID:             "run-scope",
		ScopeCheck: func(toolUseBlock) (bool, string) {
			return false, "outside worker ownership"
		},
		PreHook: func(toolUseBlock) (bool, string) {
			preHooks++
			return false, ""
		},
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
	})

	if len(results) != 1 || !results[0].Synthetic || !results[0].IsError || results[0].Executed {
		t.Fatalf("scope result = %#v", results)
	}
	if requester.opened.ToolName != "" || preHooks != 0 || executed != 0 {
		t.Fatalf("scope denial opened=%q preHooks=%d executed=%d", requester.opened.ToolName, preHooks, executed)
	}
}

type allowingApprovalRequester struct {
	opened approval.Draft
}

type cancelOnApprovalRequester struct {
	cancel  context.CancelFunc
	awaited bool
}

func (r *cancelOnApprovalRequester) Open(draft approval.Draft) (approval.Pending, error) {
	return approval.Pending{
		ID:              "approval-cancel",
		SessionID:       draft.SessionID,
		SessionRevision: draft.SessionRevision,
		RunID:           draft.RunID,
		ToolCallID:      draft.ToolCallID,
		ToolName:        draft.ToolName,
		RedactedInput:   draft.RedactedInput,
		InputDigest:     draft.InputDigest,
		DangerLevel:     draft.DangerLevel,
		Scope:           draft.Scope,
		ExpiresAt:       time.Now().Add(time.Minute),
	}, nil
}

func (r *cancelOnApprovalRequester) Await(
	_ context.Context,
	_ string,
	approvalID string,
) (approval.Resolution, error) {
	r.awaited = true
	r.cancel()
	return approval.Resolution{
		ApprovalID: approvalID,
		Outcome:    approval.OutcomeAllowOnce,
		Reason:     approval.ReasonUser,
	}, nil
}

func (r *allowingApprovalRequester) Open(draft approval.Draft) (approval.Pending, error) {
	r.opened = draft
	return approval.Pending{
		ID:              "approval-test",
		SessionID:       draft.SessionID,
		SessionRevision: draft.SessionRevision,
		RunID:           draft.RunID,
		ToolCallID:      draft.ToolCallID,
		ToolName:        draft.ToolName,
		RedactedInput:   draft.RedactedInput,
		InputDigest:     draft.InputDigest,
		DangerLevel:     draft.DangerLevel,
		Scope:           draft.Scope,
		ExpiresAt:       time.Now().Add(time.Minute),
	}, nil
}

func (r *allowingApprovalRequester) Await(
	_ context.Context,
	sessionID string,
	approvalID string,
) (approval.Resolution, error) {
	return approval.Resolution{
		ApprovalID: approvalID,
		Outcome:    approval.OutcomeAllowOnce,
		Reason:     approval.ReasonUser,
	}, nil
}

func dispatchTestCall(
	id string,
	name string,
	input map[string]interface{},
) toolUseBlock {
	raw, _ := json.Marshal(input)
	return toolUseBlock{
		ID:       id,
		Name:     name,
		InputRaw: string(raw),
		Input:    raw,
	}
}

func dispatchAllowedTools(names ...string) map[string]struct{} {
	definitions := make([]types.ToolDef, 0, len(names))
	builtIns := currentBuiltInToolDefinitions()
	for _, name := range names {
		if definition, exists := builtIns[name]; exists {
			definitions = append(definitions, definition)
			continue
		}
		definitions = append(definitions, types.ToolDef{
			Name:        name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	return toolCatalogNames(definitions)
}

func dispatchPermissionConfig(autoApprove string) PermissionConfig {
	config := DefaultPermissionConfig()
	config.AutoApprove = autoApprove
	return config
}

func writeDispatchFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
