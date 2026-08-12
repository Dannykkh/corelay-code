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

func TestRunChronosRecoversCatalogAliasAndFailsClosedWithoutApproval(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "notes.txt"), []byte("chronos evidence"), 0o600); err != nil {
		t.Fatalf("write Chronos fixture: %v", err)
	}

	provider := &alternateLoopTestProvider{
		name: "remote",
		models: []types.ModelInfo{{
			ID:            "test-model",
			ContextWindow: 16_384,
			MaxOutput:     2_048,
		}},
		steps: []alternateLoopStep{
			{text: `{"tool":"read_file","args":{"file_path":"notes.txt"}}`},
			{
				toolName: "Bash",
				toolID:   "dangerous-1",
				inputChunks: []string{
					`{"command":"rm -rf build"}`,
				},
			},
			{text: "[COMPLETE]"},
		},
	}
	events := make(chan Event, 64)
	cfg := DefaultChronosConfig()
	cfg.MaxCycles = 1
	cfg.MaxIterations = 3
	cfg.TotalTimeout = 5 * time.Second
	// This regression exercises response recovery and approval, not the
	// <=16K two-stage selector. Pin direct routing so selector traffic cannot
	// consume the scripted provider steps.
	cfg.HarnessProfile = directAlternateLoopHarness(t)

	RunChronos(
		context.Background(),
		provider,
		"test-model",
		"Inspect notes and finish safely",
		workDir,
		cfg,
		events,
	)
	var observed []Event
	for event := range events {
		observed = append(observed, event)
	}

	requests := provider.requestsSnapshot()
	if len(requests) != 3 {
		t.Fatalf("provider requests = %d, want 3; events = %#v", len(requests), observed)
	}
	if got := requests[0].MaxTokens; got != 2_048 {
		t.Fatalf("Chronos MaxTokens = %d, want model reserve 2048", got)
	}
	if provider.modelCallCount() != 0 {
		t.Fatalf("Chronos Models() calls = %d, want zero with an explicit HarnessProfile", provider.modelCallCount())
	}

	secondHistory := marshalMessagesForTest(t, requests[1].Messages)
	if !strings.Contains(secondHistory, `"name":"Read"`) ||
		!strings.Contains(secondHistory, "chronos evidence") {
		t.Fatalf("recovered Read was not represented in history: %s", secondHistory)
	}
	finalHistory := marshalMessagesForTest(t, requests[2].Messages)
	if !strings.Contains(finalHistory, "Permission denied") ||
		!strings.Contains(finalHistory, "no approval requester is available") {
		t.Fatalf("dangerous call did not fail closed: %s", finalHistory)
	}
}

func TestRunChronosMaxCyclesEmitsFailedRunModeSnapshot(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model", ContextWindow: 65_536, MaxOutput: 4_096}},
		steps: []alternateLoopStep{
			{text: "one issue remains"},
			{text: "attempted the fix"},
			{text: "verification still fails"},
		},
	}
	cfg := DefaultChronosConfig()
	cfg.MaxCycles = 1
	cfg.MaxIterations = 3
	cfg.TotalTimeout = 5 * time.Second
	cfg.HarnessProfile = directAlternateLoopHarness(t)
	events := make(chan Event, 64)

	RunChronos(context.Background(), provider, "test-model", "repair the failure", t.TempDir(), cfg, events)

	var done map[string]interface{}
	var sawError bool
	for event := range events {
		if event.Type == "error" {
			sawError = true
		}
		if event.Type == "done" {
			done, _ = event.Data.(map[string]interface{})
		}
	}
	if sawError || len(provider.requestsSnapshot()) != 3 || done == nil || done["stopReason"] != "max_cycles" {
		t.Fatalf("requests=%d error=%v done=%#v", len(provider.requestsSnapshot()), sawError, done)
	}
	snapshot, ok := done["runMode"].(RunModeSnapshot)
	if !ok || snapshot.Name != "chronos" || snapshot.StopReason != "max_cycles" {
		t.Fatalf("run-mode snapshot = %#v", done["runMode"])
	}
	state, ok := snapshot.State.(ChronosState)
	if !ok || state.Phase != "failed" || state.Cycle != 1 || len(state.VerifyResults) != 1 {
		t.Fatalf("Chronos state = %#v", snapshot.State)
	}
}

func directAlternateLoopHarness(t *testing.T) *harness.HarnessProfile {
	t.Helper()
	profile, err := harness.ResolveProfile(harness.ProfileSpec{
		ID:            "alternate-loop-test-direct",
		ContextWindow: 16_384,
		OutputReserve: 2_048,
		ToolRouting:   harness.ToolRoutingDirect,
	})
	if err != nil {
		t.Fatalf("resolve direct alternate-loop harness: %v", err)
	}
	return &profile
}

type alternateLoopStep struct {
	text        string
	toolName    string
	toolID      string
	inputChunks []string
}

type alternateLoopTestProvider struct {
	mu         sync.Mutex
	name       string
	models     []types.ModelInfo
	modelCalls int
	steps      []alternateLoopStep
	requests   []*types.MessagesRequest
}

func (p *alternateLoopTestProvider) Name() string        { return p.name }
func (p *alternateLoopTestProvider) DisplayName() string { return p.name }
func (p *alternateLoopTestProvider) Validate() error     { return nil }

func (p *alternateLoopTestProvider) Models() []types.ModelInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelCalls++
	return append([]types.ModelInfo(nil), p.models...)
}

func (p *alternateLoopTestProvider) StreamMessage(
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
	step := alternateLoopStep{text: "done"}
	if index < len(p.steps) {
		step = p.steps[index]
	}
	p.mu.Unlock()

	events := make(chan types.SSEEvent, len(step.inputChunks)+4)
	go func() {
		defer close(events)
		if step.toolName != "" {
			toolID := step.toolID
			if toolID == "" {
				toolID = "tool-1"
			}
			events <- types.SSEEvent{
				Type: "content_block_start",
				ContentBlock: mustJSON(map[string]string{
					"type": "tool_use",
					"id":   toolID,
					"name": step.toolName,
				}),
			}
			for _, chunk := range step.inputChunks {
				events <- types.SSEEvent{
					Type: "content_block_delta",
					Delta: mustJSON(map[string]string{
						"type":         "input_json_delta",
						"partial_json": chunk,
					}),
				}
			}
		} else {
			events <- types.SSEEvent{
				Type: "content_block_delta",
				Delta: mustJSON(map[string]string{
					"type": "text_delta",
					"text": step.text,
				}),
			}
		}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}

func (p *alternateLoopTestProvider) requestsSnapshot() []*types.MessagesRequest {
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

func (p *alternateLoopTestProvider) modelCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.modelCalls
}

func marshalMessagesForTest(t *testing.T, messages []types.Message) string {
	t.Helper()
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(encoded)
}
