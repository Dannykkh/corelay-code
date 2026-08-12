package providers

import (
	"fmt"
	"os"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

var ProviderOrder = []string{
	"anthropic", "openai", "gemini", "groq", "ollama", "sglang", "github-copilot", "zai",
}

// customProviders stores dynamically registered providers (e.g., remote Ollama instances)
var customProviders = map[string]*types.ProviderConfig{}

// RegisterCustomProvider adds a custom provider (e.g., ollama-home, ollama-office)
func RegisterCustomProvider(name string, cfg *types.ProviderConfig) {
	customProviders[name] = cfg
	// Add to provider order if not already there
	for _, p := range ProviderOrder {
		if p == name {
			return
		}
	}
	ProviderOrder = append(ProviderOrder, name)
}

// ListCustomProviders returns all registered custom providers
func ListCustomProviders() map[string]*types.ProviderConfig {
	return customProviders
}

func Create(name string, cfg *types.ProviderConfig) (types.Provider, error) {
	return CreateWithOptions(name, cfg, CreateOptions{})
}

// CreateWithOptions creates a provider with instance-scoped dependencies.
// Create remains the compatibility entry point for existing callers.
func CreateWithOptions(name string, cfg *types.ProviderConfig, opts CreateOptions) (types.Provider, error) {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}

	// Check custom providers first (remote Ollama instances, etc.)
	if custom, ok := customProviders[name]; ok {
		merged := &types.ProviderConfig{
			APIKey:  coalesce(cfg.APIKey, custom.APIKey),
			BaseURL: coalesce(cfg.BaseURL, custom.BaseURL),
		}
		// Determine base type from name prefix
		if strings.HasPrefix(name, "ollama") {
			return newOllama(merged, opts), nil
		}
		if strings.HasPrefix(name, "openai") {
			return newOpenAI(merged, opts), nil
		}
		// Default to OpenAI-compatible
		return &OpenAICompat{
			ProviderName: name,
			ProviderDisp: name + " (custom)",
			BaseURL:      merged.BaseURL,
			AuthHeader:   func() (string, string) { return "Authorization", "Bearer " + merged.APIKey },
			HTTPDoer:     httpDoerOrDefault(opts.HTTPDoer),
			ModelList: []types.ModelInfo{
				{ID: "default", DisplayName: "Default Model"},
			},
		}, nil
	}

	switch name {
	case "anthropic":
		return NewAnthropicWithOptions(cfg, opts), nil
	case "openai":
		return newOpenAI(cfg, opts), nil
	case "gemini":
		return NewGeminiWithOptions(cfg, opts), nil
	case "groq":
		return newGroq(cfg, opts), nil
	case "ollama":
		return newOllama(cfg, opts), nil
	case "sglang":
		return newSGLang(cfg, opts), nil
	case "github-copilot":
		return newGitHubCopilot(cfg, opts), nil
	case "zai":
		return newZai(cfg, opts), nil
	default:
		return nil, fmt.Errorf("unknown provider: %s. Register custom providers via /api/providers/register", name)
	}
}

// ── Concrete providers ──

func NewOpenAI(cfg *types.ProviderConfig) types.Provider {
	return newOpenAI(cfg, CreateOptions{})
}

