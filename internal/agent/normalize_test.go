package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aniclew/aniclew/internal/types"
)

func makeMsg(role, text string) types.Message {
	content, _ := json.Marshal(text)
	return types.Message{Role: role, Content: content}
}

func TestMergeConsecutiveSameRole(t *testing.T) {
	messages := []types.Message{
		makeMsg("user", "hello"),
		makeMsg("user", "world"),
		makeMsg("assistant", "hi"),
	}

	result := mergeConsecutiveSameRole(messages)
	if len(result) != 2 {
		t.Fatalf("Expected 2 messages after merge, got %d", len(result))
	}

	text := extractText(result[0].Content)
	if !strings.Contains(text, "hello") || !strings.Contains(text, "world") {
		t.Errorf("Merged message should contain both texts, got: %s", text)
	}
	if result[1].Role != "assistant" {
		t.Errorf("Second message should be assistant, got %s", result[1].Role)
	}
}

func TestMergeNoConsecutive(t *testing.T) {
	messages := []types.Message{
		makeMsg("user", "hello"),
		makeMsg("assistant", "hi"),
		makeMsg("user", "bye"),
	}

	result := mergeConsecutiveSameRole(messages)
	if len(result) != 3 {
		t.Errorf("No merge needed, expected 3, got %d", len(result))
	}
}

func TestTruncateLargeContent(t *testing.T) {
	// Create a message with 200KB content
	large := strings.Repeat("x", 200000)
	messages := []types.Message{makeMsg("user", large)}

	result := truncateLargeContent(messages)
	text := extractText(result[0].Content)
	if len(text) >= 200000 {
		t.Errorf("Should be truncated, got %d chars", len(text))
	}
	if !strings.Contains(text, "truncated") {
		t.Error("Should contain truncation marker")
	}
}

func TestEnsureAlternatingRoles(t *testing.T) {
	messages := []types.Message{
		makeMsg("user", "a"),
		makeMsg("user", "b"), // consecutive user
		makeMsg("assistant", "c"),
	}

	result := ensureAlternatingRoles(messages)
	// Should insert a filler between the two user messages
	for i := 1; i < len(result); i++ {
		if result[i].Role == result[i-1].Role {
			t.Errorf("Messages at %d and %d have same role: %s", i-1, i, result[i].Role)
		}
	}
}

func TestMergeConsecutiveSameRolePreservesMixedContent(t *testing.T) {
	toolResult := json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}]`)
	messages := []types.Message{
		makeMsg("user", "question"),
		{Role: "user", Content: toolResult},
	}

	result := mergeConsecutiveSameRole(messages)

	if len(result) != 2 {
		t.Fatalf("messages = %d, want 2", len(result))
	}
	if extractText(result[0].Content) != "question" {
		t.Fatalf("first message changed: %s", result[0].Content)
	}
	var blocks []types.ContentBlockParam
	if err := json.Unmarshal(result[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("tool result was not preserved: %+v", blocks)
	}
}

func TestCapMessageCount(t *testing.T) {
	// Create 250 messages
	var messages []types.Message
	for i := 0; i < 250; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, makeMsg(role, "msg"))
	}

	result := capMessageCount(messages)
	if len(result) > maxTotalMessages {
		t.Errorf("Should cap at %d, got %d", maxTotalMessages, len(result))
	}
}

func TestHoistToolResultsMovesAssistantToolResultsToUser(t *testing.T) {
	content := json.RawMessage(`[
		{"type":"text","text":"reading"},
		{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}
	]`)
	messages := []types.Message{{Role: "assistant", Content: content}}

	result := HoistToolResults(messages)

	if len(result) != 2 {
		t.Fatalf("messages = %d, want 2", len(result))
	}
	if result[0].Role != "assistant" {
		t.Fatalf("first role = %q", result[0].Role)
	}
	if result[1].Role != "user" {
		t.Fatalf("second role = %q", result[1].Role)
	}
	var toolBlocks []types.ContentBlockParam
	if err := json.Unmarshal(result[1].Content, &toolBlocks); err != nil {
		t.Fatal(err)
	}
	if len(toolBlocks) != 1 || toolBlocks[0].Type != "tool_result" || toolBlocks[0].ToolUseID != "toolu_1" {
		t.Fatalf("tool result blocks = %+v", toolBlocks)
	}
}

func TestNormalizeMessagesHoistsToolResultsBeforeRoleRepair(t *testing.T) {
	content := json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]`)
	messages := []types.Message{{Role: "assistant", Content: content}}

	result := NormalizeMessages(messages)

	if len(result) != 1 {
		t.Fatalf("messages = %d, want 1", len(result))
	}
	if result[0].Role != "user" {
		t.Fatalf("role = %q, want user", result[0].Role)
	}
}

func TestNormalizeMessagesPreservesHoistedToolResultAfterUser(t *testing.T) {
	content := json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]`)
	messages := []types.Message{
		makeMsg("user", "question"),
		{Role: "assistant", Content: content},
	}

	result := NormalizeMessages(messages)

	if len(result) != 3 {
		t.Fatalf("messages = %d, want 3", len(result))
	}
	if result[0].Role != "user" || result[1].Role != "assistant" || result[2].Role != "user" {
		t.Fatalf("roles = %s/%s/%s", result[0].Role, result[1].Role, result[2].Role)
	}
	var blocks []types.ContentBlockParam
	if err := json.Unmarshal(result[2].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "toolu_1" {
		t.Fatalf("tool result was not preserved: %+v", blocks)
	}
}

func TestNormalizeMessages_Integration(t *testing.T) {
	messages := []types.Message{
		makeMsg("user", "hello"),
		makeMsg("user", "how are you"), // consecutive → merge
		makeMsg("assistant", "fine"),
		makeMsg("user", strings.Repeat("x", 200000)), // too large → truncate
	}

	result := NormalizeMessages(messages)

	// Should have 3 messages (2 user merged + 1 assistant + 1 truncated user)
	if len(result) != 3 {
		t.Errorf("Expected 3 messages, got %d", len(result))
	}

	// First message should be merged
	text := extractText(result[0].Content)
	if !strings.Contains(text, "hello") || !strings.Contains(text, "how are you") {
		t.Errorf("First message should be merged: %s", text[:50])
	}

	// Last message should be truncated
	lastText := extractText(result[2].Content)
	if len(lastText) >= 200000 {
		t.Error("Last message should be truncated")
	}
}
