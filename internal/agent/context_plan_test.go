package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestContextBudgetErrorDoesNotExposeEstimatorDetails(t *testing.T) {
	const secret = "https://user:secret-token@example.invalid/v1"
	estimator := &phase2TokenEstimator{err: errors.New(secret)}
	_, err := PlanContextRequest(ContextPlanningRequest{
		Profile:   phase2Profile("redacted-estimator-error", 16_384, 2_048),
		Protocol:  harness.WireAnthropicMessages,
		Model:     "fixture-model",
		System:    ContextSystemSections{CorePrefix: "system"},
		Messages:  []types.Message{{Role: "user", Content: mustJSON("task")}},
		MaxTokens: 1_024,
		Estimator: estimator,
	})
	if err == nil {
		t.Fatal("expected estimator failure")
	}
	if got := err.Error(); got != "token_estimation_failed" || strings.Contains(got, secret) {
		t.Fatalf("safe error = %q", got)
	}
	if !errors.Is(err, estimator.err) {
		t.Fatal("typed error did not retain its internal cause")
	}
}

type phase2TokenEstimator struct {
	requests []ContextEstimateRequest
	estimate func(ContextEstimateRequest) TokenEstimate
	err      error
}

func (e *phase2TokenEstimator) EstimateTokens(input ContextEstimateRequest) (TokenEstimate, error) {
	e.requests = append(e.requests, input)
	if e.err != nil {
		return TokenEstimate{}, e.err
	}
	if e.estimate != nil {
		return e.estimate(input), nil
	}
	return TokenEstimate{InputTokens: 1, Source: "test", Confidence: "exact"}, nil
}

func phase2Profile(id string, window, output int) harness.HarnessProfile {
	return harness.MustResolveProfile(harness.ProfileSpec{
		ID:            id,
		ContextWindow: window,
		OutputReserve: output,
		WirePolicy:    harness.WireAnthropicMessages,
	})
}

