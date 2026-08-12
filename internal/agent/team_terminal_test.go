package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestTeamRunTerminalReducerOrdering(t *testing.T) {
	type observation struct {
		event      Event
		contextErr error
	}
	tests := []struct {
		name        string
		observed    []observation
		finalErr    error
		wantSuccess bool
		wantFailure string
	}{
		{
			name:        "legacy terminal succeeds",
			observed:    []observation{{event: Event{Type: "done"}}},
			wantSuccess: true,
		},
		{
			name: "typed verified terminal succeeds",
			observed: []observation{{event: Event{Type: "done", Data: DurableRunTerminalMetadata{
				TerminalState:    EvidenceTerminalVerified,
				CompletionStatus: CompletionStatusComplete,
			}}}},
			wantSuccess: true,
		},
		{
			name: "unknown legacy metadata succeeds",
			observed: []observation{{event: Event{Type: "done", Data: map[string]interface{}{
				"legacyStopReason": "end_turn",
			}}}},
			wantSuccess: true,
		},
		{
			name: "typed blocked terminal fails",
			observed: []observation{{event: Event{Type: "done", Data: DurableRunTerminalMetadata{
				TerminalState: EvidenceTerminalBlocked,
			}}}},
			wantFailure: "terminalState=\"blocked\"",
		},
		{
			name: "typed incomplete contract fails",
			observed: []observation{{event: Event{Type: "done", Data: DurableRunTerminalMetadata{
				CompletionStatus: CompletionStatusIncomplete,
			}}}},
			wantFailure: "completionStatus=\"incomplete\"",
		},
		{
			name: "typed blocked criterion fails",
			observed: []observation{{event: Event{Type: "done", Data: DurableRunTerminalMetadata{
				CompletionBlocked: 1,
			}}}},
			wantFailure: "completionBlocked=1",
		},
		{
			name:        "channel close without terminal fails",
			wantFailure: "closed without a terminal event",
		},
		{
			name: "error before normal terminal remains failed",
			observed: []observation{
				{event: Event{Type: "error", Data: "provider failed"}},
				{event: Event{Type: "done"}},
			},
			wantFailure: "provider failed",
		},
		{
			name: "context blocked before normal terminal remains failed",
			observed: []observation{
				{event: Event{Type: "context_blocked", Data: ContextBlockedEvent{Message: "request cannot fit"}}},
				{event: Event{Type: "done"}},
			},
			wantFailure: "request cannot fit",
		},
		{
			name: "explicit cancellation before terminal fails",
			observed: []observation{
				{event: Event{Type: "canceled"}},
				{event: Event{Type: "done"}},
			},
			wantFailure: "canceled",
		},
		{
			name: "explicit deadline before terminal fails",
			observed: []observation{
				{event: Event{Type: "deadline_exceeded"}},
				{event: Event{Type: "done"}},
			},
			wantFailure: "deadline_exceeded",
		},
		{
			name: "context cancellation observed before terminal fails",
			observed: []observation{
				{event: Event{Type: "text", Data: "partial"}, contextErr: context.Canceled},
				{event: Event{Type: "done"}},
			},
			wantFailure: "context canceled",
		},
		{
			name:        "cancel on channel close fails",
			finalErr:    context.Canceled,
			wantFailure: "context canceled",
		},
		{
			name:        "deadline on channel close fails",
			finalErr:    context.DeadlineExceeded,
			wantFailure: "context deadline exceeded",
		},
		{
			name:        "normal terminal then late cancel succeeds",
			observed:    []observation{{event: Event{Type: "done"}}},
			finalErr:    context.Canceled,
			wantSuccess: true,
		},
		{
			name:        "queued normal terminal then late deadline succeeds",
			observed:    []observation{{event: Event{Type: "done"}, contextErr: context.DeadlineExceeded}},
			finalErr:    context.DeadlineExceeded,
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reducer teamRunTerminalReducer
			for _, item := range tt.observed {
				reducer.Observe(item.event, item.contextErr)
			}
			success, failure := reducer.Result(tt.finalErr)
			if success != tt.wantSuccess {
				t.Fatalf("success = %v, want %v (failure %q)", success, tt.wantSuccess, failure)
			}
			if tt.wantFailure != "" && !strings.Contains(failure, tt.wantFailure) {
				t.Fatalf("failure = %q, want containing %q", failure, tt.wantFailure)
			}
			if tt.wantSuccess && failure != "" {
				t.Fatalf("successful result carried failure %q", failure)
			}
		})
	}
}

