package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/memory"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// ---------- mock provider ----------

type mockProvider struct {
	// Responses returned in order; wraps around if needed.
	responses []string
	// callCount observed so tests can assert we actually hit the mock.
	calls int
}

func (m *mockProvider) Name() string                 { return "mock" }
func (m *mockProvider) DisplayName() string          { return "Mock" }
func (m *mockProvider) Models() []types.ModelInfo    { return nil }
func (m *mockProvider) Validate() error              { return nil }
func (m *mockProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	text := ""
	if len(m.responses) > 0 {
		text = m.responses[m.calls%len(m.responses)]
	}
	m.calls++

	ch := make(chan types.SSEEvent, 4)
	go func() {
		defer close(ch)
		// One text delta carrying the full response — callModelCollect
		// concatenates deltas, so a single chunk is legal.
		deltaBody, _ := json.Marshal(map[string]string{
			"type": "text_delta",
			"text": text,
		})
		ch <- types.SSEEvent{Type: "content_block_delta", Delta: deltaBody}
	}()
	return ch, nil
}

// ---------- BuildMemoryContext ----------

func TestBuildMemoryContext_EmptyWorkspaceStillReturnsSection(t *testing.T) {
	workDir := t.TempDir()
	msgs := []types.Message{
		{Role: "user", Content: mustJSON("what are my preferences?")},
	}
	got := BuildMemoryContext(workDir, msgs)
	if !strings.Contains(got, "## Long-term Memory") {
		t.Errorf("expected memory section header, got %q", got)
	}
	if !strings.Contains(got, "no index yet") {
		t.Errorf("expected placeholder text, got %q", got)
	}
}

func TestBuildMemoryContext_DisabledByEnv(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	got := BuildMemoryContext(t.TempDir(), []types.Message{
		{Role: "user", Content: mustJSON("hello")},
	})
	if got != "" {
		t.Errorf("expected empty context when disabled, got %q", got)
	}
}