func TestContextPlanEstimatorReceivesEveryCompletedRequestSurface(t *testing.T) {
	estimator := &phase2TokenEstimator{estimate: func(input ContextEstimateRequest) TokenEstimate {
		return TokenEstimate{
			InputTokens:            1_000,
			SystemTokens:           300,
			MessageTokens:          200,
			ToolTokens:             400,
			MetadataTokens:         100,
			ProtocolOverheadTokens: 123,
			Source:                 "fixture-tokenizer",
			Confidence:             "exact",
		}
	}}
	temperature := 0.25
	planned, err := PlanContextRequest(ContextPlanningRequest{
		Profile:  phase2Profile("surface-test", 32_768, 4_096),
		Protocol: harness.WireAnthropicMessages,
		Model:    "surface-model",
		System: ContextSystemSections{
			CorePrefix:    "BASE PROJECT SKILLS",
			RAGContext:    " RAG-SENTINEL",
			MemoryContext: " MEMORY-SENTINEL",
			CoreSuffix:    " WORKSTREAM PROMPT-SUFFIX PLAN-ANCHOR",
		},
		Messages: []types.Message{{Role: "user", Content: mustJSON("FULL-MESSAGE-SENTINEL")}},
		Tools: []types.ToolDef{{
			Name:        "SurfaceTool",
			Description: "TOOL-DESCRIPTION-SENTINEL",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"schemaSentinel":{"type":"string"}}}`),
		}},
		MaxTokens:          2_048,
		Temperature:        &temperature,
		Estimator:          estimator,
		SafetyMarginTokens: 777,
	})
	if err != nil {
		t.Fatalf("PlanContextRequest() error = %v", err)
	}
	if len(estimator.requests) != 1 {
		t.Fatalf("estimator calls = %d, want 1", len(estimator.requests))
	}
	captured := estimator.requests[0]
	encoded, _ := json.Marshal(captured.Request)
	for _, sentinel := range []string{
		"BASE", "PROJECT", "SKILLS", "RAG-SENTINEL", "MEMORY-SENTINEL",
		"WORKSTREAM", "PROMPT-SUFFIX", "PLAN-ANCHOR", "FULL-MESSAGE-SENTINEL",
		"TOOL-DESCRIPTION-SENTINEL", "schemaSentinel",
	} {
		if !strings.Contains(string(encoded), sentinel) {
			t.Errorf("completed estimator request missing %q: %s", sentinel, encoded)
		}
	}
	if captured.Protocol != harness.WireAnthropicMessages {
		t.Fatalf("protocol = %q", captured.Protocol)
	}
	plan := planned.Plan
	if plan.ContextWindowTokens != 32_768 || plan.ModelOutputLimitTokens != 4_096 {
		t.Fatalf("model limits = window %d output %d", plan.ContextWindowTokens, plan.ModelOutputLimitTokens)
	}
	if plan.RequestedOutputTokens != 2_048 || plan.SafetyMarginTokens != 777 || plan.ProtocolOverheadTokens != 123 {
		t.Fatalf("reservations = %#v", plan)
	}
	if want := 1_000 + 123 + 2_048 + 777; plan.RequiredTokens != want {
		t.Fatalf("required tokens = %d, want %d", plan.RequiredTokens, want)
	}
}

func TestContextPlanThresholdsFor16K32K128KAndOutputReserve(t *testing.T) {
	for _, window := range []int{16_384, 32_768, 131_072} {
		window := window
		t.Run(strconv.Itoa(window), func(t *testing.T) {
			const output = 2_048
			const protocol = 100
			const safety = 512
			usable := window - output - protocol - safety
			for _, fixture := range []struct {
				name  string
				input int
				fits  bool
			}{
				{name: "at-boundary", input: usable, fits: true},
				{name: "one-over", input: usable + 1, fits: false},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					estimator := &phase2TokenEstimator{estimate: func(ContextEstimateRequest) TokenEstimate {
						return TokenEstimate{
							InputTokens:            fixture.input,
							ProtocolOverheadTokens: protocol,
							Source:                 "exact-fixture",
							Confidence:             "exact",
						}
					}}
					plan, _, err := CalculateContextPlan(ContextPlanningRequest{
						Profile:            phase2Profile("threshold", window, output),
						Model:              "threshold-model",
						System:             ContextSystemSections{CorePrefix: "system"},
						Messages:           []types.Message{{Role: "user", Content: mustJSON("task")}},
						MaxTokens:          output,
						Estimator:          estimator,
						SafetyMarginTokens: safety,
					})
					if err != nil {
						t.Fatalf("CalculateContextPlan() error = %v", err)
					}
					if plan.Fits != fixture.fits {
						t.Fatalf("Fits = %v, want %v (plan=%#v)", plan.Fits, fixture.fits, plan)
					}
					if plan.UsableInputTokens != usable {
						t.Fatalf("usable = %d, want %d", plan.UsableInputTokens, usable)
					}
				})
			}
		})
	}

	_, _, err := CalculateContextPlan(ContextPlanningRequest{
		Profile:   phase2Profile("invalid-output", 16_384, 2_048),
		Model:     "model",
		System:    ContextSystemSections{CorePrefix: "system"},
		Messages:  []types.Message{{Role: "user", Content: mustJSON("task")}},
		MaxTokens: 2_049,
	})
	if err == nil || contextBudgetErrorCode(err, "") != "invalid_output_reserve" {
		t.Fatalf("output over model/profile reserve error = %v", err)
	}
}

func TestContextPlannerReductionOrderAndCatalogPairing(t *testing.T) {
	estimator := &phase2TokenEstimator{estimate: func(input ContextEstimateRequest) TokenEstimate {
		encoded, _ := json.Marshal(input.Request)
		text := string(encoded)
		tokens := 4_000
		if strings.Contains(text, "RAG_MARK") {
			tokens += 4_000
		}
		if strings.Contains(text, "MEMORY_MARK") {
			tokens += 4_000
		}
		tokens += len(input.Request.Tools) * 1_000
		if strings.Contains(text, "Historical tool result reference") {
			tokens += 200
		} else if strings.Contains(text, "RAW_LARGE_RESULT") {
			tokens += 5_000
		}
		return TokenEstimate{InputTokens: tokens, Source: "reduction-fixture", Confidence: "exact"}
	}}
	messages := phase2HistoryWithOldLargePair("RAW_LARGE_RESULT", 8_000)
	tools := []types.ToolDef{
		{Name: "AlphaOptional", Description: "alpha", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "BetaOptional", Description: "beta", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "GammaOptional", Description: "gamma", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	planned, err := PlanContextRequest(ContextPlanningRequest{
		Profile: phase2Profile("reduction-order", 10_000, 1_000),
		Model:   "model",
		System: ContextSystemSections{
			CorePrefix:    "core",
			RAGContext:    strings.Repeat("RAG_MARK ", 500),
			MemoryContext: strings.Repeat("MEMORY_MARK ", 500),
		},
		Messages:           messages,
		Tools:              tools,
		MaxTokens:          1_000,
		Estimator:          estimator,
		SafetyMarginTokens: 512,
		Task:               "alpha task",
	})
	if err != nil {
		t.Fatalf("PlanContextRequest() error = %v", err)
	}
	if !planned.Plan.Fits {
		t.Fatalf("planned request did not fit: %#v", planned.Plan)
	}
	kinds := make([]ContextReductionKind, 0, len(planned.Plan.Reductions))
	for _, reduction := range planned.Plan.Reductions {
		kinds = append(kinds, reduction.Kind)
	}
	wantPrefix := []ContextReductionKind{
		ContextReductionRAGShrink,
		ContextReductionRAGRemove,
		ContextReductionMemoryShrink,
		ContextReductionMemoryRemove,
		ContextReductionToolPrune,
	}
	if len(kinds) < len(wantPrefix)+1 || !reflect.DeepEqual(kinds[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("reduction order = %v, want prefix %v", kinds, wantPrefix)
	}
	if kinds[len(kinds)-1] != ContextReductionToolResultBound {
		t.Fatalf("last reduction = %q, want %q (all=%v)", kinds[len(kinds)-1], ContextReductionToolResultBound, kinds)
	}
	if !reflect.DeepEqual(planned.Tools, planned.Request.Tools) {
		t.Fatal("planned catalog and request tool schemas diverged")
	}
	for _, kept := range planned.Tools {
		found := false
		for _, original := range tools {
			if reflect.DeepEqual(kept, original) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("planner modified/invented tool definition: %#v", kept)
		}
	}
}

func phase2HistoryWithOldLargePair(marker string, size int) []types.Message {
	messages := []types.Message{{Role: "user", Content: mustJSON("objective")}}
	for index := 0; index < 5; index++ {
		id := "tool-" + string(rune('a'+index))
		result := "small"
		if index == 0 {
			result = marker + strings.Repeat("x", size)
		}
		messages = append(messages,
			types.Message{Role: "assistant", Content: mustJSON([]map[string]any{{
				"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"file_path": "file.txt"},
			}})},
			types.Message{Role: "user", Content: mustJSON([]map[string]any{{
				"type": "tool_result", "tool_use_id": id, "content": result, "is_error": false,
			}})},
		)
	}
	return messages
}
