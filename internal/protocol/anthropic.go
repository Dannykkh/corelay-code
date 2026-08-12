package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// Anthropic is the golden canonical wire baseline. Event ordering follows the
// official Messages streaming contract:
// https://platform.claude.com/docs/en/build-with-claude/streaming
type AnthropicAdapter struct{}

func NewAnthropicAdapter() *AnthropicAdapter { return &AnthropicAdapter{} }

func (*AnthropicAdapter) Name() Name { return AnthropicMessages }

func (*AnthropicAdapter) Decode(reader io.Reader) (*Request, error) {
	var messageRequest types.MessagesRequest
	if err := decodeStrictBoundedFor(AnthropicMessages, reader, &messageRequest); err != nil {
		return nil, err
	}
	if err := validateCanonicalRequest(&messageRequest); err != nil {
		return nil, err
	}
	streaming := messageRequest.Stream != nil && *messageRequest.Stream
	return &Request{Messages: &messageRequest, Stream: streaming}, nil
}

func (*AnthropicAdapter) WriteResponse(ctx context.Context, w http.ResponseWriter, meta ResponseMeta, events <-chan types.SSEEvent, request *Request) (Usage, error) {
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		message := newCanonicalMessage(meta)
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
				data, err := json.Marshal(event)
				if err != nil {
					return message.Usage, err
				}
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data); err != nil {
					return message.Usage, err
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}

	message, err := collectCanonical(ctx, meta, events)
	if err != nil {
		return Usage{}, err
	}
	content := make([]any, 0, len(message.Blocks))
	for _, block := range message.sortedBlocks() {
		switch block.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		case "tool_use":
			content = append(content, map[string]any{
				"type": "tool_use", "id": block.ID, "name": block.Name, "input": safeJSON(block.Arguments),
			})
		}
	}
	response := map[string]any{
		"id": message.ID, "type": "message", "role": "assistant", "model": message.Model,
		"content": content, "stop_reason": message.StopReason, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": message.Usage.InputTokens, "output_tokens": message.Usage.OutputTokens},
	}
	return message.Usage, writeJSON(w, response)
}

func (*AnthropicAdapter) WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": code, "message": message},
	})
}

func validateCanonicalRequest(request *types.MessagesRequest) error {
	if request == nil {
		return NewError(400, "invalid_request", "request is required")
	}
	if err := requireString("model", request.Model); err != nil {
		return err
	}
	if request.MaxTokens <= 0 {
		return NewError(400, "invalid_request", "max_tokens must be greater than zero")
	}
	if len(request.Messages) == 0 || len(request.Messages) > MaxMessages {
		return NewError(400, "invalid_request", "messages count is outside the protocol limit")
	}
	if len(request.Tools) > MaxTools {
		return NewError(400, "invalid_request", "tools count exceeds the protocol limit")
	}
	toolNames := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		if err := requireString("tool name", tool.Name); err != nil {
			return err
		}
		if _, duplicate := toolNames[tool.Name]; duplicate {
			return NewError(400, "duplicate_tool", "tool names must be unique")
		}
		toolNames[tool.Name] = struct{}{}
		if len(tool.Description) > MaxStringBytes || len(tool.InputSchema) > MaxStringBytes || validateJSONValue(tool.InputSchema, '{') != nil {
			return NewError(400, "invalid_tool_schema", "tool input_schema must be a JSON object")
		}
	}
	if err := validateCanonicalAuxiliary(request, toolNames); err != nil {
		return err
	}
	if err := validateCanonicalHistory(request.Messages); err != nil {
		return err
	}
	return nil
}