func TestTeamPartialResultKeepsBoundedUTF8Prefix(t *testing.T) {
	var result teamPartialResult
	result.Append(strings.Repeat("가", 200))
	result.Append("tail that must not enter memory")

	if !result.clipped {
		t.Fatal("result should report clipping")
	}
	if len(result.text) > teamTaskResultPrefixLimit {
		t.Fatalf("prefix bytes = %d, limit %d", len(result.text), teamTaskResultPrefixLimit)
	}
	if !strings.HasPrefix(strings.Repeat("가", 200), result.text) {
		t.Fatalf("result is not a stream prefix: %q", result.text)
	}
	if !strings.Contains(result.Summary("worker.txt"), "full output: worker.txt") {
		t.Fatalf("clipped summary does not point to full output: %q", result.Summary("worker.txt"))
	}
}

func TestTeamExecuteTaskConsumesKernelTerminalOnce(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")

	fullText := strings.Repeat("x", teamTaskResultPrefixLimit+200)
	provider := &teamTerminalProvider{text: fullText}
	recorder := &teamTerminalRecorder{}
	workspace := t.TempDir()
	team := NewTeam(provider, "test-model", workspace, t.TempDir(), TeamConfig{
		Name:           "terminal-integration",
		DisablePlugins: true,
		Recorder:       recorder,
	})
	task := &TeamTask{ID: "bounded-result", Name: "bounded result", Description: "return a long result"}

	team.executeTask(context.Background(), task)

	if task.Status != "completed" {
		t.Fatalf("task status = %q, result = %q", task.Status, task.Result)
	}
	wantResult := fullText[:teamTaskResultPrefixLimit] + "... (full output: " + task.OutputPath + ")"
	if task.Result != wantResult {
		t.Fatalf("task result = %q, want bounded prefix with output path", task.Result)
	}
	output, err := os.ReadFile(task.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != fullText {
		t.Fatalf("full output bytes = %d, want %d", len(output), len(fullText))
	}
	started, completed, receipts := recorder.snapshot()
	if started != 1 || completed != 1 || receipts != 0 {
		t.Fatalf("kernel recorder calls = started:%d completed:%d receipts:%d, want 1/1/0", started, completed, receipts)
	}
}

type teamTerminalProvider struct {
	text string
}

func (*teamTerminalProvider) Name() string              { return "team-terminal" }
func (*teamTerminalProvider) DisplayName() string       { return "Team Terminal" }
func (*teamTerminalProvider) Models() []types.ModelInfo { return nil }
func (*teamTerminalProvider) Validate() error           { return nil }

func (p *teamTerminalProvider) StreamMessage(
	context.Context,
	*types.MessagesRequest,
	*types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	events := make(chan types.SSEEvent, 5)
	events <- types.SSEEvent{Type: "content_block_start", ContentBlock: teamTerminalJSON(map[string]string{"type": "text"})}
	events <- types.SSEEvent{Type: "content_block_delta", Delta: teamTerminalJSON(map[string]string{
		"type": "text_delta",
		"text": p.text,
	})}
	events <- types.SSEEvent{Type: "content_block_stop"}
	events <- types.SSEEvent{Type: "message_delta", Delta: teamTerminalJSON(map[string]string{"stop_reason": "end_turn"})}
	events <- types.SSEEvent{Type: "message_stop"}
	close(events)
	return events, nil
}

func teamTerminalJSON(value interface{}) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

type teamTerminalRecorder struct {
	mu        sync.Mutex
	started   int
	completed int
	receipts  int
}

func (r *teamTerminalRecorder) RunStarted() {
	r.mu.Lock()
	r.started++
	r.mu.Unlock()
}

func (r *teamTerminalRecorder) ReceiptWritten(string, AgentReceipt) {
	r.mu.Lock()
	r.receipts++
	r.mu.Unlock()
}

func (r *teamTerminalRecorder) RunCompleted(RunSummary) {
	r.mu.Lock()
	r.completed++
	r.mu.Unlock()
}

func (r *teamTerminalRecorder) snapshot() (started, completed, receipts int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.completed, r.receipts
}
