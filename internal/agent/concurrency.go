package agent

import (
	"bytes"
	"encoding/json"
	"strings"
)

// toolCallMessageInput keeps rejected native calls representable in the
// assistant/tool-result round trip. Invalid or non-object input is reported by
// the dispatcher, while the protocol history receives a neutral JSON object
// instead of malformed JSON that could break the next provider request.
func toolCallMessageInput(call toolUseBlock) json.RawMessage {
	raw := bytes.TrimSpace(call.Input)
	if strings.TrimSpace(call.InputRaw) != "" {
		raw = bytes.TrimSpace([]byte(call.InputRaw))
	}
	var object map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &object) != nil || object == nil {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), raw...)
}

// IsConcurrencySafe checks if a tool call can run in parallel.
// Semantic analysis for concurrency safety.
func IsConcurrencySafe(toolName string, input map[string]interface{}) bool {
	switch toolName {
	// Always safe: read-only tools
	case "Read", "Glob", "Grep", "RepoMap", "LSP", loadToolResultToolName:
		return true

	// Never safe: write tools
	case "Write", "Edit":
		return false

	// Shell and Git effects share the same structural classifier used by
	// permission checks. Unknown or ambiguous commands are serial by default.
	case "Bash", "Git":
		return classifyCommandEffect(toolName, input).Kind == CommandEffectReadOnly

	default:
		return false
	}
}

// PartitionToolCalls splits tool calls into concurrent-safe and serial batches.
func PartitionToolCalls(calls []ToolCall) (concurrent []ToolCall, serial []ToolCall) {
	for _, call := range calls {
		input := make(map[string]interface{})
		if call.Input != nil {
			// Simple input parsing for concurrency check
			var m map[string]interface{}
			if err := decodeJSON(call.Input, &m); err == nil {
				input = m
			}
		}

		if IsConcurrencySafe(call.Name, input) {
			concurrent = append(concurrent, call)
		} else {
			serial = append(serial, call)
		}
	}
	return
}

// ToolCall represents a pending tool invocation.
type ToolCall struct {
	ID    string
	Name  string
	Input interface{}
}

func decodeJSON(v interface{}, out interface{}) error {
	// Helper to decode tool input
	switch val := v.(type) {
	case map[string]interface{}:
		if m, ok := out.(*map[string]interface{}); ok {
			*m = val
		}
	}
	return nil
}
