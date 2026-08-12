package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopPreservesPlanAnchorAcrossToolIterations(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("CORELAY_MAX_TOOLS", "30")

	workspace := t.TempDir()
	steps := make([]scriptedLoopStep, 0, 5)
	for index := 0; index < 4; index++ {
		name := "fixture-" + string(rune('a'+index)) + ".txt"
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, toolUseStep("read-"+name, "Read", map[string]any{"file_path": name}))
	}
	steps = append(steps, textStep("iteration sequence complete"))

	provider := &planAnchorCaptureProvider{scriptedLoopProvider: scriptedLoopProvider{steps: steps}}
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:             "plan-anchor-long-run",
		ToolBudget:     16,
		ContextWindow:  64_000,
		OutputReserve:  4_000,
		PlanAnchorMode: harness.PlanAnchorCompact,
		ToolRouting:    harness.ToolRoutingDirect,
	})
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "preserve the accepted plan",
		CurrentStep:      "inspect fixtures",
		RemainingSteps:   []string{"read four fixtures", "report completion"},
		DefinitionOfDone: []string{"every fixture is read", "final response is emitted"},
		Revision:         7,
	})
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{
		Role: "user", Content: mustJSON("Inspect the fixtures in order."),
	}}, workspace, RunOptions{HarnessProfile: &profile, PlanAnchor: &anchor}, events)
	for range events {
	}

	systems := provider.systemPrompts()
	if len(systems) != len(steps) {
		t.Fatalf("provider requests = %d, want %d", len(systems), len(steps))
	}
	wantFragments := []string{
		`<plan-anchor revision="7">`,
		"Objective: preserve the accepted plan",
		"Current step: inspect fixtures",
		"- read four fixtures",
		"- report completion",
		"- every fixture is read",
		"- final response is emitted",
	}
	for requestIndex, system := range systems {
		for _, fragment := range wantFragments {
			if !strings.Contains(system, fragment) {
				t.Fatalf("request %d plan anchor missing %q", requestIndex, fragment)
			}
		}
		if count := strings.Count(system, "<plan-anchor "); count != 1 {
			t.Fatalf("request %d plan anchor count = %d, want 1", requestIndex, count)
		}
	}
	if anchor.Revision() != 7 || anchor.CurrentStep() != "inspect fixtures" {
		t.Fatalf("caller anchor mutated to revision=%d step=%q", anchor.Revision(), anchor.CurrentStep())
	}
}

type planAnchorCaptureProvider struct {
	scriptedLoopProvider
	requestMu sync.Mutex
	systems   []string
}

func (p *planAnchorCaptureProvider) StreamMessage(
	ctx context.Context,
	request *types.MessagesRequest,
	options *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(request.System, &blocks); err != nil {
		return nil, err
	}
	if len(blocks) != 1 {
		return nil, fmt.Errorf("system block count = %d, want 1", len(blocks))
	}
	p.requestMu.Lock()
	p.systems = append(p.systems, blocks[0].Text)
	p.requestMu.Unlock()
	return p.scriptedLoopProvider.StreamMessage(ctx, request, options)
}

func (p *planAnchorCaptureProvider) systemPrompts() []string {
	p.requestMu.Lock()
	defer p.requestMu.Unlock()
	return append([]string(nil), p.systems...)
}
