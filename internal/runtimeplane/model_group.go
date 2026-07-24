package runtimeplane

import "strings"

// ClassifyModelGroup maps a model string to the runtime account group. This is
// intentionally name-based so Claude-compatible clients can steer providers
// without knowing about account internals.
func ClassifyModelGroup(model string) ProviderGroup {
	value := strings.ToLower(strings.TrimSpace(model))
	switch {
	case value == "":
		return GroupDefault
	case strings.HasPrefix(value, "claude-"),
		value == "opus",
		value == "sonnet",
		value == "haiku":
		return GroupClaude
	case strings.HasPrefix(value, "gpt-"),
		strings.HasPrefix(value, "codex"),
		strings.HasPrefix(value, "o1"),
		strings.HasPrefix(value, "o3"),
		strings.HasPrefix(value, "o4"):
		return GroupCodex
	case strings.Contains(value, "qwen"),
		strings.Contains(value, "llama"),
		strings.Contains(value, "mistral"),
		strings.Contains(value, "ollama"):
		return GroupLocal
	default:
		return GroupDefault
	}
}
