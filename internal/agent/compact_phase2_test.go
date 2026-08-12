package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestBoundHistoricalToolResultsPreservesRecentAndOrphanPairsAndRedactsStoreReference(t *testing.T) {
	messages := []types.Message{{Role: "user", Content: mustJSON("objective")}}
	messages = append(messages, phase2ToolPairMessages("old", "OldTool", strings.Repeat("old-result ", 700))...)
	orphanContent := strings.Repeat("orphan-result ", 700)
	messages = append(messages, types.Message{Role: "user", Content: mustJSON([]map[string]any{{
		"type": "tool_result", "tool_use_id": "orphan", "content": orphanContent,
	}})})
	for index := 0; index < 5; index++ {
		id := "recent-" + string(rune('a'+index))
		content := "small"
		if index == 4 {
			content = strings.Repeat("CURRENT_RESULT_MUST_STAY ", 400)
		}
		messages = append(messages, phase2ToolPairMessages(id, "Read", content)...)
	}

	store := phase2UntrustedResultStore{
		reference: strings.Repeat("password=SUPERSECRET ", 400) + "tail",
	}
	bounded, stats := BoundHistoricalToolResults(messages, store)
	if stats.Replaced != 1 {
		t.Fatalf("replaced = %d, want only the old paired result", stats.Replaced)
	}
	old := phase2ToolResultContent(t, bounded, "old")
	if len(old) > historicalToolResultThreshold+128 {
		t.Fatalf("stored reference was not bounded: %d bytes", len(old))
	}
	if strings.Contains(old, "SUPERSECRET") || !strings.Contains(old, "[REDACTED]") {
		t.Fatalf("stored reference was not redacted: %q", old[:minInt(len(old), 300)])
	}
	if got := phase2ToolResultContent(t, bounded, "orphan"); got != orphanContent {
		t.Fatal("orphan/unresolved tool_result was modified")
	}
	current := phase2ToolResultContent(t, bounded, "recent-e")
	if !strings.Contains(current, "CURRENT_RESULT_MUST_STAY") {
		t.Fatal("most recent paired tool result was modified")
	}
	if !sameStringSet(toolUseIDs(bounded[len(bounded)-2].Content), toolResultIDs(bounded[len(bounded)-1].Content)) {
		t.Fatal("current tool_use/tool_result pair identity changed")
	}
}

func TestDeterministicCompactionPreservesPlanAcceptanceRecentPairsAndHidesRawPayload(t *testing.T) {
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "implement planner api_key=sk-123456789012345678901234",
		CurrentStep:      "compact history",
		RemainingSteps:   []string{"run focused tests"},
		DefinitionOfDone: []string{"tool pairs remain valid", "tests pass"},
		Revision:         7,
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := []types.Message{{Role: "user", Content: mustJSON("objective api_key=sk-123456789012345678901234")}}
	for index := 0; index < 6; index++ {
		id := "pair-" + string(rune('a'+index))
		input := map[string]any{"file_path": "file.go", "payload": "RAW_TOOL_INPUT_SECRET"}
		result := "RAW_RESULT_SECRET"
		if index > 0 {
			input["payload"] = "ordinary"
			result = "ok"
		}
		messages = append(messages,
			types.Message{Role: "assistant", Content: mustJSON([]map[string]any{{
				"type": "tool_use", "id": id, "name": "Edit", "input": input,
			}})},
			types.Message{Role: "user", Content: mustJSON([]map[string]any{{
				"type": "tool_result", "tool_use_id": id, "content": result, "is_error": false,
			}})},
		)
	}
	state := CompactionState{
		Objective:   "objective api_key=sk-123456789012345678901234",
		PlanAnchor:  &anchor,
		EditedFiles: []string{"file.go"},
	}
	first := BuildDeterministicCompaction(messages, state)
	second := BuildDeterministicCompaction(messages, state)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("deterministic compaction changed across identical inputs")
	}
	if first.Snapshot.Plan.Revision != 7 || !reflect.DeepEqual(first.Snapshot.Plan.Acceptance, []string{"tool pairs remain valid", "tests pass"}) {
		t.Fatalf("plan/acceptance not preserved: %#v", first.Snapshot.Plan)
	}
	encoded, _ := json.Marshal(first.Snapshot)
	for _, raw := range []string{"RAW_TOOL_INPUT_SECRET", "RAW_RESULT_SECRET", "sk-123456789012345678901234"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("snapshot leaked raw payload %q: %s", raw, encoded)
		}
	}
	if !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("snapshot objective secret was not redacted: %s", encoded)
	}
	for _, id := range []string{"pair-c", "pair-d", "pair-e", "pair-f"} {
		if !phase2MessagesContainPair(first.Messages, id) {
			t.Fatalf("recent pair %q was not preserved atomically", id)
		}
	}
}

