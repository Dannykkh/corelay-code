package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestACPEventConsumerTerminalTable(t *testing.T) {
	complete := map[string]any{
		"kind":                "completed",
		"stopReason":          "end_turn",
		"durablePolicy":       "commit",
		"terminalState":       agent.EvidenceTerminalVerified,
		"completionStatus":    agent.CompletionStatusComplete,
		"completionRevision":  1,
		"completionCriteria":  1,
		"completionSatisfied": 1,
	}
	blocked := map[string]any{
		"kind":               "failed",
		"stopReason":         "completion_blocked",
		"durablePolicy":      "reconcile",
		"terminalState":      agent.EvidenceTerminalBlocked,
		"completionStatus":   agent.CompletionStatusBlocked,
		"completionCriteria": 1,
		"completionBlocked":  1,
	}
	incomplete := map[string]any{
		"kind":                "failed",
		"stopReason":          "completion_blocked",
		"durablePolicy":       "reconcile",
		"terminalState":       agent.EvidenceTerminalBlocked,
		"completionStatus":    agent.CompletionStatusIncomplete,
		"completionCriteria":  2,
		"completionSatisfied": 1,
	}
	contextTerminal := map[string]any{
		"kind":          "context_blocked",
		"stopReason":    "context_blocked",
		"durablePolicy": "reconcile",
		"terminalState": agent.EvidenceTerminalBlocked,
	}
	maxTerminal := map[string]any{
		"kind":          "max_iterations",
		"stopReason":    "max_iterations",
		"durablePolicy": "reconcile",
		"terminalState": agent.EvidenceTerminalBlocked,
	}

	tests := []struct {
		name                string
		events              []agent.Event
		wantStop            acp.StopReason
		wantRunError        bool
		wantTerminalFailure bool
		wantTerminalCode    int
		wantCompleted       bool
		wantBlocks          bool
		wantPolicy          string
		wantKind            string
		wantText            string
	}{
		{
			name: "legacy done",
			events: []agent.Event{
				{Type: "text", Data: "legacy"},
				{Type: "done"},
			},
			wantStop: acp.StopEndTurn, wantCompleted: true, wantText: "legacy",
		},
		{
			name: "typed complete done is terminal and suppresses late output",
			events: []agent.Event{
				{Type: "text", Data: "complete"},
				{Type: "done", Data: complete},
				{Type: "text", Data: "must-not-cross-terminal"},
			},
			wantStop: acp.StopEndTurn, wantCompleted: true, wantPolicy: durablePolicyCommit,
			wantKind: "completed", wantText: "complete",
		},
		{
			name: "typed blocked done is not success",
			events: []agent.Event{
				{Type: "text", Data: "partial"},
				{Type: "done", Data: blocked},
			},
			wantStop: acp.StopEndTurn, wantTerminalFailure: true, wantCompleted: true,
			wantTerminalCode: acp.CodeInvalidRequest, wantBlocks: true,
			wantPolicy: durablePolicyReconcile, wantKind: "failed", wantText: "partial",
		},
		{
			name: "typed incomplete done is not success",
			events: []agent.Event{
				{Type: "done", Data: incomplete},
			},
			wantStop: acp.StopEndTurn, wantTerminalFailure: true, wantCompleted: true,
			wantTerminalCode: acp.CodeInvalidRequest, wantBlocks: true,
			wantPolicy: durablePolicyReconcile, wantKind: "failed",
		},
		{
			name: "typed generic blocked terminal fails",
			events: []agent.Event{
				{Type: "error", Data: "provider failed"},
				{Type: "done", Data: map[string]any{
					"kind": "failed", "stopReason": "provider_retry_exhausted", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked,
				}},
			},
			wantStop: acp.StopEndTurn, wantTerminalFailure: true, wantCompleted: true,
			wantTerminalCode: acp.CodeInternalError, wantBlocks: true,
			wantPolicy: durablePolicyReconcile, wantKind: "failed",
		},
		{
			name: "typed context blocked keeps stable max tokens",
			events: []agent.Event{
				{Type: "context_blocked", Data: agent.ContextBlockedEvent{Code: "context_blocked", Message: "too large"}},
				{Type: "error", Data: "too large"},
				{Type: "done", Data: contextTerminal},
			},
			wantStop: acp.StopMaxTokens, wantCompleted: true, wantBlocks: true,
			wantPolicy: durablePolicyReconcile, wantKind: "context_blocked",
		},
		{
			name: "typed max iteration keeps stable max turns",
			events: []agent.Event{
				{Type: "error", Data: "Max iterations reached"},
				{Type: "done", Data: maxTerminal},
			},
			wantStop: acp.StopMaxTurnRequests, wantCompleted: true, wantBlocks: true,
			wantPolicy: durablePolicyReconcile, wantKind: "max_iterations",
		},
		{
			name: "legacy context blocked remains compatible",
			events: []agent.Event{
				{Type: "context_blocked", Data: agent.ContextBlockedEvent{Message: "too large"}},
				{Type: "error", Data: "too large"},
			},
			wantStop: acp.StopMaxTokens, wantCompleted: true,
		},
		{
			name: "legacy max iteration remains compatible",
			events: []agent.Event{
				{Type: "error", Data: "Max iterations reached"},
			},
			wantStop: acp.StopMaxTurnRequests, wantCompleted: true,
		},
		{
			name: "channel close without terminal fails closed",
			events: []agent.Event{
				{Type: "text", Data: "partial"},
			},
			wantStop: acp.StopEndTurn, wantRunError: true, wantText: "partial",
		},
		{
			name: "typed no-terminal diagnostic is not success",
			events: []agent.Event{
				{Type: "error", Data: "Provider stream closed without a terminal event"},
				{Type: "done", Data: map[string]any{
					"kind": "no_terminal", "stopReason": "provider_stream_closed", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked,
				}},
			},
			wantStop: acp.StopEndTurn, wantTerminalFailure: true, wantCompleted: true,
			wantTerminalCode: acp.CodeInternalError, wantBlocks: true,
			wantPolicy: durablePolicyReconcile, wantKind: "no_terminal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &Backend{}
			outcome := backend.consumeEvents(
				context.Background(), func() {}, "session", "run", "message",
				defaultReasoningMode, &recordingClient{}, eventStream(test.events...),
			)
			if (outcome.Err != nil) != test.wantRunError {
				t.Fatalf("run error = %v, want error=%v", outcome.Err, test.wantRunError)
			}
			if (outcome.TerminalFailure != nil) != test.wantTerminalFailure {
				t.Fatalf("terminal failure = %v, want failure=%v", outcome.TerminalFailure, test.wantTerminalFailure)
			}
			if test.wantTerminalFailure {
				var rpcErr *acp.RPCError
				if !errors.As(outcome.TerminalFailure, &rpcErr) || rpcErr.Code != test.wantTerminalCode {
					t.Fatalf("terminal failure = %#v, want code %d", outcome.TerminalFailure, test.wantTerminalCode)
				}
			}
			if outcome.StopReason != test.wantStop || outcome.TerminalKind != test.wantKind ||
				outcome.DurablePolicy != test.wantPolicy || outcome.Text != test.wantText {
				t.Fatalf("outcome = %#v, want stop=%q kind=%q policy=%q text=%q",
					outcome, test.wantStop, test.wantKind, test.wantPolicy, test.wantText)
			}
			if outcome.Observer.Completed() != test.wantCompleted {
				t.Fatalf("observer completed = %v, want %v", outcome.Observer.Completed(), test.wantCompleted)
			}
			metadata, metadataSet := outcome.Observer.TerminalMetadata()
			if got := metadataSet && metadata.BlocksSuccess(); got != test.wantBlocks {
				t.Fatalf("terminal metadata = %#v set=%v blocks=%v, want blocks=%v",
					metadata, metadataSet, got, test.wantBlocks)
			}
		})
	}
}

