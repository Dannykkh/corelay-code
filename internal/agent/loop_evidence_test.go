package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopEvidenceMeasureRecordsWouldBlockReceipt(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_write", "Write", map[string]string{
			"file_path": "src/app.data",
			"content":   "ok\n",
		}),
		textStep("done"),
	}}

	events := runEvidenceLoop(t, provider, workDir, EvidencePolicyConfig{Policy: EvidencePolicyMeasure})
	receipt := readOnlyAgentReceipt(t, configDir)

	if _, err := os.Stat(filepath.Join(workDir, "src", "app.data")); err != nil {
		t.Fatalf("expected src/app.data to be written: %v\nevents:\n%s", err, eventDump(events))
	}
	if receipt.Verification.Status != "not-run" || receipt.Verification.TerminalState != EvidenceTerminalUnverified || receipt.Verification.Gate != EvidenceGateWouldBlock || receipt.Verification.Mode != EvidenceModeDeep {
		t.Fatalf("verification = %+v, want not-run/unverified/would-block/deep", receipt.Verification)
	}
	if len(receipt.EditedFiles) != 1 || filepath.ToSlash(receipt.EditedFiles[0]) != "src/app.data" {
		t.Fatalf("edited files = %+v", receipt.EditedFiles)
	}
	if !eventTextContains(events, "Evidence gate: would-block") {
		t.Fatalf("events did not report would-block gate: %+v", events)
	}
}

func TestRunLoopEvidenceBlockStopsUntilVerificationEvidence(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_write", "Write", map[string]string{
			"file_path": "src/app.data",
			"content":   "ok\n",
		}),
		textStep("done without verification"),
		toolUseStep("toolu_verify", "Bash", map[string]string{
			"command": "echo verify ok",
		}),
		textStep("done with verification"),
	}}

	events := runEvidenceLoop(t, provider, workDir, EvidencePolicyConfig{
		Policy:        EvidencePolicyBlock,
		MaxStopBlocks: 2,
	})
	receipt := readOnlyAgentReceipt(t, configDir)

	if provider.callCount() != 4 {
		t.Fatalf("provider calls = %d, want 4\nevents:\n%s", provider.callCount(), eventDump(events))
	}
	if !eventTextContains(events, "Evidence gate blocked completion") {
		t.Fatalf("events did not report block gate: %+v", events)
	}
	if receipt.Verification.Status != "passed" || receipt.Verification.TerminalState != EvidenceTerminalVerified || receipt.Verification.Source != "tool" || receipt.Verification.Gate != EvidenceGateAllow {
		t.Fatalf("verification = %+v, want passed/verified/tool/allow", receipt.Verification)
	}
	if receipt.Verification.Command != "echo verify ok" || len(receipt.Verification.Evidence) != 1 {
		t.Fatalf("verification evidence not preserved: %+v", receipt.Verification)
	}
}

func isolateEvidenceLoopTest(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	return configDir
}

func runEvidenceLoop(t *testing.T, provider types.Provider, workDir string, policy EvidencePolicyConfig) []Event {
	t.Helper()
	userContent, _ := json.Marshal("크로노스. 완료까지 구현하자.")
	eventCh := make(chan Event, 64)
	runner := &fakeBashRunner{name: "evidence-loop", capabilities: fakeBashCapabilities()}
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{
		{Role: "user", Content: userContent},
	}, workDir, RunOptions{
		ResponseLang:   "auto",
		EvidencePolicy: policy,
		SandboxRunner:  runner,
		SandboxPolicy:  fakeBashPolicy(sandbox.EnforcementRequired),
	}, eventCh)

	var events []Event
	for event := range eventCh {
		events = append(events, event)
	}
	return events
}

func readOnlyAgentReceipt(t *testing.T, configDir string) AgentReceipt {
	t.Helper()
	var receiptPaths []string
	receiptsDir := filepath.Join(configDir, "receipts")
	if err := filepath.WalkDir(receiptsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		receiptPaths = append(receiptPaths, path)
		return nil
	}); err != nil {
		t.Fatalf("walk receipts: %v", err)
	}
	if len(receiptPaths) != 1 {
		t.Fatalf("receipt count = %d, want 1 under %s", len(receiptPaths), receiptsDir)
	}
	data, err := os.ReadFile(receiptPaths[0])
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	var receipt AgentReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	return receipt
}

func eventTextContains(events []Event, needle string) bool {
	for _, event := range events {
		if strings.Contains(toEventString(event.Data), needle) {
			return true
		}
	}
	return false
}

func eventDump(events []Event) string {
	var lines []string
	for _, event := range events {
		lines = append(lines, event.Type+": "+toEventString(event.Data))
	}
	return strings.Join(lines, "\n")
}

func toEventString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

type scriptedLoopStep struct {
	text      string
	toolID    string
	toolName  string
	toolInput string
}

type scriptedLoopProvider struct {
	mu    sync.Mutex
	steps []scriptedLoopStep
	calls int
}

func (p *scriptedLoopProvider) Name() string              { return "scripted" }
func (p *scriptedLoopProvider) DisplayName() string       { return "Scripted" }
func (p *scriptedLoopProvider) Models() []types.ModelInfo { return nil }
func (p *scriptedLoopProvider) Validate() error           { return nil }

func (p *scriptedLoopProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *scriptedLoopProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	p.mu.Unlock()

	if idx >= len(p.steps) {
		idx = len(p.steps) - 1
	}
	step := p.steps[idx]
	ch := make(chan types.SSEEvent, 8)
	go func() {
		defer close(ch)
		if step.text != "" {
			ch <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{"type": "text"})}
			ch <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{
				"type": "text_delta",
				"text": step.text,
			})}
			ch <- types.SSEEvent{Type: "content_block_stop"}
		}
		if step.toolName != "" {
			ch <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{
				"type": "tool_use",
				"id":   step.toolID,
				"name": step.toolName,
			})}
			ch <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{
				"type":         "input_json_delta",
				"partial_json": step.toolInput,
			})}
			ch <- types.SSEEvent{Type: "content_block_stop"}
			ch <- types.SSEEvent{Type: "message_delta", Delta: mustJSON(map[string]string{"stop_reason": "tool_use"})}
			ch <- types.SSEEvent{Type: "message_stop"}
			return
		}
		ch <- types.SSEEvent{Type: "message_delta", Delta: mustJSON(map[string]string{"stop_reason": "end_turn"})}
		ch <- types.SSEEvent{Type: "message_stop"}
	}()
	return ch, nil
}

func toolUseStep(id, name string, input any) scriptedLoopStep {
	data, _ := json.Marshal(input)
	return scriptedLoopStep{toolID: id, toolName: name, toolInput: string(data)}
}

func textStep(text string) scriptedLoopStep {
	return scriptedLoopStep{text: text}
}

func textAndToolUseStep(text, id, name string, input any) scriptedLoopStep {
	step := toolUseStep(id, name, input)
	step.text = text
	return step
}
