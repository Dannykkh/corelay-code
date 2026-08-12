// Package harness defines immutable policy values that can be resolved before
// an agent run starts. It deliberately does not depend on providers or the
// agent loop, so transport and execution policy can evolve independently.
package harness

import (
	"fmt"
	"math"
	"strings"
)

const (
	// DefaultContextWindow preserves the agent loop's current local-model
	// assumption until a model-aware resolver supplies a measured value.
	DefaultContextWindow = 200_000
	// DefaultOutputReserve preserves the current compaction reserve.
	DefaultOutputReserve = 20_000
	// DefaultRepeatLimit preserves the current repeated-action stop threshold.
	DefaultRepeatLimit = 3
	// DefaultMaxIterations preserves the main agent loop's iteration ceiling.
	DefaultMaxIterations = 25
	// DefaultMaxErrorRounds preserves the main loop's consecutive-error stop.
	DefaultMaxErrorRounds = 3
	// MaxIterationsLimit bounds declarative profiles before they are connected
	// to a production loop, preventing accidentally unbounded model turns.
	MaxIterationsLimit = 1_000
	// MaxErrorRoundsLimit bounds consecutive failed rounds independently from
	// the overall iteration ceiling.
	MaxErrorRoundsLimit = 100
)

// PlanAnchorMode controls whether and how a durable plan reminder is rendered.
type PlanAnchorMode string

const (
	PlanAnchorOff     PlanAnchorMode = "off"
	PlanAnchorCompact PlanAnchorMode = "compact"
	PlanAnchorStrict  PlanAnchorMode = "strict"
)

// Valid reports whether the mode is a supported behavior contract.
func (m PlanAnchorMode) Valid() bool {
	switch m {
	case PlanAnchorOff, PlanAnchorCompact, PlanAnchorStrict:
		return true
	default:
		return false
	}
}

// WirePolicy reserves the provider message-conversion seam.
type WirePolicy string

const (
	WireAuto                  WirePolicy = "auto"
	WireAnthropicMessages     WirePolicy = "anthropic-messages"
	WireOpenAIChatCompletions WirePolicy = "openai-chat-completions"
	WireOpenAIResponses       WirePolicy = "openai-responses"
	WireGemini                WirePolicy = "gemini"
	WireACP                   WirePolicy = "acp"
)

// Valid reports whether the policy is a supported behavior contract.
func (p WirePolicy) Valid() bool {
	switch p {
	case WireAuto, WireAnthropicMessages, WireOpenAIChatCompletions,
		WireOpenAIResponses, WireGemini, WireACP:
		return true
	default:
		return false
	}
}

// ResponsePolicy reserves the provider response-postprocessing seam.
type ResponsePolicy string

const (
	ResponseNative                 ResponsePolicy = "native"
	ResponseNativeWithTextRecovery ResponsePolicy = "native-with-text-recovery"
	ResponseMultiFormat            ResponsePolicy = "multi-format"
)

// Valid reports whether the policy is a supported behavior contract.
func (p ResponsePolicy) Valid() bool {
	switch p {
	case ResponseNative, ResponseNativeWithTextRecovery, ResponseMultiFormat:
		return true
	default:
		return false
	}
}

// EditPolicy reserves the edit execution-policy seam.
type EditPolicy string

const (
	EditCorelayWaterfall EditPolicy = "corelay-waterfall"
	// EditAniClewWaterfall is retained as a source-compatible alias. New
	// profiles serialize the Corelay value; persisted AniClew values are
	// canonicalized by ResolveProfile.
	EditAniClewWaterfall   EditPolicy = EditCorelayWaterfall
	legacyAniClewWaterfall EditPolicy = "aniclew-waterfall"
	EditPatchFirst         EditPolicy = "patch-first"
	EditExact              EditPolicy = "exact"
	EditWholeFile          EditPolicy = "whole-file"
)

// Valid reports whether the policy is a supported behavior contract.
func (p EditPolicy) Valid() bool {
	switch p {
	case EditCorelayWaterfall, legacyAniClewWaterfall, EditPatchFirst, EditExact, EditWholeFile:
		return true
	default:
		return false
	}
}

