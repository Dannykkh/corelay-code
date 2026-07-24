package runtimeplane

import "strings"

// ResolveModelTarget selects the runtime provider family for a request. The
// explicit request model is authoritative; fallbackModel is only used when a
// client omits model.
func ResolveModelTarget(requestedModel, fallbackProvider, fallbackModel string) RuntimeTarget {
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = strings.TrimSpace(fallbackModel)
	}

	modelGroup := ClassifyModelGroup(model)
	fallbackProvider = strings.TrimSpace(fallbackProvider)
	fallbackGroup := ClassifyProviderGroup(fallbackProvider)

	group := modelGroup
	if group == GroupDefault {
		group = fallbackGroup
	}

	provider := fallbackProvider
	switch modelGroup {
	case GroupClaude:
		if fallbackGroup != GroupClaude {
			provider = "anthropic"
		}
	case GroupCodex:
		if fallbackGroup != GroupCodex {
			provider = "openai"
		}
	case GroupLocal:
		if fallbackGroup != GroupLocal {
			provider = "ollama"
		}
	case GroupDefault:
		// Keep the active provider for unknown/custom model names.
	}

	return RuntimeTarget{
		Provider: provider,
		Model:    model,
		Group:    group,
	}
}
