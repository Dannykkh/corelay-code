package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// Chat request/response fields are based only on OpenAI's public REST
// reference and function-calling guide. Unsupported semantic fields are
// rejected instead of being silently discarded.
// https://developers.openai.com/api/reference/resources/chat
// https://developers.openai.com/api/docs/guides/function-calling
type ChatAdapter struct{}

func NewChatAdapter() *ChatAdapter { return &ChatAdapter{} }

func (*ChatAdapter) Name() Name { return OpenAIChat }

type chatRequestWire struct {
	Model               string            `json:"model"`
	Messages            []chatMessageWire `json:"messages"`
	Tools               []chatToolWire    `json:"tools,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage,omitempty"`
	} `json:"stream_options,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	N                 *int            `json:"n,omitempty"`
	Stop              json.RawMessage `json:"stop,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
}

type chatMessageWire struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatToolWire struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"function"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (*ChatAdapter) Decode(reader io.Reader) (*Request, error) {
	var wire chatRequestWire
	if err := decodeStrictBoundedFor(OpenAIChat, reader, &wire); err != nil {
		return nil, err
	}
	if wire.N != nil && *wire.N != 1 {
		return nil, NewError(400, "unsupported_parameter", "n must be 1")
	}
	if len(wire.Stop) > 0 || wire.TopP != nil {
		return nil, NewError(400, "unsupported_parameter", "stop and top_p are not supported by this adapter")
	}
	if wire.ParallelToolCalls != nil && !*wire.ParallelToolCalls {
		return nil, NewError(400, "unsupported_parameter", "parallel_tool_calls=false cannot be preserved by the canonical request")
	}
	maxTokens, err := chooseTokenLimit(wire.MaxTokens, wire.MaxCompletionTokens)
	if err != nil {
		return nil, err
	}
	canonical := &types.MessagesRequest{
		Model: wire.Model, MaxTokens: maxTokens, Temperature: wire.Temperature,
	}
	canonical.ToolChoice, err = openAIToolChoiceToCanonical(wire.ToolChoice, true)
	if err != nil {
		return nil, err
	}
	stream := wire.Stream
	canonical.Stream = &stream

	if len(wire.Messages) == 0 || len(wire.Messages) > MaxMessages {
		return nil, NewError(400, "invalid_messages", "messages count is outside the protocol limit")
	}
	if len(wire.Tools) > MaxTools {
		return nil, NewError(400, "invalid_tools", "tools count exceeds the protocol limit")
	}
	toolNames := make(map[string]struct{}, len(wire.Tools))
	for _, tool := range wire.Tools {
		if tool.Type != "function" {
			return nil, NewError(400, "unsupported_tool", "only function tools are supported")
		}
		if err := requireString("tool function name", tool.Function.Name); err != nil {
			return nil, err
		}
		if _, duplicate := toolNames[tool.Function.Name]; duplicate {
			return nil, NewError(400, "duplicate_tool", "tool function names must be unique")
		}
		if tool.Function.Strict != nil {
			return nil, NewError(400, "unsupported_parameter", "function strict mode cannot be preserved by the canonical request")
		}
		toolNames[tool.Function.Name] = struct{}{}
		if err := requireJSONObject(tool.Function.Parameters, "tool parameters"); err != nil {
			return nil, err
		}
		canonical.Tools = append(canonical.Tools, types.ToolDef{
			Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: cloneRaw(tool.Function.Parameters),
		})
	}

	calls := make(map[string]string)
	results := make(map[string]struct{})
	var systemParts []types.ContentBlockParam
	conversationStarted := false
	for _, message := range wire.Messages {
		if message.Name != "" {
			return nil, NewError(400, "unsupported_parameter", "message name cannot be preserved by the canonical request")
		}
		switch message.Role {
		case "system", "developer":
			if conversationStarted {
				return nil, NewError(400, "unsupported_message_order", "system and developer messages are only supported before conversation messages")
			}
			text, err := chatTextContent(message.Content)
			if err != nil {
				return nil, err
			}
			systemParts = append(systemParts, types.ContentBlockParam{Type: "text", Text: text})
		case "user":
			conversationStarted = true
			blocks, err := chatUserContent(message.Content)
			if err != nil {
				return nil, err
			}
			appendCanonicalBlocks(canonical, "user", blocks)
		case "assistant":
			conversationStarted = true
			blocks, err := chatAssistantContent(message.Content)
			if err != nil {
				return nil, err
			}
			for _, call := range message.ToolCalls {
				if call.Type != "function" || !validBoundedWireString(call.ID, 512) || !validBoundedWireString(call.Function.Name, 512) {
					return nil, NewError(400, "invalid_tool_call", "assistant tool calls require an id, type=function, and name")
				}
				if _, duplicate := calls[call.ID]; duplicate {
					return nil, NewError(400, "duplicate_tool_call_id", "tool call ids must be unique")
				}
				if len(call.Function.Arguments) > MaxStringBytes {
					return nil, NewError(400, "invalid_tool_call", "tool call arguments exceed the protocol limit")
				}
				if err := requireJSONObject([]byte(call.Function.Arguments), "tool call arguments"); err != nil {
					return nil, err
				}
				calls[call.ID] = call.Function.Name
				blocks = append(blocks, types.ContentBlockParam{
					Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments),
				})
			}
			if len(blocks) == 0 {
				return nil, NewError(400, "invalid_message_content", "assistant message must contain content or tool calls")
			}
			appendCanonicalBlocks(canonical, "assistant", blocks)
		case "tool":
			conversationStarted = true
			if !validBoundedWireString(message.ToolCallID, 512) {
				return nil, NewError(400, "invalid_tool_result", "tool message requires tool_call_id")
			}
			if _, known := calls[message.ToolCallID]; !known {
				return nil, NewError(400, "unknown_tool_call_id", "tool message references an unknown tool call")
			}
			if _, duplicate := results[message.ToolCallID]; duplicate {
				return nil, NewError(400, "duplicate_tool_result", "tool call result may appear only once")
			}
			text, err := chatTextContent(message.Content)
			if err != nil {
				return nil, err
			}
			results[message.ToolCallID] = struct{}{}
			appendCanonicalBlocks(canonical, "user", []types.ContentBlockParam{{
				Type: "tool_result", ToolUseID: message.ToolCallID, Content: mustRaw(text),
			}})
		default:
			return nil, NewError(400, "invalid_message_role", "message role is not supported")
		}
	}
	for id := range calls {
		if _, ok := results[id]; !ok {
			return nil, NewError(400, "missing_tool_result", "every historical tool call requires one tool result")
		}
	}
	if len(systemParts) > 0 {
		canonical.System = mustRaw(systemParts)
	}
	if err := validateNamedToolChoice(canonical.ToolChoice, toolNames); err != nil {
		return nil, err
	}
	if err := validateCanonicalRequest(canonical); err != nil {
		return nil, err
	}
	includeUsage := wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage
	return &Request{Messages: canonical, Stream: wire.Stream, IncludeUsage: includeUsage}, nil
}

