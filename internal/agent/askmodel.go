package agent

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// AskModel sends a direct question to the LLM without tools.
// Used for simple Q&A, explanations, translations — no tool execution.
func AskModel(
	ctx context.Context,
	provider types.Provider,
	model string,
	question string,
	systemPrompt string,
	eventCh chan<- Event,
) {
	defer close(eventCh)

	if systemPrompt == "" {
		systemPrompt = "You are a helpful assistant. Answer concisely and accurately."
	}

	userContent, _ := json.Marshal(question)
	req := &types.MessagesRequest{
		Model:  model,
		System: mustJSON([]map[string]string{{"type": "text", "text": systemPrompt}}),
		Messages: []types.Message{
			{Role: "user", Content: userContent},
		},
		MaxTokens: 4096,
		// No tools — pure conversation
	}

	ch, err := provider.StreamMessage(ctx, req, nil)
	if err != nil {
		eventCh <- Event{Type: "error", Data: err.Error()}
		return
	}

	for event := range ch {
		switch event.Type {
		case "content_block_delta":
			var delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			}
			json.Unmarshal(event.Delta, &delta)

			if delta.Type == "thinking_delta" && delta.Thinking != "" {
				eventCh <- Event{Type: "thinking", Data: delta.Thinking}
			}
			if delta.Type == "text_delta" && delta.Text != "" {
				eventCh <- Event{Type: "text", Data: delta.Text}
			}

		case "message_stop":
			eventCh <- Event{Type: "done", Data: nil}
			return
		}
	}

	eventCh <- Event{Type: "done", Data: nil}
}

// AskModelSync is a synchronous version that returns the full response.
func AskModelSync(
	ctx context.Context,
	provider types.Provider,
	model string,
	question string,
) string {
	userContent, _ := json.Marshal(question)
	req := &types.MessagesRequest{
		Model: model,
		Messages: []types.Message{
			{Role: "user", Content: userContent},
		},
		MaxTokens: 4096,
	}

	ch, err := provider.StreamMessage(ctx, req, nil)
	if err != nil {
		log.Printf("[AskModel] Error: %v", err)
		return ""
	}

	var result string
	for event := range ch {
		if event.Type == "content_block_delta" && event.Delta != nil {
			var delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			json.Unmarshal(event.Delta, &delta)
			if delta.Type == "text_delta" {
				result += delta.Text
			}
		}
		if event.Type == "message_stop" {
			break
		}
	}
	return result
}
