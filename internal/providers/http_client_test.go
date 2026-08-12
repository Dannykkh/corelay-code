package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (fn httpDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestCreateWithOptionsUsesInjectedHTTPDoer(t *testing.T) {
	customName := "custom-target-binding-test"
	customOpenAIName := "openai-target-binding-test"
	customOllamaName := "ollama-target-binding-test"
	oldOrder := append([]string(nil), ProviderOrder...)
	type priorCustom struct {
		config *types.ProviderConfig
		ok     bool
	}
	prior := make(map[string]priorCustom)
	registrations := map[string]*types.ProviderConfig{
		customName:       {APIKey: "custom-secret", BaseURL: "https://custom.invalid/tenant"},
		customOpenAIName: {APIKey: "custom-openai-secret", BaseURL: "https://custom-openai.invalid/tenant"},
		customOllamaName: {BaseURL: "http://custom-ollama.invalid/tenant"},
	}
	for name, config := range registrations {
		old, ok := customProviders[name]
		prior[name] = priorCustom{config: old, ok: ok}
		RegisterCustomProvider(name, config)
	}
	t.Cleanup(func() {
		ProviderOrder = oldOrder
		for name, old := range prior {
			if old.ok {
				customProviders[name] = old.config
			} else {
				delete(customProviders, name)
			}
		}
	})

	tests := []struct {
		name         string
		providerName string
		config       *types.ProviderConfig
		wantPath     string
	}{
		{
			name:         "anthropic",
			providerName: "anthropic",
			config:       &types.ProviderConfig{APIKey: "anthropic-secret", BaseURL: "https://anthropic.invalid/tenant"},
			wantPath:     "/tenant/v1/messages",
		},
		{
			name:         "openai compatible",
			providerName: "openai",
			config:       &types.ProviderConfig{APIKey: "openai-secret", BaseURL: "https://openai.invalid/tenant"},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "groq compatible",
			providerName: "groq",
			config:       &types.ProviderConfig{APIKey: "groq-secret", BaseURL: "https://groq.invalid/tenant"},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "ollama compatible",
			providerName: "ollama",
			config:       &types.ProviderConfig{BaseURL: "http://ollama.invalid/tenant"},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "sglang compatible",
			providerName: "sglang",
			config:       &types.ProviderConfig{BaseURL: "http://sglang.invalid/tenant"},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "copilot compatible",
			providerName: "github-copilot",
			config:       &types.ProviderConfig{APIKey: "copilot-secret", BaseURL: "https://copilot.invalid/tenant"},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "zai compatible",
			providerName: "zai",
			config:       &types.ProviderConfig{APIKey: "zai-secret", BaseURL: "https://zai.invalid/tenant"},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "gemini",
			providerName: "gemini",
			config:       &types.ProviderConfig{APIKey: "gemini-secret", BaseURL: "https://gemini.invalid/tenant"},
			wantPath:     "/tenant/v1beta/models/test-model:streamGenerateContent",
		},
		{
			name:         "custom openai compatible",
			providerName: customName,
			config:       &types.ProviderConfig{},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "custom openai prefix",
			providerName: customOpenAIName,
			config:       &types.ProviderConfig{},
			wantPath:     "/tenant/v1/chat/completions",
		},
		{
			name:         "custom ollama prefix",
			providerName: customOllamaName,
			config:       &types.ProviderConfig{},
			wantPath:     "/tenant/v1/chat/completions",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var observed *http.Request
			doer := httpDoerFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				observed = request.Clone(context.Background())
				return &http.Response{
					StatusCode: http.StatusTeapot,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("rejected")),
					Request:    request,
				}, nil
			})

			provider, err := CreateWithOptions(test.providerName, test.config, CreateOptions{HTTPDoer: doer})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.StreamMessage(context.Background(), &types.MessagesRequest{
				Model:     "test-model",
				MaxTokens: 1,
				Messages:  []types.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
			}, &types.StreamOptions{})
			if err == nil {
				t.Fatal("expected injected upstream status error")
			}
			if calls != 1 {
				t.Fatalf("injected doer calls = %d, want 1", calls)
			}
			if observed == nil || observed.URL.Path != test.wantPath {
				t.Fatalf("request path = %v, want %q", observedURL(observed), test.wantPath)
			}
		})
	}
}

func TestLegacyConstructorsRemainUsableWithNilConfig(t *testing.T) {
	constructors := []func(*types.ProviderConfig) types.Provider{
		NewAnthropic,
		NewOpenAI,
		NewGemini,
		NewGroq,
		NewOllama,
		NewSGLang,
		NewGitHubCopilot,
		NewZai,
	}
	for index, constructor := range constructors {
		if provider := constructor(nil); provider == nil {
			t.Fatalf("constructor %d returned nil", index)
		}
	}
	if provider, err := Create("openai", nil); err != nil || provider == nil {
		t.Fatalf("legacy Create returned provider=%v err=%v", provider, err)
	}
}

func observedURL(request *http.Request) string {
	if request == nil || request.URL == nil {
		return ""
	}
	return request.URL.Path
}