func chooseTokenLimit(legacy, current *int) (int, error) {
	if legacy != nil && current != nil && *legacy != *current {
		return 0, NewError(400, "conflicting_parameters", "max_tokens and max_completion_tokens disagree")
	}
	limit := 0
	if current != nil {
		limit = *current
	} else if legacy != nil {
		limit = *legacy
	}
	if limit <= 0 {
		return 0, NewError(400, "invalid_max_tokens", "max_completion_tokens or max_tokens must be greater than zero")
	}
	return limit, nil
}

func chatTextContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", NewError(400, "invalid_message_content", "message content is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if len(text) > MaxStringBytes {
			return "", NewError(400, "invalid_message_content", "message content exceeds the protocol limit")
		}
		return text, nil
	}
	var rawParts []json.RawMessage
	if err := decodeRawArray(raw, &rawParts); err != nil || len(rawParts) == 0 {
		return "", NewError(400, "invalid_message_content", "message content must be text")
	}
	var builder strings.Builder
	for _, rawPart := range rawParts {
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := decodeRawStrict(rawPart, &part); err != nil {
			return "", err
		}
		if part.Type != "text" {
			return "", NewError(400, "unsupported_content_part", "only text content parts are supported for this role")
		}
		builder.WriteString(part.Text)
		if builder.Len() > MaxStringBytes {
			return "", NewError(400, "invalid_message_content", "message content exceeds the protocol limit")
		}
	}
	return builder.String(), nil
}

