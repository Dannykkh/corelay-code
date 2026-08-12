package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestDefaultRegistryGoldenBaseline(t *testing.T) {
	want := []Name{AnthropicMessages, OpenAIChat, OpenAIResponses}
	if got := DefaultRegistry().Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry names = %v, want %v", got, want)
	}
}

func TestDecodeRejectsDuplicateKeysAndOversize(t *testing.T) {
	adapter := NewChatAdapter()
	_, err := adapter.Decode(strings.NewReader(`{"model":"one","model":"two","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	assertProtocolCode(t, err, "invalid_json")
	_, err = adapter.Decode(strings.NewReader(`{"model":"safe","Model":"other","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	assertProtocolCode(t, err, "invalid_json")
	_, err = adapter.Decode(strings.NewReader(`{"model":"safe","max_tokens":1,"messages":[{"role":"user","Role":"assistant","content":"hi"}]}`))
	assertProtocolCode(t, err, "invalid_json")

	_, err = adapter.Decode(strings.NewReader(strings.Repeat("x", MaxRequestBytes+1)))
	assertProtocolCode(t, err, "request_too_large")
}

func TestCanonicalStateRejectsMalformedOrderingAndDuplicateToolIDs(t *testing.T) {
	idx0, idx1 := 0, 1
	message := newCanonicalMessage(ResponseMeta{})
	if err := message.consume(types.SSEEvent{Type: "content_block_start", Index: &idx0, ContentBlock: json.RawMessage(`{"type":"text","text":""}`)}); err == nil {
		t.Fatal("event before message_start was accepted")
	}

	message = newCanonicalMessage(ResponseMeta{})
	start := testMessageStart()
	if err := message.consume(start); err != nil {
		t.Fatal(err)
	}
	if err := message.consume(start); err == nil {
		t.Fatal("duplicate message_start was accepted")
	}

	message = newCanonicalMessage(ResponseMeta{})
	if err := message.consume(testMessageStart()); err != nil {
		t.Fatal(err)
	}
	first := types.SSEEvent{Type: "content_block_start", Index: &idx0, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_same","name":"one","input":{}}`)}
	second := types.SSEEvent{Type: "content_block_start", Index: &idx1, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_same","name":"two","input":{}}`)}
	if err := message.consume(first); err != nil {
		t.Fatal(err)
	}
	if err := message.consume(second); err == nil {
		t.Fatal("duplicate tool call id was accepted")
	}
}

func TestCanonicalEmptyInitialToolInputAcceptsIncrementalJSON(t *testing.T) {
	idx := 0
	message := newCanonicalMessage(ResponseMeta{})
	events := []types.SSEEvent{
		testMessageStart(),
		{Type: "content_block_start", Index: &idx, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_1","name":"weather","input":{}}`)},
		{Type: "content_block_delta", Index: &idx, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"city\":\"Seoul\"}"}`)},
		{Type: "content_block_stop", Index: &idx},
	}
	for _, event := range events {
		if err := message.consume(event); err != nil {
			t.Fatalf("consume %s: %v", event.Type, err)
		}
	}
	if got := message.byIndex[0].Arguments; got != `{"city":"Seoul"}` {
		t.Fatalf("arguments = %q", got)
	}
}

func TestWriteResponseHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewChatAdapter().WriteResponse(ctx, newTestResponseWriter(), ResponseMeta{Model: "gpt-test", Created: time.Unix(1, 0)}, make(chan types.SSEEvent), &Request{Stream: false})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestAnthropicStreamingGoldenBaseline(t *testing.T) {
	stream := true
	request, err := NewAnthropicAdapter().Decode(strings.NewReader(`{"model":"claude-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.Stream != stream {
		t.Fatalf("stream = %v", request.Stream)
	}
	w := newTestResponseWriter()
	_, err = NewAnthropicAdapter().WriteResponse(context.Background(), w, ResponseMeta{Model: "claude-test"}, eventChannel(textEvents()), request)
	if err != nil {
		t.Fatal(err)
	}
	body := string(w.body)
	for _, expected := range []string{
		"event: message_start\n", "event: content_block_start\n", "event: content_block_delta\n",
		"event: content_block_stop\n", "event: message_delta\n", "event: message_stop\n",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("Anthropic baseline missing %q: %s", expected, body)
		}
	}
}

func TestAnthropicGoldenPassthroughFieldsRemainAccepted(t *testing.T) {
	body := `{
		"model":"claude-test",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},
		"thinking":{"type":"enabled","budget_tokens":16},
		"betas":["context-1"],
		"metadata":{"user_id":"bounded-user"},
		"speed":"fast"
	}`
	request, err := NewAnthropicAdapter().Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages.System) == 0 || len(request.Messages.Thinking) == 0 || len(request.Messages.Metadata) == 0 {
		t.Fatalf("raw passthrough fields were lost: %+v", request.Messages)
	}
	if !reflect.DeepEqual(request.Messages.Betas, []string{"context-1"}) || request.Messages.Speed != "fast" {
		t.Fatalf("passthrough fields = betas:%v speed:%q", request.Messages.Betas, request.Messages.Speed)
	}
}

func TestAnthropicMalformedHistoryFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown block", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"user","content":[{"type":"mystery"}]}]}`},
		{"orphan result", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"no"}]}]}`},
		{"missing result", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]}]}`},
		{"duplicate call id", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"same","name":"one","input":{}},{"type":"tool_use","id":"same","name":"two","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"same","content":"ok"}]}]}`},
		{"duplicate result", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"one","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"one"},{"type":"tool_result","tool_use_id":"call_1","content":"two"}]}]}`},
		{"malformed system", `{"model":"claude-test","max_tokens":1,"system":[{"type":"image","text":"no"}],"messages":[{"role":"user","content":"hi"}]}`},
		{"malformed tool choice", `{"model":"claude-test","max_tokens":1,"tool_choice":{"type":"tool","name":"missing"},"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewAnthropicAdapter().Decode(strings.NewReader(test.body)); err == nil {
				t.Fatal("malformed Anthropic history was accepted")
			}
		})
	}
}

func textEvents() []types.SSEEvent {
	index := 0
	return []types.SSEEvent{
		{Type: "message_start", Message: &types.SSEMessage{ID: "msg_text", Type: "message", Role: "assistant", Model: "claude-test", Content: json.RawMessage(`[]`), Usage: &types.SSEUsage{InputTokens: 1}}},
		{Type: "content_block_start", Index: &index, ContentBlock: json.RawMessage(`{"type":"text","text":""}`)},
		{Type: "content_block_delta", Index: &index, Delta: json.RawMessage(`{"type":"text_delta","text":"ok"}`)},
		{Type: "content_block_stop", Index: &index},
		{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"end_turn","stop_sequence":null}`), Usage: &types.SSEUsage{OutputTokens: 1}},
		{Type: "message_stop"},
	}
}

