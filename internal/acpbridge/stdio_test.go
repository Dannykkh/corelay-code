package acpbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type stdioFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acp.RPCError   `json:"error,omitempty"`
}

type stdioHarness struct {
	input  *io.PipeWriter
	frames <-chan []byte
	done   <-chan error
}

func newStdioHarness(t *testing.T, backend acp.Backend) *stdioHarness {
	t.Helper()
	serverInput, clientOutput := io.Pipe()
	clientInput, serverOutput := io.Pipe()
	connection := acp.NewConnection(serverInput, serverOutput, backend, acp.Options{})
	done := make(chan error, 1)
	go func() {
		done <- connection.Serve(context.Background())
	}()
	frames := make(chan []byte, 64)
	go func() {
		defer close(frames)
		defer clientInput.Close()
		scanner := bufio.NewScanner(clientInput)
		scanner.Buffer(make([]byte, 4096), acp.DefaultMaxFrameBytes+1)
		for scanner.Scan() {
			frames <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	return &stdioHarness{input: clientOutput, frames: frames, done: done}
}

func (h *stdioHarness) send(t *testing.T, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if _, err := h.input.Write(data); err != nil {
		t.Fatalf("stdio write error = %v", err)
	}
}

func (h *stdioHarness) read(t *testing.T) ([]byte, stdioFrame) {
	t.Helper()
	select {
	case data, ok := <-h.frames:
		if !ok {
			t.Fatal("stdio output closed")
		}
		var frame stdioFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("invalid stdio frame %q: %v", data, err)
		}
		return data, frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdio frame")
	}
	return nil, stdioFrame{}
}

func (h *stdioHarness) response(t *testing.T, id string) ([]byte, stdioFrame) {
	t.Helper()
	var observed []byte
	for {
		data, frame := h.read(t)
		observed = append(observed, data...)
		if string(frame.ID) == id && frame.Method == "" {
			return observed, frame
		}
	}
}

func (h *stdioHarness) close(t *testing.T) {
	t.Helper()
	_ = h.input.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Connection.Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not shut down after EOF")
	}
}

func initializeStdio(t *testing.T, harness *stdioHarness) {
	t.Helper()
	harness.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  acp.MethodInitialize,
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
				"terminal": false,
				"session":  map[string]any{"configOptions": map[string]any{}},
			},
		},
	})
	_, frame := harness.response(t, "1")
	if frame.Error != nil {
		t.Fatalf("initialize error = %#v", frame.Error)
	}
}

func newStdioSession(t *testing.T, harness *stdioHarness, workspace string) string {
	t.Helper()
	harness.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  acp.MethodSessionNew,
		"params": map[string]any{
			"cwd":        workspace,
			"mcpServers": []any{},
		},
	})
	_, frame := harness.response(t, "2")
	if frame.Error != nil {
		t.Fatalf("session/new error = %#v", frame.Error)
	}
	var response acp.NewSessionResponse
	if err := json.Unmarshal(frame.Result, &response); err != nil || response.SessionID == "" {
		t.Fatalf("session/new result = %s, error = %v", frame.Result, err)
	}
	return response.SessionID
}

func TestStdioEndToEndPromptPermissionStreamingAndSecretBoundary(t *testing.T) {
	const rawSecret = "raw-permission-secret"
	store := agent.NewSessionStore(t.TempDir())
	runner := RunnerFunc(func(
		ctx context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		options agent.RunOptions,
	) (<-chan agent.Event, error) {
		stream := make(chan agent.Event, 2)
		go func() {
			defer close(stream)
			pending, err := options.ApprovalRequester.Open(approval.Draft{
				SessionID:     options.SessionID,
				RunID:         "run-1",
				ToolName:      "Bash",
				RedactedInput: `{"password":"` + rawSecret + `"}`,
				InputDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			})
			if err != nil {
				stream <- agent.Event{Type: "error", Data: "raw backend error " + rawSecret}
				return
			}
			resolution, err := options.ApprovalRequester.Await(ctx, options.SessionID, pending.ID)
			if err != nil || !resolution.Allowed() {
				stream <- agent.Event{Type: "error", Data: "permission denied " + rawSecret}
				return
			}
			stream <- agent.Event{Type: "text", Data: "allowed Authorization: Bearer " + rawSecret}
			stream <- agent.Event{Type: "done"}
		}()
		return stream, nil
	})
	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a", DisplayName: "Model A"}}},
		DefaultModel: "model-a",
		Store:        store,
		Runner:       runner,
		Version:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newStdioHarness(t, backend)
	initializeStdio(t, harness)
	sessionID := newStdioSession(t, harness, t.TempDir())

	harness.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  acp.MethodSessionPrompt,
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": "run"}},
		},
	})
	var wire strings.Builder
	permissionAnswered := false
	for {
		data, frame := harness.read(t)
		wire.Write(data)
		if frame.Method == acp.MethodSessionRequestPermission {
			if strings.Contains(string(data), rawSecret) || strings.Contains(string(data), "password") {
				t.Fatalf("permission request leaked raw input: %s", data)
			}
			harness.send(t, struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  any             `json:"result"`
			}{
				JSONRPC: "2.0",
				ID:      frame.ID,
				Result: map[string]any{
					"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"},
				},
			})
			permissionAnswered = true
			continue
		}
		if string(frame.ID) == "3" && frame.Method == "" {
			if frame.Error != nil {
				t.Fatalf("prompt error = %#v", frame.Error)
			}
			var response acp.PromptResponse
			if err := json.Unmarshal(frame.Result, &response); err != nil || response.StopReason != acp.StopEndTurn {
				t.Fatalf("prompt result = %s, error = %v", frame.Result, err)
			}
			break
		}
	}
	if !permissionAnswered {
		t.Fatal("permission request was not observed")
	}
	if strings.Contains(wire.String(), rawSecret) || !strings.Contains(wire.String(), "[REDACTED]") {
		t.Fatalf("stdio output secret boundary failed: %s", wire.String())
	}
	harness.close(t)
}

func TestStdioSessionCancelReturnsStableCancelledStopReason(t *testing.T) {
	started := make(chan struct{})
	runner := RunnerFunc(func(
		ctx context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		_ agent.RunOptions,
	) (<-chan agent.Event, error) {
		stream := make(chan agent.Event)
		close(started)
		go func() {
			<-ctx.Done()
			close(stream)
		}()
		return stream, nil
	})
	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a", DisplayName: "Model A"}}},
		DefaultModel: "model-a",
		Store:        agent.NewSessionStore(t.TempDir()),
		Runner:       runner,
		Version:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newStdioHarness(t, backend)
	initializeStdio(t, harness)
	sessionID := newStdioSession(t, harness, t.TempDir())
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": acp.MethodSessionPrompt,
		"params": map[string]any{"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": "wait"}}},
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "method": acp.MethodSessionCancel,
		"params": map[string]string{"sessionId": sessionID},
	})
	_, frame := harness.response(t, "3")
	if frame.Error != nil {
		t.Fatalf("cancelled prompt error = %#v", frame.Error)
	}
	var response acp.PromptResponse
	if err := json.Unmarshal(frame.Result, &response); err != nil || response.StopReason != acp.StopCancelled {
		t.Fatalf("cancelled prompt result = %s, error = %v", frame.Result, err)
	}
	harness.close(t)
}
