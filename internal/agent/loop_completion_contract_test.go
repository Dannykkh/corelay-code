package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

var completionEvidencePromptPattern = regexp.MustCompile(`evidence-digest=(sha256:[0-9a-f]{64})`)

type completionLoopStep func(*types.MessagesRequest) (scriptedLoopStep, error)

type completionLoopProvider struct {
	mu       sync.Mutex
	steps    []completionLoopStep
	requests []*types.MessagesRequest
	errs     []error
}

func (*completionLoopProvider) Name() string              { return "completion-loop" }
func (*completionLoopProvider) DisplayName() string       { return "Completion Loop" }
func (*completionLoopProvider) Models() []types.ModelInfo { return nil }
func (*completionLoopProvider) Validate() error           { return nil }

func (p *completionLoopProvider) StreamMessage(
	ctx context.Context,
	request *types.MessagesRequest,
	options *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, cloneMessagesRequest(request))
	var step completionLoopStep
	if index < len(p.steps) {
		step = p.steps[index]
	}
	p.mu.Unlock()

	result := textStep("completion fixture exhausted")
	if step != nil {
		var err error
		result, err = step(request)
		if err != nil {
			p.mu.Lock()
			p.errs = append(p.errs, err)
			p.mu.Unlock()
			result = textStep("completion fixture failed")
		}
	}
	delegate := &scriptedLoopProvider{steps: []scriptedLoopStep{result}}
	return delegate.StreamMessage(ctx, request, options)
}

func (p *completionLoopProvider) snapshot() ([]*types.MessagesRequest, []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := make([]*types.MessagesRequest, 0, len(p.requests))
	for _, request := range p.requests {
		requests = append(requests, cloneMessagesRequest(request))
	}
	return requests, append([]error(nil), p.errs...)
}

type completionLoopRecorder struct {
	mu        sync.Mutex
	summaries []RunSummary
	receipts  []AgentReceipt
	failures  []string
}

func (*completionLoopRecorder) RunStarted() {}
func (r *completionLoopRecorder) ReceiptWritten(_ string, receipt AgentReceipt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts = append(r.receipts, receipt)
}
func (r *completionLoopRecorder) RunCompleted(summary RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summaries = append(r.summaries, summary)
}
func (r *completionLoopRecorder) RunFailed(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, message)
}
func (r *completionLoopRecorder) snapshot() ([]RunSummary, []AgentReceipt, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunSummary(nil), r.summaries...),
		append([]AgentReceipt(nil), r.receipts...),
		append([]string(nil), r.failures...)
}

func strictCompletionProfile(id string, routing harness.ToolRoutingPolicy) harness.HarnessProfile {
	return harness.MustResolveProfile(harness.ProfileSpec{
		ID:              id,
		ContextWindow:   65_536,
		OutputReserve:   4_096,
		MaxIterations:   8,
		MaxErrorRounds:  3,
		PlanAnchorMode:  harness.PlanAnchorStrict,
		ToolRouting:     routing,
		ReadBeforeWrite: harness.SomeBool(true),
	})
}

func completionLoopAnchor(t *testing.T, definition string) PlanAnchor {
	t.Helper()
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "finish through the machine-readable contract",
		CurrentStep:      "collect evidence and report completion",
		DefinitionOfDone: []string{definition},
	})
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}

func runCompletionContractLoop(
	t *testing.T,
	provider types.Provider,
	workDir string,
	profile harness.HarnessProfile,
	anchor PlanAnchor,
	opts RunOptions,
) ([]Event, *DurableRunObserver) {
	t.Helper()
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("CORELAY_OFFLINE", "1")
	user := mustJSON("Read proof.txt and finish the exact Definition of Done.")
	opts.HarnessProfile = &profile
	opts.PlanAnchor = &anchor
	opts.DisablePlugins = true
	opts.PluginDirs = []string{}
	opts.DisableWorkspaceMCP = true
	opts.EvidencePolicy = EvidencePolicyConfig{Policy: EvidencePolicyOff}
	events := make(chan Event, 256)
	go RunLoopWithOptions(
		context.Background(),
		provider,
		"completion-model",
		[]types.Message{{Role: "user", Content: user}},
		workDir,
		opts,
		events,
	)
	observer := NewDurableRunObserver("completion-test")
	var collected []Event
	for event := range events {
		collected = append(collected, event)
		observer.Observe(event)
	}
	return collected, observer
}

