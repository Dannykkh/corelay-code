package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestResolveRunHarnessPreservesPrecedenceAndUsesModelMetadata(t *testing.T) {
	temperature := 0.25
	provider := &policyTestProvider{
		name: "ollama",
		models: []types.ModelInfo{{
			ID:            "QWEN-CODER:3B",
			ContextWindow: 32_768,
			MaxOutput:     4_096,
		}},
	}
	resolved, err := resolveRunHarness(
		provider,
		"qwen-coder:3b",
		nil,
		config.Config{
			LocalToolBudget:  9,
			AgentTemperature: &temperature,
		},
		7,
	)
	if err != nil {
		t.Fatalf("resolveRunHarness() error = %v", err)
	}

	profile := resolved.Profile
	if got, want := profile.ID(), "local-coder"; got != want {
		t.Fatalf("profile ID = %q, want %q", got, want)
	}
	if got, want := profile.ToolBudget(), 7; got != want {
		t.Fatalf("tool budget = %d, want env override %d", got, want)
	}
	if got, ok := profile.Temperature(); !ok || got != temperature {
		t.Fatalf("temperature = (%v, %v), want (%v, true)", got, ok, temperature)
	}
	if got, want := profile.ContextWindow(), 32_768; got != want {
		t.Fatalf("context window = %d, want %d", got, want)
	}
	if got, want := profile.OutputReserve(), 4_096; got != want {
		t.Fatalf("output reserve = %d, want %d", got, want)
	}
	if !resolved.Matched || resolved.Label != "coder (coding-tuned)" {
		t.Fatalf("legacy adapter metadata = %#v", resolved)
	}
	if provider.modelCalls != 1 {
		t.Fatalf("provider Models() calls = %d, want once", provider.modelCalls)
	}
}

func TestResolveRunHarnessExplicitOverrideWinsWithoutMetadataRead(t *testing.T) {
	override := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "explicit-test",
		ToolBudget:      3,
		ContextWindow:   65_536,
		OutputReserve:   2_048,
		ReadBeforeWrite: harness.SomeBool(false),
		RepeatLimit:     5,
		PromptSuffix:    "explicit suffix",
		PlanAnchorMode:  harness.PlanAnchorCompact,
	})
	temperature := 1.0
	provider := &policyTestProvider{
		name: "ollama",
		models: []types.ModelInfo{{
			ID:            "model",
			ContextWindow: 4_096,
			MaxOutput:     1_024,
		}},
	}
	resolved, err := resolveRunHarness(
		provider,
		"model",
		&override,
		config.Config{
			LocalToolBudget:  20,
			AgentTemperature: &temperature,
		},
		30,
	)
	if err != nil {
		t.Fatalf("resolveRunHarness() error = %v", err)
	}
	if got := resolved.Profile.ID(); got != "explicit-test" {
		t.Fatalf("profile ID = %q", got)
	}
	if got := resolved.Profile.ToolBudget(); got != 3 {
		t.Fatalf("tool budget = %d, want 3", got)
	}
	if got := resolved.Profile.ContextWindow(); got != 65_536 {
		t.Fatalf("context window = %d, want 65536", got)
	}
	if _, configured := resolved.Profile.Temperature(); configured {
		t.Fatal("explicit absent temperature was overwritten")
	}
	if provider.modelCalls != 0 {
		t.Fatalf("explicit override read model metadata %d times", provider.modelCalls)
	}
}

func TestResolveRunHarnessAdaptsSmallWindowReserveAndRejectsInvalidMetadata(t *testing.T) {
	small := &policyTestProvider{
		name: "remote",
		models: []types.ModelInfo{{
			ID:            "small",
			ContextWindow: 16_000,
		}},
	}
	resolved, err := resolveRunHarness(small, "small", nil, config.Config{}, 0)
	if err != nil {
		t.Fatalf("resolveRunHarness(small) error = %v", err)
	}
	if got, want := resolved.Profile.OutputReserve(), 4_000; got != want {
		t.Fatalf("small-model output reserve = %d, want %d", got, want)
	}

	invalid := &policyTestProvider{
		name: "remote",
		models: []types.ModelInfo{{
			ID:            "invalid",
			ContextWindow: 4_096,
			MaxOutput:     4_096,
		}},
	}
	if _, err := resolveRunHarness(invalid, "invalid", nil, config.Config{}, 0); err == nil {
		t.Fatal("resolveRunHarness() accepted output reserve that exhausts context")
	}
}

