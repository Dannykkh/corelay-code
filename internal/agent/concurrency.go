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
	case "Read", "Glob", "Grep", "RepoMap", loadToolResultToolName:
		return true

	// Never safe: write tools
	case "Write", "Edit":
		return false

	// Bash: depends on command content
	case "Bash":
		cmd, ok := input["command"].(string)
		if !ok {
			return false
		}
		return isBashConcurrencySafe(cmd)

	default:
		return false
	}
}

// isBashConcurrencySafe analyzes a bash command for safety.
func isBashConcurrencySafe(cmd string) bool {
	cmd = strings.TrimSpace(cmd)

	// Unsafe patterns: state-changing commands
	unsafePatterns := []string{
		"cd ", "cd\t", // directory change
		"rm ", "rm\t", // file deletion
		"mv ", "mv\t", // file move
		"cp ", "cp\t", // file copy (can overwrite)
		"mkdir ", "touch ", // create
		"chmod ", "chown ", // permissions
		">", ">>", // output redirection
		"git push", "git commit", "git reset", "git checkout",
		"npm install", "npm run", "yarn ", "pnpm ",
		"pip install", "go install", "cargo build",
		"kill ", "pkill ",
		"sudo ",
		"docker ", "kubectl ",
	}

	lower := strings.ToLower(cmd)
	for _, p := range unsafePatterns {
		if strings.Contains(lower, p) {
			return false
		}
	}

	// Safe patterns: read-only commands
	safePatterns := []string{
		"ls", "cat", "head", "tail", "wc",
		"find", "grep", "rg", "ag",
		"git status", "git log", "git diff", "git branch",
		"echo", "printf", "date", "whoami",
		"go version", "node --version", "python --version",
		"which ", "where ", "type ",
	}

	for _, p := range safePatterns {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}

	return false // default unsafe
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
