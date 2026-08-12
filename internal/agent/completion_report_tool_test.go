package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func newReportCompletionContract(
	t *testing.T,
	runID string,
	criterion string,
	evidenceRequired bool,
) (*CompletionContract, string) {
	t.Helper()
	spec := CompletionContractSpec{
		RunID:      runID,
		PlanAnchor: completionTestAnchor(t, []string{criterion}),
	}
	if !evidenceRequired {
		spec.EvidenceNotRequiredCriteria = []string{criterion}
	}
	contract, err := NewCompletionContract(spec)
	if err != nil {
		t.Fatal(err)
	}
	criterionID, err := CompletionCriterionID(criterion)
	if err != nil {
		t.Fatal(err)
	}
	return contract, criterionID
}

func reportCompletionInput(t *testing.T, expectedRevision uint64, claims []CompletionClaim) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"expectedRevision": expectedRevision,
		"claims":           claims,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func requireContentFreeCompletionReceipt(t *testing.T, content string) completionReportReceipt {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &fields); err != nil {
		t.Fatalf("completion receipt is not JSON: %q: %v", content, err)
	}
	if len(fields) != 3 || fields["revision"] == nil || fields["status"] == nil || fields["count"] == nil {
		t.Fatalf("completion receipt fields = %#v", fields)
	}
	var receipt completionReportReceipt
	if err := json.Unmarshal([]byte(content), &receipt); err != nil {
		t.Fatalf("decode completion receipt: %v", err)
	}
	return receipt
}

func dispatchReportCompletion(
	t *testing.T,
	runID string,
	contract *CompletionContract,
	resolver CompletionEvidenceResolver,
	input json.RawMessage,
) toolDispatchResult {
	t.Helper()
	definition := ReportCompletionToolDef()
	permission := DefaultPermissionConfig()
	permission.AutoApprove = "safe"
	results := dispatchToolCalls([]toolUseBlock{{
		ID:       "report-completion-call",
		Name:     reportCompletionToolName,
		Input:    append(json.RawMessage(nil), input...),
		InputRaw: string(input),
	}}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          t.TempDir(),
		AllowedTools:     toolCatalogNamesForRun([]types.ToolDef{definition}),
		PermissionConfig: permission,
		RunID:            runID,
		Execute: func(call toolUseBlock) (string, bool) {
			return ExecuteToolWithOptions(call.Name, call.Input, t.TempDir(), ToolExecutionOptions{
				Context:                    context.Background(),
				ExpectedRunID:              runID,
				CompletionContract:         contract,
				CompletionEvidenceResolver: resolver,
			})
		},
	})
	if len(results) != 1 {
		t.Fatalf("dispatch results = %#v", results)
	}
	return results[0]
}

