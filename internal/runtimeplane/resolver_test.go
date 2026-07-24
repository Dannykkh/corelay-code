package runtimeplane

import "testing"

func TestResolveModelTargetSwitchesProviderByExplicitModelName(t *testing.T) {
	cases := []struct {
		name             string
		requestedModel   string
		fallbackProvider string
		fallbackModel    string
		wantProvider     string
		wantModel        string
		wantGroup        ProviderGroup
	}{
		{
			name:             "gpt model switches from anthropic to openai",
			requestedModel:   "gpt-5.5",
			fallbackProvider: "anthropic",
			fallbackModel:    "claude-sonnet-4-20250514",
			wantProvider:     "openai",
			wantModel:        "gpt-5.5",
			wantGroup:        GroupCodex,
		},
		{
			name:             "claude model switches from openai to anthropic",
			requestedModel:   "claude-opus-4-8",
			fallbackProvider: "openai",
			fallbackModel:    "gpt-5.5",
			wantProvider:     "anthropic",
			wantModel:        "claude-opus-4-8",
			wantGroup:        GroupClaude,
		},
		{
			name:             "local model switches to ollama",
			requestedModel:   "qwen3:14b",
			fallbackProvider: "anthropic",
			fallbackModel:    "claude-sonnet-4-20250514",
			wantProvider:     "ollama",
			wantModel:        "qwen3:14b",
			wantGroup:        GroupLocal,
		},
		{
			name:             "same provider group preserves custom active account",
			requestedModel:   "gpt-5.5-codex",
			fallbackProvider: "openai-work",
			fallbackModel:    "gpt-5.5",
			wantProvider:     "openai-work",
			wantModel:        "gpt-5.5-codex",
			wantGroup:        GroupCodex,
		},
		{
			name:             "unknown explicit model stays on active provider",
			requestedModel:   "custom-model",
			fallbackProvider: "anthropic",
			fallbackModel:    "claude-sonnet-4-20250514",
			wantProvider:     "anthropic",
			wantModel:        "custom-model",
			wantGroup:        GroupClaude,
		},
		{
			name:             "omitted model uses fallback model and its group",
			requestedModel:   "",
			fallbackProvider: "anthropic",
			fallbackModel:    "gpt-5.5",
			wantProvider:     "openai",
			wantModel:        "gpt-5.5",
			wantGroup:        GroupCodex,
		},
		{
			name:             "trim requested model",
			requestedModel:   "  claude-sonnet-4-6  ",
			fallbackProvider: "openai",
			fallbackModel:    "gpt-5.5",
			wantProvider:     "anthropic",
			wantModel:        "claude-sonnet-4-6",
			wantGroup:        GroupClaude,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveModelTarget(tc.requestedModel, tc.fallbackProvider, tc.fallbackModel)
			if got.Provider != tc.wantProvider || got.Model != tc.wantModel || got.Group != tc.wantGroup {
				t.Fatalf("ResolveModelTarget() = %+v, want provider=%q model=%q group=%q", got, tc.wantProvider, tc.wantModel, tc.wantGroup)
			}
		})
	}
}
