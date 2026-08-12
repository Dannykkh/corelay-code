package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const subAgentPartialTextLimit = 8 << 10

// subAgentRunMode is deliberately task-local. It decides only that the first
// tool-less assistant turn is terminal; the common Agent Kernel owns every
// provider, tool, safety, evidence, and persistence transition.
type subAgentRunMode struct {
	taskID      string
	taskName    string
	instruction string

	started        bool
	finished       bool
	startedAt      time.Time
	finalIteration int
	finalText      string
}

var _ RunMode = (*subAgentRunMode)(nil)

type subAgentRunModeState struct {
	TaskID          string    `json:"taskId,omitempty"`
	Started         bool      `json:"started"`
	Finished        bool      `json:"finished"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	FinalIteration  int       `json:"finalIteration,omitempty"`
	TotalTools      int       `json:"totalTools"`
	EstimatedTokens int       `json:"estimatedTokens"`
	SandboxReports  int       `json:"sandboxReports"`
}

func newSubAgentRunMode(task *SubAgentTask) *subAgentRunMode {
	mode := &subAgentRunMode{}
	if task == nil {
		return mode
	}
	mode.taskID = strings.TrimSpace(task.ID)
	mode.taskName = strings.TrimSpace(task.Name)
	mode.instruction = task.Instruction
	return mode
}

func (m *subAgentRunMode) Name() string { return "subagent" }

func (m *subAgentRunMode) SystemPromptSuffix() string {
	taskName := m.taskName
	if taskName == "" {
		taskName = m.taskID
	}
	return fmt.Sprintf(`/no_think
## SubAgent Task
Task: %s
Instruction: %s
Write/Edit targets are restricted to the ownership scope assigned to this task by the Agent Kernel.`,
		taskName,
		m.instruction,
	)
}

func (m *subAgentRunMode) Start(now time.Time) (RunModeDirective, error) {
	if m.started {
		return RunModeDirective{}, fmt.Errorf("SubAgent run mode has already started")
	}
	m.started = true
	m.startedAt = now
	return RunModeDirective{
		Continue:   true,
		UserPrompt: m.instruction,
	}, nil
}

func (m *subAgentRunMode) Advance(turn RunModeTurn) (RunModeDirective, error) {
	if !m.started {
		return RunModeDirective{}, fmt.Errorf("SubAgent run mode has not started")
	}
	// The kernel can revisit a terminal mode after its independent verification
	// or CompletionContract gate requests more work. Each such tool-less turn is
	// still terminal, and the latest one is the result preserved for the caller.
	m.finished = true
	m.finalIteration = turn.Iteration
	m.finalText = turn.Text
	return RunModeDirective{
		Continue:   false,
		StopReason: "complete",
	}, nil
}

func (m *subAgentRunMode) CurrentStep() string { return m.taskID }

func (m *subAgentRunMode) Snapshot(metrics RunModeMetrics) RunModeSnapshot {
	totalTools := metrics.TotalTools
	if totalTools < 0 {
		totalTools = 0
	}
	estimatedTokens := metrics.EstimatedTokens
	if estimatedTokens < 0 {
		estimatedTokens = 0
	}
	stopReason := ""
	if m.finished {
		stopReason = "complete"
	}
	return RunModeSnapshot{
		Name:       m.Name(),
		StopReason: stopReason,
		State: subAgentRunModeState{
			TaskID:          sanitizeSnapshotText(m.taskID, maxRunModeNameBytes),
			Started:         m.started,
			Finished:        m.finished,
			StartedAt:       m.startedAt,
			FinalIteration:  m.finalIteration,
			TotalTools:      totalTools,
			EstimatedTokens: estimatedTokens,
			SandboxReports:  len(metrics.Sandbox),
		},
	}
}

func (m *subAgentRunMode) FinalText() string {
	if m == nil {
		return ""
	}
	return m.finalText
}

type subAgentRunResult struct {
	Success   bool
	Text      string
	ToolCalls int
	Sandbox   []SandboxExecutionRecord
}

// subAgentEventReducer consumes the transport-neutral kernel event stream. It
// is observational only: recorder calls remain inside RunLoopWithOptions.
type subAgentEventReducer struct {
	observer  *DurableRunObserver
	mode      *subAgentRunMode
	failure   string
	toolCalls int
	sandbox   []SandboxExecutionRecord
	partial   string
}

func newSubAgentEventReducer(taskID string, mode *subAgentRunMode) *subAgentEventReducer {
	return &subAgentEventReducer{
		observer: NewDurableRunObserver(taskID),
		mode:     mode,
	}
}

func (r *subAgentEventReducer) Observe(event Event) {
	if r == nil {
		return
	}
	// Every kernel event reaches the durable observer, including events this
	// reducer does not otherwise interpret.
	r.observer.Observe(event)

	switch event.Type {
	case "tool_result":
		r.toolCalls++
		r.partial = ""
		if record, ok := subAgentSandboxRecord(event.Data); ok {
			r.sandbox = append(r.sandbox, record)
		}
	case "text":
		if text, ok := event.Data.(string); ok {
			r.appendPartial(text)
		}
	case "context_blocked":
		r.setFailure(subAgentContextBlockedMessage(event.Data))
	case "error":
		r.setFailure(subAgentEventMessage(event.Data, "Sub-agent run failed"))
	case "blocked":
		r.setFailure(subAgentEventMessage(event.Data, "Sub-agent run was blocked"))
	case "incomplete":
		r.setFailure(subAgentEventMessage(event.Data, "Sub-agent run remained incomplete"))
	case "canceled", "cancelled", "context_canceled", "context_cancelled":
		r.setFailure("Sub-agent run was canceled")
	case "deadline", "deadline_exceeded":
		r.setFailure("Sub-agent run deadline exceeded")
	case "done":
		if metadata, ok := DecodeDurableRunTerminalMetadata(event.Data); ok && metadata.BlocksSuccess() {
			r.setFailure(subAgentBlockedCompletionMessage(metadata))
		}
	}
}

func (r *subAgentEventReducer) Result(runErr error) subAgentRunResult {
	result := subAgentRunResult{
		ToolCalls: r.toolCalls,
		Sandbox:   append([]SandboxExecutionRecord(nil), r.sandbox...),
	}

	if r.failure != "" {
		result.Text = r.failureText(r.failure)
		return result
	}
	// A normal done event is the kernel's terminal commit point. Cancellation
	// observed only after that point must not retroactively fail the task.
	if r.observer != nil && r.observer.Completed() {
		if metadata, ok := r.observer.TerminalMetadata(); ok && metadata.BlocksSuccess() {
			result.Text = r.failureText(subAgentBlockedCompletionMessage(metadata))
			return result
		}
		result.Success = true
		if r.mode != nil {
			result.Text = r.mode.FinalText()
		}
		return result
	}
	if runErr != nil {
		switch {
		case errors.Is(runErr, context.DeadlineExceeded):
			result.Text = r.failureText("Sub-agent run deadline exceeded")
		case errors.Is(runErr, context.Canceled):
			result.Text = r.failureText("Sub-agent run was canceled")
		default:
			result.Text = r.failureText("Sub-agent run context ended")
		}
		return result
	}
	if r.observer == nil || !r.observer.Completed() {
		result.Text = r.failureText("Sub-agent event stream closed without a done event")
		return result
	}
	return result
}

func (r *subAgentEventReducer) appendPartial(value string) {
	if value == "" || len(r.partial) >= subAgentPartialTextLimit {
		return
	}
	remaining := subAgentPartialTextLimit - len(r.partial)
	if len(value) > remaining {
		value = contextUTF8Prefix(value, remaining)
	}
	r.partial += value
}

func (r *subAgentEventReducer) failureText(reason string) string {
	result := subAgentFailureText(reason)
	partial := sanitizeSnapshotText(r.partial, subAgentPartialTextLimit)
	if partial != "" {
		result += "\n\nPartial response:\n" + partial
	}
	return result
}

func (r *subAgentEventReducer) setFailure(message string) {
	if r.failure != "" {
		return
	}
	r.failure = strings.TrimSpace(message)
	if r.failure == "" {
		r.failure = "Sub-agent run failed"
	}
}

func subAgentFailureText(message string) string {
	message = sanitizeSnapshotText(message, 1000)
	if message == "" {
		message = "Sub-agent run failed"
	}
	if strings.HasPrefix(strings.ToLower(message), "error:") {
		return message
	}
	return "Error: " + message
}

func subAgentBlockedCompletionMessage(metadata DurableRunTerminalMetadata) string {
	switch metadata.CompletionStatus {
	case CompletionStatusIncomplete:
		return "Sub-agent completion remained incomplete"
	case CompletionStatusBlocked:
		return "Sub-agent completion was blocked"
	default:
		return "Sub-agent completion terminal was blocked"
	}
}

func subAgentContextBlockedMessage(data any) string {
	switch value := data.(type) {
	case ContextBlockedEvent:
		return subAgentEventMessage(value.Message, "Sub-agent context planning was blocked")
	case *ContextBlockedEvent:
		if value != nil {
			return subAgentEventMessage(value.Message, "Sub-agent context planning was blocked")
		}
	}
	var decoded ContextBlockedEvent
	if encoded, err := json.Marshal(data); err == nil && json.Unmarshal(encoded, &decoded) == nil {
		return subAgentEventMessage(decoded.Message, "Sub-agent context planning was blocked")
	}
	return "Sub-agent context planning was blocked"
}

func subAgentEventMessage(data any, fallback string) string {
	switch value := data.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			return value
		}
	case error:
		if value != nil && strings.TrimSpace(value.Error()) != "" {
			return value.Error()
		}
	case fmt.Stringer:
		if value != nil && strings.TrimSpace(value.String()) != "" {
			return value.String()
		}
	}
	var decoded struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Reason  string `json:"reason"`
	}
	if encoded, err := json.Marshal(data); err == nil && json.Unmarshal(encoded, &decoded) == nil {
		for _, value := range []string{decoded.Message, decoded.Error, decoded.Reason} {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return fallback
}

func subAgentSandboxRecord(data any) (SandboxExecutionRecord, bool) {
	var decoded struct {
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Sandbox *sandbox.Report `json:"sandbox"`
	}
	encoded, err := json.Marshal(data)
	if err != nil || json.Unmarshal(encoded, &decoded) != nil || decoded.Sandbox == nil {
		return SandboxExecutionRecord{}, false
	}
	return SandboxExecutionRecord{
		ToolID:   decoded.ID,
		ToolName: decoded.Name,
		Report:   *decoded.Sandbox,
	}, true
}