func TestChatGoldenRequestConversion(t *testing.T) {
	data, err := os.ReadFile("testdata/chat_parallel_request.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJSONValue(data, 0); err != nil {
		t.Fatalf("fixture JSON validation: %v", err)
	}
	request, err := NewChatAdapter().Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !request.Stream || !request.IncludeUsage || len(request.Messages.Tools) != 1 {
		t.Fatalf("unexpected request flags/tools: %+v", request)
	}
	callIDs := canonicalHistoryToolIDs(t, request.Messages.Messages)
	if !reflect.DeepEqual(callIDs, []string{"call_seoul", "call_busan"}) {
		t.Fatalf("call IDs = %v", callIDs)
	}
}

func TestResponsesGoldenRequestConversion(t *testing.T) {
	data, err := os.ReadFile("testdata/responses_parallel_request.json")
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewResponsesAdapter().Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !request.Stream || len(request.Messages.Tools) != 1 {
		t.Fatalf("unexpected request flags/tools: %+v", request)
	}
	callIDs := canonicalHistoryToolIDs(t, request.Messages.Messages)
	if !reflect.DeepEqual(callIDs, []string{"call_seoul", "call_busan"}) {
		t.Fatalf("call IDs = %v", callIDs)
	}
}

func TestKnownButUnpreservedFieldsFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		adapter Adapter
		body    string
	}{
		{"chat message name", NewChatAdapter(), `{"model":"gpt-test","max_tokens":1,"messages":[{"role":"user","name":"alice","content":"hi"}]}`},
		{"chat strict tool", NewChatAdapter(), `{"model":"gpt-test","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{},"strict":true}}]}`},
		{"responses metadata", NewResponsesAdapter(), `{"model":"gpt-test","max_output_tokens":1,"input":"hi","metadata":{"tenant":"one"}}`},
		{"responses store", NewResponsesAdapter(), `{"model":"gpt-test","max_output_tokens":1,"input":"hi","store":true}`},
		{"chat late system", NewChatAdapter(), `{"model":"gpt-test","max_tokens":1,"messages":[{"role":"user","content":"hi"},{"role":"system","content":"late"}]}`},
		{"responses late developer", NewResponsesAdapter(), `{"model":"gpt-test","max_output_tokens":1,"input":[{"type":"message","role":"user","content":"hi"},{"type":"message","role":"developer","content":"late"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.adapter.Decode(strings.NewReader(test.body)); err == nil {
				t.Fatal("unsupported field was silently accepted")
			}
		})
	}
}

func canonicalHistoryToolIDs(t *testing.T, messages []types.Message) []string {
	t.Helper()
	var ids []string
	for _, message := range messages {
		var blocks []types.ContentBlockParam
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		for _, block := range blocks {
			if block.Type == "tool_use" {
				ids = append(ids, block.ID)
			}
		}
	}
	return ids
}

func assertProtocolCode(t *testing.T, err error, code string) {
	t.Helper()
	var protocolErr *Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != code {
		t.Fatalf("error = %#v, want protocol code %q", err, code)
	}
}

func testMessageStart() types.SSEEvent {
	return types.SSEEvent{Type: "message_start", Message: &types.SSEMessage{ID: "msg_test", Type: "message", Role: "assistant", Model: "gpt-test", Content: json.RawMessage(`[]`), Usage: &types.SSEUsage{}}}
}
