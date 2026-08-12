package protocol

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestChatStreamParallelCallsGolden(t *testing.T) {
	w := newTestResponseWriter()
	usage, err := NewChatAdapter().WriteResponse(
		context.Background(), w,
		ResponseMeta{ID: "chatcmpl-test", Model: "gpt-test", Created: time.Unix(123, 0)},
		eventChannel(parallelToolEvents()),
		&Request{Stream: true, IncludeUsage: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (Usage{InputTokens: 3, OutputTokens: 7}) {
		t.Fatalf("usage = %+v", usage)
	}
	frames, done := openAIDataFrames(t, string(w.body))
	if !done {
		t.Fatal("Chat stream did not terminate with [DONE]")
	}
	starts := make(map[string]int)
	deltas := make(map[int]string)
	finish := ""
	usageFrames := 0
	for _, frame := range frames {
		if rawUsage, ok := frame["usage"]; ok {
			usageFrames++
			var parsed struct {
				CompletionTokens int `json:"completion_tokens"`
			}
			decodeInto(t, rawUsage, &parsed)
			if parsed.CompletionTokens != 7 {
				t.Fatalf("completion tokens = %d", parsed.CompletionTokens)
			}
		}
		choices, _ := frame["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		if value, ok := choice["finish_reason"].(string); ok {
			finish = value
		}
		delta, _ := choice["delta"].(map[string]any)
		toolCalls, _ := delta["tool_calls"].([]any)
		for _, rawCall := range toolCalls {
			call := rawCall.(map[string]any)
			index := int(call["index"].(float64))
			if id, ok := call["id"].(string); ok {
				starts[id] = index
			}
			function := call["function"].(map[string]any)
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				deltas[index] += arguments
			}
		}
	}
	if !reflect.DeepEqual(starts, map[string]int{"call_a": 0, "call_b": 1}) {
		t.Fatalf("tool starts = %v", starts)
	}
	if !reflect.DeepEqual(deltas, map[int]string{0: `{"a":1}`, 1: `{"b":2}`}) {
		t.Fatalf("tool deltas = %v", deltas)
	}
	if finish != "tool_calls" || usageFrames != 1 {
		t.Fatalf("finish=%q usageFrames=%d", finish, usageFrames)
	}
}

func TestResponsesStreamParallelCallsGolden(t *testing.T) {
	w := newTestResponseWriter()
	usage, err := NewResponsesAdapter().WriteResponse(
		context.Background(), w,
		ResponseMeta{ID: "resp_test", Model: "gpt-test", Created: time.Unix(123, 0)},
		eventChannel(parallelToolEvents()),
		&Request{Stream: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (Usage{InputTokens: 3, OutputTokens: 7}) {
		t.Fatalf("usage = %+v", usage)
	}
	frames, done := openAIDataFrames(t, string(w.body))
	if done {
		t.Fatal("Responses stream must not emit Chat's [DONE] sentinel")
	}
	wantTypes := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	var gotTypes []string
	added := make(map[string]int)
	deltaIndexes := make(map[string]int)
	for index, frame := range frames {
		gotTypes = append(gotTypes, frame["type"].(string))
		if sequence := int(frame["sequence_number"].(float64)); sequence != index {
			t.Fatalf("frame %d sequence_number = %d", index, sequence)
		}
		switch frame["type"] {
		case "response.output_item.added":
			item := frame["item"].(map[string]any)
			added[item["call_id"].(string)] = int(frame["output_index"].(float64))
		case "response.function_call_arguments.delta":
			deltaIndexes[frame["delta"].(string)] = int(frame["output_index"].(float64))
		}
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("event types = %v", gotTypes)
	}
	if !reflect.DeepEqual(added, map[string]int{"call_a": 0, "call_b": 1}) {
		t.Fatalf("added output indexes = %v", added)
	}
	if !reflect.DeepEqual(deltaIndexes, map[string]int{`{"a":1}`: 0, `{"b":2}`: 1}) {
		t.Fatalf("delta output indexes = %v", deltaIndexes)
	}
}

func TestNonStreamingEgressPreservesCallsUsageAndFinish(t *testing.T) {
	tests := []struct {
		name    string
		adapter Adapter
		check   func(*testing.T, map[string]any)
	}{
		{
			name:    "chat",
			adapter: NewChatAdapter(),
			check: func(t *testing.T, body map[string]any) {
				choices := body["choices"].([]any)
				choice := choices[0].(map[string]any)
				if choice["finish_reason"] != "tool_calls" {
					t.Fatalf("finish_reason = %v", choice["finish_reason"])
				}
				message := choice["message"].(map[string]any)
				if len(message["tool_calls"].([]any)) != 2 {
					t.Fatalf("tool calls = %v", message["tool_calls"])
				}
			},
		},
		{
			name:    "responses",
			adapter: NewResponsesAdapter(),
			check: func(t *testing.T, body map[string]any) {
				if body["status"] != "completed" || len(body["output"].([]any)) != 2 {
					t.Fatalf("response = %v", body)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := newTestResponseWriter()
			usage, err := test.adapter.WriteResponse(context.Background(), w, ResponseMeta{ID: "fixed", Model: "gpt-test", Created: time.Unix(123, 0)}, eventChannel(parallelToolEvents()), &Request{})
			if err != nil {
				t.Fatal(err)
			}
			if usage != (Usage{InputTokens: 3, OutputTokens: 7}) {
				t.Fatalf("usage = %+v", usage)
			}
			var body map[string]any
			if err := json.Unmarshal(w.body, &body); err != nil {
				t.Fatal(err)
			}
			test.check(t, body)
		})
	}
}

func TestMidstreamInvariantViolationTerminatesWithoutSuccessSentinel(t *testing.T) {
	index := 0
	events := []types.SSEEvent{
		{Type: "message_start", Message: &types.SSEMessage{ID: "msg_bad", Type: "message", Role: "assistant", Model: "gpt-test", Content: json.RawMessage(`[]`)}},
		{Type: "content_block_start", Index: &index, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_bad","name":"bad","input":{}}`)},
		{Type: "content_block_delta", Index: &index, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"x\":1,\"x\":2}"}`)},
		{Type: "content_block_stop", Index: &index},
		{Type: "message_stop"},
	}
	tests := []struct {
		name      string
		adapter   Adapter
		forbidden string
	}{
		{"chat", NewChatAdapter(), "data: [DONE]"},
		{"responses", NewResponsesAdapter(), `"type":"response.completed"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := newTestResponseWriter()
			_, err := test.adapter.WriteResponse(context.Background(), w, ResponseMeta{ID: "fixed", Model: "gpt-test", Created: time.Unix(1, 0)}, eventChannel(events), &Request{Stream: true})
			if err == nil {
				t.Fatal("midstream invariant violation was accepted")
			}
			if strings.Contains(string(w.body), test.forbidden) {
				t.Fatalf("stream reported successful completion: %s", w.body)
			}
		})
	}
}

func parallelToolEvents() []types.SSEEvent {
	indexA, indexB := 5, 9
	return []types.SSEEvent{
		{Type: "message_start", Message: &types.SSEMessage{ID: "msg_upstream", Type: "message", Role: "assistant", Model: "gpt-test", Content: json.RawMessage(`[]`), Usage: &types.SSEUsage{InputTokens: 3}}},
		{Type: "content_block_start", Index: &indexA, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_a","name":"alpha","input":{}}`)},
		{Type: "content_block_start", Index: &indexB, ContentBlock: json.RawMessage(`{"type":"tool_use","id":"call_b","name":"beta","input":{}}`)},
		{Type: "content_block_delta", Index: &indexB, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"b\":2}"}`)},
		{Type: "content_block_delta", Index: &indexA, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"a\":1}"}`)},
		{Type: "content_block_stop", Index: &indexB},
		{Type: "content_block_stop", Index: &indexA},
		{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"tool_use","stop_sequence":null}`), Usage: &types.SSEUsage{OutputTokens: 7}},
		{Type: "message_stop"},
	}
}

func eventChannel(events []types.SSEEvent) <-chan types.SSEEvent {
	ch := make(chan types.SSEEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func openAIDataFrames(t *testing.T, body string) ([]map[string]any, bool) {
	t.Helper()
	var frames []map[string]any
	done := false
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", data, err)
		}
		frames = append(frames, frame)
	}
	return frames, done
}

func decodeInto(t *testing.T, value any, target any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
