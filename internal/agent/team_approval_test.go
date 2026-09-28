package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// delayedApprovalRequester mints a distinct approval per tool call and
// resolves each one only after delay, standing in for a slow human reviewer.
type delayedApprovalRequester struct {
	mu         sync.Mutex
	delay      time.Duration
	nextID     int
	opened     []string
	resolved   []string
	waiting    int
	maxWaiting int
}

func (r *delayedApprovalRequester) Open(draft approval.Draft) (approval.Pending, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := fmt.Sprintf("delayed-appr-%d", r.nextID)
	r.opened = append(r.opened, id)
	return approval.Pending{
		ID:                      id,
		SessionID:               draft.SessionID,
		SessionRevision:         draft.SessionRevision,
		RunID:                   draft.RunID,
		ToolCallID:              draft.ToolCallID,
		ToolName:                draft.ToolName,
		ExecutorID:              draft.ExecutorID,
		RedactedInput:           draft.RedactedInput,
		InputDigest:             draft.InputDigest,
		ExecutionPolicyRevision: draft.ExecutionPolicyRevision,
		FullSelectionRevision:   draft.FullSelectionRevision,
		ApprovalSource:          approval.ApprovalSourceUser,
		DangerLevel:             draft.DangerLevel,
		Scope:                   draft.Scope,
		ExpiresAt:               time.Now().Add(5 * time.Minute),
	}, nil
}

func (r *delayedApprovalRequester) Await(ctx context.Context, sessionID, approvalID string) (approval.Resolution, error) {
	r.mu.Lock()
	r.waiting++
	if r.waiting > r.maxWaiting {
		r.maxWaiting = r.waiting
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.waiting--
		r.mu.Unlock()
	}()
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		r.mu.Lock()
		r.resolved = append(r.resolved, approvalID)
		r.mu.Unlock()
		return approval.Resolution{
			ApprovalID: approvalID,
			Outcome:    approval.OutcomeAllowOnce,
			Reason:     approval.ReasonUser,
			ResolvedAt: time.Now(),
		}, nil
	case <-ctx.Done():
		return approval.Resolution{}, ctx.Err()
	}
}

func (r *delayedApprovalRequester) snapshot() (opened, resolved []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.opened...), append([]string(nil), r.resolved...)
}

// approvalSimulatingProvider asks each worker for one mutating Bash call on its
// first turn and finishes once the transcript holds that assistant turn, so
// concurrent workers each reach exactly one approval.
type approvalSimulatingProvider struct {
	mu    sync.Mutex
	calls int
}

func (*approvalSimulatingProvider) Name() string              { return "approval-sim" }
func (*approvalSimulatingProvider) DisplayName() string       { return "Approval Simulating Provider" }
func (*approvalSimulatingProvider) Models() []types.ModelInfo { return nil }
func (*approvalSimulatingProvider) Validate() error           { return nil }

func (p *approvalSimulatingProvider) StreamMessage(
	ctx context.Context,
	req *types.MessagesRequest,
	opts *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.calls++
	callNum := p.calls
	p.mu.Unlock()

	finish := false
	for _, message := range req.Messages {
		if message.Role == "assistant" {
			finish = true
			break
		}
	}

	events := make(chan types.SSEEvent, 10)
	if !finish {
		events <- types.SSEEvent{
			Type: "content_block_start",
			ContentBlock: teamTerminalJSON(map[string]string{
				"type": "tool_use",
				"id":   fmt.Sprintf("call-bash-%d", callNum),
				"name": "Bash",
			}),
		}
		events <- types.SSEEvent{
			Type: "content_block_delta",
			Delta: teamTerminalJSON(map[string]string{
				"type":         "input_json_delta",
				"partial_json": fmt.Sprintf(`{"command":"mkdir approval_dir_%d"}`, callNum),
			}),
		}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_delta", Delta: teamTerminalJSON(map[string]string{"stop_reason": "tool_use"})}
		events <- types.SSEEvent{Type: "message_stop"}
	} else {
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: teamTerminalJSON(map[string]string{"type": "text"})}
		events <- types.SSEEvent{
			Type: "content_block_delta",
			Delta: teamTerminalJSON(map[string]string{
				"type": "text_delta",
				"text": "Task finished successfully.",
			}),
		}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_delta", Delta: teamTerminalJSON(map[string]string{"stop_reason": "end_turn"})}
		events <- types.SSEEvent{Type: "message_stop"}
	}
	close(events)
	return events, nil
}

