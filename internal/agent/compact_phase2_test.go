package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
		reference: strings.Repeat("password=SUPERSECRET🙂 ", 400) + "tail",
	}
	bounded, stats := BoundHistoricalToolResults(messages, store)
	if stats.Replaced != 1 {
		t.Fatalf("replaced = %d, want only the old paired result", stats.Replaced)
	}
	old := phase2ToolResultContent(t, bounded, "old")
	if len(old) > historicalToolResultThreshold+128 {
		t.Fatalf("stored reference was not bounded: %d bytes", len(old))
	}
	if strings.Contains(old, "SUPERSECRET") || !strings.Contains(old, "[REDACTED]") ||
		!utf8.ValidString(old) || len(old) > historicalToolResultThreshold {
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

func TestBoundHistoricalToolResultsFallbackReferenceKeepsUTF8HeadAndTail(t *testing.T) {
	blob := "BLOB-HEAD-🌱" + strings.Repeat("중간자료🙂", 250) + "MIDDLE-BLOB-EXCLUDED" + strings.Repeat("중간자료🙂", 250) + "BLOB-TAIL-🚀"
	messages := []types.Message{{Role: "user", Content: mustJSON("inspect archived output")}}
	messages = append(messages, phase2ToolPairMessages("old-unicode", "Read", blob)...)
	for index := 0; index < compactionRecentUnits; index++ {
		messages = append(messages, phase2ToolPairMessages(fmt.Sprintf("recent-%d", index), "Read", "ok")...)
	}

	bounded, stats := BoundHistoricalToolResults(messages, nil)
	reference := phase2ToolResultContent(t, bounded, "old-unicode")
	if stats.Replaced != 1 || len(reference) > historicalToolResultThreshold || !utf8.ValidString(reference) {
		t.Fatalf("bounded Unicode reference stats=%+v bytes=%d valid_utf8=%v", stats, len(reference), utf8.ValidString(reference))
	}
	if !strings.Contains(reference, "BLOB-HEAD-🌱") || !strings.Contains(reference, "BLOB-TAIL-🚀") ||
		strings.Contains(reference, "MIDDLE-BLOB-EXCLUDED") || strings.Contains(reference, blob) {
		t.Fatalf("reference did not retain bounded head/tail without the raw blob: %q", reference)
	}
	if !strings.Contains(reference, "content-digest=sha256:"+strings.TrimPrefix(sha256String(blob), "sha256:")) || !phase2MessagesContainPair(bounded, "old-unicode") {
		t.Fatalf("reference digest/pair identity was lost: %q", reference)
	}
	if strings.Contains(reference, "tool-result://") || strings.Contains(reference, "LoadToolResult") ||
		!strings.Contains(reference, "reload=unavailable") {
		t.Fatalf("unstored preview falsely advertises a reloadable blob: %q", reference)
	}
	state := BuildDeterministicCompaction(bounded, CompactionState{})
	if len(state.Snapshot.DurableReferences) != 0 {
		t.Fatalf("non-durable preview was recorded as loadable durable reference: %v", state.Snapshot.DurableReferences)
	}
}

func TestBoundHistoricalToolResultsUsesStrict4096ByteThreshold(t *testing.T) {
	for _, size := range []int{historicalToolResultThreshold, historicalToolResultThreshold + 1} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			messages := []types.Message{{Role: "user", Content: mustJSON("inspect archived output")}}
			messages = append(messages, phase2ToolPairMessages("old", "Read", strings.Repeat("x", size))...)
			for index := 0; index < compactionRecentUnits; index++ {
				messages = append(messages, phase2ToolPairMessages(fmt.Sprintf("recent-%d", index), "Read", "ok")...)
			}
			bounded, stats := BoundHistoricalToolResults(messages, nil)
			if size == historicalToolResultThreshold {
				if stats.Replaced != 0 || phase2ToolResultContent(t, bounded, "old") != strings.Repeat("x", size) {
					t.Fatalf("exact threshold result was replaced: stats=%+v", stats)
				}
				return
			}
			reference := phase2ToolResultContent(t, bounded, "old")
			if stats.Replaced != 1 || len(reference) > historicalToolResultThreshold ||
				strings.Contains(reference, "tool-result://") || !strings.Contains(reference, "reload=unavailable") {
				t.Fatalf("threshold+1 result did not become a bounded, non-loadable preview: stats=%+v ref=%q", stats, reference)
			}
		})
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

