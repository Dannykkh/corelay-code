package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// Responses conversion and event names follow OpenAI's official Responses
// reference. sequence_number and output_index are assigned at this boundary so
// parallel canonical tool blocks cannot alias one another.
// https://developers.openai.com/api/reference/resources/responses
// https://developers.openai.com/api/reference/resources/responses/subresources/input-items
// https://platform.openai.com/docs/api-reference/responses-streaming
type ResponsesAdapter struct{}

func NewResponsesAdapter() *ResponsesAdapter { return &ResponsesAdapter{} }

func (*ResponsesAdapter) Name() Name { return OpenAIResponses }

type responsesRequestWire struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      json.RawMessage   `json:"instructions,omitempty"`
	Tools             []responsesTool   `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Store             *bool             `json:"store,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

func (*ResponsesAdapter) Decode(reader io.Reader) (*Request, error) {
	var wire responsesRequestWire
	if err := decodeStrictBoundedFor(OpenAIResponses, reader, &wire); err != nil {
		return nil, err
	}
	if wire.ParallelToolCalls != nil && !*wire.ParallelToolCalls {
		return nil, NewError(400, "unsupported_parameter", "parallel_tool_calls=false cannot be preserved by the canonical request")
	}
	if len(wire.Metadata) > 0 {
		return nil, NewError(400, "unsupported_parameter", "metadata cannot be preserved by the canonical request")
	}
	if wire.Store != nil && *wire.Store {
		return nil, NewError(400, "unsupported_parameter", "store=true is not supported by this stateless adapter")
	}
	if wire.MaxOutputTokens == nil || *wire.MaxOutputTokens <= 0 {
		return nil, NewError(400, "invalid_max_output_tokens", "max_output_tokens must be greater than zero")
	}
	canonical := &types.MessagesRequest{Model: wire.Model, MaxTokens: *wire.MaxOutputTokens, Temperature: wire.Temperature}
	stream := wire.Stream
	canonical.Stream = &stream
	var err error
	canonical.ToolChoice, err = openAIToolChoiceToCanonical(wire.ToolChoice, false)
	if err != nil {
		return nil, err
	}
	if len(wire.Tools) > MaxTools {
		return nil, NewError(400, "invalid_tools", "tools count exceeds the protocol limit")
	}
	toolNames := make(map[string]struct{}, len(wire.Tools))
	for _, tool := range wire.Tools {
		if tool.Type != "function" {
			return nil, NewError(400, "unsupported_tool", "only function tools are supported")
		}
		if err := requireString("tool function name", tool.Name); err != nil {
			return nil, err
		}
		if _, duplicate := toolNames[tool.Name]; duplicate {
			return nil, NewError(400, "duplicate_tool", "tool function names must be unique")
		}
		if tool.Strict != nil {
			return nil, NewError(400, "unsupported_parameter", "function strict mode cannot be preserved by the canonical request")
		}
		toolNames[tool.Name] = struct{}{}
		if err := requireJSONObject(tool.Parameters, "tool parameters"); err != nil {
			return nil, err
		}
		canonical.Tools = append(canonical.Tools, types.ToolDef{Name: tool.Name, Description: tool.Description, InputSchema: cloneRaw(tool.Parameters)})
	}

	var systemParts []types.ContentBlockParam
	if len(wire.Instructions) > 0 && string(wire.Instructions) != "null" {
		var instructions string
		if err := json.Unmarshal(wire.Instructions, &instructions); err != nil || instructions == "" || len(instructions) > MaxStringBytes {
			return nil, NewError(400, "invalid_instructions", "instructions must be a bounded string")
		}
		systemParts = append(systemParts, types.ContentBlockParam{Type: "text", Text: instructions})
	}

	calls := make(map[string]string)
	results := make(map[string]struct{})
	if err := decodeResponsesInput(wire.Input, canonical, &systemParts, calls, results); err != nil {
		return nil, err
	}
	for id := range calls {
		if _, ok := results[id]; !ok {
			return nil, NewError(400, "missing_function_call_output", "every historical function_call requires one function_call_output")
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
	return &Request{Messages: canonical, Stream: wire.Stream, IncludeUsage: true}, nil
}

func decodeResponsesInput(raw json.RawMessage, canonical *types.MessagesRequest, systemParts *[]types.ContentBlockParam, calls map[string]string, results map[string]struct{}) error {
	if len(raw) == 0 || string(raw) == "null" {
		return NewError(400, "invalid_input", "input is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" || len(text) > MaxStringBytes {
			return NewError(400, "invalid_input", "input string is empty or exceeds the protocol limit")
		}
		appendCanonicalBlocks(canonical, "user", []types.ContentBlockParam{{Type: "text", Text: text}})
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 || len(items) > MaxMessages*2 {
		return NewError(400, "invalid_input", "input must be a string or bounded item array")
	}
	conversationStarted := false
	for _, item := range items {
		var discriminator struct {
			Type string `json:"type"`
		}
		if err := validateJSONValue(item, '{'); err != nil || json.Unmarshal(item, &discriminator) != nil {
			return NewError(400, "invalid_input_item", "input item contains duplicate keys, excessive nesting, or malformed JSON")
		}
		switch discriminator.Type {
		case "message":
			var message struct {
				Type    string          `json:"type"`
				ID      string          `json:"id,omitempty"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Status  string          `json:"status,omitempty"`
			}
			if err := decodeRawStrict(item, &message); err != nil {
				return err
			}
			if message.ID != "" && !validBoundedWireString(message.ID, 512) {
				return NewError(400, "invalid_item_id", "message item id is invalid")
			}
			if message.Status != "" && message.Status != "completed" {
				return NewError(400, "unsupported_item_status", "only completed historical messages are supported")
			}
			if message.Role == "system" || message.Role == "developer" {
				if conversationStarted {
					return NewError(400, "unsupported_message_order", "system and developer messages are only supported before conversation items")
				}
				text, err := responsesMessageText(message.Content)
				if err != nil {
					return err
				}
				*systemParts = append(*systemParts, types.ContentBlockParam{Type: "text", Text: text})
				continue
			}
			if message.Role != "user" && message.Role != "assistant" {
				return NewError(400, "invalid_message_role", "Responses message role is not supported")
			}
			conversationStarted = true
			blocks, err := responsesMessageBlocks(message.Role, message.Content)
			if err != nil {
				return err
			}
			appendCanonicalBlocks(canonical, message.Role, blocks)
		case "function_call":
			conversationStarted = true
			var call struct {
				Type      string `json:"type"`
				ID        string `json:"id,omitempty"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Status    string `json:"status,omitempty"`
			}
			if err := decodeRawStrict(item, &call); err != nil {
				return err
			}
			if call.ID != "" && !validBoundedWireString(call.ID, 512) {
				return NewError(400, "invalid_item_id", "function call item id is invalid")
			}
			if call.Status != "" && call.Status != "completed" {
				return NewError(400, "unsupported_item_status", "only completed historical function calls are supported")
			}
			if !validBoundedWireString(call.CallID, 512) || !validBoundedWireString(call.Name, 512) {
				return NewError(400, "invalid_function_call", "function_call requires call_id and name")
			}
			if _, duplicate := calls[call.CallID]; duplicate {
				return NewError(400, "duplicate_function_call_id", "function call ids must be unique")
			}
			if len(call.Arguments) > MaxStringBytes {
				return NewError(400, "invalid_function_call", "function call arguments exceed the protocol limit")
			}
			if err := requireJSONObject([]byte(call.Arguments), "function call arguments"); err != nil {
				return err
			}
			calls[call.CallID] = call.Name
			appendCanonicalBlocks(canonical, "assistant", []types.ContentBlockParam{{Type: "tool_use", ID: call.CallID, Name: call.Name, Input: json.RawMessage(call.Arguments)}})
		case "function_call_output":
			conversationStarted = true
			var output struct {
				Type   string          `json:"type"`
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
				ID     string          `json:"id,omitempty"`
				Status string          `json:"status,omitempty"`
			}
			if err := decodeRawStrict(item, &output); err != nil {
				return err
			}
			if output.ID != "" && !validBoundedWireString(output.ID, 512) {
				return NewError(400, "invalid_item_id", "function call output item id is invalid")
			}
			if output.Status != "" && output.Status != "completed" {
				return NewError(400, "unsupported_item_status", "only completed historical function outputs are supported")
			}
			if !validBoundedWireString(output.CallID, 512) {
				return NewError(400, "invalid_function_call_output", "function_call_output call_id is invalid")
			}
			if _, known := calls[output.CallID]; !known {
				return NewError(400, "unknown_function_call_id", "function_call_output references an unknown function call")
			}
			if _, duplicate := results[output.CallID]; duplicate {
				return NewError(400, "duplicate_function_call_output", "function call output may appear only once")
			}
			if len(output.Output) == 0 || len(output.Output) > MaxStringBytes || validateJSONValue(output.Output, 0) != nil {
				return NewError(400, "invalid_function_call_output", "function call output is invalid or exceeds the protocol limit")
			}
			results[output.CallID] = struct{}{}
			appendCanonicalBlocks(canonical, "user", []types.ContentBlockParam{{Type: "tool_result", ToolUseID: output.CallID, Content: cloneRaw(output.Output)}})
		default:
			return NewError(400, "unsupported_input_item", "Responses input item type is not supported")
		}
	}
	return nil
}

func responsesMessageText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var rawParts []json.RawMessage
	if err := decodeRawArray(raw, &rawParts); err != nil || len(rawParts) == 0 {
		return "", NewError(400, "invalid_message_content", "Responses message content is invalid")
	}
	var builder strings.Builder
	for _, rawPart := range rawParts {
		var part struct {
			Type        string          `json:"type"`
			Text        string          `json:"text"`
			Annotations json.RawMessage `json:"annotations,omitempty"`
		}
		if err := decodeRawStrict(rawPart, &part); err != nil {
			return "", err
		}
		if part.Type != "input_text" && part.Type != "output_text" {
			return "", NewError(400, "unsupported_content_part", "message content part is not text")
		}
		if len(part.Annotations) > 0 && string(part.Annotations) != "[]" {
			return "", NewError(400, "unsupported_parameter", "message annotations cannot be preserved by the canonical request")
		}
		builder.WriteString(part.Text)
		if builder.Len() > MaxStringBytes {
			return "", NewError(400, "invalid_message_content", "message content exceeds the protocol limit")
		}
	}
	return builder.String(), nil
}

func responsesMessageBlocks(role string, raw json.RawMessage) ([]types.ContentBlockParam, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []types.ContentBlockParam{{Type: "text", Text: text}}, nil
	}
	var rawParts []json.RawMessage
	if err := decodeRawArray(raw, &rawParts); err != nil || len(rawParts) == 0 {
		return nil, NewError(400, "invalid_message_content", "Responses message content is invalid")
	}
	blocks := make([]types.ContentBlockParam, 0, len(rawParts))
	for _, rawPart := range rawParts {
		var part struct {
			Type        string          `json:"type"`
			Text        string          `json:"text,omitempty"`
			ImageURL    string          `json:"image_url,omitempty"`
			FileID      string          `json:"file_id,omitempty"`
			Detail      string          `json:"detail,omitempty"`
			Annotations json.RawMessage `json:"annotations,omitempty"`
		}
		if err := decodeRawStrict(rawPart, &part); err != nil {
			return nil, err
		}
		switch part.Type {
		case "input_text", "output_text":
			if part.ImageURL != "" || part.FileID != "" || part.Detail != "" {
				return nil, NewError(400, "invalid_content_part", "text content contains image fields")
			}
			if len(part.Annotations) > 0 && string(part.Annotations) != "[]" {
				return nil, NewError(400, "unsupported_parameter", "message annotations cannot be preserved by the canonical request")
			}
			blocks = append(blocks, types.ContentBlockParam{Type: "text", Text: part.Text})
		case "input_image":
			if role != "user" {
				return nil, NewError(400, "unsupported_content_part", "input_image is only valid for user messages")
			}
			if part.FileID != "" || part.Detail != "" {
				return nil, NewError(400, "unsupported_parameter", "image file_id and detail cannot be preserved by the canonical request")
			}
			mediaType, data, err := decodeDataURL(part.ImageURL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, types.ContentBlockParam{Type: "image", Source: &types.MediaSource{Type: "base64", MediaType: mediaType, Data: data}})
		default:
			return nil, NewError(400, "unsupported_content_part", "Responses message content part is not supported")
		}
	}
	return blocks, nil
}

func openAIToolChoiceToCanonical(raw json.RawMessage, chat bool) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var simple string
	if json.Unmarshal(raw, &simple) == nil {
		switch simple {
		case "auto", "none":
			return mustRaw(map[string]string{"type": simple}), nil
		case "required":
			return mustRaw(map[string]string{"type": "any"}), nil
		default:
			return nil, NewError(400, "invalid_tool_choice", "tool_choice is not supported")
		}
	}
	if chat {
		var named struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := decodeRawStrict(raw, &named); err != nil || named.Type != "function" || !validBoundedWireString(named.Function.Name, 512) {
			return nil, NewError(400, "invalid_tool_choice", "named Chat tool_choice is invalid")
		}
		return mustRaw(map[string]string{"type": "tool", "name": named.Function.Name}), nil
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := decodeRawStrict(raw, &named); err != nil || named.Type != "function" || !validBoundedWireString(named.Name, 512) {
		return nil, NewError(400, "invalid_tool_choice", "named Responses tool_choice is invalid")
	}
	return mustRaw(map[string]string{"type": "tool", "name": named.Name}), nil
}

func decodeRawStrict(raw json.RawMessage, target any) error {
	if err := validateJSONValue(raw, '{'); err != nil {
		return NewError(400, "invalid_input_item", "input item contains duplicate keys, excessive nesting, or malformed JSON")
	}
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil || rejectFoldedDuplicateKeys(envelope) != nil {
		return NewError(400, "invalid_input_item", "input item contains semantically duplicate fields")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return NewError(400, "invalid_input_item", "input item contains unsupported or malformed fields")
	}
	return nil
}

func (*ResponsesAdapter) WriteResponse(ctx context.Context, w http.ResponseWriter, meta ResponseMeta, events <-chan types.SSEEvent, request *Request) (Usage, error) {
	meta, err := ensureResponseMeta(meta, "resp_")
	if err != nil {
		return Usage{}, err
	}
	if !request.Stream {
		message, err := collectCanonical(ctx, meta, events)
		if err != nil {
			return Usage{}, err
		}
		response := responsesObject(meta, message, "completed")
		response["completed_at"] = meta.Created.Unix()
		if err := writeJSON(w, response); err != nil {
			return message.Usage, err
		}
		return message.Usage, nil
	}
	return writeResponsesStream(ctx, w, meta, events)
}

func responsesObject(meta ResponseMeta, message *canonicalMessage, status string) map[string]any {
	output := make([]any, 0, len(message.Blocks))
	for index, block := range message.sortedBlocks() {
		if block.Type == "text" {
			output = append(output, responseTextItem(responseItemID("msg", meta.ID, index), block.Text, "completed"))
		} else {
			output = append(output, responseFunctionItem(responseItemID("fc", meta.ID, index), block, "completed"))
		}
	}
	return map[string]any{
		"id": meta.ID, "object": "response", "created_at": meta.Created.Unix(), "status": status,
		"error": nil, "incomplete_details": nil, "model": message.Model, "output": output,
		"parallel_tool_calls": true,
		"usage":               map[string]int{"input_tokens": message.Usage.InputTokens, "output_tokens": message.Usage.OutputTokens, "total_tokens": message.Usage.InputTokens + message.Usage.OutputTokens},
	}
}

func responseTextItem(id, text, status string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": status,
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func responseTextItemAdded(id string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
}

func responseFunctionItem(id string, block *canonicalBlock, status string) map[string]any {
	return map[string]any{"id": id, "type": "function_call", "call_id": block.ID, "name": block.Name, "arguments": block.Arguments, "status": status}
}

func responseItemID(prefix, responseID string, index int) string {
	trimmed := strings.TrimPrefix(responseID, "resp_")
	return fmt.Sprintf("%s_%s_%d", prefix, trimmed, index)
}

type responseStreamItem struct {
	outputIndex int
	itemID      string
	block       *canonicalBlock
}

func writeResponsesStream(ctx context.Context, w http.ResponseWriter, meta ResponseMeta, events <-chan types.SSEEvent) (Usage, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	message := newCanonicalMessage(meta)
	items := make(map[int]responseStreamItem)
	nextOutputIndex := 0
	sequence := 0
	writeEvent := func(event map[string]any) error {
		event["sequence_number"] = sequence
		sequence++
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
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
				created := responsesObject(meta, message, "in_progress")
				created["output"] = []any{}
				created["usage"] = nil
				if err := writeEvent(map[string]any{"type": "response.created", "response": created}); err != nil {
					return message.Usage, err
				}
				if err := writeEvent(map[string]any{"type": "response.in_progress", "response": created}); err != nil {
					return message.Usage, err
				}
			case "content_block_start":
				block := message.byIndex[*event.Index]
				item := responseStreamItem{outputIndex: nextOutputIndex, block: block}
				nextOutputIndex++
				if block.Type == "text" {
					item.itemID = responseItemID("msg", meta.ID, item.outputIndex)
					if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": responseTextItemAdded(item.itemID)}); err != nil {
						return message.Usage, err
					}
					if err := writeEvent(map[string]any{"type": "response.content_part.added", "item_id": item.itemID, "output_index": item.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
						return message.Usage, err
					}
				} else {
					item.itemID = responseItemID("fc", meta.ID, item.outputIndex)
					if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": responseFunctionItem(item.itemID, block, "in_progress")}); err != nil {
						return message.Usage, err
					}
				}
				items[block.Index] = item
			case "content_block_delta":
				item := items[*event.Index]
				var delta struct {
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				}
				_ = json.Unmarshal(event.Delta, &delta)
				if item.block.Type == "text" {
					if err := writeEvent(map[string]any{"type": "response.output_text.delta", "item_id": item.itemID, "output_index": item.outputIndex, "content_index": 0, "delta": delta.Text}); err != nil {
						return message.Usage, err
					}
				} else if err := writeEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item.itemID, "output_index": item.outputIndex, "delta": delta.PartialJSON}); err != nil {
					return message.Usage, err
				}
			case "content_block_stop":
				item := items[*event.Index]
				if item.block.Type == "text" {
					if err := writeEvent(map[string]any{"type": "response.output_text.done", "item_id": item.itemID, "output_index": item.outputIndex, "content_index": 0, "text": item.block.Text}); err != nil {
						return message.Usage, err
					}
					if err := writeEvent(map[string]any{"type": "response.content_part.done", "item_id": item.itemID, "output_index": item.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": item.block.Text, "annotations": []any{}}}); err != nil {
						return message.Usage, err
					}
					if err := writeEvent(map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": responseTextItem(item.itemID, item.block.Text, "completed")}); err != nil {
						return message.Usage, err
					}
				} else {
					if err := writeEvent(map[string]any{"type": "response.function_call_arguments.done", "item_id": item.itemID, "output_index": item.outputIndex, "name": item.block.Name, "arguments": item.block.Arguments}); err != nil {
						return message.Usage, err
					}
					if err := writeEvent(map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": responseFunctionItem(item.itemID, item.block, "completed")}); err != nil {
						return message.Usage, err
					}
				}
			case "message_stop":
				completed := responsesObject(meta, message, "completed")
				completed["completed_at"] = meta.Created.Unix()
				if err := writeEvent(map[string]any{"type": "response.completed", "response": completed}); err != nil {
					return message.Usage, err
				}
			}
		}
	}
}

func (*ResponsesAdapter) WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": code, "param": nil, "code": code},
	})
}