func chatUserContent(raw json.RawMessage) ([]types.ContentBlockParam, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if len(text) > MaxStringBytes {
			return nil, NewError(400, "invalid_message_content", "message content exceeds the protocol limit")
		}
		return []types.ContentBlockParam{{Type: "text", Text: text}}, nil
	}
	var rawParts []json.RawMessage
	if err := decodeRawArray(raw, &rawParts); err != nil || len(rawParts) == 0 {
		return nil, NewError(400, "invalid_message_content", "user content is invalid")
	}
	blocks := make([]types.ContentBlockParam, 0, len(rawParts))
	for _, rawPart := range rawParts {
		var part struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL *struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			} `json:"image_url,omitempty"`
		}
		if err := decodeRawStrict(rawPart, &part); err != nil {
			return nil, err
		}
		switch part.Type {
		case "text":
			blocks = append(blocks, types.ContentBlockParam{Type: "text", Text: part.Text})
		case "image_url":
			if part.ImageURL == nil {
				return nil, NewError(400, "invalid_image", "image_url content is missing its url")
			}
			if part.ImageURL.Detail != "" {
				return nil, NewError(400, "unsupported_parameter", "image detail cannot be preserved by the canonical request")
			}
			mediaType, data, err := decodeDataURL(part.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, types.ContentBlockParam{Type: "image", Source: &types.MediaSource{Type: "base64", MediaType: mediaType, Data: data}})
		default:
			return nil, NewError(400, "unsupported_content_part", "user content part is not supported")
		}
	}
	return blocks, nil
}

func chatAssistantContent(raw json.RawMessage) ([]types.ContentBlockParam, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	text, err := chatTextContent(raw)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	return []types.ContentBlockParam{{Type: "text", Text: text}}, nil
}

func appendCanonicalBlocks(request *types.MessagesRequest, role string, blocks []types.ContentBlockParam) {
	if len(blocks) == 0 {
		return
	}
	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == role {
		var current []types.ContentBlockParam
		if json.Unmarshal(request.Messages[len(request.Messages)-1].Content, &current) == nil {
			current = append(current, blocks...)
			request.Messages[len(request.Messages)-1].Content = mustRaw(current)
			return
		}
	}
	request.Messages = append(request.Messages, types.Message{Role: role, Content: mustRaw(blocks)})
}

func decodeDataURL(value string) (string, string, error) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", NewError(400, "unsupported_image_url", "only base64 data image URLs are supported")
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 || !strings.HasSuffix(value[:comma], ";base64") {
		return "", "", NewError(400, "invalid_image_url", "image URL must be base64 encoded")
	}
	mediaType := strings.TrimSuffix(strings.TrimPrefix(value[:comma], "data:"), ";base64")
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", "", NewError(400, "invalid_image_url", "data URL image media type is not supported")
	}
	data := value[comma+1:]
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) > MaxStringBytes*4 {
		return "", "", NewError(400, "invalid_image_url", "image data is invalid or exceeds the protocol limit")
	}
	return mediaType, data, nil
}

