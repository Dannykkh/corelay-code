package agent

import "github.com/Dannykkh/corelay-code/internal/harness"

// ModelProfile is per-model agent-loop tuning, applied by name match for local
// (Ollama/SGLang) models. It sits between explicit user config and the generic
// local default in the resolution chain:
//
//	tool budget : env CORELAY_MAX_TOOLS > config localToolBudget > profile > default(16)
//	temperature : config agentTemperature > profile (default 0)
type ModelProfile struct {
	name        string  // label, shown to the user and logged
	toolBudget  int     // max tools handed to the model
	temperature float64 // sampling temperature
	resolved    harness.HarnessProfile
}

// HarnessProfile exposes the immutable policy behind the legacy loop adapter.
func (p ModelProfile) HarnessProfile() harness.HarnessProfile {
	return p.resolved
}

// modelProfiles are matched in order; the first whose alias is a
// case-insensitive substring of the model id wins. Put specific aliases (e.g.
// "coder") before generic size buckets.
//
// Provenance:
//   - "coder" 16/0 is VERIFIED (qwen3-coder:30b: reliable multi-file edits).
//   - the rest are conservative HEURISTICS: smaller models tolerate fewer tools
//     (see translate.PruneTools — tool-count hallucination), and temperature
//     stays 0 everywhere because low temp is what made local tool calling
//     reliable (prose-drift at the provider default). Tune via config as needed.
var modelProfiles = []ModelProfile{
	newLegacyModelProfile("local-coder", "coder (coding-tuned)", 16, harness.ToolRoutingDirect, "coder"),
	newLegacyModelProfile("local-devstral", "devstral (agentic-coding)", 16, harness.ToolRoutingDirect, "devstral"),
	newLegacyModelProfile("local-deepseek-r1", "deepseek-r1 (reasoning)", 14, harness.ToolRoutingDirect, "deepseek-r1"),
	newLegacyModelProfile("local-qwq", "qwq (reasoning)", 14, harness.ToolRoutingDirect, "qwq"),
	// size buckets (Ollama tag form, e.g. "qwen3:8b"): smaller -> fewer tools.
	newLegacyModelProfile("local-small-3b", "small (<=4B)", 8, harness.ToolRoutingTwoStage, ":3b"),
	newLegacyModelProfile("local-small-4b", "small (<=4B)", 8, harness.ToolRoutingTwoStage, ":4b"),
	newLegacyModelProfile("local-small-7b", "small (7-8B)", 10, harness.ToolRoutingDeterministic, ":7b"),
	newLegacyModelProfile("local-small-8b", "small (7-8B)", 10, harness.ToolRoutingDeterministic, ":8b"),
}

var localDefaultModelProfile = newLegacyModelProfile(
	"local-default",
	"local-default",
	defaultLocalToolBudget,
	harness.ToolRoutingDirect,
)

// profileFor returns the matching profile and true, or the generic local
// default and false when nothing matches.
func profileFor(model string) (ModelProfile, bool) {
	for _, p := range modelProfiles {
		if p.resolved.Matches(model) {
			return p, true
		}
	}
	return localDefaultModelProfile, false
}

func newLegacyModelProfile(
	id, name string,
	toolBudget int,
	routing harness.ToolRoutingPolicy,
	aliases ...string,
) ModelProfile {
	resolved := harness.MustResolveProfile(harness.ProfileSpec{
		ID:          id,
		ToolBudget:  toolBudget,
		Temperature: harness.SomeFloat64(defaultLocalTemperature),
		Aliases:     aliases,
		ToolRouting: routing,
	})
	temperature, _ := resolved.Temperature()
	return ModelProfile{
		name:        name,
		toolBudget:  resolved.ToolBudget(),
		temperature: temperature,
		resolved:    resolved,
	}
}
