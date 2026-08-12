package translate

import (
	"encoding/json"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func names(defs []types.ToolDef) map[string]bool {
	m := map[string]bool{}
	for _, d := range defs {
		m[d.Name] = true
	}
	return m
}

func TestPruneToolsDisabledOrUnderBudget(t *testing.T) {
	tools := []types.ToolDef{{Name: "Read"}, {Name: "Write"}}
	// budget 0 = disabled
	if got, dropped := PruneTools(tools, "", 0); dropped != 0 || len(got) != 2 {
		t.Errorf("budget 0 should be no-op, got len=%d dropped=%d", len(got), dropped)
	}
	// under budget
	if got, dropped := PruneTools(tools, "", 10); dropped != 0 || len(got) != 2 {
		t.Errorf("under budget should be no-op, got len=%d dropped=%d", len(got), dropped)
	}
}

func TestPruneToolsKeepsCoreAndRanksRelevanceDemotesMCP(t *testing.T) {
	tools := []types.ToolDef{
		{Name: "Read"}, {Name: "Write"}, {Name: "Edit"}, {Name: "Bash"},
		{Name: "Glob"}, {Name: "Grep"}, {Name: "LS"}, {Name: "Git"}, // 8 core
		{Name: "Screenshot", Description: "take a screenshot of the screen"},
		{Name: "DockerDeploy", Description: "deploy a docker container to kubernetes"},
		{Name: "mcp__orchestrator__create_task", Description: "create an orchestrator task"},
		{Name: "mcp__orchestrator__claim_task", Description: "claim an orchestrator task"},
	}
	// budget 10: 8 core + 2 fillers. Query favors "docker".
	kept, dropped := PruneTools(tools, "deploy a docker container", 10)
	if dropped != 2 {
		t.Fatalf("expected 2 dropped, got %d (kept %d)", dropped, len(kept))
	}
	n := names(kept)
	for _, core := range []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "LS", "Git"} {
		if !n[core] {
			t.Errorf("core tool %s must be kept", core)
		}
	}
	// DockerDeploy is the most relevant non-core, non-MCP → should be kept.
	if !n["DockerDeploy"] {
		t.Errorf("relevant non-MCP tool DockerDeploy should be kept; kept=%v", n)
	}
	// MCP tools are demoted; with only 2 filler slots and one taken by the
	// relevant non-MCP tool, at most one MCP slot remains.
	mcpKept := 0
	if n["mcp__orchestrator__create_task"] {
		mcpKept++
	}
	if n["mcp__orchestrator__claim_task"] {
		mcpKept++
	}
	if mcpKept > 1 {
		t.Errorf("MCP tools should be demoted; kept %d", mcpKept)
	}
}

func TestPruneToolsDoesNotMutateInput(t *testing.T) {
	tools := make([]types.ToolDef, 20)
	for i := range tools {
		tools[i] = types.ToolDef{Name: "mcp__x__" + string(rune('a'+i))}
	}
	_, _ = PruneTools(tools, "", 5)
	if len(tools) != 20 {
		t.Errorf("input slice mutated: len=%d", len(tools))
	}
}

func TestLastUserMessageText(t *testing.T) {
	msgs := []types.Message{
		{Role: "user", Content: json.RawMessage(`"first"`)},
		{Role: "assistant", Content: json.RawMessage(`"reply"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"},{"type":"text","text":"world"}]`)},
	}
	if got := lastUserMessageText(msgs); got != "hello world " {
		t.Errorf("got %q", got)
	}
	// plain string content
	msgs2 := []types.Message{{Role: "user", Content: json.RawMessage(`"plain"`)}}
	if got := lastUserMessageText(msgs2); got != "plain" {
		t.Errorf("got %q", got)
	}
}
