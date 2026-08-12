package server

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

// capabilityProfileEndpointSource is an optional provider contract for
// transports that can truthfully report the endpoint they will use. The value
// is consumed only to construct a one-way target digest and is never logged or
// retained by AutomaticSelection.
type capabilityProfileEndpointSource interface {
	CapabilityProfileEndpoint() string
}

// selectAutomaticCapabilityProfile is intentionally best-effort. Missing or
// ineligible evidence and targets whose actual endpoint cannot be established
// preserve the existing metadata/legacy HarnessProfile fallback.
func selectAutomaticCapabilityProfile(provider types.Provider, model string, now time.Time) *capabilityprofile.AutomaticSelection {
	if provider == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	endpoint, ok := actualCapabilityProfileEndpoint(provider)
	if !ok {
		return nil
	}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider.Name(),
		Model:    model,
		Endpoint: endpoint,
	})
	if err != nil {
		return nil
	}
	store, err := capabilityprofile.NewStore(config.CapabilityProfileDir())
	if err != nil {
		return nil
	}
	profile, err := store.AutoSelect(target, now)
	if err != nil {
		return nil
	}
	selection, err := capabilityprofile.NewAutomaticSelection(target, profile, now)
	if err != nil {
		return nil
	}
	return &selection
}

func actualCapabilityProfileEndpoint(provider types.Provider) (string, bool) {
	if source, ok := provider.(capabilityProfileEndpointSource); ok {
		endpoint := strings.TrimSpace(source.CapabilityProfileEndpoint())
		return endpoint, endpoint != ""
	}
	if compatible, ok := provider.(*providers.OpenAICompat); ok {
		endpoint := strings.TrimSpace(compatible.BaseURL)
		return endpoint, endpoint != ""
	}
	// AnthropicProvider and GeminiProvider intentionally keep their endpoint
	// private. Guessing from provider name or current config could bind evidence
	// to a different live instance, so opaque transports fail closed.
	return "", false
}

// capabilityPlanAnchor supplies the structured state required by empirical
// compact/strict plan-anchor recommendations. Existing runs remain unchanged
// because callers invoke this only when an empirical selection was made.
func capabilityPlanAnchor(messages []types.Message, ws *workstream.Workstream) *agent.PlanAnchor {
	objective := ""
	currentStep := "Address the current user request"
	var definitionOfDone []string
	if ws != nil {
		objective = strings.TrimSpace(ws.Goal.Objective)
		if next := strings.TrimSpace(ws.NextAction); next != "" {
			currentStep = next
		}
		definitionOfDone = append(definitionOfDone, ws.Goal.DefinitionOfDone...)
		if len(definitionOfDone) == 0 {
			definitionOfDone = append(definitionOfDone, ws.Goal.AcceptanceCriteria...)
		}
	}
	if objective == "" {
		objective = lastPlainUserMessage(messages)
	}
	if objective == "" {
		return nil
	}
	if len(definitionOfDone) == 0 {
		definitionOfDone = []string{
			"Satisfy the user's stated objective and report available verification evidence",
		}
	}
	anchor, err := agent.NewPlanAnchor(agent.PlanAnchorSpec{
		Objective:        objective,
		CurrentStep:      currentStep,
		DefinitionOfDone: definitionOfDone,
	})
	if err != nil {
		return nil
	}
	return &anchor
}

func lastPlainUserMessage(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(messages[i].Content, &text) == nil {
			if text = strings.TrimSpace(text); text != "" {
				return text
			}
		}
	}
	return ""
}
