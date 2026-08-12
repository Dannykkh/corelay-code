package agent

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type toolMutationPreview struct {
	File   string
	Before string
}

type toolDispatchResult struct {
	Tool       toolUseBlock
	Content    string
	Display    string
	IsError    bool
	Executed   bool
	Synthetic  bool
	Concurrent bool
	Mutation   toolMutationPreview
}

type toolDispatchOptions struct {
	Context             context.Context
	WorkDir             string
	AllowedTools        map[string]struct{}
	PlanMode            bool
	PermissionConfig    PermissionConfig
	SnapshotDecision    func(toolUseBlock) string
	ApprovalRequester   approval.Requester
	SessionID           string
	RunID               string
	ReadBeforeWrite     bool
	ReadLedger          *ReadLedger
	RunGuard            *RunGuard
	PlanStep            string
	ScopeCheck          func(toolUseBlock) (bool, string)
	PreHook             func(toolUseBlock) (bool, string)
	PostHook            func(toolUseBlock, string, bool)
	BeforeExecute       func(toolUseBlock) toolMutationPreview
	PreExecutionJournal ToolExecutionJournal
	Execute             func(toolUseBlock) (string, bool)
	Emit                func(Event)
}

type preparedToolCall struct {
	index          int
	tool           toolUseBlock
	concurrent     bool
	hostApproval   *hostInteractionApprovalProof
	pluginApproval *pluginApprovalProof
}

var fallbackRunIDCounter atomic.Uint64

func newActiveRunID() string {
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		return "run_" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf(
		"run_%x_%x",
		time.Now().UnixNano(),
		fallbackRunIDCounter.Add(1),
	)
}