func newOpenAI(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	key := cfg.APIKey
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	return &OpenAICompat{
		ProviderName: "openai",
		ProviderDisp: "OpenAI",
		BaseURL:      coalesce(cfg.BaseURL, "https://api.openai.com"),
		AuthHeader:   func() (string, string) { return "Authorization", "Bearer " + key },
		HTTPDoer:     httpDoerOrDefault(opts.HTTPDoer),
		ModelList: []types.ModelInfo{
			{ID: "gpt-5.5", DisplayName: "GPT-5.5 (최신 플래그십)", ContextWindow: 1000000},
			{ID: "gpt-5.5-mini", DisplayName: "GPT-5.5 Mini", ContextWindow: 1000000},
			{ID: "gpt-5.5-codex", DisplayName: "GPT-5.5 Codex (코딩 특화)", ContextWindow: 1000000},
			{ID: "gpt-5.4", DisplayName: "GPT-5.4 (legacy)", ContextWindow: 1000000},
			{ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 Mini (legacy)", ContextWindow: 1000000},
			{ID: "gpt-5.3-codex", DisplayName: "GPT-5.3 Codex (legacy)", ContextWindow: 1000000},
			{ID: "gpt-4.1", DisplayName: "GPT-4.1 (legacy)", ContextWindow: 128000},
			{ID: "gpt-4o", DisplayName: "GPT-4o (legacy)", ContextWindow: 128000},
			{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini (legacy)", ContextWindow: 128000},
			{ID: "o4-mini", DisplayName: "o4 Mini (추론)", ContextWindow: 200000},
			{ID: "o3", DisplayName: "o3 (추론 legacy)", ContextWindow: 200000},
		},
	}
}

func NewOllama(cfg *types.ProviderConfig) types.Provider {
	return newOllama(cfg, CreateOptions{})
}

func newOllama(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	base := coalesce(cfg.BaseURL, os.Getenv("OLLAMA_BASE_URL"), "http://localhost:11434")
	return &OpenAICompat{
		ProviderName: "ollama",
		ProviderDisp: "Ollama (local)",
		BaseURL:      base,
		AuthHeader:   func() (string, string) { return "", "" },
		HTTPDoer:     httpDoerOrDefault(opts.HTTPDoer),
		ModelList: []types.ModelInfo{
			{ID: "qwen3:8b", DisplayName: "Qwen3 8B (가벼움)"},
			{ID: "qwen3:14b", DisplayName: "Qwen3 14B (균형)"},
			{ID: "qwen3:32b", DisplayName: "Qwen3 32B (고품질)"},
			{ID: "qwen3:72b", DisplayName: "Qwen3 72B (최상급)"},
			{ID: "gemma4:27b", DisplayName: "Gemma 4 27B (Google)"},
			{ID: "codestral:latest", DisplayName: "Codestral (코딩 특화)"},
			{ID: "llama4-scout:latest", DisplayName: "Llama 4 Scout (Meta)"},
			{ID: "deepseek-r1:14b", DisplayName: "DeepSeek R1 14B (추론)"},
			{ID: "mistral:latest", DisplayName: "Mistral"},
		},
	}
}

// NewSGLang connects to a self-hosted SGLang server (OpenAI-compatible).
// SGLang serves models on /v1/chat/completions and usually runs without auth
// unless launched with --api-key. Tool calling / reasoning separation are
// configured server-side (--tool-call-parser / --reasoning-parser), so this
// provider just forwards the OpenAI-compatible request like Ollama does.
func NewSGLang(cfg *types.ProviderConfig) types.Provider {
	return newSGLang(cfg, CreateOptions{})
}

func newSGLang(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	base := coalesce(cfg.BaseURL, os.Getenv("SGLANG_BASE_URL"), "http://localhost:30000")
	key := coalesce(cfg.APIKey, os.Getenv("SGLANG_API_KEY"))
	return &OpenAICompat{
		ProviderName: "sglang",
		ProviderDisp: "SGLang (self-hosted)",
		BaseURL:      base,
		AuthHeader: func() (string, string) {
			if key == "" {
				return "", "" // no auth — server launched without --api-key
			}
			return "Authorization", "Bearer " + key
		},
		HTTPDoer: httpDoerOrDefault(opts.HTTPDoer),
		// Examples that fit a 16GB GPU (quantized). Set ID to match the
		// --model-path / --served-model-name you actually launched.
		ModelList: []types.ModelInfo{
			{ID: "Qwen/Qwen2.5-Coder-7B-Instruct", DisplayName: "Qwen2.5 Coder 7B (coding)", ContextWindow: 32768},
			{ID: "Qwen/Qwen2.5-Coder-14B-Instruct", DisplayName: "Qwen2.5 Coder 14B (coding)", ContextWindow: 32768},
			{ID: "Qwen/Qwen3-8B", DisplayName: "Qwen3 8B (reasoning + tools)", ContextWindow: 32768},
			{ID: "meta-llama/Llama-3.1-8B-Instruct", DisplayName: "Llama 3.1 8B", ContextWindow: 131072},
		},
	}
}

func NewGroq(cfg *types.ProviderConfig) types.Provider {
	return newGroq(cfg, CreateOptions{})
}

func newGroq(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	key := cfg.APIKey
	if key == "" {
		key = os.Getenv("GROQ_API_KEY")
	}
	return &OpenAICompat{
		ProviderName: "groq",
		ProviderDisp: "Groq",
		BaseURL:      coalesce(cfg.BaseURL, "https://api.groq.com/openai"),
		AuthHeader:   func() (string, string) { return "Authorization", "Bearer " + key },
		HTTPDoer:     httpDoerOrDefault(opts.HTTPDoer),
		ModelList: []types.ModelInfo{
			{ID: "openai/gpt-oss-120b", DisplayName: "GPT-OSS 120B (최신)"},
			{ID: "qwen/qwen3-32b", DisplayName: "Qwen3 32B"},
			{ID: "meta-llama/llama-4-scout-17b-16e-instruct", DisplayName: "Llama 4 Scout"},
			{ID: "llama-3.3-70b-versatile", DisplayName: "Llama 3.3 70B"},
			{ID: "llama-3.1-8b-instant", DisplayName: "Llama 3.1 8B (빠름)"},
			{ID: "deepseek-r1-distill-llama-70b", DisplayName: "DeepSeek R1 70B (추론)"},
		},
	}
}

func NewGitHubCopilot(cfg *types.ProviderConfig) types.Provider {
	return newGitHubCopilot(cfg, CreateOptions{})
}

func newGitHubCopilot(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	token := cfg.APIKey
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	return &OpenAICompat{
		ProviderName: "github-copilot",
		ProviderDisp: "GitHub Copilot Models",
		BaseURL:      coalesce(cfg.BaseURL, "https://models.inference.ai.azure.com"),
		AuthHeader:   func() (string, string) { return "Authorization", "Bearer " + token },
		HTTPDoer:     httpDoerOrDefault(opts.HTTPDoer),
		ModelList: []types.ModelInfo{
			{ID: "gpt-4o", DisplayName: "GPT-4o"},
			{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini"},
			{ID: "o3-mini", DisplayName: "o3 Mini (추론)"},
			{ID: "claude-3.5-sonnet", DisplayName: "Claude 3.5 Sonnet"},
			{ID: "Mistral-Large-2411", DisplayName: "Mistral Large"},
		},
	}
}

func NewZai(cfg *types.ProviderConfig) types.Provider {
	return newZai(cfg, CreateOptions{})
}

func newZai(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	key := cfg.APIKey
	if key == "" {
		key = os.Getenv("XAI_API_KEY")
	}
	return &OpenAICompat{
		ProviderName: "zai",
		ProviderDisp: "z.ai (Grok)",
		BaseURL:      coalesce(cfg.BaseURL, "https://api.x.ai"),
		AuthHeader:   func() (string, string) { return "Authorization", "Bearer " + key },
		HTTPDoer:     httpDoerOrDefault(opts.HTTPDoer),
		ModelList: []types.ModelInfo{
			{ID: "grok-4", DisplayName: "Grok 4 (최신 플래그십)"},
			{ID: "grok-4.1", DisplayName: "Grok 4.1 (저가)"},
			{ID: "grok-3", DisplayName: "Grok 3"},
			{ID: "grok-3-mini", DisplayName: "Grok 3 Mini"},
		},
	}
}

func coalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