func TestLLMCompactionFailureAndDeadlineUseDeterministicFallback(t *testing.T) {
	messages := phase2HistoryWithOldLargePair("history", 100)
	fallback := BuildDeterministicCompaction(messages, CompactionState{Objective: "objective"})
	failing := &phase2CompactionProvider{streamErr: errors.New("summary unavailable")}
	first := TryLLMCompaction(context.Background(), failing, "model", messages, CompactionState{Objective: "objective"}, fallback)
	second := TryLLMCompaction(context.Background(), failing, "model", messages, CompactionState{Objective: "objective"}, fallback)
	if !first.UsedFallback || first.Snapshot.LLMFallbackReason != "stream_error" {
		t.Fatalf("failure did not select fallback: %#v", first.Snapshot)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("LLM failure fallback was not deterministic")
	}

	blocking := &phase2CompactionProvider{block: true}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	timed := TryLLMCompaction(ctx, blocking, "model", messages, CompactionState{Objective: "objective"}, fallback)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("compaction ignored injected deadline: %s", elapsed)
	}
	if !timed.UsedFallback || timed.Snapshot.LLMFallbackReason != "timeout" {
		t.Fatalf("deadline fallback = %#v", timed.Snapshot)
	}
}

func TestCompactionHooksExecuteExactlyOnceAndCannotBypassAttempt(t *testing.T) {
	runner := &phase2HookRunner{}
	attempts := 0
	ctx := context.WithValue(context.Background(), phase2HookContextKey{}, "compaction-run")
	withCompactionHooks(ctx, runner, map[string]string{"MESSAGE_COUNT": "12"}, func() {
		attempts++
	})
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if runner.counts[hooks.HookPreCompact] != 1 || runner.counts[hooks.HookPostCompact] != 1 {
		t.Fatalf("hook counts = %#v", runner.counts)
	}
	if runner.contextValue != "compaction-run" {
		t.Fatalf("hook context value = %q", runner.contextValue)
	}
}

