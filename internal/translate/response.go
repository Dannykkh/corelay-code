package translate

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// Translator converts OpenAI stream chunks into Anthropic SSE events.
type Translator struct {
	model          string
	messageID      string
	nextBlockIndex int
	textIndex      int
	thinkingIndex  int
	textOpen       bool
	thinkingOpen   bool
	toolCalls      map[int]openToolCall // OpenAI index -> canonical block
	inTokens       int
	outTokens      int
}

type openToolCall struct {
	BlockIndex int
	ID         string
}

func NewTranslator(model string) *Translator {
	return &Translator{
		model:     model,
		messageID: generateID("msg_proxy_"),
		toolCalls: make(map[int]openToolCall),
	}
}

// Start yields the message_start event.
func (t *Translator) Start() types.SSEEvent {
	return types.SSEEvent{
		Type: "message_start",
		Message: &types.SSEMessage{
			ID:      t.messageID,
			Type:    "message",
			Role:    "assistant",
			Model:   t.model,
			Content: json.RawMessage(`[]`),
			Usage:   &types.SSEUsage{InputTokens: 0, OutputTokens: 0},
		},
	}
}

// Translate converts one OpenAI chunk into zero or more Anthropic events.
func (t *Translator) Translate(chunk types.OAIStreamChunk) []types.SSEEvent {
	var events []types.SSEEvent

	if chunk.Usage != nil {
		t.inTokens = chunk.Usage.PromptTokens
		t.outTokens = chunk.Usage.CompletionTokens
	}

	if len(chunk.Choices) == 0 {
		return events
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	// ── Reasoning/thinking content ──
	if delta.Reasoning != nil && *delta.Reasoning != "" {
		if !t.thinkingOpen {
			// Close text block if open
			if t.textOpen {
				events = append(events, t.closeTextBlock()...)
			}
			idx := t.nextBlockIndex
			t.nextBlockIndex++
			t.thinkingIndex = idx
			events = append(events, types.SSEEvent{
				Type:         "content_block_start",
				Index:        &idx,
				ContentBlock: mustMarshal(map[string]string{"type": "thinking", "thinking": ""}),
			})
			t.thinkingOpen = true
		}
		idx := t.thinkingIndex
		events = append(events, types.SSEEvent{
			Type:  "content_block_delta",
			Index: &idx,
			Delta: mustMarshal(map[string]string{"type": "thinking_delta", "thinking": *delta.Reasoning}),
		})
	}

	// ── Text content ──
	if delta.Content != nil && *delta.Content != "" {
		// Close thinking block if open (transition from thinking to answer)
		if t.thinkingOpen {
			idx := t.thinkingIndex
			events = append(events, types.SSEEvent{Type: "content_block_stop", Index: &idx})
			t.thinkingOpen = false
		}
		if !t.textOpen {
			idx := t.nextBlockIndex
			t.nextBlockIndex++
			t.textIndex = idx
			events = append(events, types.SSEEvent{
				Type:         "content_block_start",
				Index:        &idx,
				ContentBlock: mustMarshal(map[string]string{"type": "text", "text": ""}),
			})
			t.textOpen = true
		}
		idx := t.textIndex
		events = append(events, types.SSEEvent{
			Type:  "content_block_delta",
			Index: &idx,
			Delta: mustMarshal(map[string]string{"type": "text_delta", "text": *delta.Content}),
		})
	}

	// ── Tool calls ──
	for _, tc := range delta.ToolCalls {
		if t.textOpen {
			events = append(events, t.closeTextBlock()...)
		}

		// New tool call
		if tc.ID != "" && tc.Function != nil && tc.Function.Name != "" {
			if _, exists := t.toolCalls[tc.Index]; !exists {
				idx := t.nextBlockIndex
				t.nextBlockIndex++
				t.toolCalls[tc.Index] = openToolCall{BlockIndex: idx, ID: tc.ID}
				events = append(events, types.SSEEvent{
					Type:  "content_block_start",
					Index: &idx,
					ContentBlock: mustMarshal(map[string]string{
						"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": "",
					}),
				})
			}
		}

		// Argument delta
		if tc.Function != nil && tc.Function.Arguments != "" {
			call, exists := t.toolCalls[tc.Index]
			if !exists {
				continue
			}
			idx := call.BlockIndex
			events = append(events, types.SSEEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: mustMarshal(map[string]string{
					"type": "input_json_delta", "partial_json": tc.Function.Arguments,
				}),
			})
		}
	}

	// ── Finish ──
	if choice.FinishReason != nil {
		events = append(events, t.Finish(*choice.FinishReason)...)
	}

	return events
}

