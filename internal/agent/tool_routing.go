package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const deterministicToolRouteMinimumConfidence = 0.25

type toolRoutingPhase uint8

const (
	toolRoutingDirect toolRoutingPhase = iota
	toolRoutingSelector
	toolRoutingFiltered
	toolRoutingWidened
)

// toolRoutingState owns only catalog exposure. It cannot execute a tool. The
// normal dispatcher remains the sole authorization and execution boundary.
type toolRoutingState struct {
	policy   harness.ToolRoutingPolicy
	phase    toolRoutingPhase
	base     []types.ToolDef
	active   []types.ToolDef
	decision translate.ToolRouteDecision
}

type toolRoutingRecord struct {
	Policy     harness.ToolRoutingPolicy `json:"policy"`
	Category   translate.ToolCategory    `json:"category,omitempty"`
	Confidence float64                   `json:"confidence,omitempty"`
	Fallback   bool                      `json:"fallback,omitempty"`
	Exposed    int                       `json:"exposed"`
	Total      int                       `json:"total"`
	Phase      string                    `json:"phase"`
}

func newToolRoutingState(
	profile harness.HarnessProfile,
	tools []types.ToolDef,
	task string,
) (*toolRoutingState, error) {
	if !profile.Valid() {
		return nil, fmt.Errorf("tool routing requires a resolved harness profile")
	}
	base := append([]types.ToolDef(nil), tools...)
	state := &toolRoutingState{
		policy: profile.ToolRouting(),
		phase:  toolRoutingDirect,
		base:   base,
		active: append([]types.ToolDef(nil), base...),
	}

	switch state.policy {
	case harness.ToolRoutingDirect:
		return state, nil
	case harness.ToolRoutingDeterministic:
		active, decision, dropped := translate.RouteTools(
			base,
			task,
			deterministicToolRouteMinimumConfidence,
		)
		state.active = active
		state.decision = decision
		if dropped > 0 {
			state.phase = toolRoutingFiltered
		}
		return state, nil
	case harness.ToolRoutingTwoStage:
		selectorName := translate.ToolCategorySelectorName()
		for _, tool := range base {
			if tool.Name == selectorName {
				return nil, fmt.Errorf("tool catalog collides with reserved routing selector %q", selectorName)
			}
		}
		state.phase = toolRoutingSelector
		state.active = []types.ToolDef{translate.ToolCategorySelectorDef()}
		return state, nil
	default:
		return nil, fmt.Errorf("unsupported tool-routing policy %q", state.policy)
	}
}

func (s *toolRoutingState) tools() []types.ToolDef {
	if s == nil {
		return nil
	}
	return append([]types.ToolDef(nil), s.active...)
}

func (s *toolRoutingState) record() toolRoutingRecord {
	if s == nil {
		return toolRoutingRecord{Policy: harness.ToolRoutingDirect, Phase: "direct"}
	}
	phase := "direct"
	switch s.phase {
	case toolRoutingSelector:
		phase = "selector"
	case toolRoutingFiltered:
		phase = "filtered"
	case toolRoutingWidened:
		phase = "widened"
	}
	return toolRoutingRecord{
		Policy:     s.policy,
		Category:   s.decision.Category,
		Confidence: s.decision.Confidence,
		Fallback:   s.decision.Fallback,
		Exposed:    len(s.active),
		Total:      len(s.base),
		Phase:      phase,
	}
}

func (s *toolRoutingState) awaitingSelector() bool {
	return s != nil && s.phase == toolRoutingSelector
}

