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

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestApplyToolResponsePolicyGolden(t *testing.T) {
	tools := responsePolicyTestTools()
	liquid := `before <|tool_call_start|>[read_file(file_path='notes.txt')]<|tool_call_end|> after`
	tests := []struct {
		name           string
		policy         harness.ResponsePolicy
		visible        string
		reasoning      string
		native         []toolUseBlock
		wantStatus     ToolParseStatus
		wantFormat     ToolCallFormat
		wantCalls      int
		wantVisible    string
		wantCorrection bool
	}{
		{
			name:        "native ignores text envelopes",
			policy:      harness.ResponseNative,
			visible:     `{"name":"Read","arguments":{"file_path":"notes.txt"}}`,
			wantStatus:  ToolParseNotApplicable,
			wantFormat:  ToolCallFormatNative,
			wantVisible: `{"name":"Read","arguments":{"file_path":"notes.txt"}}`,
		},
		{
			name:        "default legacy recovery remains compatible",
			policy:      harness.ResponseNativeWithTextRecovery,
			visible:     `{"tool":"read_file","args":{"file_path":"notes.txt"}}`,
			wantStatus:  ToolParseParsed,
			wantFormat:  ToolCallFormatLegacy,
			wantCalls:   1,
			wantVisible: "",
		},
		{
			name:        "multi-format removes only exact Liquid range",
			policy:      harness.ResponseMultiFormat,
			visible:     liquid,
			wantStatus:  ToolParseParsed,
			wantFormat:  ToolCallFormatLiquid,
			wantCalls:   1,
			wantVisible: "before  after",
		},
		{
			name:        "multi-format uses reasoning only when visible is empty",
			policy:      harness.ResponseMultiFormat,
			visible:     " \n",
			reasoning:   `<|tool_call_start|>[read_file(file_path='notes.txt')]<|tool_call_end|>`,
			wantStatus:  ToolParseParsed,
			wantFormat:  ToolCallFormatLiquid,
			wantCalls:   1,
			wantVisible: "",
		},
		{
			name:        "visible prose suppresses reasoning fallback",
			policy:      harness.ResponseMultiFormat,
			visible:     "final answer",
			reasoning:   `<|tool_call_start|>[read_file(file_path='notes.txt')]<|tool_call_end|>`,
			wantStatus:  ToolParseNotApplicable,
			wantFormat:  ToolCallFormatCascade,
			wantVisible: "final answer",
		},
		{
			name:    "native remains authoritative for dispatcher validation",
			policy:  harness.ResponseMultiFormat,
			visible: `{"name":"Write","arguments":{"file_path":"ignored","content":"ignored"}}`,
			native: []toolUseBlock{{
				ID: "native-invalid-schema", Name: "Read", InputRaw: `{}`,
			}},
			wantStatus:  ToolParseParsed,
			wantFormat:  ToolCallFormatNative,
			wantCalls:   1,
			wantVisible: `{"name":"Write","arguments":{"file_path":"ignored","content":"ignored"}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := applyToolResponsePolicy(test.policy, test.visible, test.reasoning, test.native, tools)
			if decision.Parse.Status != test.wantStatus || decision.Parse.Format != test.wantFormat {
				t.Fatalf("parse=%s/%s reason=%s", decision.Parse.Status, decision.Parse.Format, decision.Parse.Reason)
			}
			if len(decision.Calls) != test.wantCalls || decision.VisibleText != test.wantVisible || decision.NeedsCorrection != test.wantCorrection {
				t.Fatalf("decision calls=%d visible=%q correction=%v", len(decision.Calls), decision.VisibleText, decision.NeedsCorrection)
			}
		})
	}
}

func TestMultiFormatPolicyRejectsCatalogSchemaAndAmbiguity(t *testing.T) {
	tools := responsePolicyTestTools()
	tests := []struct {
		name   string
		text   string
		reason ToolParseReason
	}{
		{name: "unknown exact name", text: `{"name":"read","arguments":{"file_path":"notes.txt"}}`, reason: ToolParseReasonUnknownTool},
		{name: "schema mismatch", text: `{"name":"Read","arguments":{}}`, reason: ToolParseReasonSchema},
		{
			name: "ambiguous formats",
			text: `<tool_call>{"name":"Read","arguments":{"file_path":"notes.txt"}}</tool_call>` + "\n" +
				"```json\n{\"name\":\"Write\",\"arguments\":{\"file_path\":\"out.txt\",\"content\":\"x\"}}\n```",
			reason: ToolParseReasonAmbiguous,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := applyToolResponsePolicy(harness.ResponseMultiFormat, test.text, "", nil, tools)
			if decision.Parse.Status != ToolParseRejected || decision.Parse.Reason != test.reason || !decision.NeedsCorrection || len(decision.Calls) != 0 {
				t.Fatalf("decision=%#v", decision)
			}
		})
	}
}