// dispatchToolCalls is the single authorization and scheduling boundary used
// by the main agent loop. Every call is authorized before it is placed into a
// concurrent or serial queue, and every returned slot matches the model's
// original call order.
func dispatchToolCalls(calls []toolUseBlock, opts toolDispatchOptions) []toolDispatchResult {
	results := make([]toolDispatchResult, len(calls))
	concurrent := make([]preparedToolCall, 0, len(calls))
	serial := make([]preparedToolCall, 0, len(calls))

	for index, original := range calls {
		call, err := normalizeDispatchToolCall(original)
		if err != nil {
			results[index] = deniedToolResult(call, "Invalid tool input: "+err.Error())
			continue
		}
		if _, allowed := opts.AllowedTools[call.Name]; !allowed {
			results[index] = deniedToolResult(call, "Tool is not present in the immutable catalog: "+call.Name)
			continue
		}
		schema, err := schemaForAllowedTool(opts.AllowedTools, call.Name)
		if err != nil {
			results[index] = deniedToolResult(call, "Tool input schema unavailable: "+err.Error())
			continue
		}
		if err := validateToolInputSchema(call.Input, schema); err != nil {
			results[index] = deniedToolResult(call, "Tool input schema validation failed: "+err.Error())
			continue
		}
		identity, err := executorIdentityForAllowedTool(opts.AllowedTools, call.Name)
		if err != nil {
			results[index] = deniedToolResult(call, "Tool executor identity is unavailable: "+err.Error())
			continue
		}
		// Validate dynamic executable identity before opening an approval. It is
		// checked again immediately before execution to close the approval gap.
		if err := validateToolExecutorIdentity(opts.AllowedTools, call.Name); err != nil {
			results[index] = deniedToolResult(call, "Tool executor identity check failed: "+err.Error())
			continue
		}
		if opts.PlanMode && !planModeTools[call.Name] {
			results[index] = deniedToolResult(
				call,
				"Plan mode is read-only; describe the change instead of executing it",
			)
			continue
		}
		if OfflineMode() && IsEgressTool(call.Name) {
			results[index] = deniedToolResult(
				call,
				call.Name+" is disabled in air-gap mode",
			)
			continue
		}
		if opts.ScopeCheck != nil {
			if allowed, reason := opts.ScopeCheck(call); !allowed {
				if strings.TrimSpace(reason) == "" {
					reason = "Tool target is outside the assigned scope"
				}
				results[index] = deniedToolResult(call, reason)
				continue
			}
		}

		snapshotDecision := ""
		if opts.SnapshotDecision != nil {
			snapshotDecision = opts.SnapshotDecision(call)
		}
		permission := ResolvePermission(
			call.Name,
			call.Input,
			opts.WorkDir,
			opts.PermissionConfig,
			snapshotDecision,
		)
		emitToolInput(opts.Emit, call, permission.Danger)
		var completedApproval approval.Pending
		switch permission.Decision {
		case PermissionDeny:
			results[index] = deniedToolResult(call, permission.Reason)
			continue
		case PermissionApproval:
			var reason string
			completedApproval, reason = requestToolApproval(call, permission, opts)
			if reason != "" {
				results[index] = deniedToolResult(call, reason)
				continue
			}
		}
		// Desktop control always requires an explicit, per-call operator
		// approval even when a broad automatic permission rule would otherwise
		// classify the call as allowed.
		if isHostInteractionTool(call.Name) && completedApproval.ID == "" {
			var reason string
			completedApproval, reason = requestToolApproval(call, PermissionResult{
				Decision: PermissionApproval,
				Reason:   "Explicit host interaction approval required",
				Danger:   permission.Danger,
			}, opts)
			if reason != "" {
				results[index] = deniedToolResult(call, reason)
				continue
			}
		}
		// Executable plugins are third-party process code. Every individual call
		// requires an operator round-trip even when permission snapshots or
		// AutoApprove would otherwise allow it.
		if identity.Kind == toolExecutorPlugin && completedApproval.ID == "" {
			var reason string
			completedApproval, reason = requestToolApproval(call, PermissionResult{
				Decision: PermissionApproval,
				Reason:   "Explicit executable plugin approval required",
				Danger:   permission.Danger,
			}, opts)
			if reason != "" {
				results[index] = deniedToolResult(call, reason)
				continue
			}
		}
		// An explicit allow-once is a real user decision and therefore starts a
		// fresh repetition budget. Automatic permission rules never reach this
		// branch and cannot be used by the model to reset the guard.
		if completedApproval.ID != "" && opts.RunGuard != nil {
			opts.RunGuard.Reset()
		}
		// Project hooks are executable code, not part of authorization. Run
		// them serially only after every hard boundary, permission/approval,
		// and scope check has allowed this exact call. This prevents a denied
		// model call from using PreToolUse as an execution side channel while
		// preserving deterministic hook ordering for concurrent-safe tools.
		if reason := dispatchContextDenial(opts.Context); reason != "" {
			results[index] = deniedToolResult(call, reason)
			continue
		}
		if opts.PreHook != nil {
			if blocked, reason := opts.PreHook(call); blocked {
				results[index] = deniedToolResult(call, "Pre-tool hook blocked: "+strings.TrimSpace(reason))
				continue
			}
		}

		inputMap := make(map[string]interface{})
		_ = json.Unmarshal(call.Input, &inputMap)
		prepared := preparedToolCall{
			index:      index,
			tool:       call,
			concurrent: IsConcurrencySafe(call.Name, inputMap),
		}
		if isHostInteractionTool(call.Name) {
			proof, err := mintHostInteractionApproval(
				completedApproval.ID,
				opts.SessionID,
				opts.RunID,
				call.Name,
				call.Input,
				completedApproval.ExpiresAt,
			)
			if err != nil {
				results[index] = deniedToolResult(call, "Host interaction approval proof could not be created")
				continue
			}
			prepared.hostApproval = &proof
		}
		if identity.Kind == toolExecutorPlugin {
			proof, err := mintPluginApproval(
				completedApproval.ID,
				opts.SessionID,
				opts.RunID,
				call.Name,
				identity.ExecutorID,
				call.Input,
				completedApproval.ExpiresAt,
			)
			if err != nil {
				results[index] = deniedToolResult(call, "Plugin approval proof could not be created")
				continue
			}
			prepared.pluginApproval = &proof
		}
		if prepared.concurrent {
			concurrent = append(concurrent, prepared)
		} else {
			serial = append(serial, prepared)
		}
	}

	// All concurrency-safe reads finish before serial mutations are checked.
	// This intentionally lets a Read+Edit batch establish fresh ledger evidence
	// without changing the original result/event slots.
	if len(concurrent) > 0 {
		type indexedResult struct {
			index  int
			result toolDispatchResult
		}
		resultCh := make(chan indexedResult, len(concurrent))
		for _, prepared := range concurrent {
			go func(call preparedToolCall) {
				result := executePreparedToolCall(call.tool, call.hostApproval, call.pluginApproval, opts)
				result.Concurrent = true
				resultCh <- indexedResult{index: call.index, result: result}
			}(prepared)
		}
		for range concurrent {
			indexed := <-resultCh
			results[indexed.index] = indexed.result
		}
	}

	if hasPreparedFileMutations(serial) {
		fileMutationBatchMu.Lock()
		defer fileMutationBatchMu.Unlock()
	}
	preflightBlocked := preflightFileMutationBatch(serial, opts)
	journal := make([]committedFileMutation, 0, len(serial))
	mutationBatchFailed := false
	for _, prepared := range serial {
		isMutation := isFileMutationTool(prepared.tool.Name)
		if reason := preflightBlocked[prepared.index]; reason != "" {
			results[prepared.index] = deniedToolResult(prepared.tool, reason)
			mutationBatchFailed = true
			continue
		}
		if isMutation && mutationBatchFailed {
			results[prepared.index] = deniedToolResult(
				prepared.tool,
				"File mutation batch was aborted after an earlier mutation failed",
			)
			continue
		}

		var snapshot fileMutationSnapshot
		if isMutation {
			var err error
			snapshot, err = captureFileMutationSnapshot(prepared.tool, opts)
			if err != nil {
				results[prepared.index] = deniedToolResult(
					prepared.tool,
					"File mutation snapshot failed: "+err.Error(),
				)
				mutationBatchFailed = true
				rollbackErrors := rollbackFileMutationBatch(journal, results, opts.ReadLedger)
				if len(rollbackErrors) > 0 {
					results[prepared.index].Content += fmt.Sprintf(
						"; %d earlier change(s) could not be rolled back safely",
						len(rollbackErrors),
					)
				}
				journal = nil
				continue
			}
		}

		result := executePreparedToolCall(prepared.tool, prepared.hostApproval, prepared.pluginApproval, opts)
		results[prepared.index] = result
		if !isMutation {
			continue
		}
		if result.IsError {
			mutationBatchFailed = true
			rollbackErrors := rollbackFileMutationBatch(journal, results, opts.ReadLedger)
			if len(rollbackErrors) > 0 {
				results[prepared.index].Content += fmt.Sprintf(
					"; %d earlier change(s) could not be rolled back safely",
					len(rollbackErrors),
				)
			}
			journal = nil
			continue
		}
		postRevision, err := committedMutationRevision(snapshot)
		if err != nil {
			results[prepared.index].IsError = true
			results[prepared.index].Content = "[TRANSACTION INCOMPLETE] The mutation executed, but its committed revision could not be captured: " + err.Error()
			results[prepared.index].Display = results[prepared.index].Content
			mutationBatchFailed = true
			rollbackErrors := rollbackFileMutationBatch(journal, results, opts.ReadLedger)
			if len(rollbackErrors) > 0 {
				results[prepared.index].Content += fmt.Sprintf(
					"; %d earlier change(s) could not be rolled back safely",
					len(rollbackErrors),
				)
			}
			journal = nil
			continue
		}
		journal = append(journal, committedFileMutation{
			ResultIndex:  prepared.index,
			Snapshot:     snapshot,
			PostRevision: postRevision,
		})
	}
	return results
}

