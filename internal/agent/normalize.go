package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	maxToolResultSize = 50000  // 50KB per tool result
	maxMessageSize    = 100000 // 100KB per message
	maxTotalMessages  = 200    // conversation cap
)

// NormalizeMessages prepares messages for the LLM API.
// Fixes issues that cause API errors or waste tokens.
func NormalizeMessages(messages []types.Message) []types.Message {
	if len(messages) == 0 {
		return messages
	}

	messages = HoistToolResults(messages)
	messages = mergeConsecutiveSameRole(messages)
	messages = truncateLargeContent(messages)
	messages = ensureAlternatingRoles(messages)
	messages = capMessageCount(messages)

	return messages
}

// mergeConsecutiveSameRole combines adjacent messages with the same role.
// API requires alternating user/assistant — consecutive same-role messages cause errors.
func mergeConsecutiveSameRole(messages []types.Message) []types.Message {
	if len(messages) <= 1 {
		return messages
	}

	var merged []types.Message
	current := messages[0]

	for i := 1; i < len(messages); i++ {
		msg := messages[i]

		if msg.Role == current.Role {
			// Same role — merge content
			// If both are tool results (array format), merge arrays
			if isArrayContent(current.Content) && isArrayContent(msg.Content) {
				current.Content = mergeArrayContent(current.Content, msg.Content)
				continue
			}

			currentIsArray := isArrayContent(current.Content)
			nextIsArray := isArrayContent(msg.Content)
			currentText := extractText(current.Content)
			nextText := extractText(msg.Content)

			if !currentIsArray && !nextIsArray && currentText != "" && nextText != "" {
				combined := currentText + "\n\n" + nextText
				current.Content = mustMarshalString(combined)
			} else if !currentIsArray && !nextIsArray && nextText != "" {
				current.Content = msg.Content
			} else {
				merged = append(merged, current)
				current = msg
			}
		} else {
			merged = append(merged, current)
			current = msg
		}
	}
	merged = append(merged, current)

	return merged
}

// truncateLargeContent shrinks oversized messages to save tokens.
func truncateLargeContent(messages []types.Message) []types.Message {
	for i, msg := range messages {
		content := msg.Content

		// Check total size
		if len(content) > maxMessageSize {
			// Try to extract text and truncate
			text := extractText(content)
			if text != "" && len(text) > maxMessageSize {
				truncated := text[:maxMessageSize/2] + "\n\n... (truncated) ...\n\n" + text[len(text)-maxMessageSize/4:]
				messages[i].Content = mustMarshalString(truncated)
				continue
			}

			// Array content — truncate individual tool results
			if isArrayContent(content) {
				messages[i].Content = truncateArrayContent(content)
			}
		}
	}
	return messages
}

// ensureAlternatingRoles adds empty messages to maintain user/assistant alternation.
func ensureAlternatingRoles(messages []types.Message) []types.Message {
	if len(messages) <= 1 {
		return messages
	}

	var result []types.Message
	result = append(result, messages[0])

	for i := 1; i < len(messages); i++ {
		prev := result[len(result)-1]
		curr := messages[i]

		// If same role appears twice, insert a filler
		if prev.Role == curr.Role {
			fillerRole := "assistant"
			if curr.Role == "assistant" {
				fillerRole = "user"
			}
			result = append(result, types.Message{
				Role:    fillerRole,
				Content: mustMarshalString("(continued)"),
			})
		}
		result = append(result, curr)
	}

	return result
}

// capMessageCount removes oldest messages if conversation exceeds limit.
func capMessageCount(messages []types.Message) []types.Message {
	if len(messages) <= maxTotalMessages {
		return messages
	}

	// Keep first 2 (initial context) and last N messages
	keep := maxTotalMessages - 2
	result := make([]types.Message, 0, maxTotalMessages)
	result = append(result, messages[:2]...)
	result = append(result, messages[len(messages)-keep:]...)
	return result
}

// ── Tool result specific normalization ──

// truncateArrayContent shrinks individual tool results in array-format content.
func truncateArrayContent(content json.RawMessage) json.RawMessage {
	var items []map[string]interface{}
	if json.Unmarshal(content, &items) != nil {
		return content
	}

	for i, item := range items {
		if contentStr, ok := item["content"].(string); ok {
			if len(contentStr) > maxToolResultSize {
				items[i]["content"] = contentStr[:maxToolResultSize/2] +
					fmt.Sprintf("\n\n... (truncated from %d to %d chars) ...\n\n", len(contentStr), maxToolResultSize) +
					contentStr[len(contentStr)-maxToolResultSize/4:]
			}
		}
	}

	data, _ := json.Marshal(items)
	return data
}

// HoistToolResults moves tool results that appear in assistant messages
// to proper user-role tool_result messages.
func HoistToolResults(messages []types.Message) []types.Message {
	result := make([]types.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != "assistant" || !isArrayContent(msg.Content) {
			result = append(result, msg)
			continue
		}

		var blocks []types.ContentBlockParam
		if json.Unmarshal(msg.Content, &blocks) != nil {
			result = append(result, msg)
			continue
		}

		var assistantBlocks []types.ContentBlockParam
		var toolResults []types.ContentBlockParam
		for _, block := range blocks {
			if block.Type == "tool_result" {
				toolResults = append(toolResults, block)
			} else {
				assistantBlocks = append(assistantBlocks, block)
			}
		}
		if len(toolResults) == 0 {
			result = append(result, msg)
			continue
		}
		if len(assistantBlocks) > 0 {
			content, _ := json.Marshal(assistantBlocks)
			result = append(result, types.Message{Role: "assistant", Content: content})
		}
		content, _ := json.Marshal(toolResults)
		result = append(result, types.Message{Role: "user", Content: content})
	}
	return result
}

// ── Helpers ──

func extractText(content json.RawMessage) string {
	// Try string
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}

	// Try blocks array
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

func isArrayContent(content json.RawMessage) bool {
	return len(content) > 0 && content[0] == '['
}

func mergeArrayContent(a, b json.RawMessage) json.RawMessage {
	var aItems, bItems []json.RawMessage
	json.Unmarshal(a, &aItems)
	json.Unmarshal(b, &bItems)
	merged := append(aItems, bItems...)
	data, _ := json.Marshal(merged)
	return data
}

func mustMarshalString(s string) json.RawMessage {
	data, _ := json.Marshal(s)
	return data
}
