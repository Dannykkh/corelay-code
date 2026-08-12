package protocol

import (
	"encoding/json"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type canonicalCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

func validateCanonicalAuxiliary(request *types.MessagesRequest, toolNames map[string]struct{}) error {
	if len(request.System) > 0 {
		var text string
		if json.Unmarshal(request.System, &text) == nil {
			if len(text) > MaxStringBytes {
				return NewError(400, "invalid_system", "system prompt exceeds the protocol limit")
			}
		} else {
			var blocks []json.RawMessage
			if err := decodeRawArray(request.System, &blocks); err != nil || len(blocks) == 0 {
				return NewError(400, "invalid_system", "system prompt must be a string or nonempty text block array")
			}
			for _, raw := range blocks {
				var block struct {
					Type         string          `json:"type"`
					Text         string          `json:"text"`
					CacheControl json.RawMessage `json:"cache_control,omitempty"`
				}
				if decodeInboundStrict(raw, &block) != nil || block.Type != "text" || len(block.Text) > MaxStringBytes {
					return NewError(400, "invalid_system", "system prompt contains an invalid block")
				}
				if err := validateCacheControl(block.CacheControl); err != nil {
					return err
				}
			}
		}
	}
	if len(request.ToolChoice) > 0 {
		var choice struct {
			Type                   string `json:"type"`
			Name                   string `json:"name,omitempty"`
			DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
		}
		if decodeInboundStrict(request.ToolChoice, &choice) != nil {
			return NewError(400, "invalid_tool_choice", "tool_choice is malformed")
		}
		switch choice.Type {
		case "auto", "any", "none":
			if choice.Name != "" {
				return NewError(400, "invalid_tool_choice", "unnamed tool_choice contains a name")
			}
		case "tool":
			if !validBoundedWireString(choice.Name, 512) {
				return NewError(400, "invalid_tool_choice", "named tool_choice has an invalid name")
			}
			if _, ok := toolNames[choice.Name]; !ok {
				return NewError(400, "unknown_tool_choice", "named tool_choice does not exist in tools")
			}
		default:
			return NewError(400, "invalid_tool_choice", "tool_choice type is not supported")
		}
	}
	if len(request.Metadata) > 0 && validateJSONValue(request.Metadata, '{') != nil {
		return NewError(400, "invalid_metadata", "metadata must be a bounded strict JSON object")
	}
	if len(request.Thinking) > 0 && validateJSONValue(request.Thinking, '{') != nil {
		return NewError(400, "invalid_thinking", "thinking must be a bounded strict JSON object")
	}
	if request.Speed != "" && !validBoundedWireString(request.Speed, 128) {
		return NewError(400, "invalid_speed", "speed is invalid")
	}
	for _, beta := range request.Betas {
		if !validBoundedWireString(beta, 256) {
			return NewError(400, "invalid_beta", "beta header value is invalid")
		}
	}
	return nil
}

func validateCanonicalHistory(messages []types.Message) error {
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			return NewError(400, "invalid_message_role", "canonical messages only accept user and assistant roles")
		}
		if len(message.Content) == 0 || len(message.Content) > MaxStringBytes*4 {
			return NewError(400, "invalid_message_content", "message content is invalid or exceeds the protocol limit")
		}
		var plain string
		if json.Unmarshal(message.Content, &plain) == nil {
			if len(plain) > MaxStringBytes {
				return NewError(400, "invalid_message_content", "message text exceeds the protocol limit")
			}
			continue
		}
		var blocks []json.RawMessage
		if err := decodeRawArray(message.Content, &blocks); err != nil || len(blocks) == 0 {
			return NewError(400, "invalid_message_content", "message content must be a string or nonempty block array")
		}
		for _, raw := range blocks {
			var discriminator struct {
				Type string `json:"type"`
			}
			if validateJSONValue(raw, '{') != nil || json.Unmarshal(raw, &discriminator) != nil {
				return NewError(400, "invalid_content_block", "content block is malformed")
			}
			switch discriminator.Type {
			case "text":
				if err := validateCanonicalTextBlock(raw); err != nil {
					return err
				}
			case "image":
				if message.Role != "user" {
					return NewError(400, "invalid_content_block", "image blocks require user role")
				}
				if err := validateCanonicalImageBlock(raw); err != nil {
					return err
				}
			case "tool_use":
				if message.Role != "assistant" {
					return NewError(400, "invalid_content_block", "tool_use blocks require assistant role")
				}
				id, err := validateCanonicalToolUseBlock(raw)
				if err != nil {
					return err
				}
				if _, duplicate := calls[id]; duplicate {
					return NewError(400, "duplicate_tool_call_id", "tool call ids must be unique")
				}
				calls[id] = struct{}{}
			case "tool_result":
				if message.Role != "user" {
					return NewError(400, "invalid_content_block", "tool_result blocks require user role")
				}
				id, err := validateCanonicalToolResultBlock(raw)
				if err != nil {
					return err
				}
				if _, known := calls[id]; !known {
					return NewError(400, "unknown_tool_call_id", "tool_result references an unknown tool call")
				}
				if _, duplicate := results[id]; duplicate {
					return NewError(400, "duplicate_tool_result", "tool call result may appear only once")
				}
				results[id] = struct{}{}
			case "thinking", "redacted_thinking":
				if message.Role != "assistant" || validateCanonicalThinkingBlock(raw, discriminator.Type) != nil {
					return NewError(400, "invalid_content_block", "thinking block is invalid")
				}
			default:
				return NewError(400, "unsupported_content_block", "content block type is not supported")
			}
		}
	}
	for id := range calls {
		if _, ok := results[id]; !ok {
			return NewError(400, "missing_tool_result", "every historical tool call requires one tool result")
		}
	}
	return nil
}

