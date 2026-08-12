package agent

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestDurableRunObserverBuildsRedactedOrderedTranscript(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	observer := NewDurableRunObserver("run-fallback")
	observer.Observe(Event{Type: "text", Data: "before"})
	observer.Observe(Event{Type: "thinking", Data: "do-not-persist"})
	observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "call-1", "name": "Bash", "inputDigest": digest, "runId": "run-exact",
	}})
	observer.Observe(Event{Type: "tool_result", Data: map[string]any{
		"id": "call-1", "name": "Bash", "result": "password=super-secret-value", "executed": true,
	}})
	observer.Observe(Event{Type: "tool_result", Data: map[string]any{
		"id": "call-1", "name": "Bash", "result": "duplicate", "executed": true,
	}})
	observer.Observe(Event{Type: "text", Data: "after"})
	observer.Observe(Event{Type: "done"})

	if !observer.Completed() {
		t.Fatal("observer did not record normal completion")
	}
	messages := observer.Messages()
	if len(messages) != 3 || messages[0].Role != "assistant" || messages[0].Content != "before" ||
		messages[1].Role != "tool" || messages[2].Role != "assistant" || messages[2].Content != "after" {
		t.Fatalf("unexpected durable transcript: %#v", messages)
	}
	if strings.Contains(messages[1].ToolResult, "super-secret-value") || !strings.Contains(messages[1].ToolResult, "[REDACTED]") {
		t.Fatalf("tool result was not redacted: %q", messages[1].ToolResult)
	}
	reference, ok := messages[1].ToolInput.(map[string]string)
	if !ok || reference["toolCallId"] != "call-1" || reference["inputDigest"] != digest {
		t.Fatalf("unexpected digest-only tool reference: %#v", messages[1].ToolInput)
	}
	if observer.Interruption("disconnect") != nil {
		t.Fatal("completed run produced an interruption marker")
	}
	// Returned messages are copies, not mutable observer state.
	reference["inputDigest"] = "forged"
	if got := observer.Messages()[1].ToolInput.(map[string]string)["inputDigest"]; got != digest {
		t.Fatalf("observer message state was mutated through a returned snapshot: %q", got)
	}
}

func TestDurableRunObserverPreservesSequentialReusedToolCallID(t *testing.T) {
	observer := NewDurableRunObserver("run-reused")
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "call-reused", "name": "Write", "inputDigest": digestA, "runId": "run-reused",
	}})
	observer.Observe(Event{Type: "tool_result", Data: map[string]any{
		"id": "call-reused", "name": "Write", "result": "first", "executed": true,
	}})
	observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "call-reused", "name": "Edit", "inputDigest": digestB, "runId": "run-reused",
	}})
	observer.Observe(Event{Type: "done"})

	if !observer.ReconciliationRequired() {
		t.Fatal("unresolved reused tool-call ID was treated as resumable")
	}
	marker := observer.ReconciliationMarker("reused call ID interrupted")
	if marker == nil || marker.ToolName != "multiple_tools" || marker.ToolCallID != "multiple" ||
		marker.InputDigest == digestA || marker.InputDigest == digestB {
		t.Fatalf("reused occurrences were not aggregated: %#v", marker)
	}
	if messages := observer.Messages(); len(messages) != 1 || messages[0].ToolResult != "first" {
		t.Fatalf("completed occurrence transcript = %#v", messages)
	}
}

func TestDurableRunObserverOverlappingReusedIDRemainsAmbiguous(t *testing.T) {
	observer := NewDurableRunObserver("run-overlap")
	for _, digestRune := range []string{"c", "d"} {
		observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-overlap", "name": "Bash", "inputDigest": "sha256:" + strings.Repeat(digestRune, 64), "runId": "run-overlap",
		}})
	}
	for _, result := range []string{"one", "two"} {
		observer.Observe(Event{Type: "tool_result", Data: map[string]any{
			"id": "call-overlap", "name": "Bash", "result": result, "executed": true,
		}})
	}
	observer.Observe(Event{Type: "done"})
	if !observer.ReconciliationRequired() || observer.ReconciliationMarker("ambiguous") == nil {
		t.Fatal("overlapping duplicate tool-call IDs were silently accepted")
	}
}

