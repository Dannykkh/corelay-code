package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type canonicalBlock struct {
	Index           int
	Type            string
	Text            string
	ID              string
	Name            string
	Arguments       string
	closed          bool
	hasInitialInput bool
	textBuilder     strings.Builder
	argumentBuilder strings.Builder
}

type canonicalMessage struct {
	ID         string
	Model      string
	Blocks     []*canonicalBlock
	StopReason string
	Usage      Usage
	stopped    bool
	started    bool
	deltaSeen  bool
	byIndex    map[int]*canonicalBlock
	toolIDs    map[string]struct{}
	totalBytes int
	events     int
}

func newCanonicalMessage(meta ResponseMeta) *canonicalMessage {
	return &canonicalMessage{ID: meta.ID, Model: meta.Model, byIndex: make(map[int]*canonicalBlock), toolIDs: make(map[string]struct{})}
}

func (m *canonicalMessage) consume(event types.SSEEvent) error {
	m.events++
	if m.events > MaxEvents {
		return NewError(502, "upstream_event_limit", "upstream emitted too many events")
	}
	m.totalBytes += len(event.ContentBlock) + len(event.Delta)
	if m.totalBytes > MaxOutputBytes {
		return NewError(502, "upstream_output_limit", "upstream output exceeded the protocol limit")
	}
	if m.stopped {
		return NewError(502, "invalid_upstream_event", "upstream emitted an event after message_stop")
	}
	if event.Type != "message_start" && !m.started {
		return NewError(502, "invalid_upstream_event", "upstream emitted an event before message_start")
	}

	switch event.Type {
	case "message_start":
		if m.started || event.Message == nil {
			return NewError(502, "invalid_upstream_event", "upstream must emit exactly one valid message_start")
		}
		if !validBoundedWireString(event.Message.ID, 512) || !validBoundedWireString(event.Message.Model, 512) {
			return NewError(502, "invalid_upstream_event", "upstream message id or model is invalid")
		}
		if event.Message.Type != "message" || event.Message.Role != "assistant" {
			return NewError(502, "invalid_upstream_event", "upstream message_start has an invalid type or role")
		}
		if len(event.Message.Content) > 0 && string(event.Message.Content) != "null" && string(event.Message.Content) != "[]" {
			return NewError(502, "invalid_upstream_event", "message_start must not contain prefilled content")
		}
		m.started = true
		m.ID = event.Message.ID
		m.Model = event.Message.Model
		if event.Message.Usage != nil {
			if event.Message.Usage.InputTokens < 0 || event.Message.Usage.OutputTokens < 0 {
				return NewError(502, "invalid_upstream_event", "upstream usage must not be negative")
			}
			m.Usage.InputTokens = event.Message.Usage.InputTokens
			m.Usage.OutputTokens = event.Message.Usage.OutputTokens
		}
	case "content_block_start":
		if event.Index == nil || *event.Index < 0 || *event.Index >= MaxEvents {
			return NewError(502, "invalid_upstream_event", "content block start is missing an index")
		}
		if _, exists := m.byIndex[*event.Index]; exists {
			return NewError(502, "duplicate_upstream_index", "content block index was opened more than once")
		}
		var block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := validateJSONValue(event.ContentBlock, '{'); err != nil {
			return NewError(502, "invalid_upstream_event", "content block start is not strict JSON")
		}
		if err := decodeUpstreamStrict(event.ContentBlock, &block); err != nil {
			return NewError(502, "invalid_upstream_event", "content block start is malformed")
		}
		if block.Type != "text" && block.Type != "tool_use" {
			return NewError(502, "unsupported_upstream_block", "upstream emitted an unsupported content block")
		}
		if block.Type == "tool_use" && (!validBoundedWireString(block.ID, 512) || !validBoundedWireString(block.Name, 512)) {
			return NewError(502, "invalid_upstream_tool_call", "upstream tool call is missing its id or name")
		}
		if block.Type == "tool_use" {
			if _, duplicate := m.toolIDs[block.ID]; duplicate {
				return NewError(502, "duplicate_upstream_tool_call_id", "upstream tool call ids must be unique")
			}
			m.toolIDs[block.ID] = struct{}{}
		}
		args := ""
		hasInitialInput := false
		if len(block.Input) > 0 && string(block.Input) != "null" && string(block.Input) != `""` && string(block.Input) != `{}` {
			if err := validateJSONValue(block.Input, '{'); err != nil {
				return NewError(502, "invalid_upstream_tool_call", "upstream initial tool input is not a strict JSON object")
			}
			args = string(block.Input)
			hasInitialInput = true
		}
		entry := &canonicalBlock{Index: *event.Index, Type: block.Type, ID: block.ID, Name: block.Name, hasInitialInput: hasInitialInput}
		if block.Type == "text" {
			entry.textBuilder.WriteString(block.Text)
		} else if args != "" {
			entry.argumentBuilder.WriteString(args)
		}
		m.byIndex[entry.Index] = entry
		m.Blocks = append(m.Blocks, entry)
	case "content_block_delta":
		if event.Index == nil {
			return NewError(502, "invalid_upstream_event", "content block delta is missing an index")
		}
		block := m.byIndex[*event.Index]
		if block == nil || block.closed {
			return NewError(502, "invalid_upstream_event", "content block delta does not reference an open block")
		}
		var delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		}
		if err := validateJSONValue(event.Delta, '{'); err != nil {
			return NewError(502, "invalid_upstream_event", "content block delta is not strict JSON")
		}
		if err := decodeUpstreamStrict(event.Delta, &delta); err != nil {
			return NewError(502, "invalid_upstream_event", "content block delta is malformed")
		}
		switch {
		case block.Type == "text" && delta.Type == "text_delta":
			if block.textBuilder.Len()+len(delta.Text) > MaxOutputBytes {
				return NewError(502, "upstream_output_limit", "upstream text block exceeded the protocol limit")
			}
			block.textBuilder.WriteString(delta.Text)
		case block.Type == "tool_use" && delta.Type == "input_json_delta":
			if block.hasInitialInput {
				return NewError(502, "invalid_upstream_tool_call", "upstream mixed complete and incremental tool input")
			}
			if block.argumentBuilder.Len()+len(delta.PartialJSON) > MaxOutputBytes {
				return NewError(502, "upstream_output_limit", "upstream tool arguments exceeded the protocol limit")
			}
			block.argumentBuilder.WriteString(delta.PartialJSON)
		default:
			return NewError(502, "invalid_upstream_event", "content block delta type does not match its block")
		}
	case "content_block_stop":
		if event.Index == nil || m.byIndex[*event.Index] == nil || m.byIndex[*event.Index].closed {
			return NewError(502, "invalid_upstream_event", "content block stop does not reference an open block")
		}
		block := m.byIndex[*event.Index]
		block.closed = true
		block.Text = block.textBuilder.String()
		block.Arguments = block.argumentBuilder.String()
		if block.Type == "tool_use" {
			if block.Arguments == "" {
				block.Arguments = "{}"
			}
			if err := validateJSONValue([]byte(block.Arguments), '{'); err != nil {
				return NewError(502, "invalid_upstream_tool_call", "upstream tool arguments are not a JSON object")
			}
		}
	case "message_delta":
		if m.deltaSeen {
			return NewError(502, "invalid_upstream_event", "upstream emitted message_delta more than once")
		}
		for _, block := range m.Blocks {
			if !block.closed {
				return NewError(502, "invalid_upstream_event", "message_delta arrived before content blocks closed")
			}
		}
		m.deltaSeen = true
		if event.Usage != nil {
			if event.Usage.InputTokens < 0 || event.Usage.OutputTokens < 0 {
				return NewError(502, "invalid_upstream_event", "upstream usage must not be negative")
			}
			if event.Usage.InputTokens > 0 {
				m.Usage.InputTokens = event.Usage.InputTokens
			}
			if event.Usage.OutputTokens > 0 {
				m.Usage.OutputTokens = event.Usage.OutputTokens
			}
		}
		var delta struct {
			StopReason   string          `json:"stop_reason"`
			StopSequence json.RawMessage `json:"stop_sequence,omitempty"`
		}
		if len(event.Delta) > 0 {
			if err := validateJSONValue(event.Delta, '{'); err != nil || decodeUpstreamStrict(event.Delta, &delta) != nil {
				return NewError(502, "invalid_upstream_event", "message delta is not strict JSON")
			}
			if delta.StopReason != "" {
				switch delta.StopReason {
				case "end_turn", "tool_use", "max_tokens", "stop_sequence", "pause_turn", "refusal":
					m.StopReason = delta.StopReason
				default:
					return NewError(502, "invalid_upstream_event", "upstream stop reason is not supported")
				}
			}
		}
	case "message_stop":
		if m.stopped {
			return NewError(502, "invalid_upstream_event", "upstream stopped the message more than once")
		}
		if !m.deltaSeen {
			return NewError(502, "invalid_upstream_event", "message_stop arrived before message_delta")
		}
		for _, block := range m.Blocks {
			if !block.closed {
				return NewError(502, "invalid_upstream_event", "upstream stopped with an open content block")
			}
		}
		m.stopped = true
	case "ping":
		return nil
	default:
		return NewError(502, "unsupported_upstream_event", "upstream emitted an unsupported event")
	}
	return nil
}

func decodeUpstreamStrict(raw json.RawMessage, target any) error {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil || rejectFoldedDuplicateKeys(envelope) != nil {
		return errors.New("semantic duplicate upstream fields")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func collectCanonical(ctx context.Context, meta ResponseMeta, events <-chan types.SSEEvent) (*canonicalMessage, error) {
	message := newCanonicalMessage(meta)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-events:
			if !ok {
				if !message.stopped {
					return nil, NewError(502, "upstream_truncated", "upstream ended before message_stop")
				}
				return message, nil
			}
			if err := message.consume(event); err != nil {
				return nil, err
			}
		}
	}
}

func (m *canonicalMessage) sortedBlocks() []*canonicalBlock {
	blocks := append([]*canonicalBlock(nil), m.Blocks...)
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].Index < blocks[j].Index })
	return blocks
}

func writeJSON(w http.ResponseWriter, value any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(value)
}

func safeJSON(raw string) json.RawMessage {
	if raw == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

func canonicalStopToChat(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

func requireString(name, value string) error {
	if !validBoundedWireString(value, 512) {
		return NewError(400, "invalid_request", fmt.Sprintf("%s is missing, invalid, or exceeds the protocol limit", name))
	}
	return nil
}
