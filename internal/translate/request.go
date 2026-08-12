package translate

import (
	"log"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ToOpenAI converts an Anthropic Messages request to an OpenAI Chat Completions request.
func ToOpenAI(req *types.MessagesRequest, model string) types.OAIChatRequest {
	var msgs []types.OAIMessage

	// System prompt
	if sys := SystemToOAI(req.System); sys != nil {
		msgs = append(msgs, *sys)
	}

	// Messages
	msgs = append(msgs, MessagesToOAI(req.Messages)...)

	result := types.OAIChatRequest{
		Model:         model,
		Messages:      msgs,
		Stream:        true,
		StreamOptions: &types.StreamOpts{IncludeUsage: true},
		MaxTokens:     req.MaxTokens,
	}

	// Temperature
	if req.Temperature != nil && len(req.Thinking) == 0 {
		result.Temperature = req.Temperature
	}

	// Tools — optionally pruned to a budget (CORELAY_MAX_TOOLS; legacy
	// ANICLEW_MAX_TOOLS) so a weak local
	// model is not overwhelmed by a huge tool list (e.g. an MCP-inflated CLI
	// sending 90 tools). Opt-in; no-op when unset or when under budget.
	reqTools := req.Tools
	if budget := ToolBudget(); budget > 0 {
		var dropped int
		reqTools, dropped = PruneTools(reqTools, lastUserMessageText(req.Messages), budget)
		if dropped > 0 {
			log.Printf("[ToolBudget] pruned %d tools (kept %d, budget %d)", dropped, len(reqTools), budget)
		}
	}
	if tools := ToolDefsToOAI(reqTools); tools != nil {
		result.Tools = tools
	}

	// Tool choice
	if tc := ToolChoiceToOAI(req.ToolChoice); tc != nil {
		result.ToolChoice = tc
	}

	return result
}