func validateCanonicalTextBlock(raw json.RawMessage) error {
	var block struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control,omitempty"`
	}
	if decodeInboundStrict(raw, &block) != nil || block.Type != "text" || len(block.Text) > MaxStringBytes {
		return NewError(400, "invalid_content_block", "text block is invalid")
	}
	return validateCacheControl(block.CacheControl)
}

func validateCanonicalImageBlock(raw json.RawMessage) error {
	var block struct {
		Type   string `json:"type"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
		CacheControl json.RawMessage `json:"cache_control,omitempty"`
	}
	if decodeInboundStrict(raw, &block) != nil || block.Type != "image" || block.Source.Type != "base64" || len(block.Source.Data) > MaxStringBytes*4 {
		return NewError(400, "invalid_content_block", "image block is invalid")
	}
	switch block.Source.MediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return NewError(400, "invalid_content_block", "image media type is not supported")
	}
	return validateCacheControl(block.CacheControl)
}

func validateCanonicalToolUseBlock(raw json.RawMessage) (string, error) {
	var block struct {
		Type         string          `json:"type"`
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Input        json.RawMessage `json:"input"`
		CacheControl json.RawMessage `json:"cache_control,omitempty"`
	}
	if decodeInboundStrict(raw, &block) != nil || block.Type != "tool_use" || !validBoundedWireString(block.ID, 512) || !validBoundedWireString(block.Name, 512) || validateJSONValue(block.Input, '{') != nil {
		return "", NewError(400, "invalid_tool_call", "tool_use block is invalid")
	}
	if err := validateCacheControl(block.CacheControl); err != nil {
		return "", err
	}
	return block.ID, nil
}

func validateCanonicalToolResultBlock(raw json.RawMessage) (string, error) {
	var block struct {
		Type         string          `json:"type"`
		ToolUseID    string          `json:"tool_use_id"`
		Content      json.RawMessage `json:"content"`
		IsError      *bool           `json:"is_error,omitempty"`
		CacheControl json.RawMessage `json:"cache_control,omitempty"`
	}
	if decodeInboundStrict(raw, &block) != nil || block.Type != "tool_result" || !validBoundedWireString(block.ToolUseID, 512) || len(block.Content) == 0 || len(block.Content) > MaxStringBytes*4 || validateJSONValue(block.Content, 0) != nil {
		return "", NewError(400, "invalid_tool_result", "tool_result block is invalid")
	}
	if err := validateCacheControl(block.CacheControl); err != nil {
		return "", err
	}
	return block.ToolUseID, nil
}

func validateCanonicalThinkingBlock(raw json.RawMessage, kind string) error {
	if kind == "thinking" {
		var block struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}
		if decodeInboundStrict(raw, &block) != nil || block.Type != kind || len(block.Thinking) > MaxStringBytes || len(block.Signature) > MaxStringBytes {
			return NewError(400, "invalid_content_block", "thinking block is invalid")
		}
		return nil
	}
	var block struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if decodeInboundStrict(raw, &block) != nil || block.Type != kind || block.Data == "" || len(block.Data) > MaxStringBytes {
		return NewError(400, "invalid_content_block", "redacted thinking block is invalid")
	}
	return nil
}

func validateCacheControl(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var cache canonicalCacheControl
	if decodeInboundStrict(raw, &cache) != nil || cache.Type != "ephemeral" || (cache.TTL != "" && cache.TTL != "5m" && cache.TTL != "1h") {
		return NewError(400, "invalid_cache_control", "cache_control is invalid")
	}
	return nil
}

func decodeInboundStrict(raw json.RawMessage, target any) error {
	if validateJSONValue(raw, '{') != nil {
		return NewError(400, "invalid_json", "object contains duplicate keys, excessive nesting, or malformed JSON")
	}
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil || rejectFoldedDuplicateKeys(envelope) != nil {
		return NewError(400, "invalid_json", "object contains semantically duplicate fields")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return NewError(400, "invalid_json", "object contains unsupported or malformed fields")
	}
	return nil
}
