package translate

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestTranslatorAssignsDistinctDeterministicParallelToolBlockIndexes(t *testing.T) {
	translator := NewTranslator("gpt-test")
	finish := "tool_calls"
	chunks := []types.OAIStreamChunk{
		{Choices: []types.OAIStreamChoice{{Delta: types.OAIStreamDelta{ToolCalls: []types.OAIStreamToolCall{
			{Index: 0, ID: "call_a", Type: "function", Function: &types.OAIStreamFnCall{Name: "alpha", Arguments: `{"a":`}},
			{Index: 1, ID: "call_b", Type: "function", Function: &types.OAIStreamFnCall{Name: "beta", Arguments: `{"b":`}},
		}}}}},
		{Choices: []types.OAIStreamChoice{{Delta: types.OAIStreamDelta{ToolCalls: []types.OAIStreamToolCall{
			{Index: 1, Function: &types.OAIStreamFnCall{Arguments: `2}`}},
			{Index: 0, Function: &types.OAIStreamFnCall{Arguments: `1}`}},
		}}, FinishReason: &finish}}},
	}

	var starts []int
	deltas := make(map[int]string)
	var stops []int
	for _, chunk := range chunks {
		for _, event := range translator.Translate(chunk) {
			switch event.Type {
			case "content_block_start":
				starts = append(starts, *event.Index)
			case "content_block_delta":
				var delta struct {
					PartialJSON string `json:"partial_json"`
				}
				if err := json.Unmarshal(event.Delta, &delta); err != nil {
					t.Fatal(err)
				}
				deltas[*event.Index] += delta.PartialJSON
			case "content_block_stop":
				stops = append(stops, *event.Index)
			}
		}
	}
	if !reflect.DeepEqual(starts, []int{0, 1}) {
		t.Fatalf("start indexes = %v", starts)
	}
	if !reflect.DeepEqual(deltas, map[int]string{0: `{"a":1}`, 1: `{"b":2}`}) {
		t.Fatalf("deltas = %v", deltas)
	}
	if !reflect.DeepEqual(stops, []int{0, 1}) {
		t.Fatalf("stop indexes = %v", stops)
	}
}

func TestTranslatorEndClosesUnfinishedParallelCallsAndPreservesUsage(t *testing.T) {
	translator := NewTranslator("gpt-test")
	translator.Translate(types.OAIStreamChunk{Choices: []types.OAIStreamChoice{{Delta: types.OAIStreamDelta{ToolCalls: []types.OAIStreamToolCall{
		{Index: 0, ID: "call_a", Type: "function", Function: &types.OAIStreamFnCall{Name: "alpha", Arguments: `{}`}},
		{Index: 1, ID: "call_b", Type: "function", Function: &types.OAIStreamFnCall{Name: "beta", Arguments: `{}`}},
	}}}}})
	translator.Translate(types.OAIStreamChunk{Usage: &types.OAIUsage{PromptTokens: 4, CompletionTokens: 6}})
	events := translator.End()
	var stops []int
	var usage *types.SSEUsage
	var stopReason string
	for _, event := range events {
		if event.Type == "content_block_stop" {
			stops = append(stops, *event.Index)
		}
		if event.Type == "message_delta" {
			usage = event.Usage
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				t.Fatal(err)
			}
			stopReason = delta.StopReason
		}
	}
	if !reflect.DeepEqual(stops, []int{0, 1}) || stopReason != "tool_use" {
		t.Fatalf("stops=%v stopReason=%q", stops, stopReason)
	}
	if usage == nil || usage.InputTokens != 4 || usage.OutputTokens != 6 {
		t.Fatalf("usage = %+v", usage)
	}
}
