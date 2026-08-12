package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func routingTestProfile(t *testing.T, policy harness.ToolRoutingPolicy) harness.HarnessProfile {
	t.Helper()
	profile, err := harness.ResolveProfile(harness.ProfileSpec{
		ID:          "routing-test",
		ToolRouting: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func routingTestTool(name, description string) types.ToolDef {
	return types.ToolDef{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}
}

func routingTestCatalog() []types.ToolDef {
	return []types.ToolDef{
		routingTestTool("Read", "Read a file"),
		routingTestTool("Write", "Write a file"),
		routingTestTool("Bash", "Run a command or test"),
		routingTestTool("WebSearch", "Search the internet"),
	}
}

func TestToolRoutingDirectAndDeterministicFallbackPreserveCatalog(t *testing.T) {
	base := routingTestCatalog()
	for _, test := range []struct {
		name   string
		policy harness.ToolRoutingPolicy
		task   string
	}{
		{name: "direct", policy: harness.ToolRoutingDirect, task: "write the file"},
		{name: "deterministic fallback", policy: harness.ToolRoutingDeterministic, task: "xyzzy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, err := newToolRoutingState(routingTestProfile(t, test.policy), base, test.task)
			if err != nil {
				t.Fatal(err)
			}
			got := state.tools()
			if !sameToolCatalog(got, base) {
				t.Fatalf("catalog changed: got=%v want=%v", toolNames(got), toolNames(base))
			}
			got[0].Name = "mutated"
			if state.tools()[0].Name != "Read" {
				t.Fatal("caller mutated routing state through returned slice")
			}
		})
	}
}

func TestToolRoutingDeterministicFiltersThenWidensAfterExecution(t *testing.T) {
	base := routingTestCatalog()
	state, err := newToolRoutingState(
		routingTestProfile(t, harness.ToolRoutingDeterministic),
		base,
		"테스트를 실행하고 빌드를 확인해줘",
	)
	if err != nil {
		t.Fatal(err)
	}
	record := state.record()
	if record.Category != translate.ToolCategoryRun || record.Phase != "filtered" || record.Exposed >= record.Total {
		t.Fatalf("unexpected deterministic route: %#v tools=%v", record, toolNames(state.tools()))
	}
	if state.observeDispatch([]toolDispatchResult{{Executed: false}}) {
		t.Fatal("denied call widened the catalog")
	}
	if state.observeDispatch([]toolDispatchResult{{Executed: true, Synthetic: true}}) {
		t.Fatal("synthetic call widened the catalog")
	}
	if !state.observeDispatch([]toolDispatchResult{{Executed: true}}) {
		t.Fatal("real execution did not widen the catalog")
	}
	if !sameToolCatalog(state.tools(), base) || state.record().Phase != "widened" {
		t.Fatalf("catalog was not restored: %#v", state.record())
	}
}

func TestToolRoutingTwoStageStrictSelectorAndRoundTrip(t *testing.T) {
	base := routingTestCatalog()
	state, err := newToolRoutingState(routingTestProfile(t, harness.ToolRoutingTwoStage), base, "modify the file")
	if err != nil {
		t.Fatal(err)
	}
	initial := state.tools()
	if len(initial) != 1 || initial[0].Name != translate.ToolCategorySelectorName() {
		t.Fatalf("initial tools = %v, want selector only", toolNames(initial))
	}

	handled, assistant, result, err := state.consumeSelector([]toolUseBlock{{
		ID:       "selector-1",
		Name:     translate.ToolCategorySelectorName(),
		InputRaw: `{"category":"write"}`,
	}}, "I need an editing tool.")
	if !handled || err != nil {
		t.Fatalf("consumeSelector = handled %v err %v", handled, err)
	}
	if assistant.Role != "assistant" || result.Role != "user" || !json.Valid(assistant.Content) || !json.Valid(result.Content) {
		t.Fatalf("invalid selector round trip: assistant=%s result=%s", assistant.Content, result.Content)
	}
	selected := state.tools()
	if !containsTool(selected, "Write") || containsTool(selected, "WebSearch") || containsTool(selected, translate.ToolCategorySelectorName()) {
		t.Fatalf("write category tools = %v", toolNames(selected))
	}
	if strings.Contains(string(result.Content), "modify the file") {
		t.Fatal("selector result leaked task content")
	}
}

func TestToolRoutingTwoStageRejectsAmbiguousOrReservedSelector(t *testing.T) {
	base := routingTestCatalog()
	for _, raw := range []string{
		`{"category":"read","category":"write"}`,
		`{"category":"unknown"}`,
		`[]`,
	} {
		state, err := newToolRoutingState(routingTestProfile(t, harness.ToolRoutingTwoStage), base, "read")
		if err != nil {
			t.Fatal(err)
		}
		handled, _, _, consumeErr := state.consumeSelector([]toolUseBlock{{
			Name:     translate.ToolCategorySelectorName(),
			InputRaw: raw,
		}}, "")
		if !handled || consumeErr == nil || !state.awaitingSelector() {
			t.Fatalf("invalid selector %q = handled %v err %v awaiting %v", raw, handled, consumeErr, state.awaitingSelector())
		}
	}

	reserved := append(routingTestCatalog(), translate.ToolCategorySelectorDef())
	if _, err := newToolRoutingState(routingTestProfile(t, harness.ToolRoutingTwoStage), reserved, "read"); err == nil {
		t.Fatal("reserved selector collision was accepted")
	}
}

func TestRunLoopTwoStageSelectorNeverReachesExecutorAndWidens(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "sample.txt"), []byte("sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{
		{
			nativeName: translate.ToolCategorySelectorName(),
			nativeID:   "selector-1",
			nativeJSON: `{"category":"read"}`,
		},
		{nativeName: "Read", nativeID: "read-1", nativeJSON: `{"file_path":"sample.txt"}`},
		{visible: "done"},
	}}
	profile := routingTestProfile(t, harness.ToolRoutingTwoStage)
	events := make(chan Event, 64)
	go RunLoopWithOptions(
		context.Background(),
		provider,
		"routing-model",
		[]types.Message{{Role: "user", Content: mustJSON("read sample.txt")}},
		workDir,
		RunOptions{HarnessProfile: &profile, ResponseLang: "en"},
		events,
	)
	for event := range events {
		if event.Type == "error" {
			t.Fatalf("RunLoop error: %v", event.Data)
		}
		if event.Type == "tool_result" {
			if data, ok := event.Data.(map[string]interface{}); ok && data["name"] == translate.ToolCategorySelectorName() {
				t.Fatal("synthetic selector reached the executor/tool-result event path")
			}
		}
	}

	requests := provider.requestsSnapshot()
	if len(requests) < 3 {
		t.Fatalf("provider requests = %d, want at least 3", len(requests))
	}
	if got := toolNames(requests[0].Tools); len(got) != 1 || got[0] != translate.ToolCategorySelectorName() {
		t.Fatalf("first request tools = %v", got)
	}
	if !containsTool(requests[1].Tools, "Read") || containsTool(requests[1].Tools, "Write") {
		t.Fatalf("second request was not read-filtered: %v", toolNames(requests[1].Tools))
	}
	if !containsTool(requests[2].Tools, "Read") || !containsTool(requests[2].Tools, "Write") || containsTool(requests[2].Tools, translate.ToolCategorySelectorName()) {
		t.Fatalf("third request was not widened to executable base catalog: %v", toolNames(requests[2].Tools))
	}
}

func containsTool(tools []types.ToolDef, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func toolNames(tools []types.ToolDef) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}
