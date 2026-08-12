package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// Golden provenance: Gemini GenerateContent Part and function calling shapes.
// https://ai.google.dev/api/generate-content
// https://ai.google.dev/gemini-api/docs/function-calling
func TestBuildGeminiRequestRoundTripsParallelFunctionHistory(t *testing.T) {
	request := &types.MessagesRequest{
		Model:     "gemini-test",
		MaxTokens: 128,
		Messages: []types.Message{
			{Role: "user", Content: json.RawMessage(`"Compare two cities"`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"Checking."},
				{"type":"tool_use","id":"call_a","name":"weather","input":{"city":"Seoul"}},
				{"type":"tool_use","id":"call_b","name":"weather","input":{"city":"Busan"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"call_a","content":"18 C"},
				{"type":"tool_result","tool_use_id":"call_b","content":{"temperature":21}}
			]`)},
		},
	}

	built, err := buildGeminiRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Contents []struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Contents) != 3 || wire.Contents[1].Role != "model" || wire.Contents[2].Role != "user" {
		t.Fatalf("contents = %+v", wire.Contents)
	}
	functionCalls := wire.Contents[1].Parts[1:]
	if got := functionCalls[0]["functionCall"].(map[string]any)["id"]; got != "call_a" {
		t.Fatalf("first function call id = %v", got)
	}
	if got := functionCalls[1]["functionCall"].(map[string]any)["id"]; got != "call_b" {
		t.Fatalf("second function call id = %v", got)
	}
	responses := wire.Contents[2].Parts
	if got := responses[0]["functionResponse"].(map[string]any)["id"]; got != "call_a" {
		t.Fatalf("first function response id = %v", got)
	}
	if got := responses[1]["functionResponse"].(map[string]any)["id"]; got != "call_b" {
		t.Fatalf("second function response id = %v", got)
	}
	if got := responses[0]["functionResponse"].(map[string]any)["name"]; got != "weather" {
		t.Fatalf("function response name = %v", got)
	}
}

func TestGeminiAuthenticationUsesHeaderNotCredentialURL(t *testing.T) {
	const apiKey = "test-secret-key"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("key"); got != "" {
			t.Fatalf("credential leaked into URL query")
		}
		if got := request.Header.Get("x-goog-api-key"); got != apiKey {
			t.Fatalf("x-goog-api-key header = %q", got)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	provider := &GeminiProvider{apiKey: apiKey, baseURL: upstream.URL}
	_, err := provider.StreamMessage(context.Background(), &types.MessagesRequest{
		Model: "gemini-test", MaxTokens: 1,
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}, &types.StreamOptions{})
	if err == nil {
		t.Fatal("expected upstream status error")
	}
}

func TestBuildGeminiRequestRejectsUnknownAndDuplicateCallIDs(t *testing.T) {
	tests := []struct {
		name     string
		messages []types.Message
	}{
		{
			name: "unknown result",
			messages: []types.Message{{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"missing","content":"no"}
			]`)}},
		},
		{
			name: "duplicate calls",
			messages: []types.Message{{Role: "assistant", Content: json.RawMessage(`[
				{"type":"tool_use","id":"same","name":"one","input":{}},
				{"type":"tool_use","id":"same","name":"two","input":{}}
			]`)}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildGeminiRequest(&types.MessagesRequest{Model: "gemini-test", MaxTokens: 1, Messages: test.messages})
			if err == nil {
				t.Fatal("invalid Gemini history was accepted")
			}
		})
	}
}