func requireJSONObject(raw []byte, field string) error {
	if len(raw) == 0 || validateJSONValue(raw, '{') != nil {
		return NewError(400, "invalid_json_object", field+" must be a JSON object")
	}
	return nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

func validateNamedToolChoice(raw json.RawMessage, tools map[string]struct{}) error {
	if len(raw) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil || choice.Type != "tool" {
		return nil
	}
	if _, ok := tools[choice.Name]; !ok {
		return NewError(400, "unknown_tool_choice", "named tool_choice does not exist in tools")
	}
	return nil
}

func mustRaw(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func decodeRawArray(raw json.RawMessage, target *[]json.RawMessage) error {
	if err := validateJSONValue(raw, '['); err != nil {
		return NewError(400, "invalid_content_array", "content array contains duplicate keys, excessive nesting, or malformed JSON")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return NewError(400, "invalid_content_array", "content array is malformed")
	}
	return nil
}

func (*ChatAdapter) WriteResponse(ctx context.Context, w http.ResponseWriter, meta ResponseMeta, events <-chan types.SSEEvent, request *Request) (Usage, error) {
	meta, err := ensureResponseMeta(meta, "chatcmpl-")
	if err != nil {
		return Usage{}, err
	}
	if !request.Stream {
		message, err := collectCanonical(ctx, meta, events)
		if err != nil {
			return Usage{}, err
		}
		response := chatCompletionResponse(meta, message)
		if err := writeJSON(w, response); err != nil {
			return message.Usage, err
		}
		return message.Usage, nil
	}
	return writeChatStream(ctx, w, meta, events, request.IncludeUsage)
}

func chatCompletionResponse(meta ResponseMeta, message *canonicalMessage) map[string]any {
	var text strings.Builder
	toolCalls := make([]any, 0)
	for _, block := range message.sortedBlocks() {
		if block.Type == "text" {
			text.WriteString(block.Text)
		} else {
			toolCalls = append(toolCalls, map[string]any{
				"id": block.ID, "type": "function",
				"function": map[string]string{"name": block.Name, "arguments": block.Arguments},
			})
		}
	}
	content := any(text.String())
	if text.Len() == 0 {
		content = nil
	}
	assistant := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		assistant["tool_calls"] = toolCalls
	}
	return map[string]any{
		"id": meta.ID, "object": "chat.completion", "created": meta.Created.Unix(), "model": message.Model,
		"choices": []any{map[string]any{"index": 0, "message": assistant, "finish_reason": canonicalStopToChat(message.StopReason), "logprobs": nil}},
		"usage":   map[string]int{"prompt_tokens": message.Usage.InputTokens, "completion_tokens": message.Usage.OutputTokens, "total_tokens": message.Usage.InputTokens + message.Usage.OutputTokens},
	}
}

func writeChatStream(ctx context.Context, w http.ResponseWriter, meta ResponseMeta, events <-chan types.SSEEvent, includeUsage bool) (Usage, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	message := newCanonicalMessage(meta)
	toolOrdinals := make(map[int]int)
	nextToolOrdinal := 0
	finishReason := "stop"
	writeChunk := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	base := func(choices any) map[string]any {
		return map[string]any{"id": meta.ID, "object": "chat.completion.chunk", "created": meta.Created.Unix(), "model": meta.Model, "choices": choices}
	}
	for {
		select {
		case <-ctx.Done():
			return message.Usage, ctx.Err()
		case event, ok := <-events:
			if !ok {
				if !message.stopped {
					return message.Usage, NewError(502, "upstream_truncated", "upstream ended before message_stop")
				}
				return message.Usage, nil
			}
			if err := message.consume(event); err != nil {
				return message.Usage, err
			}
			switch event.Type {
			case "message_start":
				if err := writeChunk(base([]any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": ""}, "finish_reason": nil}})); err != nil {
					return message.Usage, err
				}
			case "content_block_start":
				block := message.byIndex[*event.Index]
				if block.Type == "tool_use" {
					ordinal := nextToolOrdinal
					nextToolOrdinal++
					toolOrdinals[block.Index] = ordinal
					delta := map[string]any{"tool_calls": []any{map[string]any{
						"index": ordinal, "id": block.ID, "type": "function", "function": map[string]string{"name": block.Name, "arguments": ""},
					}}}
					if err := writeChunk(base([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}})); err != nil {
						return message.Usage, err
					}
				}
			case "content_block_delta":
				block := message.byIndex[*event.Index]
				var deltaWire struct {
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				}
				_ = json.Unmarshal(event.Delta, &deltaWire)
				var delta any
				if block.Type == "text" {
					delta = map[string]string{"content": deltaWire.Text}
				} else {
					delta = map[string]any{"tool_calls": []any{map[string]any{
						"index": toolOrdinals[block.Index], "function": map[string]string{"arguments": deltaWire.PartialJSON},
					}}}
				}
				if err := writeChunk(base([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}})); err != nil {
					return message.Usage, err
				}
			case "message_delta":
				finishReason = canonicalStopToChat(message.StopReason)
			case "message_stop":
				if err := writeChunk(base([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}})); err != nil {
					return message.Usage, err
				}
				if includeUsage {
					chunk := base([]any{})
					chunk["usage"] = map[string]int{"prompt_tokens": message.Usage.InputTokens, "completion_tokens": message.Usage.OutputTokens, "total_tokens": message.Usage.InputTokens + message.Usage.OutputTokens}
					if err := writeChunk(chunk); err != nil {
						return message.Usage, err
					}
				}
				if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
					return message.Usage, err
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}

func (*ChatAdapter) WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": code, "param": nil, "code": code},
	})
}
