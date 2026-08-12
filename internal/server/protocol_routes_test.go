package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type protocolRouteProvider struct {
	stream func(context.Context, *types.MessagesRequest) (<-chan types.SSEEvent, error)
}

func (protocolRouteProvider) Name() string        { return "protocol-test" }
func (protocolRouteProvider) DisplayName() string { return "Protocol Test" }
func (protocolRouteProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{{ID: "model-test", DisplayName: "Model Test"}}
}
func (protocolRouteProvider) Validate() error { return nil }
func (p protocolRouteProvider) StreamMessage(ctx context.Context, request *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	return p.stream(ctx, request)
}

func TestOpenAIProtocolHandlersUseCanonicalProviderKernel(t *testing.T) {
	provider := protocolRouteProvider{stream: func(_ context.Context, request *types.MessagesRequest) (<-chan types.SSEEvent, error) {
		if request.Model != "model-test" || len(request.Messages) != 1 {
			t.Fatalf("canonical request = %+v", request)
		}
		return serverProtocolEvents(), nil
	}}
	server := New(provider, "model-test", 0)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		path    string
		body    string
		object  string
	}{
		{
			name: "chat nonstream", handler: server.handleOpenAIChatCompletions, path: "/v1/chat/completions",
			body:   `{"model":"model-test","max_completion_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			object: "chat.completion",
		},
		{
			name: "responses nonstream", handler: server.handleOpenAIResponses, path: "/v1/responses",
			body:   `{"model":"model-test","max_output_tokens":16,"input":"hi"}`,
			object: "response",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			test.handler(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["object"] != test.object || response["model"] != "model-test" {
				t.Fatalf("response = %v", response)
			}
		})
	}
}

func TestOpenAIProtocolDisconnectCancelsProviderContext(t *testing.T) {
	started := make(chan struct{})
	providerCancelled := make(chan struct{})
	provider := protocolRouteProvider{stream: func(ctx context.Context, _ *types.MessagesRequest) (<-chan types.SSEEvent, error) {
		ch := make(chan types.SSEEvent)
		close(started)
		go func() {
			<-ctx.Done()
			close(providerCancelled)
			close(ch)
		}()
		return ch, nil
	}}
	server := New(provider, "model-test", 0)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(
		`{"model":"model-test","max_completion_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	)).WithContext(ctx)
	recorder := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		server.handleOpenAIChatCompletions(recorder, request)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider was not started")
	}
	cancel()
	select {
	case <-providerCancelled:
	case <-time.After(time.Second):
		t.Fatal("provider context was not cancelled")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("protocol handler did not stop after disconnect")
	}
}

func TestClassifyProtocolWriteErrorDoesNotMislabelServerTimeoutAsClientInput(t *testing.T) {
	tests := []struct {
		name       string
		writeErr   error
		requestErr error
		status     int
		code       string
	}{
		{"idle cancellation", context.Canceled, nil, http.StatusGatewayTimeout, "upstream_timeout"},
		{"upstream deadline", context.DeadlineExceeded, nil, http.StatusGatewayTimeout, "upstream_timeout"},
		{"client disconnect", context.Canceled, context.Canceled, 499, "request_cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, code, _ := classifyProtocolWriteError(test.writeErr, test.requestErr)
			if status != test.status || code != test.code {
				t.Fatalf("status/code = %d/%s, want %d/%s", status, code, test.status, test.code)
			}
		})
	}
}

func TestProtocolRouteOversizeReturns413BeforeProvider(t *testing.T) {
	called := false
	provider := protocolRouteProvider{stream: func(context.Context, *types.MessagesRequest) (<-chan types.SSEEvent, error) {
		called = true
		return serverProtocolEvents(), nil
	}}
	server := New(provider, "model-test", 0)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(make([]byte, (8<<20)+1)))
	server.handleOpenAIChatCompletions(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if called {
		t.Fatal("oversized request reached provider")
	}
}

func serverProtocolEvents() <-chan types.SSEEvent {
	index := 0
	ch := make(chan types.SSEEvent, 6)
	ch <- types.SSEEvent{Type: "message_start", Message: &types.SSEMessage{ID: "msg_server", Type: "message", Role: "assistant", Model: "model-test", Content: json.RawMessage(`[]`), Usage: &types.SSEUsage{InputTokens: 2}}}
	ch <- types.SSEEvent{Type: "content_block_start", Index: &index, ContentBlock: json.RawMessage(`{"type":"text","text":""}`)}
	ch <- types.SSEEvent{Type: "content_block_delta", Index: &index, Delta: json.RawMessage(`{"type":"text_delta","text":"ok"}`)}
	ch <- types.SSEEvent{Type: "content_block_stop", Index: &index}
	ch <- types.SSEEvent{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"end_turn","stop_sequence":null}`), Usage: &types.SSEUsage{OutputTokens: 1}}
	ch <- types.SSEEvent{Type: "message_stop"}
	close(ch)
	return ch
}