func TestDurableRunObserverRequiresExactExecutionStart(t *testing.T) {
	observer := NewDurableRunObserver("run-1")
	observer.Observe(Event{Type: "tool_result", Data: map[string]any{
		"id": "proposal", "name": "Write", "result": "denied", "executed": false,
	}})
	observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
		"id": "invalid", "name": "Write", "inputDigest": "not-a-digest",
	}})
	if marker := observer.Interruption("disconnect"); marker != nil {
		t.Fatalf("proposal or invalid start created interruption marker: %#v", marker)
	}
	if messages := observer.Messages(); messages != nil {
		t.Fatalf("incomplete run exposed commit messages: %#v", messages)
	}
}

func TestDurableRunObserverParallelInterruptionDigestIsDeterministic(t *testing.T) {
	build := func(reverse bool) *SessionInterruption {
		observer := NewDurableRunObserver("run-parallel")
		calls := []string{"call-a", "call-b"}
		if reverse {
			calls[0], calls[1] = calls[1], calls[0]
		}
		for _, id := range calls {
			observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
				"id": id, "name": "Write", "inputDigest": "sha256:" + strings.Repeat(string(id[len(id)-1]), 64), "runId": "run-parallel",
			}})
		}
		observer.Observe(Event{Type: "tool_result", Data: map[string]any{
			"id": "call-a", "name": "Write", "result": "ok", "executed": true,
		}})
		return observer.Interruption("transport disconnected")
	}
	left, right := build(false), build(true)
	if left == nil || right == nil {
		t.Fatal("parallel activity did not produce an interruption marker")
	}
	if left.ToolName != "multiple_tools" || left.ToolCallID != "multiple" ||
		left.SideEffectState != SessionSideEffectMayHaveApplied {
		t.Fatalf("unexpected aggregate marker: %#v", left)
	}
	if left.InputDigest != right.InputDigest {
		t.Fatalf("aggregate digest depends on event scheduling: %q != %q", left.InputDigest, right.InputDigest)
	}
	if err := ValidateSessionInterruption(*left); err != nil {
		t.Fatalf("aggregate marker is not persistable: %v", err)
	}
}

func TestDurableRunObserverConcurrentStartsAndUTF8Bound(t *testing.T) {
	observer := NewDurableRunObserver("run-many")
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			id := "call-" + string(rune('a'+index))
			observer.Observe(Event{Type: "tool_execution_start", Data: map[string]string{
				"id": id, "name": "Read", "inputDigest": "sha256:" + strings.Repeat("b", 64), "runId": "run-many",
			}})
		}(index)
	}
	wait.Wait()
	if marker := observer.Interruption("cancelled"); marker == nil || marker.ToolName != "multiple_tools" {
		t.Fatalf("concurrent starts were not recorded safely: %#v", marker)
	}

	textObserver := NewDurableRunObserver("run-text")
	textObserver.Observe(Event{Type: "text", Data: strings.Repeat("가", durableRunAssistantLimitBytes)})
	textObserver.Observe(Event{Type: "done"})
	messages := textObserver.Messages()
	if len(messages) != 1 || !utf8.ValidString(messages[0].Content) ||
		!strings.Contains(messages[0].Content, "[durable transcript truncated]") {
		t.Fatalf("assistant truncation is not UTF-8 safe: length=%d", len(messages[0].Content))
	}
}

func TestDurableRunObserverRedactsPEMWhoseEndFallsPastBound(t *testing.T) {
	observer := NewDurableRunObserver("run-pem")
	prefix := strings.Repeat("x", durableRunAssistantLimitBytes-80)
	observer.Observe(Event{Type: "text", Data: prefix + "-----BEGIN PRIVATE KEY-----\nraw-private-material"})
	observer.Observe(Event{Type: "text", Data: "more-private-material\n-----END PRIVATE KEY-----"})
	observer.Observe(Event{Type: "done"})
	messages := observer.Messages()
	if len(messages) != 1 || strings.Contains(messages[0].Content, "raw-private-material") ||
		!strings.Contains(messages[0].Content, "[REDACTED PRIVATE KEY]") {
		t.Fatalf("bounded PEM tail was not redacted: %q", messages[0].Content[len(messages[0].Content)-160:])
	}
}
