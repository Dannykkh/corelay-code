package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	durableRunAssistantLimitBytes  = 1 << 20
	durableRunToolResultLimitBytes = 64 << 10
)

// DurableRunObserver converts transport-neutral agent lifecycle events into a
// bounded persisted transcript delta and, when execution is interrupted, a
// digest-only reconciliation marker. It never retains raw tool input.
//
// Observe may be called concurrently by parallel tool execution paths. The
// resulting multi-tool interruption digest is sorted by tool-call ID so it is
// independent of goroutine scheduling.
type DurableRunObserver struct {
	mu sync.Mutex

	runID            string
	assistant        strings.Builder
	assistantClipped bool
	messages         []SessionMessage
	tools            []durableRunTool
	toolIndexes      map[string][]int
	ambiguousToolIDs bool
	completed        bool
	terminal         DurableRunTerminalMetadata
	terminalSet      bool
}

// DurableRunTerminalMetadata is a content-free snapshot of a typed done
// event. It deliberately contains no prompt, criterion text, evidence, tool
// input, or tool result fields.
type DurableRunTerminalMetadata struct {
	TerminalState       string                   `json:"terminalState,omitempty"`
	CompletionStatus    CompletionContractStatus `json:"completionStatus,omitempty"`
	CompletionRevision  uint64                   `json:"completionRevision,omitempty"`
	CompletionCriteria  int                      `json:"completionCriteria,omitempty"`
	CompletionSatisfied int                      `json:"completionSatisfied,omitempty"`
	CompletionBlocked   int                      `json:"completionBlocked,omitempty"`
}

type durableRunTool struct {
	RunID       string
	ToolName    string
	ToolCallID  string
	InputDigest string
	SideEffect  SessionSideEffectState
	ResultSeen  bool
}

// NewDurableRunObserver creates one run-owned observer. runID is only a
// fallback for legacy event producers; exact tool_execution_start runId values
// take precedence.
func NewDurableRunObserver(runID string) *DurableRunObserver {
	return &DurableRunObserver{runID: durableRunBoundedText(runID, 256)}
}

// SetRunID supplies the active kernel run ID before tool execution starts.
func (o *DurableRunObserver) SetRunID(runID string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.completed {
		o.runID = durableRunBoundedText(runID, 256)
	}
}

// Observe consumes only durable lifecycle data. Thinking, raw tool input,
// status text, approval metadata, and transport-specific events are ignored.
func (o *DurableRunObserver) Observe(event Event) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.completed {
		return
	}

	switch event.Type {
	case "text":
		text, ok := event.Data.(string)
		if ok {
			o.appendAssistantLocked(text)
		}
	case "tool_execution_start":
		var data struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			InputDigest string `json:"inputDigest"`
			RunID       string `json:"runId"`
		}
		if decodeDurableRunEvent(event.Data, &data) != nil {
			return
		}
		data.ID = durableRunBoundedText(data.ID, 256)
		data.Name = durableRunBoundedText(data.Name, 128)
		if data.ID == "" || data.Name == "" || !sessionInputDigestPattern.MatchString(data.InputDigest) {
			return
		}
		if o.toolIndexes == nil {
			o.toolIndexes = make(map[string][]int)
		}
		for _, index := range o.toolIndexes[data.ID] {
			if index >= 0 && index < len(o.tools) && !o.tools[index].ResultSeen {
				o.ambiguousToolIDs = true
				break
			}
		}
		o.flushAssistantLocked()
		runID := durableRunBoundedText(data.RunID, 256)
		if runID == "" {
			runID = o.runID
		}
		index := len(o.tools)
		o.tools = append(o.tools, durableRunTool{
			RunID:       runID,
			ToolName:    data.Name,
			ToolCallID:  data.ID,
			InputDigest: data.InputDigest,
			SideEffect:  SessionSideEffectStarted,
		})
		o.toolIndexes[data.ID] = append(o.toolIndexes[data.ID], index)
	case "tool_result":
		var data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Result   string `json:"result"`
			IsError  bool   `json:"isError"`
			Executed bool   `json:"executed"`
		}
		if decodeDurableRunEvent(event.Data, &data) != nil || !data.Executed {
			return
		}
		id := durableRunBoundedText(data.ID, 256)
		indexes := o.toolIndexes[id]
		unresolved := make([]int, 0, len(indexes))
		for _, index := range indexes {
			if index >= 0 && index < len(o.tools) && !o.tools[index].ResultSeen {
				unresolved = append(unresolved, index)
			}
		}
		if len(unresolved) == 0 {
			// A result without the exact pre-execution event is a proposal or an
			// incomplete lifecycle. Never synthesize durable activity from it.
			return
		}
		if len(unresolved) > 1 {
			o.ambiguousToolIDs = true
		}
		// Match the most recent unresolved occurrence. Sequential ID reuse is
		// exact; overlapping reuse remains explicitly ambiguous and is forced
		// through reconciliation at the composition boundary.
		tool := &o.tools[unresolved[len(unresolved)-1]]
		o.flushAssistantLocked()
		if data.IsError {
			tool.SideEffect = SessionSideEffectMayHaveApplied
		} else {
			tool.SideEffect = SessionSideEffectApplied
		}
		tool.ResultSeen = true
		result := durableRunToolResult(data.Result)
		o.messages = append(o.messages, SessionMessage{
			Role:       "tool",
			Content:    result,
			ToolName:   tool.ToolName,
			ToolInput:  durableRunToolReference(tool),
			ToolResult: result,
			IsError:    data.IsError,
			Timestamp:  time.Now().UTC(),
		})
	case "done":
		if terminal, ok := durableRunTerminalFromEvent(event.Data); ok {
			o.terminal = terminal
			o.terminalSet = true
		}
		o.flushAssistantLocked()
		o.completed = true
	}
}

