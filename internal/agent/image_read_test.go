package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestImageReadPassesValidatedImageBlockToNextProviderRequest(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	imageBytes := imageReadPNGFixture(t)
	if err := os.WriteFile(filepath.Join(workDir, "tiny.png"), imageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &imageReadCaptureProvider{steps: []scriptedLoopStep{
		toolUseStep("image-call-1", "ImageRead", map[string]string{"file_path": "tiny.png"}),
		textStep("I inspected the image."),
	}}
	events := runEvidenceLoop(t, provider, workDir, EvidencePolicyConfig{})

	requests := provider.requestSnapshot()
	if len(requests) < 2 {
		t.Fatalf("provider calls = %d, want ImageRead result in a second request; events:\n%s", len(requests), eventDump(events))
	}
	var foundToolResult, foundImage bool
	for _, message := range requests[1].Messages {
		if message.Role != "user" {
			continue
		}
		var blocks []types.ContentBlockParam
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				foundToolResult = block.ToolUseID == "image-call-1"
				if strings.Contains(string(block.Content), "Base64 length") || strings.Contains(string(block.Content), "base64,") {
					t.Fatalf("ImageRead returned encoded bytes as tool text: %s", block.Content)
				}
			case "image":
				if block.Source == nil || block.Source.MediaType != "image/png" {
					t.Fatalf("ImageRead image source = %+v, want PNG", block.Source)
				}
				decoded, err := base64.StdEncoding.Strict().DecodeString(block.Source.Data)
				if err != nil || !bytes.Equal(decoded, imageBytes) {
					t.Fatalf("ImageRead image bytes differ from the fixture: decoded=%d err=%v", len(decoded), err)
				}
				foundImage = true
			}
		}
	}
	if !foundToolResult || !foundImage {
		t.Fatalf("second provider request missing tool_result=%t image=%t; messages=%s", foundToolResult, foundImage, messagesDump(requests[1].Messages))
	}
	encoded := base64.StdEncoding.EncodeToString(imageBytes)
	if strings.Contains(eventDump(events), encoded) {
		t.Fatal("image bytes leaked into tool events")
	}
}

func TestImageReadIsRejectedBeforeExecutionWhenModelCapabilityIsNotSupported(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		capability types.ImageInputCapability
		message    string
	}{
		{name: "unsupported", capability: types.ImageInputUnsupported, message: "does not support image input"},
		{name: "unknown", capability: types.ImageInputUnknown, message: "is unverified"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			isolateEvidenceLoopTest(t)
			workDir := t.TempDir()
			imageBytes := imageReadPNGFixture(t)
			if err := os.WriteFile(filepath.Join(workDir, "tiny.png"), imageBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			provider := &imageReadCaptureProvider{
				imageInput:    testCase.capability,
				imageInputSet: true,
				steps: []scriptedLoopStep{
					toolUseStep("image-call-unsupported", "ImageRead", map[string]string{"file_path": "tiny.png"}),
					textStep("This response must never be requested."),
				},
			}
			events := runEvidenceLoop(t, provider, workDir, EvidencePolicyConfig{})
			if calls := len(provider.requestSnapshot()); calls != 1 {
				t.Fatalf("provider calls=%d, want the first tool proposal only; events:\n%s", calls, eventDump(events))
			}
			if !eventTextContains(events, testCase.message) {
				t.Fatalf("events do not explain the ImageRead target limitation:\n%s", eventDump(events))
			}
		})
	}
}

func TestRunLoopRejectsImageBeforeProviderCallWhenCapabilityIsUnknown(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	imageBytes := imageReadPNGFixture(t)
	content := mustJSON([]map[string]any{{
		"type": "image",
		"source": map[string]string{
			"type": "base64", "media_type": "image/png",
			"data": base64.StdEncoding.EncodeToString(imageBytes),
		},
	}})
	provider := &imageReadCaptureProvider{
		imageInputSet: true,
		steps:         []scriptedLoopStep{textStep("must not be called")},
	}
	events := runImageReadLoopWithMessages(t, provider, workDir, []types.Message{{Role: "user", Content: content}})
	if calls := len(provider.requestSnapshot()); calls != 0 {
		t.Fatalf("provider calls=%d, want zero; events:\n%s", calls, eventDump(events))
	}
	if !eventTextContains(events, "support for the selected provider/model is unverified") {
		t.Fatalf("events do not explain unknown image capability:\n%s", eventDump(events))
	}
}

func imageReadPNGFixture(t *testing.T) []byte {
	t.Helper()
	fixture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fixture.Set(0, 0, color.RGBA{R: 230, G: 40, B: 80, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func runImageReadLoopWithMessages(t *testing.T, provider types.Provider, workDir string, messages []types.Message) []Event {
	t.Helper()
	eventCh := make(chan Event, 64)
	capabilities := fakeBashCapabilities()
	capabilities.FilesystemIsolation = true
	runner := &fakeBashRunner{name: "image-capability", capabilities: capabilities}
	go RunLoopWithOptions(context.Background(), provider, "fake-model", messages, workDir, RunOptions{
		ResponseLang:  "auto",
		SandboxRunner: runner,
		SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired),
	}, eventCh)
	var events []Event
	for event := range eventCh {
		events = append(events, event)
	}
	return events
}

func messagesDump(messages []types.Message) string {
	encoded, _ := json.Marshal(messages)
	return string(encoded)
}

type imageReadCaptureProvider struct {
	mu            sync.Mutex
	steps         []scriptedLoopStep
	requests      []*types.MessagesRequest
	imageInput    types.ImageInputCapability
	imageInputSet bool
}

func (p *imageReadCaptureProvider) Name() string        { return "image-capture" }
func (p *imageReadCaptureProvider) DisplayName() string { return "Image Capture" }
func (p *imageReadCaptureProvider) Models() []types.ModelInfo {
	capability := p.imageInput
	if !p.imageInputSet {
		capability = types.ImageInputSupported
	}
	return []types.ModelInfo{{ID: "fake-model", ImageInput: capability}}
}
func (p *imageReadCaptureProvider) Validate() error { return nil }

func (p *imageReadCaptureProvider) requestSnapshot() []*types.MessagesRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*types.MessagesRequest(nil), p.requests...)
}

func (p *imageReadCaptureProvider) StreamMessage(_ context.Context, request *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	index := len(p.requests)
	copyRequest := *request
	copyRequest.Messages = append([]types.Message(nil), request.Messages...)
	for i := range copyRequest.Messages {
		copyRequest.Messages[i].Content = append(json.RawMessage(nil), request.Messages[i].Content...)
	}
	p.requests = append(p.requests, &copyRequest)
	step := p.steps[index]
	p.mu.Unlock()

	events := make(chan types.SSEEvent, 8)
	go func() {
		defer close(events)
		if step.text != "" {
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{"type": "text"})}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{"type": "text_delta", "text": step.text})}
			events <- types.SSEEvent{Type: "content_block_stop"}
		}
		if step.toolName != "" {
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: mustJSON(map[string]string{
				"type": "tool_use", "id": step.toolID, "name": step.toolName,
			})}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: mustJSON(map[string]string{
				"type": "input_json_delta", "partial_json": step.toolInput,
			})}
			events <- types.SSEEvent{Type: "content_block_stop"}
			events <- types.SSEEvent{Type: "message_delta", Delta: mustJSON(map[string]string{"stop_reason": "tool_use"})}
			events <- types.SSEEvent{Type: "message_stop"}
			return
		}
		events <- types.SSEEvent{Type: "message_delta", Delta: mustJSON(map[string]string{"stop_reason": "end_turn"})}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}