func TestRunLoopStrictCompletionUsesRoutedToolEvidenceAndMetadata(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	workDir := t.TempDir()
	if err := os.WriteFile(workDir+string(os.PathSeparator)+"proof.txt", []byte("proof content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const definition = "proof.txt was read successfully"
	criterionID, err := CompletionCriterionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	provider := &completionLoopProvider{}
	provider.steps = []completionLoopStep{
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("route", translate.ToolCategorySelectorName(), map[string]string{"category": "read"}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("read-proof", "Read", map[string]string{"file_path": "proof.txt"}), nil
		},
		func(request *types.MessagesRequest) (scriptedLoopStep, error) {
			system := completionRequestSystemText(request)
			match := completionEvidencePromptPattern.FindStringSubmatch(system)
			if len(match) != 2 {
				return scriptedLoopStep{}, fmt.Errorf("successful evidence digest missing from prompt")
			}
			if !strings.Contains(system, "revision=\"0\"") || !strings.Contains(system, criterionID) {
				return scriptedLoopStep{}, fmt.Errorf("current contract revision/criterion missing from prompt")
			}
			return toolUseStep("report", reportCompletionToolName, map[string]any{
				"expectedRevision": 0,
				"claims": []map[string]any{{
					"criterionId":    criterionID,
					"state":          CompletionClaimSatisfied,
					"evidenceDigest": match[1],
					"summary":        "proof file read completed",
				}},
			}), nil
		},
		func(request *types.MessagesRequest) (scriptedLoopStep, error) {
			system := completionRequestSystemText(request)
			if !strings.Contains(system, "revision=\"1\"") || !strings.Contains(system, "status=\"complete\"") {
				return scriptedLoopStep{}, fmt.Errorf("completed contract state missing from final prompt")
			}
			return textStep("CONTRACT_COMPLETE_SENTINEL"), nil
		},
	}
	recorder := &completionLoopRecorder{}
	profile := strictCompletionProfile("strict-two-stage", harness.ToolRoutingTwoStage)
	anchor := completionLoopAnchor(t, definition)
	events, observer := runCompletionContractLoop(t, provider, workDir, profile, anchor, RunOptions{Recorder: recorder})

	requests, providerErrs := provider.snapshot()
	if len(providerErrs) != 0 {
		t.Fatalf("provider fixture errors = %v\nevents:\n%s", providerErrs, eventDump(events))
	}
	if len(requests) != 4 {
		t.Fatalf("provider requests = %d, want 4\nevents:\n%s", len(requests), eventDump(events))
	}
	if !requestHasTool(requests[0], translate.ToolCategorySelectorName()) || !requestHasTool(requests[0], reportCompletionToolName) {
		t.Fatalf("first request did not expose selector+completion tools: %v", requestToolNames(requests[0]))
	}
	for index := 1; index < len(requests); index++ {
		if !requestHasTool(requests[index], reportCompletionToolName) {
			t.Fatalf("request %d lost ReportCompletion: %v", index, requestToolNames(requests[index]))
		}
	}
	if !eventTextContains(events, "CONTRACT_COMPLETE_SENTINEL") {
		t.Fatalf("final text was not emitted: %s", eventDump(events))
	}
	done := completionDoneEvent(t, events)
	if done["completionStatus"] != CompletionStatusComplete || done["completionRevision"] != uint64(1) || done["completionSatisfied"] != 1 {
		t.Fatalf("done completion metadata = %#v", done)
	}
	if !observer.Completed() {
		t.Fatal("durable observer did not accept the typed done terminal")
	}
	terminal, ok := observer.TerminalMetadata()
	if !ok || terminal.CompletionStatus != CompletionStatusComplete || terminal.CompletionRevision != 1 || terminal.CompletionSatisfied != 1 {
		t.Fatalf("durable terminal = (%#v, %v)", terminal, ok)
	}
	receipt := readOnlyAgentReceipt(t, configDir)
	if receipt.Completion == nil || receipt.Completion.Status != CompletionStatusComplete || receipt.Completion.Revision != 1 {
		t.Fatalf("receipt completion = %#v", receipt.Completion)
	}
	summaries, receipts, failures := recorder.snapshot()
	if len(failures) != 0 || len(summaries) != 1 || len(receipts) != 1 || summaries[0].Completion == nil || summaries[0].Completion.Status != CompletionStatusComplete {
		t.Fatalf("recorder state summaries=%#v receipts=%d failures=%v", summaries, len(receipts), failures)
	}
}

func TestRunLoopStrictCompletionRejectsProseThenEmitsBlockedTypedDone(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	workDir := t.TempDir()
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		textStep("FALSE_DONE_SENTINEL"),
		textStep("FALSE_DONE_SENTINEL"),
		textStep("FALSE_DONE_SENTINEL"),
	}}
	recorder := &completionLoopRecorder{}
	profile := strictCompletionProfile("strict-false-done", harness.ToolRoutingDirect)
	anchor := completionLoopAnchor(t, "operator acceptance recorded")
	events, observer := runCompletionContractLoop(t, provider, workDir, profile, anchor, RunOptions{
		Recorder:                      recorder,
		CompletionEvidenceNotRequired: []string{"operator acceptance recorded"},
	})

	if provider.callCount() != maxCompletionCorrectionAttempts+1 {
		t.Fatalf("provider calls = %d, want %d\nevents:\n%s", provider.callCount(), maxCompletionCorrectionAttempts+1, eventDump(events))
	}
	for _, event := range events {
		if event.Type == "text" && strings.Contains(toEventString(event.Data), "FALSE_DONE_SENTINEL") {
			t.Fatalf("false completion prose escaped to user output: %#v", event)
		}
	}
	if !eventTextContains(events, "Completion blocked: the Definition of Done remained incomplete") {
		t.Fatalf("blocked terminal explanation missing: %s", eventDump(events))
	}
	done := completionDoneEvent(t, events)
	if done["terminalState"] != EvidenceTerminalBlocked || done["completionStatus"] != CompletionStatusIncomplete {
		t.Fatalf("blocked done metadata = %#v", done)
	}
	terminal, ok := observer.TerminalMetadata()
	if !observer.Completed() || !ok || terminal.TerminalState != EvidenceTerminalBlocked || terminal.CompletionStatus != CompletionStatusIncomplete {
		t.Fatalf("durable blocked terminal = (%#v, %v), completed=%v", terminal, ok, observer.Completed())
	}
	receipt := readOnlyAgentReceipt(t, configDir)
	if receipt.Completion == nil || receipt.Completion.Status != CompletionStatusIncomplete || receipt.Verification.TerminalState != EvidenceTerminalBlocked {
		t.Fatalf("blocked receipt = completion:%#v verification:%#v", receipt.Completion, receipt.Verification)
	}
	summaries, _, failures := recorder.snapshot()
	// A blocked completion is a durable stream boundary, not a successful run:
	// the common finalizer must choose RunFailed instead of RunCompleted.
	if len(failures) != 1 || len(summaries) != 0 || !strings.Contains(failures[0], "Completion contract blocked") {
		t.Fatalf("blocked recorder state summaries=%#v failures=%v", summaries, failures)
	}
}