func TestToolResponseCorrectionCapAndRedaction(t *testing.T) {
	const rawSecret = "do-not-retain-this-secret"
	result := toolParseResult(
		ToolCallFormatHermes,
		ToolParseRejected,
		ToolParseReasonSchema,
		digestToolText(rawSecret),
		nil,
		nil,
	)
	state := newToolResponseCorrectionState()
	for attempt := 1; attempt <= defaultToolResponseCorrectionLimit; attempt++ {
		correction, terminal := state.next(result)
		if terminal != nil || correction == "" || strings.Contains(correction, rawSecret) {
			t.Fatalf("attempt %d correction=%q terminal=%#v", attempt, correction, terminal)
		}
	}
	correction, terminal := state.next(result)
	if correction != "" || terminal == nil || terminal.Attempts != defaultToolResponseCorrectionLimit || strings.Contains(terminal.Error(), rawSecret) {
		t.Fatalf("terminal correction=%q failure=%#v", correction, terminal)
	}
}

func TestParserTraceEventAndSpanAreMetadataOnly(t *testing.T) {
	const secret = "parser-trace-secret"
	result := toolParseResult(ToolCallFormatBareJSON, ToolParseRejected, ToolParseReasonSchema, digestToolText(secret), nil, nil)
	events := make(chan Event, 1)
	recorder := &responseParseRecorder{}
	recordToolParseResult(events, recorder, "parse-1", result)

	event := <-events
	encoded, err := json.Marshal(event.Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || len(recorder.started) != 1 || len(recorder.completed) != 1 {
		t.Fatalf("unsafe trace event=%s recorder=%#v", encoded, recorder)
	}
	for key := range recorder.started[0] {
		switch key {
		case "digest", "format", "status", "reason":
		default:
			t.Fatalf("unexpected trace field %q", key)
		}
	}
}

func TestRunLoopMultiFormatReasoningFallbackDoesNotPersistReasoning(t *testing.T) {
	workDir := responsePolicyWorkDir(t)
	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{
		{reasoning: `REASONING-SENTINEL <|tool_call_start|>[read_file(file_path='notes.txt')]<|tool_call_end|>`},
		{visible: "done"},
	}}
	profile := responsePolicyProfile(harness.ResponseMultiFormat)
	events := make(chan Event, 256)
	RunLoopWithOptions(
		context.Background(),
		provider,
		"test-model",
		[]types.Message{{Role: "user", Content: mustJSON("read notes")}},
		workDir,
		RunOptions{HarnessProfile: &profile},
		events,
	)
	for range events {
	}

	requests := provider.requestsSnapshot()
	if len(requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(requests))
	}
	history := marshalMessagesForTest(t, requests[1].Messages)
	if !strings.Contains(history, `"name":"Read"`) || !strings.Contains(history, "notes evidence") {
		t.Fatalf("recovered tool missing from history: %s", history)
	}
	if strings.Contains(history, "REASONING-SENTINEL") || strings.Contains(history, "tool_call_start") {
		t.Fatalf("raw reasoning persisted in history: %s", history)
	}
}