func TestBuildMemoryContext_IncludesExistingEntries(t *testing.T) {
	workDir := t.TempDir()

	_, err := memory.WriteEntry(workDir, "db", memory.Entry{
		Name: "DB rule", Description: "no mocks in integration tests",
		Type: memory.TypeFeedback, Body: "Must hit a real database.",
	})
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := memory.Scan(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.UpdateIndex(workDir, scanned); err != nil {
		t.Fatal(err)
	}

	msgs := []types.Message{
		{Role: "user", Content: mustJSON("should I use a real database for integration tests?")},
	}
	got := BuildMemoryContext(workDir, msgs)
	if !strings.Contains(got, "DB rule") {
		t.Errorf("memory context should include existing entry name:\n%s", got)
	}
	// Relevant body should be appended (query contains "database").
	if !strings.Contains(got, "Must hit a real database") {
		t.Errorf("relevance-appended body missing:\n%s", got)
	}
}

// ---------- lastUserText / renderConversationForExtraction ----------

func TestLastUserText_PicksMostRecentStringMessage(t *testing.T) {
	msgs := []types.Message{
		{Role: "user", Content: mustJSON("old question")},
		{Role: "assistant", Content: mustJSON("old answer")},
		// A tool_result message — user role, but content is a JSON array,
		// not a plain string. Should be skipped.
		{Role: "user", Content: mustJSON([]map[string]string{{"type": "tool_result"}})},
		{Role: "user", Content: mustJSON("current question")},
	}
	if got := lastUserText(msgs); got != "current question" {
		t.Errorf("lastUserText: got %q, want %q", got, "current question")
	}
}

func TestLastUserText_EmptyOrNoUser(t *testing.T) {
	if got := lastUserText(nil); got != "" {
		t.Errorf("empty input: got %q", got)
	}
	if got := lastUserText([]types.Message{
		{Role: "assistant", Content: mustJSON("hi")},
	}); got != "" {
		t.Errorf("no user messages: got %q", got)
	}
}

func TestRenderConversationForExtraction_SkipsNonTextContent(t *testing.T) {
	msgs := []types.Message{
		{Role: "user", Content: mustJSON("hello")},
		{Role: "assistant", Content: mustJSON("hi there")},
		// tool_result block (JSON array) — skipped.
		{Role: "user", Content: mustJSON([]map[string]string{{"type": "tool_result"}})},
		{Role: "assistant", Content: mustJSON("final answer")},
	}
	got := renderConversationForExtraction(msgs)

	for _, want := range []string{"user: hello", "assistant: hi there", "assistant: final answer"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "tool_result") {
		t.Errorf("tool_result block should be dropped:\n%s", got)
	}
}

// ---------- ExtractMemoriesAsync end-to-end with mock ----------

func TestExtractMemoriesAsync_SavesOnValidResponse(t *testing.T) {
	workDir := t.TempDir()

	// Ensure memory is enabled for this test.
	t.Setenv("CORELAY_MEMORY", "on")

	mock := &mockProvider{
		responses: []string{
			`[
				{
					"name": "User language preference",
					"description": "Wants all responses in Korean",
					"type": "user",
					"body": "User prefers Korean responses."
				}
			]`,
		},
	}

	msgs := []types.Message{
		{Role: "user", Content: mustJSON("please respond in Korean from now on")},
		{Role: "assistant", Content: mustJSON("understood")},
	}

	ExtractMemoriesAsync(context.Background(), mock, "mock-model", workDir, msgs)

	if !WaitForMemoryTasks(5 * time.Second) {
		t.Fatal("memory tasks did not finish within timeout")
	}
	if mock.calls != 1 {
		t.Errorf("expected exactly 1 provider call, got %d", mock.calls)
	}

	scanned, err := memory.Scan(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 {
		t.Fatalf("expected 1 saved entry, got %d", len(scanned))
	}
	if scanned[0].Name != "User language preference" {
		t.Errorf("unexpected name: %q", scanned[0].Name)
	}
}

func TestExtractMemoriesAsync_NoopOnShortConversation(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("CORELAY_MEMORY", "on")

	mock := &mockProvider{responses: []string{"[]"}}
	ExtractMemoriesAsync(context.Background(), mock, "mock-model", workDir,
		[]types.Message{
			{Role: "user", Content: mustJSON("hi")},
		})

	WaitForMemoryTasks(1 * time.Second)
	if mock.calls != 0 {
		t.Errorf("provider should not be called for <2 messages, got %d calls", mock.calls)
	}
}

func TestExtractMemoriesAsync_NoopWhenDisabled(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("CORELAY_MEMORY", "off")

	mock := &mockProvider{responses: []string{`[{"name":"x","description":"x","type":"user","body":"x"}]`}}
	ExtractMemoriesAsync(context.Background(), mock, "mock-model", workDir,
		[]types.Message{
			{Role: "user", Content: mustJSON("a")},
			{Role: "assistant", Content: mustJSON("b")},
		})
	WaitForMemoryTasks(500 * time.Millisecond)

	if mock.calls != 0 {
		t.Error("disabled memory should not call the provider")
	}
}

func TestExtractMemoriesAsync_HandlesEmptyArray(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("CORELAY_MEMORY", "on")

	mock := &mockProvider{responses: []string{"[]"}}
	ExtractMemoriesAsync(context.Background(), mock, "mock-model", workDir,
		[]types.Message{
			{Role: "user", Content: mustJSON("a")},
			{Role: "assistant", Content: mustJSON("b")},
		})
	if !WaitForMemoryTasks(2 * time.Second) {
		t.Fatal("timeout")
	}

	entries, _ := memory.Scan(workDir)
	if len(entries) != 0 {
		t.Errorf("empty extraction should not create files, got %d entries", len(entries))
	}
}

// ---------- MaybeConsolidateAsync gate behavior ----------

func TestMaybeConsolidateAsync_SkipsWhenGateNotReady(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("CORELAY_MEMORY", "on")

	// No entries yet → count gate fails → no provider call.
	mock := &mockProvider{responses: []string{"[]"}}
	MaybeConsolidateAsync(context.Background(), mock, "mock-model", workDir)
	WaitForMemoryTasks(500 * time.Millisecond)

	if mock.calls != 0 {
		t.Errorf("provider should not be called when gate is not ready, got %d calls", mock.calls)
	}
}

// ---------- memoryEnabled ----------

func TestMemoryEnabled_EnvToggle(t *testing.T) {
	cases := map[string]bool{
		"":      true,
		"on":    true,
		"1":     true,
		"true":  true,
		"off":   false,
		"OFF":   false,
		"0":     false,
		"false": false,
	}
	for val, want := range cases {
		if val == "" {
			os.Unsetenv("CORELAY_MEMORY")
		} else {
			t.Setenv("CORELAY_MEMORY", val)
		}
		if got := memoryEnabled(); got != want {
			t.Errorf("CORELAY_MEMORY=%q: got %v, want %v", val, got, want)
		}
	}
}
