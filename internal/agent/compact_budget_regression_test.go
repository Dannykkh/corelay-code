package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	compactBudgetTestWindow = 600
	compactBudgetTestOutput = 40
	compactBudgetTestSafety = 10
)

func TestCompactPlannedContextFallsBackWhenVerboseNarrativeIsOneTokenOver(t *testing.T) {
	const (
		fallbackInput = compactBudgetTestWindow - compactBudgetTestOutput - compactBudgetTestSafety
		overflowInput = fallbackInput + 1
	)
	estimator := &compactBudgetRegressionEstimator{
		initialInput:   overflowInput,
		fallbackInput:  fallbackInput,
		narrativeInput: overflowInput,
	}
	base, planned := compactBudgetRegressionPlan(t, estimator)
	if got, want := planned.Plan.RequiredTokens, compactBudgetTestWindow+1; got != want {
		t.Fatalf("initial RequiredTokens = %d, want %d (window + 1)", got, want)
	}
	if planned.Plan.Fits {
		t.Fatal("initial request must be over budget")
	}

	provider := &compactBudgetRegressionProvider{text: strings.Repeat("LLM-VERBOSE-SENTINEL ", 1_000)}
	outcome := compactPlannedContext(
		context.Background(), provider, nil, base, planned, CompactionState{Objective: "objective"},
	)
	if outcome.Err != nil {
		t.Fatalf("compactPlannedContext() error = %v", outcome.Err)
	}
	if outcome.Blocked || !outcome.Planned.Plan.Fits || outcome.Planned.Plan.NeedsCompaction {
		t.Fatalf("fallback should fit after verbose candidate overflow: blocked=%t plan=%#v", outcome.Blocked, outcome.Planned.Plan)
	}
	if got, want := outcome.Planned.Plan.RequiredTokens, compactBudgetTestWindow; got != want {
		t.Fatalf("fallback RequiredTokens = %d, want exact window boundary %d", got, want)
	}
	if outcome.Planned.Plan.RemainingTokens != 0 {
		t.Fatalf("fallback RemainingTokens = %d, want 0", outcome.Planned.Plan.RemainingTokens)
	}
	if got, want := provider.calls, 1; got != want {
		t.Fatalf("summarizer calls = %d, want %d", got, want)
	}
	if outcome.Snapshot.Strategy != "deterministic-fallback" {
		t.Fatalf("snapshot strategy = %q, want deterministic fallback", outcome.Snapshot.Strategy)
	}
	if outcome.Snapshot.LLMFallbackReason != "llm_candidate_overflow" {
		t.Fatalf("LLM fallback reason = %q, want llm_candidate_overflow", outcome.Snapshot.LLMFallbackReason)
	}

	wantEstimates := []int{overflowInput, fallbackInput, overflowInput, fallbackInput}
	if got := estimator.inputEstimates(); !equalInts(got, wantEstimates) {
		t.Fatalf("input estimates = %v, want initial/fallback/overflow/final sequence %v", got, wantEstimates)
	}
	if got, want := estimator.requiredTokens(), []int{
		compactBudgetTestWindow + 1,
		compactBudgetTestWindow,
		compactBudgetTestWindow + 1,
		compactBudgetTestWindow,
	}; !equalInts(got, want) {
		t.Fatalf("observed RequiredTokens = %v, want exact boundary sequence %v", got, want)
	}
}

func TestCompactPlannedContextBlocksWhenDeterministicFallbackIsOneTokenOver(t *testing.T) {
	const (
		fallbackInput = compactBudgetTestWindow - compactBudgetTestOutput - compactBudgetTestSafety + 1
		initialInput  = fallbackInput + 1
	)
	estimator := &compactBudgetRegressionEstimator{
		initialInput:   initialInput,
		fallbackInput:  fallbackInput,
		narrativeInput: fallbackInput,
	}
	base, planned := compactBudgetRegressionPlan(t, estimator)
	if got, want := planned.Plan.RequiredTokens, compactBudgetTestWindow+2; got != want {
		t.Fatalf("initial RequiredTokens = %d, want %d", got, want)
	}
	provider := &compactBudgetRegressionProvider{text: "this must never be requested"}

	outcome := compactPlannedContext(
		context.Background(), provider, nil, base, planned, CompactionState{Objective: "objective"},
	)
	if outcome.Err != nil {
		t.Fatalf("compactPlannedContext() error = %v", outcome.Err)
	}
	if !outcome.Blocked || !outcome.Planned.Plan.Blocked || outcome.Planned.Plan.Fits {
		t.Fatalf("one-token overflow must block: blocked=%t plan=%#v", outcome.Blocked, outcome.Planned.Plan)
	}
	if outcome.BlockCode != "context_overflow_after_compaction" || outcome.Planned.Plan.BlockCode != outcome.BlockCode {
		t.Fatalf("block codes = outcome:%q plan:%q", outcome.BlockCode, outcome.Planned.Plan.BlockCode)
	}
	if got, want := outcome.Planned.Plan.RequiredTokens, compactBudgetTestWindow+1; got != want {
		t.Fatalf("blocked RequiredTokens = %d, want exact window + 1 (%d)", got, want)
	}
	if outcome.Planned.Plan.RemainingTokens != -1 {
		t.Fatalf("blocked RemainingTokens = %d, want -1", outcome.Planned.Plan.RemainingTokens)
	}
	if provider.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0 when deterministic fallback is one token over", provider.calls)
	}
	if outcome.Snapshot.Strategy != "deterministic-fallback" {
		t.Fatalf("snapshot strategy = %q, want deterministic fallback", outcome.Snapshot.Strategy)
	}
	wantEstimates := []int{initialInput, fallbackInput, fallbackInput}
	if got := estimator.inputEstimates(); !equalInts(got, wantEstimates) {
		t.Fatalf("input estimates = %v, want initial/fallback/final sequence %v", got, wantEstimates)
	}
	wantRequired := []int{
		compactBudgetTestWindow + 2,
		compactBudgetTestWindow + 1,
		compactBudgetTestWindow + 1,
	}
	if got := estimator.requiredTokens(); !equalInts(got, wantRequired) {
		t.Fatalf("observed RequiredTokens = %v, want %v", got, wantRequired)
	}
}