// consumeSelector intercepts the synthetic first-stage call and turns it into
// a protocol-valid assistant/tool-result pair. No selector call is ever sent
// to dispatchToolCalls or an external executor.
func (s *toolRoutingState) consumeSelector(
	calls []toolUseBlock,
	visibleText string,
) (handled bool, assistant types.Message, result types.Message, err error) {
	if !s.awaitingSelector() || len(calls) == 0 {
		return false, types.Message{}, types.Message{}, nil
	}
	selectorName := translate.ToolCategorySelectorName()
	if len(calls) != 1 || calls[0].Name != selectorName {
		return true, types.Message{}, types.Message{}, fmt.Errorf("routing selector requires exactly one %s call", selectorName)
	}

	call := calls[0]
	raw := bytes.TrimSpace(call.Input)
	if strings.TrimSpace(call.InputRaw) != "" {
		raw = bytes.TrimSpace([]byte(call.InputRaw))
	}
	if len(raw) == 0 || len(raw) > defaultToolArgumentBytes {
		return true, types.Message{}, types.Message{}, fmt.Errorf("routing selector input is missing or too large")
	}
	decoded, decodeErr := decodeStrictJSON(raw, defaultToolParseDepth)
	if decodeErr != nil {
		return true, types.Message{}, types.Message{}, fmt.Errorf("routing selector input is not strict JSON")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return true, types.Message{}, types.Message{}, fmt.Errorf("routing selector input must be an object")
	}
	selector := translate.ToolCategorySelectorDef()
	if validateToolInputSchema(raw, selector.InputSchema) != nil {
		return true, types.Message{}, types.Message{}, fmt.Errorf("routing selector input does not match its schema")
	}
	categoryText, ok := object["category"].(string)
	if !ok {
		return true, types.Message{}, types.Message{}, fmt.Errorf("routing selector category is missing")
	}
	category := translate.ToolCategory(categoryText)
	selected := translate.FilterToolsForCategory(s.base, category)

	if strings.TrimSpace(call.ID) == "" {
		call.ID = "route_selector"
	}
	call.Input = append(json.RawMessage(nil), raw...)
	call.InputRaw = string(raw)
	assistantBlocks := make([]map[string]any, 0, 2)
	if text := strings.TrimSpace(visibleText); text != "" {
		assistantBlocks = append(assistantBlocks, map[string]any{"type": "text", "text": text})
	}
	assistantBlocks = append(assistantBlocks, map[string]any{
		"type":  "tool_use",
		"id":    call.ID,
		"name":  selectorName,
		"input": json.RawMessage(raw),
	})
	resultBlocks := []map[string]any{{
		"type":        "tool_result",
		"tool_use_id": call.ID,
		"content": fmt.Sprintf(
			"Tool category %q selected; %d executable tools are available for the next action.",
			category,
			len(selected),
		),
		"is_error": false,
	}}

	s.active = append([]types.ToolDef(nil), selected...)
	s.decision = translate.ToolRouteDecision{Category: category, Confidence: 1}
	s.phase = toolRoutingFiltered
	if sameToolCatalog(s.active, s.base) {
		s.phase = toolRoutingWidened
	}
	return true,
		types.Message{Role: "assistant", Content: mustJSON(assistantBlocks)},
		types.Message{Role: "user", Content: mustJSON(resultBlocks)},
		nil
}

func (s *toolRoutingState) selectorCorrection() string {
	return "The routing selector call was invalid. Call SelectToolCategory exactly once with a strict JSON object containing only category: read, write, search, run, plan, web, host, or respond."
}

// observeDispatch widens a filtered catalog only after at least one real tool
// crossed the dispatcher and executed. Synthetic/denied calls cannot widen it.
func (s *toolRoutingState) observeDispatch(results []toolDispatchResult) bool {
	if s == nil || s.phase != toolRoutingFiltered {
		return false
	}
	for _, result := range results {
		if result.Executed && !result.Synthetic {
			s.active = append([]types.ToolDef(nil), s.base...)
			s.phase = toolRoutingWidened
			return true
		}
	}
	return false
}

func sameToolCatalog(left, right []types.ToolDef) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name ||
			!bytes.Equal(bytes.TrimSpace(left[index].InputSchema), bytes.TrimSpace(right[index].InputSchema)) {
			return false
		}
	}
	return true
}