func TestACPPromptDurableTypedTerminalPolicy(t *testing.T) {
	tests := []struct {
		name          string
		events        []agent.Event
		wantStop      acp.StopReason
		wantErrorCode int
		wantReconcile bool
		wantTerminal  bool
		wantBlocks    bool
		wantMessages  int
	}{
		{
			name: "legacy done commits",
			events: []agent.Event{
				{Type: "text", Data: "legacy answer"},
				{Type: "done"},
			},
			wantStop: acp.StopEndTurn, wantMessages: 2,
		},
		{
			name: "typed complete commits metadata",
			events: []agent.Event{
				{Type: "text", Data: "typed answer"},
				{Type: "done", Data: map[string]any{
					"kind": "completed", "stopReason": "end_turn", "durablePolicy": "commit",
					"terminalState": agent.EvidenceTerminalVerified, "completionStatus": agent.CompletionStatusComplete,
					"completionRevision": 1, "completionCriteria": 1, "completionSatisfied": 1,
				}},
			},
			wantStop: acp.StopEndTurn, wantTerminal: true, wantMessages: 2,
		},
		{
			name: "typed blocked commits then requires reconciliation",
			events: []agent.Event{
				{Type: "text", Data: "bounded partial"},
				{Type: "done", Data: map[string]any{
					"kind": "failed", "stopReason": "completion_blocked", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked, "completionStatus": agent.CompletionStatusBlocked,
					"completionCriteria": 1, "completionBlocked": 1,
				}},
			},
			wantErrorCode: acp.CodeInvalidRequest, wantReconcile: true, wantTerminal: true,
			wantBlocks: true, wantMessages: 2,
		},
		{
			name: "typed incomplete commits then requires reconciliation",
			events: []agent.Event{
				{Type: "done", Data: map[string]any{
					"kind": "failed", "stopReason": "completion_blocked", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked, "completionStatus": agent.CompletionStatusIncomplete,
					"completionCriteria": 2, "completionSatisfied": 1,
				}},
			},
			wantErrorCode: acp.CodeInvalidRequest, wantReconcile: true, wantTerminal: true,
			wantBlocks: true, wantMessages: 1,
		},
		{
			name: "typed context blocked returns max tokens and reconciles",
			events: []agent.Event{
				{Type: "context_blocked", Data: agent.ContextBlockedEvent{Message: "too large"}},
				{Type: "error", Data: "too large"},
				{Type: "done", Data: map[string]any{
					"kind": "context_blocked", "stopReason": "context_blocked", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked,
				}},
			},
			wantStop: acp.StopMaxTokens, wantReconcile: true, wantTerminal: true,
			wantBlocks: true, wantMessages: 1,
		},
		{
			name: "typed max iteration returns max turns and reconciles tool transcript",
			events: []agent.Event{
				{Type: "tool_execution_start", Data: map[string]string{
					"id": "call-max", "name": "Bash", "runId": "run-max",
					"inputDigest": "sha256:" + strings.Repeat("a", 64),
				}},
				{Type: "tool_result", Data: map[string]any{
					"id": "call-max", "name": "Bash", "result": "ok", "executed": true,
				}},
				{Type: "error", Data: "Max iterations reached"},
				{Type: "done", Data: map[string]any{
					"kind": "max_iterations", "stopReason": "max_iterations", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked,
				}},
			},
			wantStop: acp.StopMaxTurnRequests, wantReconcile: true, wantTerminal: true,
			wantBlocks: true, wantMessages: 2,
		},
		{
			name: "close without terminal is internal failure",
			events: []agent.Event{
				{Type: "text", Data: "partial"},
			},
			wantErrorCode: acp.CodeInternalError, wantMessages: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBackendFixture(t, scriptedRunner(test.events...))
			sessionID := fixture.newSession(t)
			response, promptErr := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
				SessionID: sessionID,
				Prompt:    []acp.ContentBlock{{Type: "text", Text: "perform terminal scenario"}},
			}, fixture.client)
			if test.wantErrorCode == 0 {
				if promptErr != nil || response.StopReason != test.wantStop {
					t.Fatalf("Prompt() = (%#v, %v), want stop %q", response, promptErr, test.wantStop)
				}
			} else {
				var rpcErr *acp.RPCError
				if !errors.As(promptErr, &rpcErr) || rpcErr.Code != test.wantErrorCode {
					t.Fatalf("Prompt() error = %#v, want code %d", promptErr, test.wantErrorCode)
				}
			}

			persisted, err := fixture.store.Get(sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.ReconcileRequired != test.wantReconcile || len(persisted.Messages) != test.wantMessages {
				t.Fatalf("persisted session = %#v, want reconcile=%v messages=%d",
					persisted, test.wantReconcile, test.wantMessages)
			}
			if (persisted.LastRunTerminal != nil) != test.wantTerminal {
				t.Fatalf("last terminal = %#v, want present=%v", persisted.LastRunTerminal, test.wantTerminal)
			}
			if persisted.LastRunTerminal != nil && persisted.LastRunTerminal.BlocksSuccess() != test.wantBlocks {
				t.Fatalf("last terminal = %#v, want blocks=%v", persisted.LastRunTerminal, test.wantBlocks)
			}
		})
	}
}