func TestRunLoopStrictCompletionWithholdsSelectorAndInvalidReportProse(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	workDir := t.TempDir()
	const definition = "operator acceptance recorded"
	criterionID, err := CompletionCriterionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		textAndToolUseStep(
			"FALSE_SELECTOR_DONE_SENTINEL",
			"route",
			translate.ToolCategorySelectorName(),
			map[string]string{"category": "read"},
		),
		textAndToolUseStep(
			"FALSE_REPORT_DONE_SENTINEL",
			"invalid-report",
			reportCompletionToolName,
			map[string]any{
				"expectedRevision": 1,
				"claims": []map[string]any{{
					"criterionId": criterionID,
					"state":       CompletionClaimSatisfied,
					"summary":     "unapproved completion",
				}},
			},
		),
		textStep("FALSE_CORRECTION_DONE_SENTINEL"),
		textStep("FALSE_CORRECTION_DONE_SENTINEL"),
		textStep("FALSE_CORRECTION_DONE_SENTINEL"),
	}}
	profile := strictCompletionProfile("strict-mixed-prose", harness.ToolRoutingTwoStage)
	anchor := completionLoopAnchor(t, definition)
	events, observer := runCompletionContractLoop(t, provider, workDir, profile, anchor, RunOptions{
		CompletionEvidenceNotRequired: []string{definition},
	})

	if provider.callCount() != 5 {
		t.Fatalf("provider calls = %d, want 5\nevents:\n%s", provider.callCount(), eventDump(events))
	}
	for _, sentinel := range []string{
		"FALSE_SELECTOR_DONE_SENTINEL",
		"FALSE_REPORT_DONE_SENTINEL",
		"FALSE_CORRECTION_DONE_SENTINEL",
	} {
		if eventTextContains(events, sentinel) {
			t.Fatalf("strict completion prose escaped before contract terminal: %q\n%s", sentinel, eventDump(events))
		}
		for _, message := range observer.Messages() {
			if strings.Contains(string(message.Content), sentinel) {
				t.Fatalf("strict completion prose entered durable transcript: %q in %#v", sentinel, message)
			}
		}
	}
	done := completionDoneEvent(t, events)
	if done["terminalState"] != EvidenceTerminalBlocked ||
		done["completionStatus"] != CompletionStatusIncomplete ||
		done["completionRevision"] != uint64(0) {
		t.Fatalf("blocked terminal metadata = %#v", done)
	}
}