// stalledProvider never answers; its stream closes only when the run context
// ends, so the wave budget is the only thing that can stop the worker.
type stalledProvider struct{}

func (stalledProvider) Name() string              { return "stalled" }
func (stalledProvider) DisplayName() string       { return "Stalled Provider" }
func (stalledProvider) Models() []types.ModelInfo { return nil }
func (stalledProvider) Validate() error           { return nil }

func (stalledProvider) StreamMessage(
	ctx context.Context,
	_ *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	events := make(chan types.SSEEvent)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events, nil
}

type teamFailureRecorder struct {
	mu       sync.Mutex
	failures []string
}

func (*teamFailureRecorder) RunStarted()                         {}
func (*teamFailureRecorder) ReceiptWritten(string, AgentReceipt) {}
func (*teamFailureRecorder) RunCompleted(RunSummary)             {}

func (r *teamFailureRecorder) RunFailed(message string) {
	r.mu.Lock()
	r.failures = append(r.failures, message)
	r.mu.Unlock()
}

func (r *teamFailureRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.failures...)
}

// executeTeamWavesCollectingApprovals drains the team event channel until the
// run returns, keeping approval_required payloads for assertions.
func executeTeamWavesCollectingApprovals(team *Team) ([]map[string]interface{}, error) {
	eventCh := make(chan Event, 50)
	var approvals []map[string]interface{}
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for event := range eventCh {
			if event.Type != "approval_required" {
				continue
			}
			if data, ok := event.Data.(map[string]interface{}); ok {
				approvals = append(approvals, data)
			}
		}
	}()
	err := team.ExecuteWaves(context.Background(), eventCh)
	close(eventCh)
	<-doneCh
	return approvals, err
}

func TestTeamWave_ApprovalWaitDoesNotExhaustCycleTimeout(t *testing.T) {
	isolateEvidenceLoopTest(t)
	cycleTimeout := 1500 * time.Millisecond
	approvalDelay := 2 * time.Second

	requester := &delayedApprovalRequester{delay: approvalDelay}
	provider := &approvalSimulatingProvider{}
	workspace := t.TempDir()

	team := NewTeam(provider, "test-model", workspace, t.TempDir(), TeamConfig{
		Name:              "team-approval-timeout-test",
		CycleTimeout:      cycleTimeout,
		ApprovalRequester: requester,
		SessionID:         "session-test-approval",
		DisablePlugins:    true,
	})

	task := &TeamTask{
		ID:          "task-with-approval",
		Name:        "task with approval",
		Description: "run a command needing approval",
	}
	team.tasks = []*TeamTask{task}

	if _, err := executeTeamWavesCollectingApprovals(team); err != nil {
		t.Fatalf("expected wave to succeed despite approval wait > cycleTimeout, got: %v", err)
	}

	if task.Status != "completed" {
		t.Fatalf("expected task status 'completed', got %q (result %q)", task.Status, task.Result)
	}

	opened, resolved := requester.snapshot()
	if len(opened) != 1 || len(resolved) != 1 {
		t.Fatalf("expected one approval opened and resolved, got opened=%v resolved=%v", opened, resolved)
	}
}