func TestRunLoopNativePolicyDoesNotRecoverText(t *testing.T) {
	workDir := responsePolicyWorkDir(t)
	leaked := `{"name":"Read","arguments":{"file_path":"notes.txt"}}`
	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{{visible: leaked}}}
	profile := responsePolicyProfile(harness.ResponseNative)
	events := make(chan Event, 256)
	RunLoopWithOptions(
		context.Background(),
		provider,
		"test-model",
		[]types.Message{{Role: "user", Content: mustJSON("answer only")}},
		workDir,
		RunOptions{HarnessProfile: &profile},
		events,
	)
	var visible strings.Builder
	toolStarts := 0
	for event := range events {
		if event.Type == "text" {
			if text, ok := event.Data.(string); ok {
				visible.WriteString(text)
			}
		}
		if event.Type == "tool_start" {
			toolStarts++
		}
	}
	if got := len(provider.requestsSnapshot()); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
	if toolStarts != 0 || visible.String() != leaked {
		t.Fatalf("native policy recovered text: starts=%d visible=%q", toolStarts, visible.String())
	}
}

func TestDefaultResponsePolicyPreservesLegacyRecoveryInMainAndSubAgent(t *testing.T) {
	profile := harness.MustResolveProfile(harness.ProfileSpec{ID: "default-response-policy"})
	if profile.ResponsePolicy() != harness.ResponseNativeWithTextRecovery {
		t.Fatalf("default response policy=%s", profile.ResponsePolicy())
	}

	t.Run("main", func(t *testing.T) {
		workDir := responsePolicyWorkDir(t)
		provider := &responsePolicyTestProvider{steps: []responsePolicyStep{
			{visible: `{"tool":"read_file","args":{"file_path":"notes.txt"}}`},
			{visible: "done"},
		}}
		events := make(chan Event, 256)
		RunLoopWithOptions(
			context.Background(), provider, "test-model",
			[]types.Message{{Role: "user", Content: mustJSON("read")}},
			workDir, RunOptions{HarnessProfile: &profile}, events,
		)
		for range events {
		}
		requests := provider.requestsSnapshot()
		if len(requests) != 2 {
			t.Fatalf("requests=%d, want 2", len(requests))
		}
		history := marshalMessagesForTest(t, requests[1].Messages)
		if !strings.Contains(history, `"name":"Read"`) || !strings.Contains(history, "notes evidence") {
			t.Fatalf("legacy recovery missing: %s", history)
		}
	})

	t.Run("subagent", func(t *testing.T) {
		workDir := responsePolicyWorkDir(t)
		provider := &responsePolicyTestProvider{steps: []responsePolicyStep{
			{visible: `{"tool":"read_file","args":{"file_path":"notes.txt"}}`},
			{visible: "done"},
		}}
		manager := NewSubAgentManagerWithOptions(provider, "test-model", workDir, SubAgentManagerOptions{
			Context: context.Background(), HarnessProfile: &profile,
		})
		task := &SubAgentTask{ID: "default-policy", Name: "legacy", Instruction: "read", Status: "pending"}
		manager.run(task)
		if task.Status != "completed" || task.ToolCalls != 1 {
			t.Fatalf("task=%#v", task)
		}
	})
}

func TestBoundedReasoningBufferKeepsUTF8SafeSuffix(t *testing.T) {
	marker := `<|tool_call_start|>[read_file(file_path='notes.txt')]<|tool_call_end|>`
	var buffer boundedReasoningBuffer
	buffer.Append(strings.Repeat("한", maxRecoveryReasoningBytes) + marker)
	if len(buffer.String()) > maxRecoveryReasoningBytes || !strings.HasSuffix(buffer.String(), marker) || !json.Valid([]byte(`"`+buffer.String()+`"`)) {
		t.Fatalf("bounded buffer bytes=%d suffix=%v", len(buffer.String()), strings.HasSuffix(buffer.String(), marker))
	}
}

