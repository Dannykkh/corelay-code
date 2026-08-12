package translate

import (
	"encoding/json"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ToolDefsToOAI converts Anthropic tool definitions to OpenAI format.
func ToolDefsToOAI(tools []types.ToolDef) []types.OAIToolDef {
	if len(tools) == 0 {
		return nil
	}
	result := make([]types.OAIToolDef, len(tools))
	for i, t := range tools {
		result[i] = types.OAIToolDef{
			Type: "function",
			Function: types.OAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}
	}
	return result
}

// ToolChoiceToOAI converts Anthropic tool_choice to OpenAI format.
func ToolChoiceToOAI(tc json.RawMessage) json.RawMessage {
	if len(tc) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(tc, &choice); err != nil {
		return nil
	}

	switch choice.Type {
	case "auto":
		return mustMarshal("auto")
	case "none":
		return mustMarshal("none")
	case "any":
		return mustMarshal("required")
	case "tool":
		return mustMarshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": choice.Name},
		})
	}
	return nil
}