func TestTeamWave_ConcurrentApprovalsBothPauseCycleBudget(t *testing.T) {
	isolateEvidenceLoopTest(t)
	cycleTimeout := 1500 * time.Millisecond
	approvalDelay := 2500 * time.Millisecond

	requester := &delayedApprovalRequester{delay: approvalDelay}
	provider := &approvalSimulatingProvider{}
	team := NewTeam(provider, "test-model", t.TempDir(), t.TempDir(), TeamConfig{
		Name:              "team-concurrent-approval-test",
		CycleTimeout:      cycleTimeout,
		ApprovalRequester: requester,
		SessionID:         "session-test-concurrent-approval",
		DisablePlugins:    true,
		// Unlocked file scope lets both unscoped tasks share one batch so
		// their approval waits overlap.
		Capacity: CapacityConfig{MaxParallelTasks: 2, FileScopeLockConfigured: true},
	})
	first := &TeamTask{ID: "approval-a", Name: "approval a", Description: "run a command needing approval"}
	second := &TeamTask{ID: "approval-b", Name: "approval b", Description: "run a command needing approval"}
	team.tasks = []*TeamTask{first, second}
	if waves, err := team.ComputeWaves(); err != nil || len(waves) != 1 || len(waves[0]) != 2 {
		t.Fatalf("expected both tasks in one wave, got waves=%v err=%v", waves, err)
	}

	approvals, err := executeTeamWavesCollectingApprovals(team)
	if err != nil {
		t.Fatalf("expected wave to succeed while two approvals outlast the cycle budget, got: %v", err)
	}
	for _, task := range []*TeamTask{first, second} {
		if task.Status != "completed" {
			t.Fatalf("task %s status = %q (result %q), want completed", task.ID, task.Status, task.Result)
		}
	}
	requester.mu.Lock()
	maxWaiting := requester.maxWaiting
	requester.mu.Unlock()
	if maxWaiting != 2 {
		t.Fatalf("approval waits did not overlap: max concurrent waits = %d, want 2", maxWaiting)
	}

	opened, resolved := requester.snapshot()
	sort.Strings(opened)
	sort.Strings(resolved)
	if len(opened) != 2 || opened[0] == opened[1] || strings.Join(opened, ",") != strings.Join(resolved, ",") {
		t.Fatalf("expected two distinct approvals opened and resolved, got opened=%v resolved=%v", opened, resolved)
	}

	var seenIDs []string
	toolCallIDs := map[string]bool{}
	for _, data := range approvals {
		id, _ := data["id"].(string)
		toolCallID, _ := data["toolCallId"].(string)
		if toolCallID == "" {
			t.Fatalf("approval_required event lacks toolCallId: %#v", data)
		}
		seenIDs = append(seenIDs, id)
		toolCallIDs[toolCallID] = true
	}
	sort.Strings(seenIDs)
	if strings.Join(seenIDs, ",") != strings.Join(opened, ",") || len(toolCallIDs) != 2 {
		t.Fatalf("approval events ids=%v toolCallIds=%v, want %v with two distinct tool calls", seenIDs, toolCallIDs, opened)
	}
}

func TestTeamWave_BudgetExpiryWithoutApprovalRecordsDeadlineExceeded(t *testing.T) {
	isolateEvidenceLoopTest(t)
	// Stays below the one-second heartbeat so the kernel's terminal frame is
	// the first event the worker observes after expiry.
	cycleTimeout := 600 * time.Millisecond

	requester := &delayedApprovalRequester{delay: time.Minute}
	recorder := &teamFailureRecorder{}
	team := NewTeam(stalledProvider{}, "test-model", t.TempDir(), t.TempDir(), TeamConfig{
		Name:              "team-budget-deadline-test",
		CycleTimeout:      cycleTimeout,
		ApprovalRequester: requester,
		SessionID:         "session-test-budget-deadline",
		DisablePlugins:    true,
		Recorder:          recorder,
	})
	task := &TeamTask{ID: "stalled-task", Name: "stalled task", Description: "wait for a provider that never answers"}
	team.tasks = []*TeamTask{task}

	approvals, err := executeTeamWavesCollectingApprovals(team)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected wave to fail with the cycle deadline, got: %v", err)
	}
	if len(approvals) != 0 {
		t.Fatalf("no approval should have been requested, got %#v", approvals)
	}
	if opened, _ := requester.snapshot(); len(opened) != 0 {
		t.Fatalf("no approval should have been opened, got %v", opened)
	}
	if task.Status != "failed" || !strings.Contains(task.Result, "deadline") || strings.Contains(task.Result, "cancel") {
		t.Fatalf("task status = %q result = %q, want failed with a deadline cause", task.Status, task.Result)
	}
	if failures := recorder.snapshot(); len(failures) != 1 || failures[0] != context.DeadlineExceeded.Error() {
		t.Fatalf("worker run failures = %q, want one %q", failures, context.DeadlineExceeded.Error())
	}
}