func TestRunLoopMultiFormatAmbiguityUsesBoundedCorrectionThenFails(t *testing.T) {
	workDir := responsePolicyWorkDir(t)
	ambiguous := `<tool_call>{"name":"Read","arguments":{"file_path":"notes.txt"}}</tool_call>` + "\n" +
		"```json\n{\"name\":\"Write\",\"arguments\":{\"file_path\":\"out.txt\",\"content\":\"x\"}}\n```"
	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{
		{visible: ambiguous}, {visible: ambiguous}, {visible: ambiguous},
	}}
	profile := responsePolicyProfile(harness.ResponseMultiFormat)
	events := make(chan Event, 256)
	RunLoopWithOptions(
		context.Background(),
		provider,
		"test-model",
		[]types.Message{{Role: "user", Content: mustJSON("do not execute ambiguous calls")}},
		workDir,
		RunOptions{HarnessProfile: &profile},
		events,
	)
	var terminal *ToolResponseFailure
	for event := range events {
		if failure, ok := event.Data.(*ToolResponseFailure); event.Type == "error" && ok {
			terminal = failure
		}
	}

	requests := provider.requestsSnapshot()
	if len(requests) != defaultToolResponseCorrectionLimit+1 {
		t.Fatalf("requests=%d, want %d", len(requests), defaultToolResponseCorrectionLimit+1)
	}
	if terminal == nil || terminal.Code != "ambiguous_tool_call" {
		t.Fatalf("terminal=%#v", terminal)
	}
	for _, request := range requests[1:] {
		history := marshalMessagesForTest(t, request.Messages)
		if strings.Contains(history, "tool_call_start") || strings.Contains(history, `"name":"Write"`) {
			t.Fatalf("poisoned payload persisted in correction history: %s", history)
		}
	}
}

func TestChronosAndSubAgentUseMultiFormatReasoningFallback(t *testing.T) {
	for _, runner := range []struct {
		name string
		run  func(*testing.T, string, *responsePolicyTestProvider, harness.HarnessProfile)
	}{
		{
			name: "chronos",
			run: func(t *testing.T, workDir string, provider *responsePolicyTestProvider, profile harness.HarnessProfile) {
				events := make(chan Event, 256)
				cfg := DefaultChronosConfig()
				cfg.MaxCycles = 1
				cfg.TotalTimeout = 5 * time.Second
				cfg.HarnessProfile = &profile
				RunChronos(context.Background(), provider, "test-model", "read notes", workDir, cfg, events)
				for range events {
				}
			},
		},
		{
			name: "subagent",
			run: func(t *testing.T, workDir string, provider *responsePolicyTestProvider, profile harness.HarnessProfile) {
				manager := NewSubAgentManagerWithOptions(provider, "test-model", workDir, SubAgentManagerOptions{
					Context: context.Background(), HarnessProfile: &profile,
				})
				task := &SubAgentTask{ID: "response-policy", Name: "parser", Instruction: "read notes", Status: "pending"}
				manager.run(task)
				if task.Status != "completed" || task.ToolCalls != 1 {
					t.Fatalf("task status=%s calls=%d result=%q", task.Status, task.ToolCalls, task.Result)
				}
			},
		},
	} {
		t.Run(runner.name, func(t *testing.T) {
			workDir := responsePolicyWorkDir(t)
			provider := &responsePolicyTestProvider{steps: []responsePolicyStep{
				{reasoning: `ALT-REASONING-SENTINEL <|tool_call_start|>[read_file(file_path='notes.txt')]<|tool_call_end|>`},
				{visible: "[COMPLETE]"},
			}}
			profile := responsePolicyProfile(harness.ResponseMultiFormat)
			runner.run(t, workDir, provider, profile)
			requests := provider.requestsSnapshot()
			if len(requests) < 2 {
				t.Fatalf("requests=%d, want at least 2", len(requests))
			}
			history := marshalMessagesForTest(t, requests[1].Messages)
			if !strings.Contains(history, `"name":"Read"`) || !strings.Contains(history, "notes evidence") {
				t.Fatalf("tool recovery missing: %s", history)
			}
			if strings.Contains(history, "ALT-REASONING-SENTINEL") || strings.Contains(history, "tool_call_start") {
				t.Fatalf("reasoning persisted: %s", history)
			}
		})
	}
}