// ToolRoutingPolicy controls how much of the tool catalog is exposed before
// the first real action. Direct preserves the complete post-budget catalog,
// deterministic uses the local intent classifier, and two-stage asks the
// model to select a bounded category before exposing executable tools.
type ToolRoutingPolicy string

const (
	ToolRoutingDirect        ToolRoutingPolicy = "direct"
	ToolRoutingDeterministic ToolRoutingPolicy = "deterministic"
	ToolRoutingTwoStage      ToolRoutingPolicy = "two-stage"
)

// Valid reports whether the policy is a supported behavior contract.
func (p ToolRoutingPolicy) Valid() bool {
	switch p {
	case ToolRoutingDirect, ToolRoutingDeterministic, ToolRoutingTwoStage:
		return true
	default:
		return false
	}
}

// OptionalFloat64 distinguishes an explicitly configured zero from no value.
// Its fields are private so resolved profiles cannot be mutated through it.
type OptionalFloat64 struct {
	value float64
	set   bool
}

// SomeFloat64 constructs an explicitly configured value.
func SomeFloat64(value float64) OptionalFloat64 {
	return OptionalFloat64{value: value, set: true}
}

// Value returns the value and whether it was configured.
func (o OptionalFloat64) Value() (float64, bool) {
	return o.value, o.set
}

// OptionalBool distinguishes an explicitly configured false from no value.
type OptionalBool struct {
	value bool
	set   bool
}

// SomeBool constructs an explicitly configured value.
func SomeBool(value bool) OptionalBool {
	return OptionalBool{value: value, set: true}
}

// Value returns the value and whether it was configured.
func (o OptionalBool) Value() (bool, bool) {
	return o.value, o.set
}

// ProfileSpec is the declarative input to profile resolution. Callers may
// mutate the spec after resolution without affecting the HarnessProfile.
type ProfileSpec struct {
	ID            string
	ToolBudget    int
	Temperature   OptionalFloat64
	ContextWindow int
	OutputReserve int
	// ReliableInputTokens is an empirical ceiling for request input. It is
	// distinct from the advertised total context window and output reserve.
	// Zero means no empirical ceiling has been selected.
	ReliableInputTokens int
	Aliases             []string
	ReadBeforeWrite     OptionalBool
	RepeatLimit         int
	MaxIterations       int
	MaxErrorRounds      int
	PromptSuffix        string
	PlanAnchorMode      PlanAnchorMode
	WirePolicy          WirePolicy
	ResponsePolicy      ResponsePolicy
	EditPolicy          EditPolicy
	ToolRouting         ToolRoutingPolicy
}

// HarnessProfile is an immutable, fully resolved run policy. All mutable
// inputs are copied and all fields are exposed read-only through methods.
type HarnessProfile struct {
	id                  string
	toolBudget          int
	temperature         OptionalFloat64
	contextWindow       int
	outputReserve       int
	reliableInputTokens int
	aliases             []string
	readBeforeWrite     bool
	repeatLimit         int
	maxIterations       int
	maxErrorRounds      int
	promptSuffix        string
	planAnchorMode      PlanAnchorMode
	wirePolicy          WirePolicy
	responsePolicy      ResponsePolicy
	editPolicy          EditPolicy
	toolRouting         ToolRoutingPolicy
}

