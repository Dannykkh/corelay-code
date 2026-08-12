package harness

import (
	"fmt"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
)

const defaultTwoStageRoutingUsableInputTokens = 16_000

// CompositionInput contains already-resolved, immutable inputs for selecting
// one run HarnessProfile. The caller remains responsible for selecting the
// legacy/configured base and for constructing an exact-target empirical
// selection; this package only applies the documented precedence once.
type CompositionInput struct {
	Base HarnessProfile

	ProviderName string
	Model        string
	Now          time.Time

	ModelContextWindow int
	ModelOutputLimit   int
	Empirical          *capabilityprofile.AutomaticSelection

	ExplicitToolBudget  int
	ExplicitTemperature OptionalFloat64
}

// CompositionResult identifies whether empirical evidence was applied without
// exposing target endpoint, serving parameters, or credentials.
type CompositionResult struct {
	Profile            HarnessProfile
	EmpiricalApplied   bool
	EmpiricalProfileID string
}

// ComposeProfile applies policy inputs from lowest to highest precedence:
// legacy/configured base, provider metadata, eligible empirical evidence, and
// explicit environment/config values. An explicit run HarnessProfile is
// handled by the caller before invoking this function.
func ComposeProfile(input CompositionInput) (CompositionResult, error) {
	if !input.Base.Valid() {
		return CompositionResult{}, fmt.Errorf("base HarnessProfile is unresolved")
	}
	if input.ModelContextWindow < 0 || input.ModelOutputLimit < 0 {
		return CompositionResult{}, fmt.Errorf("provider model limits must be non-negative")
	}
	if input.ExplicitToolBudget < 0 {
		return CompositionResult{}, fmt.Errorf("explicit tool budget must be non-negative")
	}

	base := input.Base
	toolBudget := base.ToolBudget()
	temperature := optionalTemperature(base)
	contextWindow := base.ContextWindow()
	outputReserve := base.OutputReserve()
	reliableInputTokens := base.ReliableInputTokens()
	readBeforeWrite := base.ReadBeforeWrite()
	repeatLimit := base.RepeatLimit()
	maxIterations := base.MaxIterations()
	maxErrorRounds := base.MaxErrorRounds()
	planAnchorMode := base.PlanAnchorMode()
	wirePolicy := base.WirePolicy()
	responsePolicy := base.ResponsePolicy()
	editPolicy := base.EditPolicy()
	toolRouting := base.ToolRouting()

	// Provider metadata is more specific than legacy name heuristics, but it is
	// still below verified empirical evidence and explicit values.
	if input.ModelContextWindow > 0 {
		contextWindow = input.ModelContextWindow
	}
	if input.ModelOutputLimit > 0 {
		outputReserve = input.ModelOutputLimit
	}
	if outputReserve >= contextWindow && contextWindow > 1 {
		if input.ModelOutputLimit > 0 {
			return CompositionResult{}, fmt.Errorf(
				"provider output limit %d exhausts context window %d",
				outputReserve,
				contextWindow,
			)
		}
		outputReserve = contextWindow / 4
	}

	// Small effective input budgets need staged tool selection even when the
	// configured context window is larger. Eligible empirical evidence below
	// remains authoritative for the exact provider/model target.
	if effectiveUsableInputTokens(contextWindow, outputReserve, reliableInputTokens) <= defaultTwoStageRoutingUsableInputTokens {
		toolRouting = ToolRoutingTwoStage
	}

	result := CompositionResult{}
	if input.Empirical != nil {
		if recommendation, profileID, ok := input.Empirical.RecommendationsFor(
			input.ProviderName,
			input.Model,
			input.Now,
		); ok {
			if recommendation.MaxReliableTools > 0 {
				toolBudget = recommendation.MaxReliableTools
			}
			if recommendation.ReliableInputTokens > 0 {
				reliableInputTokens = recommendation.ReliableInputTokens
			}
			if recommendation.RepeatLimit > 0 {
				repeatLimit = recommendation.RepeatLimit
			}
			wirePolicy = WirePolicy(recommendation.WirePolicy)
			responsePolicy = ResponsePolicy(recommendation.ResponsePolicy)
			editPolicy = EditPolicy(recommendation.EditPolicy)
			planAnchorMode = PlanAnchorMode(recommendation.PlanAnchorMode)
			readBeforeWrite = recommendation.ReadBeforeWrite
			if recommendation.UseTwoStageRouting {
				toolRouting = ToolRoutingTwoStage
			} else {
				toolRouting = ToolRoutingDeterministic
			}
			result.EmpiricalApplied = true
			result.EmpiricalProfileID = profileID
		}
	}

	// Explicit environment/config values are applied last. Zero means absent;
	// OptionalFloat64 preserves an explicitly configured temperature of zero.
	if input.ExplicitToolBudget > 0 {
		toolBudget = input.ExplicitToolBudget
	}
	if _, configured := input.ExplicitTemperature.Value(); configured {
		temperature = input.ExplicitTemperature
	}

	profile, err := ResolveProfile(ProfileSpec{
		ID:                  base.ID(),
		ToolBudget:          toolBudget,
		Temperature:         temperature,
		ContextWindow:       contextWindow,
		OutputReserve:       outputReserve,
		ReliableInputTokens: reliableInputTokens,
		Aliases:             base.Aliases(),
		ReadBeforeWrite:     SomeBool(readBeforeWrite),
		RepeatLimit:         repeatLimit,
		MaxIterations:       maxIterations,
		MaxErrorRounds:      maxErrorRounds,
		PromptSuffix:        base.PromptSuffix(),
		PlanAnchorMode:      planAnchorMode,
		WirePolicy:          wirePolicy,
		ResponsePolicy:      responsePolicy,
		EditPolicy:          editPolicy,
		ToolRouting:         toolRouting,
	})
	if err != nil {
		return CompositionResult{}, fmt.Errorf("compose HarnessProfile: %w", err)
	}
	result.Profile = profile
	return result, nil
}

func effectiveUsableInputTokens(contextWindow, outputReserve, reliableInputTokens int) int {
	usableInputTokens := contextWindow - outputReserve
	if usableInputTokens < 0 {
		usableInputTokens = 0
	}
	if reliableInputTokens > 0 && reliableInputTokens < usableInputTokens {
		return reliableInputTokens
	}
	return usableInputTokens
}

func optionalTemperature(profile HarnessProfile) OptionalFloat64 {
	if value, configured := profile.Temperature(); configured {
		return SomeFloat64(value)
	}
	return OptionalFloat64{}
}