func normalizeDispatchToolCall(call toolUseBlock) (toolUseBlock, error) {
	raw := bytes.TrimSpace(call.Input)
	if strings.TrimSpace(call.InputRaw) != "" {
		raw = bytes.TrimSpace([]byte(call.InputRaw))
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return call, err
	}
	if object == nil {
		return call, fmt.Errorf("tool input must be a JSON object")
	}
	call.Input = append(json.RawMessage(nil), raw...)
	call.InputRaw = string(raw)
	return call, nil
}

func executePreparedToolCall(
	call toolUseBlock,
	hostApproval *hostInteractionApprovalProof,
	pluginApproval *pluginApprovalProof,
	opts toolDispatchOptions,
) toolDispatchResult {
	if reason := dispatchContextDenial(opts.Context); reason != "" {
		return deniedToolResult(call, reason)
	}
	identity, err := executorIdentityForAllowedTool(opts.AllowedTools, call.Name)
	if err != nil {
		return deniedToolResult(call, "Tool executor identity is unavailable: "+err.Error())
	}
	if err := validateToolExecutorIdentity(opts.AllowedTools, call.Name); err != nil {
		return deniedToolResult(call, "Tool executor identity check failed: "+err.Error())
	}
	reason, mutationDecision, guardInput := checkDispatchRuntimePolicy(call, opts)
	if reason != "" {
		return deniedToolResult(call, reason)
	}
	if opts.Execute == nil {
		return deniedToolResult(call, "Tool executor is unavailable")
	}

	var mutation toolMutationPreview
	if opts.BeforeExecute != nil {
		mutation = opts.BeforeExecute(call)
	}
	// Approval may have completed well before a serial call reaches this point.
	// Re-check cancellation immediately before invoking the executor so an
	// allow/cancel race fails closed.
	if reason := dispatchContextDenial(opts.Context); reason != "" {
		return deniedToolResult(call, reason)
	}
	if err := validateToolExecutorIdentity(opts.AllowedTools, call.Name); err != nil {
		return deniedToolResult(call, "Tool executor identity check failed: "+err.Error())
	}
	boundInput, err := bindToolExecutionInput(call.Input, identity)
	if err != nil {
		return deniedToolResult(call, "Tool executor binding failed: "+err.Error())
	}
	mutationToken := ""
	if mutationDecision != nil {
		boundInput, mutationToken, err = bindFileMutationExecutionInput(
			boundInput,
			call.Name,
			*mutationDecision,
		)
		if err != nil {
			return deniedToolResult(call, "File mutation precondition binding failed: "+err.Error())
		}
		defer discardFileMutationPrecondition(mutationToken)
	}
	if isHostInteractionTool(call.Name) {
		if hostApproval == nil {
			return deniedToolResult(call, "Host interaction approval proof is unavailable")
		}
		boundInput, err = bindHostInteractionExecutionInput(boundInput, *hostApproval)
		if err != nil {
			return deniedToolResult(call, "Host interaction approval binding failed")
		}
	}
	if identity.Kind == toolExecutorPlugin {
		if pluginApproval == nil {
			return deniedToolResult(call, "Executable plugin approval proof is unavailable")
		}
		boundInput, err = bindPluginApprovalExecutionInput(boundInput, *pluginApproval)
		if err != nil {
			return deniedToolResult(call, "Executable plugin approval binding failed")
		}
	}
	executionCall := call
	executionCall.Input = boundInput
	executionCall.InputRaw = string(boundInput)
	if opts.PreExecutionJournal != nil {
		if err := opts.PreExecutionJournal(ToolExecutionJournalEntry{
			ID:          call.ID,
			Name:        call.Name,
			InputDigest: toolInputDigest(call),
			RunID:       opts.RunID,
		}); err != nil {
			// The journal owns its diagnostic details. Never surface an error
			// that may contain storage paths or other persistence internals.
			return deniedToolResult(call, "Tool execution journal failed")
		}
	}
	if opts.Emit != nil {
		// Lifecycle-only event: never include raw arguments. Transport adapters
		// use this exact pre-execution boundary to mark interrupted durable
		// sessions without treating a model proposal as an applied side effect.
		opts.Emit(Event{Type: "tool_execution_start", Data: map[string]string{
			"id":          call.ID,
			"name":        call.Name,
			"inputDigest": toolInputDigest(call),
			"runId":       opts.RunID,
		}})
	}
	content, isError := opts.Execute(executionCall)
	updateDispatchReadLedger(call, isError, opts.ReadLedger)
	if guardInput != nil && opts.RunGuard != nil {
		opts.RunGuard.ObserveResult(*guardInput, content, isError)
	}
	if opts.PostHook != nil {
		opts.PostHook(call, content, isError)
	}
	return toolDispatchResult{
		Tool:     call,
		Content:  content,
		Display:  content,
		IsError:  isError,
		Executed: true,
		Mutation: mutation,
	}
}