func TestReportCompletionDefinitionIsStrictBoundedReservedAndOptIn(t *testing.T) {
	definition := ReportCompletionToolDef()
	if definition.Name != reportCompletionToolName || len(definition.InputSchema) == 0 {
		t.Fatalf("definition = %#v", definition)
	}
	for _, tool := range StaticToolDefs(t.TempDir()) {
		if tool.Name == reportCompletionToolName {
			t.Fatal("ReportCompletion entered the default catalog")
		}
	}
	reserved := currentBuiltInToolDefinitions()[reportCompletionToolName]
	if reserved.Name != reportCompletionToolName ||
		string(reserved.InputSchema) != string(definition.InputSchema) {
		t.Fatalf("reserved definition = %#v", reserved)
	}
	if level, _ := ClassifyDanger(reportCompletionToolName, json.RawMessage(`{}`)); level != DangerSafe {
		t.Fatalf("danger = %q, want safe", level)
	}
	permission := ResolvePermission(
		reportCompletionToolName,
		json.RawMessage(`{}`),
		t.TempDir(),
		DefaultPermissionConfig(),
		"",
	)
	if permission.Decision != PermissionAllow || permission.Danger != DangerSafe {
		t.Fatalf("permission = %#v, want safe internal allow", permission)
	}
	if IsConcurrencySafe(reportCompletionToolName, map[string]any{}) {
		t.Fatal("ReportCompletion must remain serial")
	}

	_, criterionID := newReportCompletionContract(t, "run-schema", "manual review", false)
	validClaim := CompletionClaim{
		CriterionID: criterionID,
		State:       CompletionClaimSatisfied,
		Assertion:   "reviewed",
	}
	valid := reportCompletionInput(t, 0, []CompletionClaim{validClaim})
	if err := validateToolInputSchema(valid, definition.InputSchema); err != nil {
		t.Fatalf("valid schema input rejected: %v", err)
	}

	tooMany := make([]CompletionClaim, maxCompletionCriteria+1)
	for index := range tooMany {
		tooMany[index] = CompletionClaim{
			CriterionID: fmt.Sprintf("criterion:sha256:%064x", index),
			State:       CompletionClaimPending,
		}
	}
	invalid := []struct {
		name  string
		input json.RawMessage
	}{
		{name: "missing claims", input: json.RawMessage(`{"expectedRevision":0}`)},
		{name: "unknown root", input: json.RawMessage(`{"expectedRevision":0,"claims":[],"raw":"secret"}`)},
		{name: "unknown claim", input: json.RawMessage(`{"expectedRevision":0,"claims":[{"criterionId":"` + criterionID + `","state":"pending","raw":"secret"}]}`)},
		{name: "empty claims", input: reportCompletionInput(t, 0, make([]CompletionClaim, 0))},
		{name: "duplicate claims", input: reportCompletionInput(t, 0, []CompletionClaim{validClaim, validClaim})},
		{name: "too many claims", input: reportCompletionInput(t, 0, tooMany)},
		{name: "oversized summary", input: reportCompletionInput(t, 0, []CompletionClaim{{
			CriterionID: criterionID, State: CompletionClaimBlocked, Summary: strings.Repeat("s", maxCompletionSummaryBytes+1),
		}})},
		{name: "negative revision", input: json.RawMessage(`{"expectedRevision":-1,"claims":[{"criterionId":"` + criterionID + `","state":"pending"}]}`)},
		{name: "unknown state", input: json.RawMessage(`{"expectedRevision":0,"claims":[{"criterionId":"` + criterionID + `","state":"done"}]}`)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := validateToolInputSchema(test.input, definition.InputSchema); err == nil {
				t.Fatalf("invalid input passed schema: %s", test.input)
			}
		})
	}

	override := definition
	override.InputSchema = json.RawMessage(`{"type":"object"}`)
	if _, err := snapshotForAllowedTools(toolCatalogNamesForRun([]types.ToolDef{override})); err == nil || !strings.Contains(err.Error(), "overrides a reserved built-in schema") {
		t.Fatalf("reserved schema override error = %v", err)
	}
}