func responsePolicyProfile(policy harness.ResponsePolicy) harness.HarnessProfile {
	return harness.MustResolveProfile(harness.ProfileSpec{
		ID:             "response-policy-test",
		ContextWindow:  32_768,
		OutputReserve:  4_096,
		ResponsePolicy: policy,
	})
}

func responsePolicyTestTools() []types.ToolDef {
	return ToolDefs("")
}

func responsePolicyWorkDir(t *testing.T) string {
	t.Helper()
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "notes.txt"), []byte("notes evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	return workDir
}

type responsePolicyStep struct {
	visible    string
	reasoning  string
	nativeName string
	nativeID   string
	nativeJSON string
}

type responsePolicyTestProvider struct {
	mu       sync.Mutex
	steps    []responsePolicyStep
	requests []*types.MessagesRequest
}

func (p *responsePolicyTestProvider) Name() string        { return "response-policy-test" }
func (p *responsePolicyTestProvider) DisplayName() string { return p.Name() }
func (p *responsePolicyTestProvider) Validate() error     { return nil }
func (p *responsePolicyTestProvider) Models() []types.ModelInfo {
	return nil
}

func (p *responsePolicyTestProvider) StreamMessage(
	_ context.Context,
	request *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	copyRequest := *request
	copyRequest.Messages = append([]types.Message(nil), request.Messages...)
	copyRequest.Tools = append([]types.ToolDef(nil), request.Tools...)
	p.requests = append(p.requests, &copyRequest)
	index := len(p.requests) - 1
	step := responsePolicyStep{visible: "done"}
	if index < len(p.steps) {
		step = p.steps[index]
	}
	p.mu.Unlock()

	events := make(chan types.SSEEvent, 12)
	go func() {
		defer close(events)
		if step.reasoning != "" {
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{"type": "thinking"})}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{"type": "thinking_delta", "thinking": step.reasoning})}
			events <- types.SSEEvent{Type: "content_block_stop"}
		}
		if step.nativeName != "" {
			id := step.nativeID
			if id == "" {
				id = "native-1"
			}
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{
				"type": "tool_use", "id": id, "name": step.nativeName,
			})}
			if step.nativeJSON != "" {
				events <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{
					"type": "input_json_delta", "partial_json": step.nativeJSON,
				})}
			}
			events <- types.SSEEvent{Type: "content_block_stop"}
		}
		if step.visible != "" || step.reasoning == "" && step.nativeName == "" {
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{"type": "text"})}
			if step.visible != "" {
				events <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{"type": "text_delta", "text": step.visible})}
			}
			events <- types.SSEEvent{Type: "content_block_stop"}
		}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}

func (p *responsePolicyTestProvider) requestsSnapshot() []*types.MessagesRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]*types.MessagesRequest, len(p.requests))
	for index, request := range p.requests {
		copyRequest := *request
		copyRequest.Messages = append([]types.Message(nil), request.Messages...)
		copyRequest.Tools = append([]types.ToolDef(nil), request.Tools...)
		result[index] = &copyRequest
	}
	return result
}

type responseParseRecorder struct {
	started   []map[string]string
	completed []map[string]string
}

func (r *responseParseRecorder) RunStarted()                         {}
func (r *responseParseRecorder) ReceiptWritten(string, AgentReceipt) {}
func (r *responseParseRecorder) RunCompleted(RunSummary)             {}
func (r *responseParseRecorder) RunSpanStarted(_ string, _ string, data map[string]string) {
	r.started = append(r.started, data)
}
func (r *responseParseRecorder) RunSpanCompleted(_ string, _ string, data map[string]string) {
	r.completed = append(r.completed, data)
}