func dispatchContextDenial(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	select {
	case <-ctx.Done():
		return "Run context was canceled before tool execution"
	default:
		return ""
	}
}

func checkDispatchRuntimePolicy(
	call toolUseBlock,
	opts toolDispatchOptions,
) (string, *ReadLedgerDecision, *ActionFingerprintInput) {
	targetRevision := ""
	ledgerCode := ReadLedgerAllowed
	ledgerReason := ""
	filePath := dispatchFilePath(call.Input)
	var mutationDecision *ReadLedgerDecision

	if opts.ReadLedger != nil && filePath != "" {
		if revision, err := opts.ReadLedger.CurrentRevision(filePath); err == nil {
			targetRevision = revision
		} else {
			targetRevision = "unavailable"
		}
	}
	if opts.ReadBeforeWrite && (call.Name == "Write" || call.Name == "Edit") {
		if opts.ReadLedger == nil {
			ledgerCode = ReadLedgerRevisionUnavailable
			ledgerReason = "Read-before-write ledger is unavailable"
		} else {
			var decision ReadLedgerDecision
			if call.Name == "Write" {
				decision = opts.ReadLedger.CheckWrite(filePath)
			} else {
				decision = opts.ReadLedger.CheckEdit(filePath)
			}
			ledgerCode = decision.Code
			if decision.CurrentRevision != "" {
				targetRevision = decision.CurrentRevision
			}
			if !decision.Allowed {
				ledgerReason = readLedgerDenialReason(decision)
			} else {
				captured := decision
				mutationDecision = &captured
			}
		}
	}

	var guardInput *ActionFingerprintInput
	if opts.RunGuard != nil {
		failureCode := ""
		if ledgerReason != "" {
			failureCode = string(ledgerCode)
		}
		input := ActionFingerprintInput{
			ToolName:       call.Name,
			Arguments:      call.Input,
			TargetRevision: targetRevision,
			FailureCode:    failureCode,
			PlanStep:       opts.PlanStep,
		}
		guardInput = &input
		decision := opts.RunGuard.Observe(input)
		if !decision.Allowed {
			return fmt.Sprintf(
				"Repeated action blocked after %d equivalent attempts",
				decision.Limit,
			), nil, guardInput
		}
	}
	if ledgerReason != "" {
		return ledgerReason, nil, guardInput
	}
	return "", mutationDecision, guardInput
}