func TestDeterministicCompactionPreservesLatestInstructionsAndDurableWorkflowState(t *testing.T) {
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "Canonical workflow objective",
		CurrentStep:      "verify the selected stage",
		RemainingSteps:   []string{"record accepted evidence"},
		DefinitionOfDone: []string{"the authorization boundary stays intact"},
		Revision:         7,
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := []types.Message{
		{Role: "user", Content: mustJSON("Original request: inspect the old token flow.")},
		{Role: "assistant", Content: mustJSON("I will inspect the token flow.")},
		{Role: "user", Content: mustJSON("Constraint: Keep the API authorization boundary unchanged.")},
		{Role: "assistant", Content: mustJSON("Understood.")},
		{Role: "user", Content: mustJSON("Correction: focus on the current token verifier, not the legacy handler.")},
		{Role: "assistant", Content: mustJSON("I will focus on the current verifier.")},
		{Role: "user", Content: mustJSON("Explain how the verified evidence reaches the current stage.")},
	}
	for index := 0; index < 5; index++ {
		messages = append(messages, phase2ToolPairMessages(
			"workflow-pair-"+string(rune('a'+index)),
			"Read",
			"evidence "+string(rune('a'+index)),
		)...)
	}
	evidence := NewEvidenceLedger("verify current evidence", EvidencePolicyConfig{Policy: EvidencePolicyMeasure})
	evidence.ObserveAutoVerify("go test ./internal/agent: PASS", false, true)
	state := CompactionState{
		Objective:  "Explain how the verified evidence reaches the current stage.",
		PlanAnchor: &anchor,
		Evidence:   evidence,
		UserInstructions: []string{
			"Original request: inspect the old token flow.",
			"Constraint: Keep the API authorization boundary unchanged.",
			"Correction: focus on the current token verifier, not the legacy handler.",
			"Explain how the verified evidence reaches the current stage.",
		},
		Context: CompactionContext{
			WorkstreamID:            "ws-auth",
			WorkstreamObjective:     "Retain safe authorization behavior.",
			Constraints:             []string{"Preserve auth checks.", "Do not change the session boundary."},
			Decisions:               []string{"Keep the stored plan canonical.", "Require verification before completion."},
			LastVerificationStatus:  "passed",
			LastVerificationSource:  "workstream",
			LastVerificationSummary: "The previous verification completed.",
			PlanID:                  "plan-auth",
			PlanRevision:            7,
			PlanStateRevision:       19,
			StageID:                 "verify",
			PlanEvidence: []CompactionPlanEvidence{{
				StageID:            "research",
				ReceiptDigest:      strings.Repeat("a", 64),
				CriteriaDigest:     "sha256:" + strings.Repeat("b", 64),
				VerificationStatus: "passed",
				CompletionStatus:   "completed",
			}},
		},
	}

	result := BuildDeterministicCompaction(messages, state)
	snapshot := result.Snapshot
	if snapshot.Objective != state.Objective || snapshot.Plan.Objective != anchor.Objective() {
		t.Fatalf("current and canonical plan objectives were conflated: objective=%q plan=%q", snapshot.Objective, snapshot.Plan.Objective)
	}
	if snapshot.WorkstreamID != "ws-auth" || snapshot.WorkstreamObjective != "Retain safe authorization behavior." {
		t.Fatalf("workstream identity/objective = %+v", snapshot)
	}
	if !strings.Contains(strings.Join(snapshot.UserInstructions, "\n"), "Keep the API authorization boundary unchanged") ||
		!strings.Contains(strings.Join(snapshot.UserInstructions, "\n"), "Correction: focus on the current token verifier") ||
		snapshot.UserInstructions[len(snapshot.UserInstructions)-1] != state.Objective {
		t.Fatalf("ordered user instructions were not preserved: %q", snapshot.UserInstructions)
	}
	if !strings.Contains(strings.Join(snapshot.Constraints, "\n"), "session boundary") ||
		!strings.Contains(strings.Join(snapshot.Decisions, "\n"), "Require verification") {
		t.Fatalf("latest workstream constraints/decisions were not preserved: constraints=%v decisions=%v", snapshot.Constraints, snapshot.Decisions)
	}
	if snapshot.Plan.ID != "plan-auth" || snapshot.Plan.WorkstreamID != "ws-auth" || snapshot.Plan.Revision != 7 ||
		snapshot.Plan.StateRevision != 19 || snapshot.Plan.StageID != "verify" {
		t.Fatalf("canonical plan reference = %+v", snapshot.Plan)
	}
	if len(snapshot.PlanEvidence) != 1 || snapshot.PlanEvidence[0].ReceiptDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("plan acceptance evidence reference = %+v", snapshot.PlanEvidence)
	}
	if snapshot.LastVerification == nil || snapshot.LastVerification.Status != "passed" ||
		snapshot.LastVerification.Source != "workstream" || snapshot.LastVerification.SummaryDigest != sha256String(state.Context.LastVerificationSummary) {
		t.Fatalf("last verification reference = %+v", snapshot.LastVerification)
	}
	if snapshot.EvidenceMode == "" || len(snapshot.Evidence) != 1 || snapshot.Evidence[0].Status != "passed" {
		t.Fatalf("run-local verification evidence was not captured: %+v", snapshot)
	}
	encodedMessages := string(mustJSON(result.Messages))
	if !strings.Contains(encodedMessages, "Keep the API authorization boundary unchanged") ||
		!strings.Contains(encodedMessages, "Correction: focus on the current token verifier") ||
		!strings.Contains(encodedMessages, state.Objective) {
		t.Fatalf("compacted messages omitted explicit user instructions: %s", encodedMessages)
	}
	if !reflect.DeepEqual(snapshot.UserInstructions, sanitizeUserInstructionList(state.UserInstructions)) {
		t.Fatalf("input/output bounded instructions differ: input=%q snapshot=%q", state.UserInstructions, snapshot.UserInstructions)
	}

	continuedMessages := append(cloneMessages(result.Messages), types.Message{
		Role: "user", Content: mustJSON("Follow-up correction: report the verifier's current file and line."),
	})
	continuedInstructions := userInstructionTexts(continuedMessages)
	for _, required := range []string{
		"Keep the API authorization boundary unchanged",
		"Correction: focus on the current token verifier",
		"Follow-up correction: report the verifier's current file and line.",
	} {
		if !strings.Contains(strings.Join(continuedInstructions, "\n"), required) {
			t.Fatalf("second compaction input lost prior instruction %q: %q", required, continuedInstructions)
		}
	}
	secondState := state
	secondState.Objective = "Follow-up correction: report the verifier's current file and line."
	secondState.UserInstructions = continuedInstructions
	second := BuildDeterministicCompaction(continuedMessages, secondState)
	secondInstructions := strings.Join(second.Snapshot.UserInstructions, "\n")
	for _, required := range []string{
		"Keep the API authorization boundary unchanged",
		"Correction: focus on the current token verifier",
		"Follow-up correction: report the verifier's current file and line.",
	} {
		if !strings.Contains(secondInstructions, required) {
			t.Fatalf("second compaction snapshot lost instruction %q: %q", required, secondInstructions)
		}
	}
}