// Completed reports whether the kernel emitted a stream-committable done
// event. A typed blocked completion is still a normal stream terminal, but its
// TerminalMetadata must not be interpreted as task success.
func (o *DurableRunObserver) Completed() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.completed
}

// TerminalMetadata returns the bounded content-free metadata carried by a
// typed done event. Legacy nil/default done events preserve historical
// behavior and return ok=false.
func (o *DurableRunObserver) TerminalMetadata() (metadata DurableRunTerminalMetadata, ok bool) {
	if o == nil {
		return DurableRunTerminalMetadata{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.completed || !o.terminalSet {
		return DurableRunTerminalMetadata{}, false
	}
	return o.terminal, true
}

// DecodeDurableRunTerminalMetadata decodes the content-free terminal fields
// carried by a done event. Consumers outside the agent package must use this
// boundary instead of treating every done frame as successful completion.
func DecodeDurableRunTerminalMetadata(value any) (DurableRunTerminalMetadata, bool) {
	return durableRunTerminalFromEvent(value)
}

// BlocksSuccess reports whether a stream-committable terminal must not be
// interpreted as task success. Blocked and incomplete CompletionContract
// states remain durable so a later resume can reconcile or continue them.
func (m DurableRunTerminalMetadata) BlocksSuccess() bool {
	return m.TerminalState == EvidenceTerminalBlocked ||
		m.CompletionStatus == CompletionStatusIncomplete ||
		m.CompletionStatus == CompletionStatusBlocked ||
		m.CompletionBlocked > 0
}

// Messages returns a deep copy of the completed durable transcript delta. It
// returns nil before normal completion so partial assistant output cannot be
// mistaken for a committed turn.
func (o *DurableRunObserver) Messages() []SessionMessage {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.completed {
		return nil
	}
	return cloneDurableRunMessages(o.messages)
}

func durableRunTerminalFromEvent(value any) (DurableRunTerminalMetadata, bool) {
	if value == nil {
		return DurableRunTerminalMetadata{}, false
	}
	var input struct {
		TerminalState       *string                   `json:"terminalState"`
		CompletionStatus    *CompletionContractStatus `json:"completionStatus"`
		CompletionRevision  *uint64                   `json:"completionRevision"`
		CompletionCriteria  *int                      `json:"completionCriteria"`
		CompletionSatisfied *int                      `json:"completionSatisfied"`
		CompletionBlocked   *int                      `json:"completionBlocked"`
	}
	if decodeDurableRunEvent(value, &input) != nil {
		return DurableRunTerminalMetadata{}, false
	}
	result := DurableRunTerminalMetadata{}
	set := false
	if input.TerminalState != nil {
		if terminalState := durableRunTerminalState(*input.TerminalState); terminalState != "" {
			result.TerminalState = terminalState
			set = true
		}
	}
	if input.CompletionStatus != nil {
		result.CompletionStatus = sanitizeCompletionContractStatus(*input.CompletionStatus)
		set = true
	}
	if input.CompletionRevision != nil {
		result.CompletionRevision = *input.CompletionRevision
		set = true
	}
	if input.CompletionCriteria != nil && validDurableRunCompletionCount(*input.CompletionCriteria) {
		result.CompletionCriteria = *input.CompletionCriteria
		set = true
	}
	if input.CompletionSatisfied != nil && validDurableRunCompletionCount(*input.CompletionSatisfied) {
		result.CompletionSatisfied = *input.CompletionSatisfied
		set = true
	}
	if input.CompletionBlocked != nil && validDurableRunCompletionCount(*input.CompletionBlocked) {
		result.CompletionBlocked = *input.CompletionBlocked
		set = true
	}
	if result.CompletionBlocked > 0 {
		result.CompletionStatus = CompletionStatusBlocked
	}
	if result.CompletionStatus == CompletionStatusIncomplete || result.CompletionStatus == CompletionStatusBlocked {
		result.TerminalState = EvidenceTerminalBlocked
	}
	return result, set
}

func durableRunTerminalState(value string) string {
	switch value {
	case EvidenceTerminalVerified, EvidenceTerminalPartiallyVerified, EvidenceTerminalUnverified, EvidenceTerminalBlocked:
		return value
	default:
		return ""
	}
}

func validDurableRunCompletionCount(value int) bool {
	return value >= 0 && value <= maxCompletionCriteria
}

// ReconciliationRequired reports lifecycle ambiguity that must not be
// committed as a normal resumable turn: an execution without a result, or
// overlapping reuse of the same tool-call ID.
func (o *DurableRunObserver) ReconciliationRequired() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ambiguousToolIDs {
		return true
	}
	for index := range o.tools {
		if !o.tools[index].ResultSeen {
			return true
		}
	}
	return false
}

// HasToolActivity reports whether an exact pre-execution event was observed.
// It is used only to decide whether a persistence failure must quarantine the
// runtime session; it exposes no input or output.
func (o *DurableRunObserver) HasToolActivity() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.tools) > 0
}

