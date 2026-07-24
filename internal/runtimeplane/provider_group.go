package runtimeplane

import "strings"

// ClassifyProviderGroup maps a configured provider/account name to its coarse
// runtime group. Model names still take precedence for routing; this is a
// fallback for account inventory and dashboard state.
func ClassifyProviderGroup(provider string) ProviderGroup {
	value := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case value == "":
		return GroupDefault
	case value == "anthropic", value == "claude", strings.HasPrefix(value, "claude-"):
		return GroupClaude
	case value == "openai", value == "codex", strings.HasPrefix(value, "openai-"), strings.HasPrefix(value, "codex-"):
		return GroupCodex
	case value == "ollama", value == "sglang", strings.HasPrefix(value, "ollama-"), strings.HasPrefix(value, "sglang-"):
		return GroupLocal
	default:
		return GroupDefault
	}
}