func TestRunPromptAndContextPolicy(t *testing.T) {
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:             "prompt-test",
		ContextWindow:  16_000,
		OutputReserve:  4_000,
		PromptSuffix:   "profile suffix",
		PlanAnchorMode: harness.PlanAnchorStrict,
	})
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "finish integration",
		CurrentStep:      "run tests",
		RemainingSteps:   []string{"report"},
		DefinitionOfDone: []string{"tests pass"},
	})
	if err != nil {
		t.Fatalf("NewPlanAnchor() error = %v", err)
	}
	rendered, err := renderRunPlanAnchor(profile, &anchor)
	if err != nil {
		t.Fatalf("renderRunPlanAnchor() error = %v", err)
	}
	prompt := appendPromptPolicy("base", profile.PromptSuffix(), rendered)
	if !strings.Contains(prompt, "base\n\nprofile suffix\n\n<plan-anchor") ||
		!strings.Contains(prompt, "Completion rule:") {
		t.Fatalf("prompt policy output = %q", prompt)
	}
	if got, want := requestMaxTokens(profile), 4_000; got != want {
		t.Fatalf("requestMaxTokens() = %d, want %d", got, want)
	}
	if shouldCompactForHarness(profile, 0, 10_800) {
		t.Fatal("small-model context compacted at threshold instead of above it")
	}
	if !shouldCompactForHarness(profile, 0, 10_801) {
		t.Fatal("small-model context did not compact above adaptive threshold")
	}
	if shouldCompactForHarness(profile, maxCompactFailures, 20_000) {
		t.Fatal("compaction ignored the existing circuit breaker")
	}
}

func TestRunLoopAppliesExplicitHarnessPromptAndPlanAnchor(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("CORELAY_MAX_TOOLS", "30")

	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:             "loop-explicit",
		ToolBudget:     8,
		Temperature:    harness.SomeFloat64(0.15),
		ContextWindow:  32_768,
		OutputReserve:  4_096,
		PromptSuffix:   "PROFILE-SUFFIX-SENTINEL",
		PlanAnchorMode: harness.PlanAnchorCompact,
	})
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "verify prompt composition",
		CurrentStep:      "capture request",
		DefinitionOfDone: []string{"sentinels are present"},
	})
	if err != nil {
		t.Fatalf("NewPlanAnchor() error = %v", err)
	}
	provider := &policyTestProvider{
		name: "remote",
		models: []types.ModelInfo{{
			ID:            "model",
			ContextWindow: 4_096,
			MaxOutput:     1_024,
		}},
		responseText: "done",
	}
	eventCh := make(chan Event, 64)
	go RunLoopWithOptions(
		context.Background(),
		provider,
		"model",
		[]types.Message{{Role: "user", Content: mustJSON("Explain this project.")}},
		t.TempDir(),
		RunOptions{
			HarnessProfile: &profile,
			PlanAnchor:     &anchor,
		},
		eventCh,
	)
	for range eventCh {
	}

	request := provider.lastRequest()
	if request == nil {
		t.Fatal("provider did not receive a request")
	}
	var systemBlocks []map[string]string
	if err := json.Unmarshal(request.System, &systemBlocks); err != nil ||
		len(systemBlocks) != 1 {
		t.Fatalf("system prompt decode = %#v, %v", systemBlocks, err)
	}
	system := systemBlocks[0]["text"]
	if !strings.Contains(system, "PROFILE-SUFFIX-SENTINEL") ||
		!strings.Contains(system, "<plan-anchor") ||
		!strings.Contains(system, "Objective: verify prompt composition") {
		t.Fatalf("system prompt missing profile policy: %q", system)
	}
	if request.MaxTokens != 4_096 {
		t.Fatalf("request MaxTokens = %d, want 4096", request.MaxTokens)
	}
	if request.Temperature == nil || *request.Temperature != 0.15 {
		t.Fatalf("request Temperature = %v, want 0.15", request.Temperature)
	}
	if len(request.Tools) > 8 {
		t.Fatalf("explicit tool budget kept %d tools, want at most 8", len(request.Tools))
	}
	if provider.modelCalls != 0 {
		t.Fatalf("explicit loop override read model metadata %d times", provider.modelCalls)
	}
}

type policyTestProvider struct {
	mu           sync.Mutex
	name         string
	models       []types.ModelInfo
	modelCalls   int
	responseText string
	request      *types.MessagesRequest
}

func (p *policyTestProvider) Name() string        { return p.name }
func (p *policyTestProvider) DisplayName() string { return p.name }
func (p *policyTestProvider) Validate() error     { return nil }

func (p *policyTestProvider) Models() []types.ModelInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelCalls++
	return append([]types.ModelInfo(nil), p.models...)
}

func (p *policyTestProvider) StreamMessage(
	_ context.Context,
	request *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	copyRequest := *request
	copyRequest.Messages = append([]types.Message(nil), request.Messages...)
	copyRequest.Tools = append([]types.ToolDef(nil), request.Tools...)
	p.request = &copyRequest
	p.mu.Unlock()

	events := make(chan types.SSEEvent, 4)
	go func() {
		defer close(events)
		events <- types.SSEEvent{
			Type:         "content_block_start",
			ContentBlock: mustJSON(map[string]string{"type": "text"}),
		}
		events <- types.SSEEvent{
			Type: "content_block_delta",
			Delta: mustJSON(map[string]string{
				"type": "text_delta",
				"text": p.responseText,
			}),
		}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}

func (p *policyTestProvider) lastRequest() *types.MessagesRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.request == nil {
		return nil
	}
	copyRequest := *p.request
	return &copyRequest
}