// Interruption returns a bounded reconciliation marker only when at least one
// exact execution-start event was observed and the run did not complete.
func (o *DurableRunObserver) Interruption(reason string) *SessionInterruption {
	return o.reconciliationMarker(reason, false)
}

// ReconciliationMarker also covers a normally completed tool run whose
// transcript failed its durable CAS commit. In that case the external side
// effect may be real even though the terminal event was observed.
func (o *DurableRunObserver) ReconciliationMarker(reason string) *SessionInterruption {
	return o.reconciliationMarker(reason, true)
}

func (o *DurableRunObserver) reconciliationMarker(reason string, includeCompleted bool) *SessionInterruption {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if (!includeCompleted && o.completed) || len(o.tools) == 0 {
		return nil
	}
	tool, summary := o.interruptionToolLocked()
	if tool.RunID == "" {
		// Preserve compatibility without inventing a structured identity.
		return &SessionInterruption{
			At:     time.Now().UTC(),
			Reason: durableRunBoundedText(reason, 1024),
		}
	}
	return &SessionInterruption{
		At:              time.Now().UTC(),
		Reason:          durableRunBoundedText(reason, 1024),
		RunID:           tool.RunID,
		ToolName:        tool.ToolName,
		ToolCallID:      tool.ToolCallID,
		InputDigest:     tool.InputDigest,
		SideEffectState: tool.SideEffect,
		Summary:         summary,
	}
}