func updateDispatchReadLedger(call toolUseBlock, isError bool, ledger *ReadLedger) {
	if ledger == nil {
		return
	}
	filePath := dispatchFilePath(call.Input)
	if filePath == "" {
		return
	}
	switch call.Name {
	case "Read":
		if !isError {
			_ = ledger.RecordRead(filePath)
		}
	case "Write", "Edit":
		if isError {
			_ = ledger.Forget(filePath)
			return
		}
		_ = ledger.RefreshAfterWrite(filePath)
	}
}

func readLedgerDenialReason(decision ReadLedgerDecision) string {
	switch decision.Code {
	case ReadLedgerReadRequired:
		return "Read-before-write blocked: read the existing file before changing it"
	case ReadLedgerStaleRead:
		return "Read-before-write blocked: the file changed after it was read"
	case ReadLedgerMissingTarget:
		return "Edit target does not exist"
	default:
		if strings.TrimSpace(decision.Detail) != "" {
			return "Read-before-write blocked: " + decision.Detail
		}
		return "Read-before-write safety check failed"
	}
}

func deniedToolResult(call toolUseBlock, reason string) toolDispatchResult {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Tool call denied"
	}
	return toolDispatchResult{
		Tool:      call,
		Content:   "Permission denied: " + reason,
		Display:   "[BLOCKED] " + reason,
		IsError:   true,
		Synthetic: true,
	}
}

func emitToolInput(emit func(Event), call toolUseBlock, danger DangerLevel) {
	if emit == nil {
		return
	}
	var inputPreview interface{}
	_ = json.Unmarshal(call.Input, &inputPreview)
	emit(Event{Type: "tool_input", Data: map[string]interface{}{
		"id":     call.ID,
		"name":   call.Name,
		"input":  inputPreview,
		"danger": string(danger),
	}})
}