func TestCompactionPreservesUTF8InstructionTailAndSnapshotBounds(t *testing.T) {
	longInstruction := "Apply the current policy: " + strings.Repeat("retain the boundary🙂 ", 70) +
		"MIDDLE-ONLY-OMIT-THIS-PART " + strings.Repeat("older clause🙂 ", 70) +
		"Correction at the end: keep the authorization boundary unchanged."
	messages := []types.Message{
		{Role: "user", Content: mustJSON(longInstruction)},
		{Role: "assistant", Content: mustJSON("Acknowledged.")},
	}
	for index := 0; index < 5; index++ {
		messages = append(messages, phase2ToolPairMessages(fmt.Sprintf("utf8-pair-%d", index), "Read", "small")...)
	}
	messages = append(messages, types.Message{Role: "user", Content: mustJSON("Where is the current verifier?")})
	longConstraint := "Policy: " + strings.Repeat("keep existing behavior🙂 ", 30) +
		"Constraint correction at end: preserve existing API boundaries."

	instructionsBefore := userInstructionTexts(messages)
	result := BuildDeterministicCompaction(messages, CompactionState{
		Objective:        "Where is the current verifier?",
		UserInstructions: instructionsBefore,
		Context:          CompactionContext{Constraints: []string{longConstraint}},
	})
	if !reflect.DeepEqual(result.Snapshot.UserInstructions, sanitizeUserInstructionList(instructionsBefore)) {
		t.Fatalf("compaction changed bounded instruction sequence: before=%q after=%q", instructionsBefore, result.Snapshot.UserInstructions)
	}
	longSnapshot := result.Snapshot.UserInstructions[0]
	if len(longSnapshot) > 600 || !utf8.ValidString(longSnapshot) ||
		!strings.HasPrefix(longSnapshot, "Apply the current policy:") ||
		!strings.Contains(longSnapshot, "[middle omitted]") ||
		!strings.HasSuffix(longSnapshot, "Correction at the end: keep the authorization boundary unchanged.") {
		t.Fatalf("long UTF-8 correction was not bounded with its important tail: bytes=%d valid=%v text=%q", len(longSnapshot), utf8.ValidString(longSnapshot), longSnapshot)
	}
	if len(result.Snapshot.Constraints) != 1 || len(result.Snapshot.Constraints[0]) > 300 ||
		!utf8.ValidString(result.Snapshot.Constraints[0]) ||
		!strings.HasSuffix(result.Snapshot.Constraints[0], "Constraint correction at end: preserve existing API boundaries.") {
		t.Fatalf("long Workstream constraint lost its bounded correction tail: %+v", result.Snapshot.Constraints)
	}
	if !reflect.DeepEqual(result.Snapshot.UserInstructions, userInstructionTexts(result.Messages)) {
		t.Fatalf("compressed transcript did not reconstruct the same bounded directions: before=%q after=%q", result.Snapshot.UserInstructions, userInstructionTexts(result.Messages))
	}
	continuedMessages := append(cloneMessages(result.Messages), types.Message{
		Role: "user", Content: mustJSON("추가 정정: 반드시 authorization boundary를 유지할 것."),
	})
	continuedInstructions := userInstructionTexts(continuedMessages)
	second := BuildDeterministicCompaction(continuedMessages, CompactionState{
		Objective:        "추가 정정: 반드시 authorization boundary를 유지할 것.",
		UserInstructions: continuedInstructions,
	})
	secondInstructions := strings.Join(second.Snapshot.UserInstructions, "\n")
	if !strings.Contains(secondInstructions, "Correction at the end: keep the authorization boundary unchanged.") ||
		!strings.Contains(secondInstructions, "추가 정정: 반드시 authorization boundary를 유지할 것.") {
		t.Fatalf("second bounded compaction lost the UTF-8 correction tail or latest direction: %q", second.Snapshot.UserInstructions)
	}
	if strings.Contains(string(mustJSON(result.Messages)), "MIDDLE-ONLY-OMIT-THIS-PART") {
		t.Fatal("old oversized instruction body survived outside its bounded structured reference")
	}

	for maxBytes := 0; maxBytes <= len("A한🙂Z")+1; maxBytes++ {
		prefix := contextUTF8Prefix("A한🙂Z", maxBytes)
		suffix := contextUTF8Suffix("A한🙂Z", maxBytes)
		if len(prefix) > maxBytes || len(suffix) > maxBytes || !utf8.ValidString(prefix) || !utf8.ValidString(suffix) ||
			!strings.HasPrefix("A한🙂Z", prefix) || !strings.HasSuffix("A한🙂Z", suffix) {
			t.Fatalf("UTF-8 boundary max=%d prefix=%q suffix=%q", maxBytes, prefix, suffix)
		}
	}
}