func (o *DurableRunObserver) interruptionToolLocked() (durableRunTool, string) {
	if len(o.tools) == 1 {
		return o.tools[0], "Agent run ended after tool execution began; reconcile the recorded side effect before resuming."
	}
	type digestState struct {
		ToolCallID  string                 `json:"toolCallId"`
		ToolName    string                 `json:"toolName"`
		InputDigest string                 `json:"inputDigest"`
		SideEffect  SessionSideEffectState `json:"sideEffectState"`
	}
	tools := append([]durableRunTool(nil), o.tools...)
	sort.SliceStable(tools, func(left, right int) bool {
		if tools[left].ToolCallID != tools[right].ToolCallID {
			return tools[left].ToolCallID < tools[right].ToolCallID
		}
		if tools[left].InputDigest != tools[right].InputDigest {
			return tools[left].InputDigest < tools[right].InputDigest
		}
		return tools[left].ToolName < tools[right].ToolName
	})
	states := make([]digestState, 0, len(tools))
	combined := SessionSideEffectApplied
	runID := ""
	for index := range tools {
		tool := &tools[index]
		states = append(states, digestState{
			ToolCallID:  tool.ToolCallID,
			ToolName:    tool.ToolName,
			InputDigest: tool.InputDigest,
			SideEffect:  tool.SideEffect,
		})
		if runID == "" {
			runID = tool.RunID
		} else if runID != tool.RunID {
			runID = o.runID
		}
		if tool.SideEffect != SessionSideEffectApplied {
			combined = SessionSideEffectMayHaveApplied
		}
	}
	encoded, _ := json.Marshal(states)
	digest := sha256.Sum256(encoded)
	return durableRunTool{
			RunID:       runID,
			ToolName:    "multiple_tools",
			ToolCallID:  "multiple",
			InputDigest: fmt.Sprintf("sha256:%x", digest),
			SideEffect:  combined,
		}, fmt.Sprintf(
			"Agent run ended after %d tool executions began; reconcile every side effect represented by the aggregate digest before resuming.",
			len(states),
		)
}

func (o *DurableRunObserver) appendAssistantLocked(value string) {
	if value == "" || o.assistantClipped {
		return
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	remaining := durableRunAssistantLimitBytes - o.assistant.Len()
	if remaining <= 0 {
		o.assistantClipped = true
		return
	}
	if len(value) > remaining {
		o.assistant.WriteString(contextUTF8Prefix(value, remaining))
		o.assistantClipped = true
		return
	}
	o.assistant.WriteString(value)
}

func (o *DurableRunObserver) flushAssistantLocked() {
	if o.assistant.Len() == 0 && !o.assistantClipped {
		return
	}
	content := o.assistant.String()
	if o.assistantClipped {
		content += "\n\n[durable transcript truncated]"
	}
	content = redactSensitiveString(content)
	o.messages = append(o.messages, SessionMessage{
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now().UTC(),
	})
	o.assistant.Reset()
	o.assistantClipped = false
}

func decodeDurableRunEvent(value any, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func durableRunToolReference(tool *durableRunTool) map[string]string {
	if tool == nil {
		return nil
	}
	return map[string]string{
		"toolCallId":  tool.ToolCallID,
		"inputDigest": tool.InputDigest,
	}
}

func durableRunToolResult(value string) string {
	value = durableRunBoundedText(value, durableRunToolResultLimitBytes)
	return sanitizeReceiptString(value)
}

func durableRunBoundedText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	if limit > 0 && len(value) > limit {
		value = contextUTF8Prefix(value, limit)
	}
	return value
}

func cloneDurableRunMessages(messages []SessionMessage) []SessionMessage {
	if messages == nil {
		return nil
	}
	result := make([]SessionMessage, len(messages))
	for index := range messages {
		result[index] = messages[index]
		if reference, ok := messages[index].ToolInput.(map[string]string); ok {
			copyReference := make(map[string]string, len(reference))
			for key, value := range reference {
				copyReference[key] = value
			}
			result[index].ToolInput = copyReference
		}
	}
	return result
}
