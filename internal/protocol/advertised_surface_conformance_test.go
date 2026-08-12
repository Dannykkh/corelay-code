package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type advertisedWireConformance struct {
	name            Name
	adapter         Adapter
	valid           string
	duplicateRoot   string
	lateSystem      string
	duplicateToolID string
	successSentinel string
}

func TestAdvertisedWireCanonicalConformance(t *testing.T) {
	wires := advertisedWireConformanceTable()
	names := make([]Name, 0, len(wires))
	var canonicalBaseline []byte
	for _, wire := range wires {
		names = append(names, wire.name)
		t.Run(string(wire.name), func(t *testing.T) {
			request, err := wire.adapter.Decode(strings.NewReader(wire.valid))
			if err != nil {
				t.Fatalf("decode valid advertised wire: %v", err)
			}
			if !request.Stream || request.Messages == nil {
				t.Fatalf("canonical request = %#v", request)
			}
			canonical := normalizedCanonicalRequest(t, request)
			if canonicalBaseline == nil {
				canonicalBaseline = canonical
			} else if !bytes.Equal(canonical, canonicalBaseline) {
				t.Fatalf("canonical ingress drifted\n got: %s\nwant: %s", canonical, canonicalBaseline)
			}

			writer := newTestResponseWriter()
			usage, err := wire.adapter.WriteResponse(
				context.Background(), writer,
				ResponseMeta{ID: "fixed", Model: "model-test", Created: time.Unix(123, 0)},
				eventChannel(textEvents()), request,
			)
			if err != nil || usage != (Usage{InputTokens: 1, OutputTokens: 1}) {
				t.Fatalf("canonical egress usage=%#v error=%v", usage, err)
			}
			body := string(writer.body)
			if !strings.Contains(body, "ok") || !strings.Contains(body, wire.successSentinel) {
				t.Fatalf("successful egress omitted text or terminal sentinel %q: %s", wire.successSentinel, body)
			}

			assertDecodeFails(t, wire.adapter, "malformed", `{`)
			assertDecodeFails(t, wire.adapter, "duplicate-root", wire.duplicateRoot)
			assertDecodeFails(t, wire.adapter, "late-system", wire.lateSystem)
			assertDecodeFails(t, wire.adapter, "duplicate-tool-id", wire.duplicateToolID)
			_, err = wire.adapter.Decode(strings.NewReader(strings.Repeat("x", MaxRequestBytes+1)))
			assertProtocolCode(t, err, "request_too_large")

			writer = newTestResponseWriter()
			_, err = wire.adapter.WriteResponse(
				context.Background(), writer,
				ResponseMeta{ID: "fixed", Model: "model-test", Created: time.Unix(123, 0)},
				eventChannel(malformedTerminalEvents()), &Request{Stream: true},
			)
			if err == nil {
				t.Fatal("malformed canonical stream was accepted")
			}
			if strings.Contains(string(writer.body), wire.successSentinel) {
				t.Fatalf("failed stream emitted success sentinel %q: %s", wire.successSentinel, writer.body)
			}
		})
	}
	if want := DefaultRegistry().Names(); !reflect.DeepEqual(names, want) {
		t.Fatalf("conformance wires = %v, advertised registry = %v", names, want)
	}
}

func advertisedWireConformanceTable() []advertisedWireConformance {
	return []advertisedWireConformance{
		{
			name:    AnthropicMessages,
			adapter: NewAnthropicAdapter(),
			valid: `{
				"model":"model-test","max_tokens":16,"stream":true,
				"system":[{"type":"text","text":"system"}],
				"tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
				"messages":[
					{"role":"user","content":[{"type":"text","text":"hello"}]},
					{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}
				]
			}`,
			duplicateRoot: `{"model":"one","model":"two","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
			lateSystem: `{"model":"model-test","max_tokens":1,"messages":[
				{"role":"user","content":"hi"},{"role":"system","content":"late"}
			]}`,
			duplicateToolID: `{"model":"model-test","max_tokens":1,"messages":[
				{"role":"assistant","content":[
					{"type":"tool_use","id":"same","name":"one","input":{}},
					{"type":"tool_use","id":"same","name":"two","input":{}}
				]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"same","content":"ok"}]}
			]}`,
			successSentinel: "event: message_stop\n",
		},
		{
			name:    OpenAIChat,
			adapter: NewChatAdapter(),
			valid: `{
				"model":"model-test","max_tokens":16,"stream":true,
				"messages":[
					{"role":"system","content":"system"},
					{"role":"user","content":"hello"},
					{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
					{"role":"tool","tool_call_id":"call_1","content":"result"}
				],
				"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]
			}`,
			duplicateRoot: `{"model":"one","model":"two","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
			lateSystem: `{"model":"model-test","max_tokens":1,"messages":[
				{"role":"user","content":"hi"},{"role":"system","content":"late"}
			]}`,
			duplicateToolID: `{"model":"model-test","max_tokens":1,"messages":[
				{"role":"assistant","tool_calls":[
					{"id":"same","type":"function","function":{"name":"one","arguments":"{}"}},
					{"id":"same","type":"function","function":{"name":"two","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"same","content":"ok"}
			]}`,
			successSentinel: "data: [DONE]",
		},
		{
			name:    OpenAIResponses,
			adapter: NewResponsesAdapter(),
			valid: `{
				"model":"model-test","max_output_tokens":16,"stream":true,"instructions":"system",
				"input":[
					{"type":"message","role":"user","content":"hello"},
					{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"},
					{"type":"function_call_output","call_id":"call_1","output":"result"}
				],
				"tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]
			}`,
			duplicateRoot: `{"model":"one","model":"two","max_output_tokens":1,"input":"hi"}`,
			lateSystem: `{"model":"model-test","max_output_tokens":1,"input":[
				{"type":"message","role":"user","content":"hi"},
				{"type":"message","role":"developer","content":"late"}
			]}`,
			duplicateToolID: `{"model":"model-test","max_output_tokens":1,"input":[
				{"type":"function_call","call_id":"same","name":"one","arguments":"{}"},
				{"type":"function_call","call_id":"same","name":"two","arguments":"{}"},
				{"type":"function_call_output","call_id":"same","output":"ok"}
			]}`,
			successSentinel: `"type":"response.completed"`,
		},
	}
}

func normalizedCanonicalRequest(t *testing.T, request *Request) []byte {
	t.Helper()
	data, err := json.Marshal(request.Messages)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func assertDecodeFails(t *testing.T, adapter Adapter, name, body string) {
	t.Helper()
	if _, err := adapter.Decode(strings.NewReader(body)); err == nil {
		t.Fatalf("%s input was accepted", name)
	}
}

func malformedTerminalEvents() []types.SSEEvent {
	index := 0
	return []types.SSEEvent{
		{Type: "message_start", Message: &types.SSEMessage{ID: "msg_bad", Type: "message", Role: "assistant", Model: "model-test", Content: json.RawMessage(`[]`)}},
		{Type: "content_block_start", Index: &index, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_bad","name":"bad","input":{}}`)},
		{Type: "content_block_delta", Index: &index, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"x\":1,\"x\":2}"}`)},
		{Type: "content_block_stop", Index: &index},
		{Type: "message_stop"},
	}
}