func requestToolApproval(
	call toolUseBlock,
	permission PermissionResult,
	opts toolDispatchOptions,
) (approval.Pending, string) {
	if opts.ApprovalRequester == nil {
		return approval.Pending{}, "Explicit approval is required but no approval requester is available"
	}
	if strings.TrimSpace(opts.SessionID) == "" {
		return approval.Pending{}, "Explicit approval is required but the active session ID is unavailable"
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	pending, err := opts.ApprovalRequester.Open(approval.Draft{
		SessionID:       opts.SessionID,
		RunID:           opts.RunID,
		ToolCallID:      call.ID,
		ToolName:        call.Name,
		RedactedInput:   redactedApprovalInput(call),
		InputDigest:     toolInputDigest(call),
		DangerLevel:     string(permission.Danger),
		Scope:           approvalScope(call),
		RememberAllowed: false,
	})
	if err != nil {
		return approval.Pending{}, "Approval request could not be opened"
	}
	if strings.TrimSpace(pending.ID) == "" || pending.SessionID != opts.SessionID ||
		pending.RunID != opts.RunID || pending.ToolName != call.Name ||
		(pending.ToolCallID != "" && pending.ToolCallID != call.ID) ||
		(pending.InputDigest != "" && pending.InputDigest != toolInputDigest(call)) ||
		(!pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(time.Now())) {
		return approval.Pending{}, "Approval request metadata is invalid"
	}
	if opts.Emit != nil {
		opts.Emit(Event{Type: "approval_required", Data: map[string]interface{}{
			"id":              pending.ID,
			"sessionId":       pending.SessionID,
			"sessionRevision": pending.SessionRevision,
			"runId":           pending.RunID,
			"toolName":        pending.ToolName,
			"redactedInput":   pending.RedactedInput,
			"inputDigest":     pending.InputDigest,
			"dangerLevel":     pending.DangerLevel,
			"scope":           pending.Scope,
			"expiresAt":       pending.ExpiresAt,
		}})
	}
	resolution, err := opts.ApprovalRequester.Await(ctx, opts.SessionID, pending.ID)
	if err != nil || resolution.ApprovalID != pending.ID || !resolution.Allowed() ||
		(!pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(time.Now())) {
		return approval.Pending{}, "Explicit approval was denied or became unavailable"
	}
	return pending, ""
}

func toolInputDigest(call toolUseBlock) string {
	digest := sha256.Sum256(append([]byte(call.Name+"\x00"), call.Input...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func redactedApprovalInput(call toolUseBlock) string {
	path := dispatchFilePath(call.Input)
	if path != "" {
		encoded, _ := json.Marshal(map[string]string{"file_path": path})
		return string(encoded)
	}
	return fmt.Sprintf(
		"{\"input\":\"omitted\",\"digest\":%q}",
		toolInputDigest(call),
	)
}

func approvalScope(call toolUseBlock) string {
	if path := dispatchFilePath(call.Input); path != "" {
		return path
	}
	return "workspace"
}

func dispatchFilePath(input json.RawMessage) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(input, &object) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path"} {
		var path string
		if raw, ok := object[key]; ok && json.Unmarshal(raw, &path) == nil {
			if path = strings.TrimSpace(path); path != "" {
				return path
			}
		}
	}
	return ""
}

func toolCatalogNames(tools []types.ToolDef) map[string]struct{} {
	names := make(map[string]struct{}, len(tools)+1)
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != "" {
			names[tool.Name] = struct{}{}
		}
	}
	names[registerToolCatalogSchemas(tools)] = struct{}{}
	return names
}

func toolCatalogNamesForRun(tools []types.ToolDef) map[string]struct{} {
	names := make(map[string]struct{}, len(tools)+1)
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != "" {
			names[tool.Name] = struct{}{}
		}
	}
	names[registerRunToolCatalogSchemas(tools)] = struct{}{}
	return names
}

func toolRecoveryOptionsForCatalog(tools []types.ToolDef) ToolRecoveryOptions {
	allowed := toolCatalogNames(tools)
	aliases := make(map[string]string)
	for alias, target := range map[string]string{
		"read_file":    "Read",
		"write_file":   "Write",
		"edit_file":    "Edit",
		"list_files":   "LS",
		"search_files": "Grep",
		"glob_files":   "Glob",
		"run_command":  "Bash",
	} {
		if _, exists := allowed[target]; exists {
			aliases[alias] = target
		}
	}
	return ToolRecoveryOptions{
		Aliases:      aliases,
		AllowedTools: allowed,
	}
}

func recoverToolCallsForCatalog(text string, tools []types.ToolDef) ([]toolUseBlock, string) {
	return recoverLeakedToolCallsWithOptions(text, toolRecoveryOptionsForCatalog(tools))
}