// ResolveProfile validates a declarative spec and fills stable defaults.
func ResolveProfile(spec ProfileSpec) (HarnessProfile, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		return HarnessProfile{}, fmt.Errorf("harness profile ID is required")
	}
	if spec.ToolBudget < 0 {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: tool budget must be non-negative", id)
	}

	if temperature, ok := spec.Temperature.Value(); ok {
		if math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 {
			return HarnessProfile{}, fmt.Errorf("harness profile %q: temperature must be finite and non-negative", id)
		}
	}

	contextWindow := spec.ContextWindow
	if contextWindow == 0 {
		contextWindow = DefaultContextWindow
	}
	if contextWindow < 0 {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: context window must be positive", id)
	}

	outputReserve := spec.OutputReserve
	if outputReserve == 0 {
		outputReserve = DefaultOutputReserve
	}
	if outputReserve < 0 || outputReserve >= contextWindow {
		return HarnessProfile{}, fmt.Errorf(
			"harness profile %q: output reserve must be non-negative and smaller than context window",
			id,
		)
	}
	if spec.ReliableInputTokens < 0 {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: reliable input tokens must be non-negative", id)
	}

	repeatLimit := spec.RepeatLimit
	if repeatLimit == 0 {
		repeatLimit = DefaultRepeatLimit
	}
	if repeatLimit < 0 {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: repeat limit must be non-negative", id)
	}

	maxIterations := spec.MaxIterations
	if maxIterations == 0 {
		maxIterations = DefaultMaxIterations
	}
	if maxIterations < 0 || maxIterations > MaxIterationsLimit {
		return HarnessProfile{}, fmt.Errorf(
			"harness profile %q: max iterations must be between 1 and %d",
			id,
			MaxIterationsLimit,
		)
	}

	maxErrorRounds := spec.MaxErrorRounds
	if maxErrorRounds == 0 {
		maxErrorRounds = DefaultMaxErrorRounds
	}
	if maxErrorRounds < 0 || maxErrorRounds > MaxErrorRoundsLimit {
		return HarnessProfile{}, fmt.Errorf(
			"harness profile %q: max error rounds must be between 1 and %d",
			id,
			MaxErrorRoundsLimit,
		)
	}

	readBeforeWrite := true
	if configured, ok := spec.ReadBeforeWrite.Value(); ok {
		readBeforeWrite = configured
	}

	planAnchorMode := spec.PlanAnchorMode
	if planAnchorMode == "" {
		planAnchorMode = PlanAnchorOff
	}
	if !planAnchorMode.Valid() {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: invalid plan-anchor mode %q", id, planAnchorMode)
	}

	wirePolicy := spec.WirePolicy
	if wirePolicy == "" {
		wirePolicy = WireAuto
	}
	if !wirePolicy.Valid() {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: invalid wire policy %q", id, wirePolicy)
	}

	responsePolicy := spec.ResponsePolicy
	if responsePolicy == "" {
		responsePolicy = ResponseNativeWithTextRecovery
	}
	if !responsePolicy.Valid() {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: invalid response policy %q", id, responsePolicy)
	}

	editPolicy := spec.EditPolicy
	if editPolicy == "" {
		editPolicy = EditCorelayWaterfall
	}
	if editPolicy == legacyAniClewWaterfall {
		editPolicy = EditCorelayWaterfall
	}
	if !editPolicy.Valid() {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: invalid edit policy %q", id, editPolicy)
	}

	toolRouting := spec.ToolRouting
	if toolRouting == "" {
		toolRouting = ToolRoutingDirect
	}
	if !toolRouting.Valid() {
		return HarnessProfile{}, fmt.Errorf("harness profile %q: invalid tool-routing policy %q", id, toolRouting)
	}

	return HarnessProfile{
		id:                  id,
		toolBudget:          spec.ToolBudget,
		temperature:         spec.Temperature,
		contextWindow:       contextWindow,
		outputReserve:       outputReserve,
		reliableInputTokens: spec.ReliableInputTokens,
		aliases:             normalizeAliases(spec.Aliases),
		readBeforeWrite:     readBeforeWrite,
		repeatLimit:         repeatLimit,
		maxIterations:       maxIterations,
		maxErrorRounds:      maxErrorRounds,
		promptSuffix:        strings.TrimSpace(spec.PromptSuffix),
		planAnchorMode:      planAnchorMode,
		wirePolicy:          wirePolicy,
		responsePolicy:      responsePolicy,
		editPolicy:          editPolicy,
		toolRouting:         toolRouting,
	}, nil
}