type compactBudgetRegressionEstimator struct {
	initialInput   int
	fallbackInput  int
	narrativeInput int
	observations   []compactBudgetRegressionEstimate
}

type compactBudgetRegressionEstimate struct {
	inputTokens int
	narrative   bool
}

func (e *compactBudgetRegressionEstimator) EstimateTokens(input ContextEstimateRequest) (TokenEstimate, error) {
	narrative := false
	for _, message := range input.Request.Messages {
		if strings.Contains(string(message.Content), "LLM-VERBOSE-SENTINEL") {
			narrative = true
			break
		}
	}

	inputTokens := e.fallbackInput
	switch {
	case narrative:
		inputTokens = e.narrativeInput
	case len(input.Request.Messages) > compactionRecentUnits+1:
		inputTokens = e.initialInput
	}
	e.observations = append(e.observations, compactBudgetRegressionEstimate{
		inputTokens: inputTokens,
		narrative:   narrative,
	})
	return TokenEstimate{InputTokens: inputTokens, Source: "budget-regression", Confidence: "exact"}, nil
}

func (e *compactBudgetRegressionEstimator) inputEstimates() []int {
	result := make([]int, 0, len(e.observations))
	for _, observation := range e.observations {
		result = append(result, observation.inputTokens)
	}
	return result
}

func (e *compactBudgetRegressionEstimator) requiredTokens() []int {
	result := make([]int, 0, len(e.observations))
	for _, observation := range e.observations {
		result = append(result, observation.inputTokens+compactBudgetTestOutput+compactBudgetTestSafety)
	}
	return result
}

func compactBudgetRegressionPlan(t *testing.T, estimator *compactBudgetRegressionEstimator) (ContextPlanningRequest, PlannedContext) {
	t.Helper()
	messages := make([]types.Message, 0, 10)
	for index := 0; index < 10; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		content, err := json.Marshal(fmt.Sprintf("history turn %d", index))
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, types.Message{Role: role, Content: content})
	}
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:            "compact-budget-regression",
		ContextWindow: compactBudgetTestWindow,
		OutputReserve: 100,
		WirePolicy:    harness.WireAnthropicMessages,
	})
	base := ContextPlanningRequest{
		Profile:            profile,
		Protocol:           harness.WireAnthropicMessages,
		Model:              "budget-regression-model",
		System:             ContextSystemSections{CorePrefix: "stable-system"},
		Messages:           messages,
		MaxTokens:          compactBudgetTestOutput,
		Estimator:          estimator,
		SafetyMarginTokens: compactBudgetTestSafety,
	}
	plan, request, err := CalculateContextPlan(base)
	if err != nil {
		t.Fatalf("CalculateContextPlan() error = %v", err)
	}
	return base, PlannedContext{
		Request:         request,
		System:          base.System,
		Messages:        base.Messages,
		Tools:           base.Tools,
		Plan:            plan,
		NeedsCompaction: true,
	}
}

type compactBudgetRegressionProvider struct {
	calls int
	text  string
}

func (p *compactBudgetRegressionProvider) Name() string              { return "compact-budget-regression" }
func (p *compactBudgetRegressionProvider) DisplayName() string       { return "compact-budget-regression" }
func (p *compactBudgetRegressionProvider) Models() []types.ModelInfo { return nil }
func (p *compactBudgetRegressionProvider) Validate() error           { return nil }

func (p *compactBudgetRegressionProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	delta, err := json.Marshal(map[string]string{"type": "text_delta", "text": p.text})
	if err != nil {
		return nil, err
	}
	stream := make(chan types.SSEEvent, 2)
	stream <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
	stream <- types.SSEEvent{Type: "message_stop"}
	close(stream)
	return stream, nil
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