func TestStdioBlockedAndIncompleteTerminalsAreErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status agent.CompletionContractStatus
		block  int
	}{
		{name: "blocked", status: agent.CompletionStatusBlocked, block: 1},
		{name: "incomplete", status: agent.CompletionStatusIncomplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := agent.NewSessionStore(t.TempDir())
			backend, err := New(Options{
				Provider: &fakeProvider{models: []types.ModelInfo{{ID: "model-a"}}}, DefaultModel: "model-a", Store: store,
				Runner: scriptedRunner(agent.Event{Type: "done", Data: map[string]any{
					"kind": "failed", "stopReason": "completion_blocked", "durablePolicy": "reconcile",
					"terminalState": agent.EvidenceTerminalBlocked, "completionStatus": test.status,
					"completionCriteria": 1, "completionBlocked": test.block,
				}}),
			})
			if err != nil {
				t.Fatal(err)
			}
			harness := newStdioHarness(t, backend)
			initializeStdio(t, harness)
			sessionID := newStdioSession(t, harness, t.TempDir())
			harness.send(t, map[string]any{
				"jsonrpc": "2.0", "id": 3, "method": acp.MethodSessionPrompt,
				"params": map[string]any{
					"sessionId": sessionID,
					"prompt":    []map[string]string{{"type": "text", "text": "complete contract"}},
				},
			})
			_, frame := harness.response(t, "3")
			if frame.Error == nil || frame.Error.Code != acp.CodeInvalidRequest || len(frame.Result) != 0 {
				t.Fatalf("blocked terminal frame = %#v", frame)
			}
			persisted, err := store.Get(sessionID)
			if err != nil || !persisted.ReconcileRequired || persisted.LastRunTerminal == nil ||
				!persisted.LastRunTerminal.BlocksSuccess() {
				t.Fatalf("blocked durable terminal = (%#v, %v)", persisted, err)
			}
			harness.close(t)
		})
	}
}