func TestReportCompletionRejectsMCPAndPluginCollisions(t *testing.T) {
	definition := ReportCompletionToolDef()
	installIdentityTestMCPClient(t, "report-completion-collision", &MCPClient{
		executorID: newMCPExecutorID(),
		tools: []MCPTool{{
			Name:        reportCompletionToolName,
			InputSchema: definition.InputSchema,
		}},
	})
	if _, err := snapshotForAllowedTools(toolCatalogNames([]types.ToolDef{definition})); err == nil || !strings.Contains(err.Error(), "collides with a reserved built-in name") {
		t.Fatalf("MCP collision error = %v", err)
	}

	pluginRoot := t.TempDir()
	pluginDir := filepath.Join(pluginRoot, "completion-collision")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"completion-collision","version":"1.0.0","tools":[{"name":"ReportCompletion","command":"must-not-run"}]}`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewPluginManager(pluginRoot)
	if err := manager.LoadAllStrict(); err == nil || !strings.Contains(err.Error(), "collides with a reserved built-in name") {
		t.Fatalf("plugin collision error = %v", err)
	}
}

func TestReportCompletionForcedAndMissingBindingsFailClosed(t *testing.T) {
	runID := "run-forced-report"
	contract, criterionID := newReportCompletionContract(t, runID, "manual review", false)
	sentinel := "RAW_ASSERTION_MUST_NOT_ECHO"
	input := reportCompletionInput(t, 0, []CompletionClaim{{
		CriterionID: criterionID,
		State:       CompletionClaimSatisfied,
		Assertion:   sentinel,
	}})
	resolver := CompletionEvidenceResolver(func(string, string) (bool, error) { return true, nil })

	for _, call := range []struct {
		name string
		run  func() (string, bool)
	}{
		{name: "legacy ExecuteTool", run: func() (string, bool) {
			return ExecuteTool(reportCompletionToolName, input, t.TempDir())
		}},
		{name: "options without catalog proof", run: func() (string, bool) {
			return ExecuteToolWithOptions(reportCompletionToolName, input, t.TempDir(), ToolExecutionOptions{
				ExpectedRunID: runID, CompletionContract: contract, CompletionEvidenceResolver: resolver,
			})
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			result, failed := call.run()
			requireContentFreeCompletionReceipt(t, result)
			if !failed || strings.Contains(result, sentinel) || contract.Revision() != 0 {
				t.Fatalf("forced result=(%q,%v) revision=%d", result, failed, contract.Revision())
			}
		})
	}

	missingContract := dispatchReportCompletion(t, runID, nil, resolver, input)
	requireContentFreeCompletionReceipt(t, missingContract.Content)
	if !missingContract.IsError || strings.Contains(missingContract.Content, sentinel) || contract.Revision() != 0 {
		t.Fatalf("missing contract result = %#v", missingContract)
	}
	missingResolver := dispatchReportCompletion(t, runID, contract, nil, input)
	requireContentFreeCompletionReceipt(t, missingResolver.Content)
	if !missingResolver.IsError || strings.Contains(missingResolver.Content, sentinel) || contract.Revision() != 0 {
		t.Fatalf("missing resolver result = %#v", missingResolver)
	}
	wrongRun := dispatchReportCompletion(t, "another-run", contract, resolver, input)
	requireContentFreeCompletionReceipt(t, wrongRun.Content)
	if !wrongRun.IsError || strings.Contains(wrongRun.Content, sentinel) || contract.Revision() != 0 {
		t.Fatalf("wrong run result = %#v", wrongRun)
	}
}

func TestReportCompletionRejectsStaleUnknownDuplicateAndEvidenceFailuresWithoutEcho(t *testing.T) {
	tests := []struct {
		name      string
		configure func(t *testing.T) (*CompletionContract, string, json.RawMessage, CompletionEvidenceResolver)
		sentinels []string
	}{
		{
			name: "stale revision",
			configure: func(t *testing.T) (*CompletionContract, string, json.RawMessage, CompletionEvidenceResolver) {
				contract, criterionID := newReportCompletionContract(t, "run-stale-report", "manual review", false)
				sentinel := "STALE_ASSERTION_MUST_NOT_ECHO"
				return contract, "run-stale-report", reportCompletionInput(t, 1, []CompletionClaim{{
					CriterionID: criterionID, State: CompletionClaimSatisfied, Assertion: sentinel,
				}}), func(string, string) (bool, error) { return true, nil }
			},
			sentinels: []string{"STALE_ASSERTION_MUST_NOT_ECHO"},
		},
		{
			name: "unknown criterion",
			configure: func(t *testing.T) (*CompletionContract, string, json.RawMessage, CompletionEvidenceResolver) {
				contract, _ := newReportCompletionContract(t, "run-unknown-report", "manual review", false)
				unknownID, _ := CompletionCriterionID("unknown private criterion")
				sentinel := "UNKNOWN_ASSERTION_MUST_NOT_ECHO"
				return contract, "run-unknown-report", reportCompletionInput(t, 0, []CompletionClaim{{
					CriterionID: unknownID, State: CompletionClaimSatisfied, Assertion: sentinel,
				}}), func(string, string) (bool, error) { return true, nil }
			},
			sentinels: []string{"UNKNOWN_ASSERTION_MUST_NOT_ECHO", "unknown private criterion"},
		},
		{
			name: "conflicting duplicate",
			configure: func(t *testing.T) (*CompletionContract, string, json.RawMessage, CompletionEvidenceResolver) {
				contract, criterionID := newReportCompletionContract(t, "run-duplicate-report", "manual review", false)
				return contract, "run-duplicate-report", reportCompletionInput(t, 0, []CompletionClaim{
					{CriterionID: criterionID, State: CompletionClaimSatisfied, Assertion: "DUPLICATE_ASSERTION_MUST_NOT_ECHO"},
					{CriterionID: criterionID, State: CompletionClaimBlocked, Summary: "DUPLICATE_SUMMARY_MUST_NOT_ECHO"},
				}), func(string, string) (bool, error) { return true, nil }
			},
			sentinels: []string{"DUPLICATE_ASSERTION_MUST_NOT_ECHO", "DUPLICATE_SUMMARY_MUST_NOT_ECHO"},
		},
		{
			name: "duplicate JSON field",
			configure: func(t *testing.T) (*CompletionContract, string, json.RawMessage, CompletionEvidenceResolver) {
				contract, criterionID := newReportCompletionContract(t, "run-duplicate-json-report", "manual review", false)
				input := json.RawMessage(`{"expectedRevision":0,"expectedRevision":0,"claims":[{"criterionId":"` + criterionID + `","state":"satisfied","assertion":"DUPLICATE_JSON_MUST_NOT_ECHO"}]}`)
				return contract, "run-duplicate-json-report", input, func(string, string) (bool, error) { return true, nil }
			},
			sentinels: []string{"DUPLICATE_JSON_MUST_NOT_ECHO"},
		},
		{
			name: "resolver failure",
			configure: func(t *testing.T) (*CompletionContract, string, json.RawMessage, CompletionEvidenceResolver) {
				contract, criterionID := newReportCompletionContract(t, "run-evidence-report", "tests pass", true)
				digest := completionTestDigest("private evidence")
				input := reportCompletionInput(t, 0, []CompletionClaim{{
					CriterionID: criterionID, State: CompletionClaimSatisfied, EvidenceDigest: digest,
					Summary: "RAW_SUMMARY_MUST_NOT_ECHO",
				}})
				return contract, "run-evidence-report", input, func(string, string) (bool, error) {
					return false, errors.New("RAW_RESOLVER_ERROR_MUST_NOT_ECHO")
				}
			},
			sentinels: []string{"RAW_SUMMARY_MUST_NOT_ECHO", "RAW_RESOLVER_ERROR_MUST_NOT_ECHO", "private evidence"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract, runID, input, resolver := test.configure(t)
			result := dispatchReportCompletion(t, runID, contract, resolver, input)
			receipt := requireContentFreeCompletionReceipt(t, result.Content)
			if !result.IsError || contract.Revision() != 0 {
				t.Fatalf("rejected result=%#v revision=%d", result, contract.Revision())
			}
			if receipt.Count != 0 {
				t.Fatalf("rejected receipt count = %d", receipt.Count)
			}
			for _, sentinel := range test.sentinels {
				if strings.Contains(result.Content, sentinel) {
					t.Fatalf("result echoed %q: %s", sentinel, result.Content)
				}
			}
		})
	}
}

func TestReportCompletionIdentityBoundValidApplyReturnsContentFreeReceipt(t *testing.T) {
	runID := "run-valid-report"
	contract, criterionID := newReportCompletionContract(t, runID, "focused tests pass", true)
	digest := completionTestDigest("go test ./internal/agent")
	summary := "FOCUSED_TEST_SUMMARY_MUST_NOT_ECHO token=private-completion-secret"
	input := reportCompletionInput(t, 0, []CompletionClaim{{
		CriterionID:    criterionID,
		State:          CompletionClaimSatisfied,
		EvidenceDigest: digest,
		Summary:        summary,
	}})
	resolverCalls := 0
	result := dispatchReportCompletion(t, runID, contract, func(gotRunID, gotDigest string) (bool, error) {
		resolverCalls++
		return gotRunID == runID && gotDigest == digest, nil
	}, input)
	if result.IsError || !result.Executed || resolverCalls != 1 {
		t.Fatalf("valid result=%#v resolverCalls=%d", result, resolverCalls)
	}
	receipt := requireContentFreeCompletionReceipt(t, result.Content)
	if receipt.Revision != 1 || receipt.Status != CompletionStatusComplete || receipt.Count != 1 {
		t.Fatalf("receipt = %#v", receipt)
	}
	for _, forbidden := range []string{summary, "private-completion-secret", criterionID, digest, "summary", "assertion", "evidence"} {
		if strings.Contains(result.Content, forbidden) {
			t.Fatalf("receipt echoed %q: %s", forbidden, result.Content)
		}
	}
	if contract.Revision() != 1 || contract.Status() != CompletionStatusComplete {
		t.Fatalf("contract state=(%d,%q)", contract.Revision(), contract.Status())
	}
}