func TestRunLoopStrictCompletionReportBlockedBypassesTwoStageSelector(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	workDir := t.TempDir()
	const definition = "external service is available"
	criterionID, _ := CompletionCriterionID(definition)
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("report-blocked", reportCompletionToolName, map[string]any{
			"expectedRevision": 0,
			"claims": []map[string]any{{
				"criterionId": criterionID,
				"state":       CompletionClaimBlocked,
				"summary":     "service unavailable",
			}},
		}),
		textStep("The required external service is unavailable."),
	}}
	profile := strictCompletionProfile("strict-selector-bypass", harness.ToolRoutingTwoStage)
	anchor := completionLoopAnchor(t, definition)
	events, _ := runCompletionContractLoop(t, provider, workDir, profile, anchor, RunOptions{})
	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2\nevents:\n%s", provider.callCount(), eventDump(events))
	}
	if eventTextContains(events, "selector correction") {
		t.Fatalf("ReportCompletion-only call was consumed by selector: %s", eventDump(events))
	}
	done := completionDoneEvent(t, events)
	if done["completionStatus"] != CompletionStatusBlocked || done["completionBlocked"] != 1 || done["terminalState"] != EvidenceTerminalBlocked {
		t.Fatalf("explicit blocked metadata = %#v", done)
	}
	receipt := readOnlyAgentReceipt(t, configDir)
	if receipt.Completion == nil || receipt.Completion.Status != CompletionStatusBlocked {
		t.Fatalf("explicit blocked receipt = %#v", receipt.Completion)
	}
}

func TestCompletionControlToolSurvivesBudgetPlanAndContextReduction(t *testing.T) {
	contract := newTestCompletionContract(t, "run-control-survival")
	report := ReportCompletionToolDef()
	read := toolDefinitionByName(t, "Read")
	write := toolDefinitionByName(t, "Write")

	kept, dropped, err := pruneToolsPreservingRequired(
		[]types.ToolDef{read, write, report},
		"read a file",
		1,
		completionRequiredToolNames(contract),
	)
	if err != nil || dropped != 2 || len(kept) != 1 || kept[0].Name != reportCompletionToolName {
		t.Fatalf("required-aware budget = names:%v dropped:%d err:%v", toolDefNames(kept), dropped, err)
	}
	planTools := filterReadOnlyTools([]types.ToolDef{write, report})
	if len(planTools) != 1 || planTools[0].Name != reportCompletionToolName {
		t.Fatalf("plan-mode tools = %v", toolDefNames(planTools))
	}
	collapsed, err := pinCompletionControlTool(nil, contract)
	if err != nil || len(collapsed) != 1 || collapsed[0].Name != reportCompletionToolName {
		t.Fatalf("collapsed control tools = %v, err=%v", toolDefNames(collapsed), err)
	}

	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:            "context-required-tool",
		ContextWindow: 10_000,
		OutputReserve: 1_000,
	})
	estimator := TokenEstimatorFunc(func(input ContextEstimateRequest) TokenEstimate {
		inputTokens := 100
		if len(input.Request.Tools) > 1 {
			inputTokens = 9_000
		}
		return TokenEstimate{InputTokens: inputTokens, Source: "required-tool-test", Confidence: "exact"}
	})
	planned, err := PlanContextRequest(ContextPlanningRequest{
		Profile:           profile,
		Model:             "model",
		System:            ContextSystemSections{CorePrefix: "system"},
		Messages:          []types.Message{{Role: "user", Content: mustJSON("read")}},
		Tools:             []types.ToolDef{read, report},
		RequiredToolNames: []string{reportCompletionToolName},
		MaxTokens:         1_000,
		Estimator:         estimator,
	})
	if err != nil || !planned.Plan.Fits || len(planned.Tools) != 1 || planned.Tools[0].Name != reportCompletionToolName {
		t.Fatalf("context required tool = tools:%v plan:%#v err:%v", toolDefNames(planned.Tools), planned.Plan, err)
	}
}