func TestStdioCancelDuringToolReturnsCancelledAndRequiresReconciliation(t *testing.T) {
	started := make(chan struct{})
	store := agent.NewSessionStore(t.TempDir())
	runner := RunnerFunc(func(
		ctx context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		_ agent.RunOptions,
	) (<-chan agent.Event, error) {
		stream := make(chan agent.Event, 4)
		stream <- agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-cancel", "name": "Write", "runId": "run-cancel",
			"inputDigest": "sha256:" + strings.Repeat("c", 64),
		}}
		close(started)
		go func() {
			<-ctx.Done()
			stream <- agent.Event{Type: "error", Data: "context canceled"}
			stream <- agent.Event{Type: "done", Data: map[string]any{
				"kind": "cancelled", "stopReason": "cancelled", "durablePolicy": "reconcile",
				"terminalState": agent.EvidenceTerminalBlocked,
			}}
			close(stream)
		}()
		return stream, nil
	})
	backend, err := New(Options{
		Provider: &fakeProvider{models: []types.ModelInfo{{ID: "model-a"}}}, DefaultModel: "model-a", Store: store,
		Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newStdioHarness(t, backend)
	initializeStdio(t, harness)
	sessionID := newStdioSession(t, harness, t.TempDir())
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": acp.MethodSessionPrompt,
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": "cancel during side effect"}},
		},
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start tool execution")
	}
	harness.send(t, map[string]any{
		"jsonrpc": "2.0", "method": acp.MethodSessionCancel,
		"params": map[string]string{"sessionId": sessionID},
	})
	_, frame := harness.response(t, "3")
	if frame.Error != nil {
		t.Fatalf("cancelled prompt error = %#v", frame.Error)
	}
	var response acp.PromptResponse
	if err := json.Unmarshal(frame.Result, &response); err != nil || response.StopReason != acp.StopCancelled {
		t.Fatalf("cancelled result = %s, error = %v", frame.Result, err)
	}
	persisted, err := store.Get(sessionID)
	if err != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.ToolCallID != "call-cancel" ||
		persisted.Interruption.SideEffectState != agent.SessionSideEffectStarted ||
		persisted.LastRunTerminal == nil || !persisted.LastRunTerminal.BlocksSuccess() {
		t.Fatalf("cancelled durable session = (%#v, %v)", persisted, err)
	}
	harness.close(t)
}

func eventStream(events ...agent.Event) <-chan agent.Event {
	stream := make(chan agent.Event, len(events))
	for _, event := range events {
		stream <- event
	}
	close(stream)
	return stream
}