func TestUserInstructionBoundKeepsOriginalAndSevenMostRecent(t *testing.T) {
	var messages []types.Message
	for index := 0; index < maxCompactionUserInstructions+1; index++ {
		messages = append(messages, types.Message{Role: "user", Content: mustJSON(fmt.Sprintf("direction-%02d", index+1))})
		messages = append(messages, types.Message{Role: "assistant", Content: mustJSON("acknowledged")})
	}
	instructions := userInstructionTexts(messages)
	if len(instructions) != maxCompactionUserInstructions || instructions[0] != "direction-01" ||
		instructions[1] != "direction-03" || instructions[len(instructions)-1] != "direction-09" {
		t.Fatalf("bounded instructions should retain the original plus latest seven, got %q", instructions)
	}
	if strings.Contains(strings.Join(instructions, "\n"), "direction-02") {
		t.Fatal("bounded instruction list unexpectedly retained a middle item instead of newest corrections")
	}
	result := BuildDeterministicCompaction(messages, CompactionState{UserInstructions: instructions})
	if !reflect.DeepEqual(result.Snapshot.UserInstructions, instructions) {
		t.Fatalf("snapshot changed the explicitly bounded instruction set: %q", result.Snapshot.UserInstructions)
	}
}

func TestCompactionNarrativeSourcePrefersRecentHistory(t *testing.T) {
	messages := []types.Message{{Role: "user", Content: mustJSON("oldest objective")}}
	for index := 0; index < 28; index++ {
		role := "assistant"
		if index%2 == 0 {
			role = "user"
		}
		messages = append(messages, types.Message{
			Role:    role,
			Content: mustJSON(fmt.Sprintf("history-%02d %s", index, strings.Repeat("older-context ", 60))),
		})
	}
	messages = append(messages, types.Message{Role: "user", Content: mustJSON("LATEST-CORRECTION-MUST-REACH-SUMMARY")})

	source := compactionNarrativeSource(messages)
	if !strings.Contains(source, "LATEST-CORRECTION-MUST-REACH-SUMMARY") {
		t.Fatalf("bounded narrative source omitted latest user correction: %s", source)
	}
	if strings.Contains(source, "oldest objective") {
		t.Fatalf("bounded narrative source spent its budget on stale objective: %s", source)
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