func TestCompletionContractCompositionIsStrictOnly(t *testing.T) {
	anchor := completionLoopAnchor(t, "criterion")
	for _, mode := range []harness.PlanAnchorMode{harness.PlanAnchorOff, harness.PlanAnchorCompact} {
		profile := harness.MustResolveProfile(harness.ProfileSpec{
			ID:             "non-strict-" + string(mode),
			ContextWindow:  32_768,
			OutputReserve:  4_096,
			PlanAnchorMode: mode,
		})
		contract, err := newRunCompletionContract(profile, &anchor, "run-non-strict", nil)
		if err != nil || contract != nil {
			t.Fatalf("mode %q contract = %#v, err=%v; want nil", mode, contract, err)
		}
		tools, err := pinCompletionControlTool([]types.ToolDef{toolDefinitionByName(t, "Read")}, contract)
		if err != nil || requestHasTool(&types.MessagesRequest{Tools: tools}, reportCompletionToolName) {
			t.Fatalf("mode %q advertised ReportCompletion: %v, err=%v", mode, toolDefNames(tools), err)
		}
	}
	strict := strictCompletionProfile("strict-composition", harness.ToolRoutingDirect)
	contract, err := newRunCompletionContract(strict, &anchor, "run-strict", nil)
	if err != nil || contract == nil {
		t.Fatalf("strict contract = %#v, err=%v", contract, err)
	}
}

func TestRunLoopUsesHarnessIterationAndErrorRoundLimits(t *testing.T) {
	for _, test := range []struct {
		name           string
		maxIterations  int
		maxErrorRounds int
		path           string
		wantCalls      int
		wantError      string
	}{
		{name: "iteration limit", maxIterations: 2, maxErrorRounds: 3, path: "proof.txt", wantCalls: 2, wantError: "Max iterations reached"},
		{name: "error round limit", maxIterations: 8, maxErrorRounds: 1, path: "missing.txt", wantCalls: 1, wantError: "Stopped after 1 consecutive failed tool rounds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateEvidenceLoopTest(t)
			workDir := t.TempDir()
			if err := os.WriteFile(workDir+string(os.PathSeparator)+"proof.txt", []byte("proof\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
				toolUseStep("read", "Read", map[string]string{"file_path": test.path}),
			}}
			profile := harness.MustResolveProfile(harness.ProfileSpec{
				ID:              "terminal-limit-" + strings.ReplaceAll(test.name, " ", "-"),
				ContextWindow:   32_768,
				OutputReserve:   4_096,
				MaxIterations:   test.maxIterations,
				MaxErrorRounds:  test.maxErrorRounds,
				PlanAnchorMode:  harness.PlanAnchorOff,
				ToolRouting:     harness.ToolRoutingDirect,
				ReadBeforeWrite: harness.SomeBool(false),
			})
			eventCh := make(chan Event, 64)
			go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
				Role: "user", Content: mustJSON("read the file repeatedly"),
			}}, workDir, RunOptions{
				HarnessProfile:      &profile,
				DisablePlugins:      true,
				PluginDirs:          []string{},
				DisableWorkspaceMCP: true,
				EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
			}, eventCh)
			var events []Event
			for event := range eventCh {
				events = append(events, event)
			}
			if provider.callCount() != test.wantCalls || !eventTextContains(events, test.wantError) {
				t.Fatalf("calls=%d want=%d error=%q events:\n%s", provider.callCount(), test.wantCalls, test.wantError, eventDump(events))
			}
		})
	}
}

func newTestCompletionContract(t *testing.T, runID string) *CompletionContract {
	t.Helper()
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:      runID,
		PlanAnchor: completionLoopAnchor(t, "criterion"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func completionRequestSystemText(request *types.MessagesRequest) string {
	if request == nil {
		return ""
	}
	var blocks []map[string]string
	if json.Unmarshal(request.System, &blocks) != nil || len(blocks) != 1 {
		return ""
	}
	return blocks[0]["text"]
}

func requestHasTool(request *types.MessagesRequest, name string) bool {
	for _, tool := range request.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func requestToolNames(request *types.MessagesRequest) []string {
	if request == nil {
		return nil
	}
	return toolDefNames(request.Tools)
}

func toolDefNames(tools []types.ToolDef) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func toolDefinitionByName(t *testing.T, name string) types.ToolDef {
	t.Helper()
	for _, tool := range AllToolDefs("") {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return types.ToolDef{}
}

func completionDoneEvent(t *testing.T, events []Event) map[string]interface{} {
	t.Helper()
	for _, event := range events {
		if event.Type != "done" {
			continue
		}
		data, ok := event.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("done payload type = %T", event.Data)
		}
		return data
	}
	t.Fatalf("done event missing:\n%s", eventDump(events))
	return nil
}

// TokenEstimatorFunc makes the context-required-tool test use the real planner
// without a second fake estimator type.
type TokenEstimatorFunc func(ContextEstimateRequest) TokenEstimate

func (f TokenEstimatorFunc) EstimateTokens(input ContextEstimateRequest) (TokenEstimate, error) {
	return f(input), nil
}
