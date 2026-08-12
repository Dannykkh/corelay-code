package providers

import (
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestProviderRegistryBuiltInAndUnknownMatrix(t *testing.T) {
	for _, name := range []string{
		"anthropic",
		"openai",
		"gemini",
		"groq",
		"ollama",
		"sglang",
		"github-copilot",
		"zai",
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := CreateWithOptions(name, nil, CreateOptions{})
			if err != nil {
				t.Fatalf("CreateWithOptions(%q): %v", name, err)
			}
			if provider == nil {
				t.Fatalf("CreateWithOptions(%q) returned nil", name)
			}
		})
	}

	provider, err := CreateWithOptions("definitely-unknown-provider", nil, CreateOptions{})
	if err == nil || provider != nil {
		t.Fatalf("unknown provider returned provider=%T err=%v", provider, err)
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider error = %q", err)
	}
}

func TestCustomProviderRegistryCompositionMatrix(t *testing.T) {
	tests := []struct {
		name             string
		registered       *types.ProviderConfig
		override         *types.ProviderConfig
		wantProviderName string
		wantBaseURL      string
	}{
		{
			name:             "matrix-generic",
			registered:       &types.ProviderConfig{APIKey: "registered-key", BaseURL: "https://registered.invalid/base"},
			override:         &types.ProviderConfig{APIKey: "override-key"},
			wantProviderName: "matrix-generic",
			wantBaseURL:      "https://registered.invalid/base",
		},
		{
			name:             "openai-matrix",
			registered:       &types.ProviderConfig{BaseURL: "https://registered.invalid/openai"},
			override:         &types.ProviderConfig{BaseURL: "https://override.invalid/openai"},
			wantProviderName: "openai",
			wantBaseURL:      "https://override.invalid/openai",
		},
		{
			name:             "ollama-matrix",
			registered:       &types.ProviderConfig{BaseURL: "http://registered.invalid/ollama"},
			override:         &types.ProviderConfig{},
			wantProviderName: "ollama",
			wantBaseURL:      "http://registered.invalid/ollama",
		},
	}

	oldOrder := append([]string(nil), ProviderOrder...)
	t.Cleanup(func() { ProviderOrder = oldOrder })

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldConfig, existed := customProviders[test.name]
			t.Cleanup(func() {
				if existed {
					customProviders[test.name] = oldConfig
				} else {
					delete(customProviders, test.name)
				}
			})

			RegisterCustomProvider(test.name, test.registered)
			provider, err := CreateWithOptions(test.name, test.override, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if provider.Name() != test.wantProviderName {
				t.Fatalf("provider name = %q, want %q", provider.Name(), test.wantProviderName)
			}
			compatible, ok := provider.(*OpenAICompat)
			if !ok {
				t.Fatalf("provider type = %T, want *OpenAICompat", provider)
			}
			if compatible.BaseURL != test.wantBaseURL {
				t.Fatalf("base URL = %q, want %q", compatible.BaseURL, test.wantBaseURL)
			}
		})
	}
}
