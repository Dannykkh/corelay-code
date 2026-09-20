package agent

import (
	"strings"
	"testing"
)

func TestSafeProjectSettingsKeepsOnlyPromptAllowlist(t *testing.T) {
	raw := `{
		"model":"claude-sonnet",
		"permissions":{"allow":["Read(src/**)","Bash(env API_KEY=permission-secret)"],"deny":["Bash(rm -rf *)"]},
		"env":{"API_KEY":"settings-secret"},
		"mcpServers":{"private":{"env":{"TOKEN":"mcp-secret"}}},
		"hooks":{"PreToolUse":[{"command":"echo hook-secret"}]}
	}`
	got := safeProjectSettings(raw)
	for _, expected := range []string{"claude-sonnet", "Read(src/**)", "Bash(rm -rf *)"} {
		if !strings.Contains(got, expected) {
			t.Errorf("filtered settings missing %q: %s", expected, got)
		}
	}
	for _, forbidden := range []string{"settings-secret", "mcp-secret", "hook-secret", "permission-secret", "mcpServers", "hooks"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("filtered settings exposed %q: %s", forbidden, got)
		}
	}
}

func TestSafeProjectSettingsRejectsMalformedAndUnknownOnlyInput(t *testing.T) {
	for _, raw := range []string{
		`{"apiKey":"must-not-appear"}`,
		`{"permissions":"wrong-type","model":42}`,
		"{broken",
	} {
		if got := safeProjectSettings(raw); got != "" {
			t.Errorf("safeProjectSettings(%q) = %q, want empty", raw, got)
		}
	}
}
