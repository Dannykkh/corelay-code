package providers

import (
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestModelImageInputCapabilityRegistry(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		want     types.ImageInputCapability
	}{
		{provider: "anthropic", model: "claude-sonnet-4-6", want: types.ImageInputSupported},
		{provider: "openai", model: "gpt-5.5", want: types.ImageInputSupported},
		{provider: "openai", model: "gpt-4.1", want: types.ImageInputSupported},
		{provider: "openai", model: "gpt-4o-mini", want: types.ImageInputSupported},
		{provider: "openai", model: "o3", want: types.ImageInputSupported},
		{provider: "openai", model: "gpt-5.5-codex", want: types.ImageInputUnknown},
		{provider: "openai", model: "not-in-registry", want: types.ImageInputUnknown},
		{provider: "gemini", model: "gemini-3-pro-preview", want: types.ImageInputUnsupported},
		{provider: "groq", model: "meta-llama/llama-4-scout-17b-16e-instruct", want: types.ImageInputUnknown},
	}
	for _, test := range tests {
		t.Run(test.provider+"/"+test.model, func(t *testing.T) {
			provider, err := CreateWithOptions(test.provider, nil, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := types.ImageInputUnknown
			for _, model := range provider.Models() {
				if model.ID == test.model {
					got = model.ImageInput
					break
				}
			}
			if got != test.want {
				t.Fatalf("image input capability for %q = %q, want %q", test.model, got, test.want)
			}
		})
	}
}

func TestCustomProviderModelImageInputDefaultsToUnknown(t *testing.T) {
	const name = "image-capability-custom"
	oldOrder := append([]string(nil), ProviderOrder...)
	oldConfig, existed := customProviders[name]
	t.Cleanup(func() {
		ProviderOrder = oldOrder
		if existed {
			customProviders[name] = oldConfig
		} else {
			delete(customProviders, name)
		}
	})
	RegisterCustomProvider(name, &types.ProviderConfig{BaseURL: "http://localhost:8080"})
	provider, err := CreateWithOptions(name, nil, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	models := provider.Models()
	if len(models) != 1 || models[0].ImageInput != types.ImageInputUnknown {
		t.Fatalf("custom models = %+v, want one unknown-capability default model", models)
	}
}

func TestCustomOpenAIPrefixImageInputDefaultsToUnknown(t *testing.T) {
	const name = "openai-image-capability-custom"
	oldOrder := append([]string(nil), ProviderOrder...)
	oldConfig, existed := customProviders[name]
	t.Cleanup(func() {
		ProviderOrder = oldOrder
		if existed {
			customProviders[name] = oldConfig
		} else {
			delete(customProviders, name)
		}
	})
	RegisterCustomProvider(name, &types.ProviderConfig{BaseURL: "http://localhost:8080"})
	provider, err := CreateWithOptions(name, nil, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	models := provider.Models()
	if len(models) == 0 {
		t.Fatal("custom OpenAI-compatible provider returned no model metadata")
	}
	for _, model := range models {
		if model.ImageInput != types.ImageInputUnknown {
			t.Fatalf("custom model %q image input capability = %q, want unknown", model.ID, model.ImageInput)
		}
	}
}

func TestOpenAIImageInputCapabilityRequiresNativeEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    types.ImageInputCapability
	}{
		{name: "default endpoint", want: types.ImageInputSupported},
		{name: "native endpoint", baseURL: "https://api.openai.com", want: types.ImageInputSupported},
		{name: "native endpoint with default port", baseURL: "https://api.openai.com:443", want: types.ImageInputSupported},
		{name: "compatible endpoint", baseURL: "https://openai-compatible.example", want: types.ImageInputUnknown},
		{name: "insecure native hostname", baseURL: "http://api.openai.com", want: types.ImageInputUnknown},
		{name: "lookalike hostname", baseURL: "https://api.openai.com.example", want: types.ImageInputUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := CreateWithOptions("openai", &types.ProviderConfig{BaseURL: test.baseURL}, CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := types.ImageInputUnknown
			for _, model := range provider.Models() {
				if model.ID == "gpt-5.5" {
					got = model.ImageInput
					break
				}
			}
			if got != test.want {
				t.Fatalf("image input capability for gpt-5.5 at %q = %q, want %q", test.baseURL, got, test.want)
			}
		})
	}
}