// End produces final events if stream ended without explicit finish.
func (t *Translator) End() []types.SSEEvent {
	if len(t.toolCalls) > 0 {
		return t.Finish("tool_calls")
	}
	var events []types.SSEEvent
	if t.thinkingOpen {
		idx := t.thinkingIndex
		events = append(events, types.SSEEvent{Type: "content_block_stop", Index: &idx})
		t.thinkingOpen = false
	}
	// If only thinking happened with no text, emit empty text block
	// (some thinking models produce reasoning but no content when max_tokens is low)
	if !t.textOpen && t.nextBlockIndex > 0 {
		idx := t.nextBlockIndex
		t.nextBlockIndex++
		events = append(events, types.SSEEvent{
			Type:         "content_block_start",
			Index:        &idx,
			ContentBlock: mustMarshal(map[string]string{"type": "text", "text": ""}),
		})
		events = append(events, types.SSEEvent{
			Type:  "content_block_delta",
			Index: &idx,
			Delta: mustMarshal(map[string]string{"type": "text_delta", "text": "(No text output — only reasoning was produced)"}),
		})
		events = append(events, types.SSEEvent{Type: "content_block_stop", Index: &idx})
	}
	if t.textOpen {
		events = append(events, t.closeTextBlock()...)
	}
	events = append(events, types.SSEEvent{
		Type:  "message_delta",
		Delta: mustMarshal(map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}),
		Usage: &types.SSEUsage{InputTokens: t.inTokens, OutputTokens: t.outTokens},
	})
	events = append(events, types.SSEEvent{Type: "message_stop"})
	return events
}

func (t *Translator) Finish(reason string) []types.SSEEvent {
	var events []types.SSEEvent
	if t.thinkingOpen {
		idx := t.thinkingIndex
		events = append(events, types.SSEEvent{Type: "content_block_stop", Index: &idx})
		t.thinkingOpen = false
	}
	if t.textOpen {
		events = append(events, t.closeTextBlock()...)
	}
	// Close any open tool blocks
	openCalls := make([]openToolCall, 0, len(t.toolCalls))
	for _, call := range t.toolCalls {
		openCalls = append(openCalls, call)
	}
	sort.Slice(openCalls, func(i, j int) bool { return openCalls[i].BlockIndex < openCalls[j].BlockIndex })
	for _, call := range openCalls {
		idx := call.BlockIndex
		events = append(events, types.SSEEvent{Type: "content_block_stop", Index: &idx})
	}
	t.toolCalls = make(map[int]openToolCall)

	stopReason := mapFinishReason(reason)
	events = append(events, types.SSEEvent{
		Type:  "message_delta",
		Delta: mustMarshal(map[string]any{"stop_reason": stopReason, "stop_sequence": nil}),
		Usage: &types.SSEUsage{InputTokens: t.inTokens, OutputTokens: t.outTokens},
	})
	events = append(events, types.SSEEvent{Type: "message_stop"})
	return events
}

func (t *Translator) closeTextBlock() []types.SSEEvent {
	idx := t.textIndex
	t.textOpen = false
	return []types.SSEEvent{{Type: "content_block_stop", Index: &idx}}
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func generateID(prefix string) string {
	b := make([]byte, 18)
	rand.Read(b)
	return fmt.Sprintf("%s%s", prefix, base64.RawURLEncoding.EncodeToString(b))
}
