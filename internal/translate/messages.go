package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// SystemToOAI converts Anthropic system prompt to OpenAI system message.
func SystemToOAI(system json.RawMessage) *types.OAIMessage {
	if len(system) == 0 {
		return nil
	}

	// Try as string first
	var s string
	if err := json.Unmarshal(system, &s); err == nil && s != "" {
		return &types.OAIMessage{Role: "system", Content: mustMarshal(s)}
	}

	// Try as array of text blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(system, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			text := strings.Join(parts, "\n\n")
			return &types.OAIMessage{Role: "system", Content: mustMarshal(text)}
		}
	}

	return nil
}

// MessagesToOAI converts Anthropic messages to OpenAI messages.
func MessagesToOAI(msgs []types.Message) []types.OAIMessage {
	var result []types.OAIMessage

	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			result = append(result, convertUserMessage(msg)...)
		case "assistant":
			result = append(result, convertAssistantMessage(msg))
		}
	}

	return result
}

func convertUserMessage(msg types.Message) []types.OAIMessage {
	// Try as plain string
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return []types.OAIMessage{{Role: "user", Content: mustMarshal(s)}}
	}

	// Array of content blocks
	var blocks []types.ContentBlockParam
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}

	var textParts []types.OAIContentPart
	var toolResults []types.OAIMessage

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, types.OAIContentPart{Type: "text", Text: b.Text})
		case "image":
			if b.Source != nil {
				url := fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
				textParts = append(textParts, types.OAIContentPart{
					Type:     "image_url",
					ImageURL: &types.OAIImageURL{URL: url},
				})
			}
		case "tool_result":
			content := extractToolResultContent(b)
			toolResults = append(toolResults, types.OAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    mustMarshal(content),
			})
		}
	}

	var result []types.OAIMessage

	// Tool results first
	result = append(result, toolResults...)

	// Then user content
	if len(textParts) == 1 && textParts[0].Type == "text" {
		result = append(result, types.OAIMessage{Role: "user", Content: mustMarshal(textParts[0].Text)})
	} else if len(textParts) > 0 {
		result = append(result, types.OAIMessage{Role: "user", Content: mustMarshal(textParts)})
	}

	return result
}

func convertAssistantMessage(msg types.Message) types.OAIMessage {
	var blocks []types.ContentBlockParam
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// Plain string
		return types.OAIMessage{Role: "assistant", Content: msg.Content}
	}

	var text string
	var toolCalls []types.OAIToolCall

	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			toolCalls = append(toolCalls, types.OAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: types.OAIFunctionCall{
					Name:      b.Name,
					Arguments: string(b.Input),
				},
			})
		}
		// thinking blocks are stripped
	}

	oaiMsg := types.OAIMessage{Role: "assistant"}
	if text != "" {
		oaiMsg.Content = mustMarshal(text)
	}
	if len(toolCalls) > 0 {
		oaiMsg.ToolCalls = toolCalls
	}

	return oaiMsg
}

func extractToolResultContent(b types.ContentBlockParam) string {
	// Try as string
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	// Try as array of blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var parts []string
		for _, bl := range blocks {
			if bl.Type == "text" {
				parts = append(parts, bl.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(b.Content)
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
