package types

import "encoding/json"

// ── Content Block Types ──

type TextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type ToolUseBlock struct {
	Type  string          `json:"type"` // "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ── Content Block Params (user messages) ──

type ContentBlockParam struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *MediaSource    `json:"source,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type MediaSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ── Messages ──

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlockParam
}

// ── Tool Definition ──

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// RuntimeBinding carries an in-process, provider-invisible execution
	// binding for dynamically composed tools. The concrete value is owned by
	// the composing package and is never serialized onto the model wire.
	RuntimeBinding any `json:"-"`
}

// ── Request ──

type MessagesRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Tools       []ToolDef       `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      *bool           `json:"stream,omitempty"`
	Thinking    json.RawMessage `json:"thinking,omitempty"`
	Betas       []string        `json:"betas,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Speed       string          `json:"speed,omitempty"`
}

// ── SSE Events ──

type SSEEvent struct {
	Type string `json:"type"`
	// message_start
	Message *SSEMessage `json:"message,omitempty"`
	// content_block_start
	Index        *int            `json:"index,omitempty"`
	ContentBlock json.RawMessage `json:"content_block,omitempty"`
	// content_block_delta
	Delta json.RawMessage `json:"delta,omitempty"`
	// message_delta
	Usage *SSEUsage `json:"usage,omitempty"`
}

type SSEMessage struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      json.RawMessage `json:"content"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        *SSEUsage       `json:"usage,omitempty"`
}

type SSEUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens"`
}