func TestCompactionRecalculatesCompletedRequestWithRAG(t *testing.T) {
	estimator := &phase2TokenEstimator{estimate: func(input ContextEstimateRequest) TokenEstimate {
		encoded, _ := json.Marshal(input.Request.Messages)
		tokens := 14_000
		if strings.Contains(string(encoded), "Structured Conversation State") {
			tokens = 3_000
		}
		if strings.Contains(string(input.Request.System), "RAG_RECALC_SENTINEL") {
			tokens += 500
		}
		return TokenEstimate{InputTokens: tokens, Source: "rag-recalc", Confidence: "exact"}
	}}
	profile := phase2Profile("rag-recalc", 16_384, 2_048)
	base := ContextPlanningRequest{
		Profile:  profile,
		Protocol: harness.WireAnthropicMessages,
		Model:    "model",
		System: ContextSystemSections{
			CorePrefix: "core",
			RAGContext: " RAG_RECALC_SENTINEL",
		},
		Messages:           phase2HistoryWithOldLargePair("history", 100),
		Tools:              []types.ToolDef{{Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:          2_048,
		Estimator:          estimator,
		SafetyMarginTokens: 512,
	}
	initialPlan, initialRequest, err := CalculateContextPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if initialPlan.Fits {
		t.Fatal("fixture must start over budget")
	}
	planned := PlannedContext{
		Request: initialRequest, System: base.System, Messages: base.Messages, Tools: base.Tools,
		Plan: initialPlan, NeedsCompaction: true,
	}
	estimator.requests = nil
	provider := &phase2CompactionProvider{streamErr: errors.New("use fallback")}
	outcome := compactPlannedContext(context.Background(), provider, nil, base, planned, CompactionState{Objective: "objective"})
	if outcome.Err != nil || outcome.Blocked || !outcome.Planned.Plan.Fits {
		t.Fatalf("compaction outcome = %#v, err=%v", outcome, outcome.Err)
	}
	if provider.Calls() != 1 {
		t.Fatalf("summarizer provider calls = %d, want 1", provider.Calls())
	}
	if len(estimator.requests) < 2 {
		t.Fatalf("post-compaction estimates = %d, want recalculation", len(estimator.requests))
	}
	for index, request := range estimator.requests {
		if !strings.Contains(string(request.Request.System), "RAG_RECALC_SENTINEL") {
			t.Fatalf("recalculation %d dropped selected RAG: %s", index, request.Request.System)
		}
	}
	if !strings.Contains(string(outcome.Planned.Request.System), "RAG_RECALC_SENTINEL") {
		t.Fatal("final completed request dropped RAG")
	}
}

type phase2UntrustedResultStore struct{ reference string }

func (s phase2UntrustedResultStore) StoreResult(string, string) (string, bool) {
	return s.reference, true
}

type phase2HookContextKey struct{}

type phase2HookRunner struct {
	counts       map[hooks.HookType]int
	contextValue string
}

func (r *phase2HookRunner) ExecuteContext(ctx context.Context, kind hooks.HookType, _ map[string]string) []hooks.HookResult {
	if r.counts == nil {
		r.counts = make(map[hooks.HookType]int)
	}
	r.counts[kind]++
	if value, ok := ctx.Value(phase2HookContextKey{}).(string); ok {
		r.contextValue = value
	}
	// A blocked result is observational for compact hooks and must not skip the
	// actual attempt or turn an over-budget plan into an allowed call.
	return []hooks.HookResult{{Blocked: true, Output: "ignore me"}}
}

type phase2CompactionProvider struct {
	mu        sync.Mutex
	calls     int
	streamErr error
	block     bool
}

func (p *phase2CompactionProvider) Name() string              { return "phase2" }
func (p *phase2CompactionProvider) DisplayName() string       { return "phase2" }
func (p *phase2CompactionProvider) Models() []types.ModelInfo { return nil }
func (p *phase2CompactionProvider) Validate() error           { return nil }
func (p *phase2CompactionProvider) Calls() int                { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }
func (p *phase2CompactionProvider) StreamMessage(ctx context.Context, _ *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	stream := make(chan types.SSEEvent)
	if p.block {
		return stream, nil
	}
	close(stream)
	return stream, nil
}

func phase2ToolPairMessages(id, name, result string) []types.Message {
	return []types.Message{
		{Role: "assistant", Content: mustJSON([]map[string]any{{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{"file_path": "file.txt"},
		}})},
		{Role: "user", Content: mustJSON([]map[string]any{{
			"type": "tool_result", "tool_use_id": id, "content": result, "is_error": false,
		}})},
	}
}

func phase2ToolResultContent(t *testing.T, messages []types.Message, id string) string {
	t.Helper()
	for _, message := range messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			var blockType, useID, content string
			_ = json.Unmarshal(block["type"], &blockType)
			_ = json.Unmarshal(block["tool_use_id"], &useID)
			if blockType == "tool_result" && useID == id {
				_ = json.Unmarshal(block["content"], &content)
				return content
			}
		}
	}
	t.Fatalf("tool result %q not found", id)
	return ""
}

func phase2MessagesContainPair(messages []types.Message, id string) bool {
	foundUse, foundResult := false, false
	for _, message := range messages {
		for _, value := range toolUseIDs(message.Content) {
			foundUse = foundUse || value == id
		}
		for _, value := range toolResultIDs(message.Content) {
			foundResult = foundResult || value == id
		}
	}
	return foundUse && foundResult
}
