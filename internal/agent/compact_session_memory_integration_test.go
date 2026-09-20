package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestPlanContextCompactionKeepsSessionMemoryBlobLoadableAtBoundary(t *testing.T) {
	for _, size := range []int{historicalToolResultThreshold, historicalToolResultThreshold + 1} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			const sessionID = "context-compaction-blob-boundary"
			store, err := OpenSessionMemory(t.TempDir(), sessionID)
			if err != nil {
				t.Fatalf("OpenSessionMemory() error = %v", err)
			}

			content := compactSessionMemoryBoundaryPayload(size)
			messages := compactSessionMemoryBoundaryMessages(content)
			estimator := &compactSessionMemoryBoundaryEstimator{}
			planned, err := PlanContextRequest(ContextPlanningRequest{
				Profile:         harness.MustResolveProfile(harness.ProfileSpec{ID: "session-blob-boundary", ContextWindow: 4_096, OutputReserve: 1_024, WirePolicy: harness.WireAnthropicMessages}),
				Model:           "session-blob-boundary-model",
				System:          ContextSystemSections{CorePrefix: "system"},
				Messages:        messages,
				MaxTokens:       512,
				Estimator:       estimator,
				ToolResultStore: store,
			})
			if err != nil {
				t.Fatalf("PlanContextRequest() error = %v", err)
			}

			if size == historicalToolResultThreshold {
				if !planned.NeedsCompaction || contextPlanHasReduction(planned.Plan, ContextReductionToolResultBound) {
					t.Fatalf("exact threshold must remain inline for later compaction: needs_compaction=%v reductions=%+v", planned.NeedsCompaction, planned.Plan.Reductions)
				}
				if results := store.ListResults(); len(results) != 0 {
					t.Fatalf("exact threshold unexpectedly stored result blobs: %+v", results)
				}
				return
			}

			if !planned.Plan.Fits || planned.NeedsCompaction || !contextPlanHasReduction(planned.Plan, ContextReductionToolResultBound) {
				t.Fatalf("threshold+1 result was not durably bounded into a fitting request: plan=%+v needs_compaction=%v", planned.Plan, planned.NeedsCompaction)
			}
			if len(store.ListResults()) != 1 {
				t.Fatalf("durable result count = %d, want 1", len(store.ListResults()))
			}

			digest := strings.TrimPrefix(sha256String(content), "sha256:")
			reference := compactSessionMemoryOldResult(t, planned.Messages)
			if !strings.Contains(reference, "tool-result://sha256:"+digest) || !strings.Contains(reference, "result_"+digest) {
				t.Fatalf("planned historical result lost its content-addressed load reference: %q", reference)
			}

			compacted := BuildDeterministicCompaction(planned.Messages, CompactionState{
				Objective: "inspect the retained historical result",
			})
			if !stringSliceContains(compacted.Snapshot.DurableReferences, "sha256:"+digest) {
				t.Fatalf("compaction snapshot omitted durable result digest: %+v", compacted.Snapshot.DurableReferences)
			}
			if !strings.Contains(string(mustJSON(compacted.Messages)), "sha256:"+digest) {
				t.Fatal("compacted conversation omitted the durable result digest")
			}

			loadInput, err := json.Marshal(map[string]any{
				"id":     "result_" + digest,
				"offset": 0,
				"limit":  len(content),
			})
			if err != nil {
				t.Fatal(err)
			}
			loadedJSON, isError := executeLoadToolResult(loadInput, store)
			if isError {
				t.Fatalf("LoadToolResult failed for compacted reference: %s", loadedJSON)
			}
			var loaded ToolResultChunk
			if err := json.Unmarshal([]byte(loadedJSON), &loaded); err != nil {
				t.Fatalf("decode LoadToolResult response: %v; response=%s", err, loadedJSON)
			}
			if loaded.ID != "result_"+digest || loaded.Content != content || loaded.TotalBytes != int64(len(content)) || !loaded.EOF {
				t.Fatalf("LoadToolResult did not return the stored content and digest: id=%q bytes=%d total=%d eof=%v", loaded.ID, len(loaded.Content), loaded.TotalBytes, loaded.EOF)
			}
			if got := sha256String(loaded.Content); got != "sha256:"+digest {
				t.Fatalf("loaded content digest = %q, want sha256:%s", got, digest)
			}
		})
	}
}

type compactSessionMemoryBoundaryEstimator struct{}

func (compactSessionMemoryBoundaryEstimator) EstimateTokens(input ContextEstimateRequest) (TokenEstimate, error) {
	encoded, err := json.Marshal(input.Request)
	if err != nil {
		return TokenEstimate{}, err
	}
	tokens := 20_000
	if strings.Contains(string(encoded), "tool-result://sha256:") {
		tokens = 100
	}
	return TokenEstimate{InputTokens: tokens, Source: "session-memory-boundary-fixture", Confidence: "exact"}, nil
}

func compactSessionMemoryBoundaryPayload(size int) string {
	const unit = "historical-read/"
	return strings.Repeat(unit, size/len(unit)) + strings.Repeat("x", size%len(unit))
}

func compactSessionMemoryBoundaryMessages(content string) []types.Message {
	messages := []types.Message{{Role: "user", Content: mustJSON("inspect the retained historical result")}}
	for index := 0; index <= compactionRecentUnits; index++ {
		id := fmt.Sprintf("historical-boundary-%d", index)
		result := "recent result"
		if index == 0 {
			result = content
		}
		toolUse, _ := json.Marshal([]map[string]any{{
			"type": "tool_use", "id": id, "name": "Read", "input": map[string]string{"file_path": "history.txt"},
		}})
		toolResult, _ := json.Marshal([]map[string]any{{
			"type": "tool_result", "tool_use_id": id, "content": result, "is_error": false,
		}})
		messages = append(messages,
			types.Message{Role: "assistant", Content: toolUse},
			types.Message{Role: "user", Content: toolResult},
		)
	}
	return messages
}

func compactSessionMemoryOldResult(t *testing.T, messages []types.Message) string {
	t.Helper()
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID == "historical-boundary-0" {
				return block.Content
			}
		}
	}
	t.Fatal("planned history omitted the old result pair")
	return ""
}

func contextPlanHasReduction(plan ContextPlan, kind ContextReductionKind) bool {
	for _, reduction := range plan.Reductions {
		if reduction.Kind == kind {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