// MustResolveProfile resolves spec or panics. It is intended for static,
// package-owned profile tables whose invalidity is a programmer error.
func MustResolveProfile(spec ProfileSpec) HarnessProfile {
	profile, err := ResolveProfile(spec)
	if err != nil {
		panic(err)
	}
	return profile
}

// Valid reports whether the profile was produced by successful resolution.
func (p HarnessProfile) Valid() bool {
	return p.id != "" &&
		p.toolBudget >= 0 &&
		p.contextWindow > 0 &&
		p.outputReserve >= 0 &&
		p.outputReserve < p.contextWindow &&
		p.reliableInputTokens >= 0 &&
		p.repeatLimit >= 0 &&
		p.maxIterations > 0 &&
		p.maxIterations <= MaxIterationsLimit &&
		p.maxErrorRounds > 0 &&
		p.maxErrorRounds <= MaxErrorRoundsLimit &&
		p.planAnchorMode.Valid() &&
		p.wirePolicy.Valid() &&
		p.responsePolicy.Valid() &&
		p.editPolicy.Valid() &&
		p.toolRouting.Valid()
}

func (p HarnessProfile) ID() string                     { return p.id }
func (p HarnessProfile) ToolBudget() int                { return p.toolBudget }
func (p HarnessProfile) Temperature() (float64, bool)   { return p.temperature.Value() }
func (p HarnessProfile) ContextWindow() int             { return p.contextWindow }
func (p HarnessProfile) OutputReserve() int             { return p.outputReserve }
func (p HarnessProfile) ReliableInputTokens() int       { return p.reliableInputTokens }
func (p HarnessProfile) ReadBeforeWrite() bool          { return p.readBeforeWrite }
func (p HarnessProfile) RepeatLimit() int               { return p.repeatLimit }
func (p HarnessProfile) MaxIterations() int             { return p.maxIterations }
func (p HarnessProfile) MaxErrorRounds() int            { return p.maxErrorRounds }
func (p HarnessProfile) PromptSuffix() string           { return p.promptSuffix }
func (p HarnessProfile) PlanAnchorMode() PlanAnchorMode { return p.planAnchorMode }
func (p HarnessProfile) WirePolicy() WirePolicy         { return p.wirePolicy }
func (p HarnessProfile) ResponsePolicy() ResponsePolicy { return p.responsePolicy }
func (p HarnessProfile) EditPolicy() EditPolicy         { return p.editPolicy }
func (p HarnessProfile) ToolRouting() ToolRoutingPolicy { return p.toolRouting }

// Aliases returns a copy so callers cannot mutate resolved matching policy.
func (p HarnessProfile) Aliases() []string {
	return append([]string(nil), p.aliases...)
}

// Matches reports whether any ordered alias is contained in model. Matching
// is case-insensitive, preserving the legacy local-profile behavior.
func (p HarnessProfile) Matches(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	for _, alias := range p.aliases {
		if strings.Contains(model, alias) {
			return true
		}
	}
	return false
}

// UsableInputTokens computes the smaller of the advertised-window budget and
// an optional empirical input ceiling. Protocol overhead and safety margin are
// explicit inputs; they are not folded into ReliableInputTokens.
func (p HarnessProfile) UsableInputTokens(protocolOverhead, safetyMargin int) (int, error) {
	if !p.Valid() {
		return 0, fmt.Errorf("cannot budget context for an unresolved harness profile")
	}
	if protocolOverhead < 0 || safetyMargin < 0 {
		return 0, fmt.Errorf("context reservations must be non-negative")
	}
	usable := p.contextWindow - p.outputReserve - protocolOverhead - safetyMargin
	if usable <= 0 {
		return 0, fmt.Errorf("context reservations exhaust profile %q", p.id)
	}
	if p.reliableInputTokens > 0 && p.reliableInputTokens < usable {
		usable = p.reliableInputTokens
	}
	return usable, nil
}

func normalizeAliases(aliases []string) []string {
	if len(aliases) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias == "" {
			continue
		}
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		normalized = append(normalized, alias)
	}
	return normalized
}
