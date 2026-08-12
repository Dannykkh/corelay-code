package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	maxDurableRunTextBytes = 1 << 20
)

var errDurableTranscriptMismatch = errors.New("durable session transcript does not match the run request")

type durableSessionStore interface {
	Get(string) (*agent.Session, error)
	SaveExpected(*agent.Session, uint64) error
	CommitInterruptedRun(*agent.Session, uint64, agent.SessionInterruption) error
	MarkInterrupted(string, uint64, agent.SessionInterruption) (*agent.Session, error)
	UpdateInterruptedRun(string, uint64, agent.SessionInterruption, agent.SessionInterruption) (*agent.Session, error)
	OpenToolResultMemory(string, string) (*agent.SessionMemory, []agent.ToolResultReference, error)
}

// durableAgentRun binds one live run to one exact persisted revision. It owns
// only transcript/checkpoint bookkeeping; model execution remains in the
// existing RunLoopWithOptions kernel.
type durableAgentRun struct {
	store            durableSessionStore
	session          agent.Session
	expectedRevision uint64
	observer         *agent.DurableRunObserver
	toolResultMemory *agent.SessionMemory
	toolResultRefs   []agent.ToolResultReference
	journalMu        sync.Mutex
	journalMarker    *agent.SessionInterruption
	journalEntries   []agent.ToolExecutionJournalEntry
	// completed mirrors observer.Completed for the existing SSE cleanup
	// decision in server.go. Transcript and interruption state remain owned by
	// the transport-neutral observer.
	completed  bool
	quarantine func(string)
}

func prepareDurableAgentRun(
	store durableSessionStore,
	sessionID string,
	expectedRevision uint64,
	workDir string,
	requestMessages []types.Message,
) (*durableAgentRun, error) {
	if store == nil {
		return nil, errors.New("durable session store is unavailable")
	}
	session, err := store.Get(strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	if session.Revision != expectedRevision {
		return nil, &agent.SessionRevisionConflictError{
			SessionID: session.ID,
			Expected:  expectedRevision,
			Current:   session.Revision,
		}
	}
	if session.ReconcileRequired || session.LifecycleStatus == agent.SessionLifecycleInterrupted ||
		session.LifecycleStatus == agent.SessionLifecycleRecoveryNeeded {
		return nil, fmt.Errorf("%w: session %s", agent.ErrSessionReconcileRequired, session.ID)
	}
	if session.LifecycleStatus == agent.SessionLifecycleClosed {
		return nil, fmt.Errorf("%w: closed session cannot start a run", agent.ErrSessionLifecycleInvalid)
	}
	if !sameDurableWorkspace(session.Workspace, workDir) {
		return nil, fmt.Errorf("%w: durable session belongs to another workspace", agent.ErrSessionConflict)
	}
	if err := validateDurableRunTranscript(session.Messages, requestMessages); err != nil {
		return nil, err
	}
	toolResultMemory, toolResultRefs, err := store.OpenToolResultMemory(session.ID, workDir)
	if err != nil {
		return nil, err
	}
	return &durableAgentRun{
		store:            store,
		session:          cloneDurableSession(*session),
		expectedRevision: expectedRevision,
		observer:         agent.NewDurableRunObserver(""),
		toolResultMemory: toolResultMemory,
		toolResultRefs:   append([]agent.ToolResultReference(nil), toolResultRefs...),
	}, nil
}

func (run *durableAgentRun) SetRuntimeRunID(id string) {
	if run != nil {
		run.observer.SetRunID(strings.TrimSpace(id))
	}
}

func (run *durableAgentRun) Observe(event agent.Event) {
	if run != nil && run.observer != nil {
		run.observer.Observe(event)
		run.completed = run.observer.Completed()
	}
}

// JournalToolExecution synchronously persists the first exact execution
// marker for this kernel run. Later tools in the same run are idempotent only
// after the marker is re-read from the store; a stale or different run never
// adopts the current revision.
func (run *durableAgentRun) JournalToolExecution(entry agent.ToolExecutionJournalEntry) error {
	if run == nil || run.store == nil {
		return errors.New("durable session store is unavailable")
	}
	entry = agent.ToolExecutionJournalEntry{
		ID: strings.TrimSpace(entry.ID), Name: strings.TrimSpace(entry.Name),
		InputDigest: strings.TrimSpace(entry.InputDigest), RunID: strings.TrimSpace(entry.RunID),
	}
	const reason = "Tool execution began; a successful terminal must atomically commit or explicit reconciliation is required."
	marker, err := agent.AggregateToolExecutionJournal([]agent.ToolExecutionJournalEntry{entry}, time.Time{}, reason)
	if err != nil {
		return err
	}

	run.journalMu.Lock()
	defer run.journalMu.Unlock()
	if run.journalMarker != nil {
		persisted, err := run.store.Get(run.session.ID)
		if err != nil {
			return err
		}
		existing := run.journalMarker
		if existing.RunID != marker.RunID ||
			!persisted.ReconcileRequired || persisted.Interruption == nil ||
			!sameDurableInterruption(persisted.Interruption, existing) ||
			persisted.Revision != run.expectedRevision {
			return agent.ErrSessionReconcileRequired
		}
		nextEntries := append(append([]agent.ToolExecutionJournalEntry(nil), run.journalEntries...), entry)
		nextMarker, aggregateErr := agent.AggregateToolExecutionJournal(nextEntries, existing.At, reason)
		if aggregateErr != nil {
			return aggregateErr
		}
		updated, updateErr := run.store.UpdateInterruptedRun(
			run.session.ID, run.expectedRevision, *existing, nextMarker,
		)
		if updateErr != nil {
			return updateErr
		}
		run.session = cloneDurableSession(*updated)
		run.expectedRevision = updated.Revision
		run.journalMarker = cloneDurableInterruption(updated.Interruption)
		run.journalEntries = nextEntries
		run.observer.SetRunID(marker.RunID)
		return nil
	}
	if run.session.ReconcileRequired {
		return agent.ErrSessionReconcileRequired
	}

	updated, err := run.store.MarkInterrupted(run.session.ID, run.expectedRevision, marker)
	if err != nil {
		// In particular, revision conflicts are not retried: another prepared
		// run may own that revision and must remain the only possible winner.
		return err
	}
	if updated.Interruption == nil || updated.Interruption.RunID != marker.RunID {
		return agent.ErrSessionReconcileRequired
	}
	run.session = cloneDurableSession(*updated)
	run.expectedRevision = updated.Revision
	run.journalMarker = cloneDurableInterruption(updated.Interruption)
	run.journalEntries = []agent.ToolExecutionJournalEntry{entry}
	run.observer.SetRunID(marker.RunID)
	return nil
}

func (run *durableAgentRun) Finalize(provider, model string) (*agent.Session, error) {
	if run == nil {
		return nil, nil
	}
	if run.observer == nil || !run.observer.Completed() {
		return nil, errors.New("durable run did not reach a normal terminal event")
	}
	terminal, terminalSet := run.observer.TerminalMetadata()
	if terminalSet && terminal.BlocksSuccess() && run.observer.HasToolActivity() {
		marker := run.observer.ReconciliationMarker(
			"Agent run reached a non-success terminal after tool execution began; explicit reconciliation is required.",
		)
		if marker == nil {
			return nil, fmt.Errorf("%w: non-success durable terminal after tool activity", agent.ErrSessionReconcileRequired)
		}
		if _, err := run.markReconciliation(*marker); err != nil {
			return nil, errors.Join(agent.ErrSessionReconcileRequired, err)
		}
		return nil, fmt.Errorf("%w: non-success durable terminal after tool activity", agent.ErrSessionReconcileRequired)
	}
	if run.observer.ReconciliationRequired() {
		marker := run.observer.ReconciliationMarker(
			"Agent run completed with ambiguous tool execution lifecycle; explicit reconciliation is required.",
		)
		if marker == nil {
			return nil, fmt.Errorf("%w: ambiguous durable tool lifecycle", agent.ErrSessionReconcileRequired)
		}
		if _, err := run.markReconciliation(*marker); err != nil {
			return nil, errors.Join(agent.ErrSessionReconcileRequired, err)
		}
		return nil, fmt.Errorf("%w: ambiguous durable tool lifecycle", agent.ErrSessionReconcileRequired)
	}
	delta := run.observer.Messages()
	if len(delta) == 0 && !terminalSet {
		copySession := cloneDurableSession(run.session)
		return &copySession, nil
	}
	candidate := cloneDurableSession(run.session)
	candidate.Messages = append(candidate.Messages, cloneDurableMessages(delta)...)
	if terminalSet {
		candidate.LastRunTerminal = &terminal
	}
	candidate.Provider = strings.TrimSpace(provider)
	candidate.Model = strings.TrimSpace(model)
	marker := run.persistedJournalMarker()
	var commitErr error
	if marker != nil {
		commitErr = run.store.CommitInterruptedRun(&candidate, run.expectedRevision, *marker)
	} else {
		commitErr = run.store.SaveExpected(&candidate, run.expectedRevision)
	}
	if commitErr != nil {
		if marker := run.observer.ReconciliationMarker(
			"Agent run completed, but its tool transcript could not be committed; explicit reconciliation is required.",
		); marker != nil {
			if _, markerErr := run.markReconciliation(*marker); markerErr != nil {
				return nil, errors.Join(commitErr, fmt.Errorf("checkpoint ambiguous tool side effect: %w", markerErr))
			}
		}
		return nil, commitErr
	}
	run.session = cloneDurableSession(candidate)
	run.expectedRevision = candidate.Revision
	return &candidate, nil
}

func (run *durableAgentRun) markReconciliation(marker agent.SessionInterruption) (*agent.Session, error) {
	if run == nil || run.store == nil {
		return nil, errors.New("durable session store is unavailable")
	}
	if persistedMarker := run.persistedJournalMarker(); persistedMarker != nil &&
		persistedMarker.RunID == strings.TrimSpace(marker.RunID) {
		current, err := run.store.Get(run.session.ID)
		if err != nil {
			return nil, err
		}
		if current.Revision != run.expectedRevision || !current.ReconcileRequired ||
			!sameDurableInterruption(current.Interruption, persistedMarker) {
			return nil, agent.ErrSessionReconcileRequired
		}
		if sameDurableInterruption(current.Interruption, &marker) {
			return current, nil
		}
		marker.At = current.Interruption.At
		updated, updateErr := run.store.UpdateInterruptedRun(
			run.session.ID, run.expectedRevision, *persistedMarker, marker,
		)
		if updateErr != nil {
			return nil, updateErr
		}
		run.journalMu.Lock()
		run.journalMarker = cloneDurableInterruption(updated.Interruption)
		run.journalMu.Unlock()
		run.session = cloneDurableSession(*updated)
		run.expectedRevision = updated.Revision
		return updated, nil
	}
	expected := run.expectedRevision
	for attempt := 0; attempt < 3; attempt++ {
		updated, err := run.store.MarkInterrupted(run.session.ID, expected, marker)
		if err == nil {
			run.session = cloneDurableSession(*updated)
			run.expectedRevision = updated.Revision
			return updated, nil
		}
		if !errors.Is(err, agent.ErrSessionRevisionConflict) {
			run.quarantineSession()
			return nil, err
		}
		current, loadErr := run.store.Get(run.session.ID)
		if loadErr != nil {
			run.quarantineSession()
			return nil, errors.Join(err, loadErr)
		}
		if current.ReconcileRequired {
			run.session = cloneDurableSession(*current)
			run.expectedRevision = current.Revision
			return current, nil
		}
		expected = current.Revision
	}
	run.quarantineSession()
	return nil, agent.ErrSessionRevisionConflict
}

func (run *durableAgentRun) persistedJournalMarker() *agent.SessionInterruption {
	if run == nil {
		return nil
	}
	run.journalMu.Lock()
	defer run.journalMu.Unlock()
	return cloneDurableInterruption(run.journalMarker)
}

func cloneDurableInterruption(value *agent.SessionInterruption) *agent.SessionInterruption {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func sameDurableInterruption(a, b *agent.SessionInterruption) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.At.Equal(b.At) && a.Reason == b.Reason && a.RunID == b.RunID &&
		a.ToolName == b.ToolName && a.ToolCallID == b.ToolCallID &&
		a.InputDigest == b.InputDigest && a.SideEffectState == b.SideEffectState &&
		a.Summary == b.Summary
}

func (run *durableAgentRun) quarantineSession() {
	if run != nil && run.quarantine != nil && run.observer != nil && run.observer.HasToolActivity() {
		run.quarantine(run.session.ID)
	}
}

func (run *durableAgentRun) MarkInterrupted(reason string) (*agent.Session, error) {
	if run == nil || run.observer == nil {
		return nil, nil
	}
	marker := run.observer.Interruption(reason)
	if marker == nil {
		return nil, nil
	}
	return run.markReconciliation(*marker)
}

func validateDurableRunTranscript(persisted []agent.SessionMessage, request []types.Message) error {
	projected := make([]agent.SessionMessage, 0, len(persisted))
	for _, message := range persisted {
		if message.Role == "user" || message.Role == "assistant" {
			projected = append(projected, message)
		}
	}
	incoming, err := durableMessagesFromCanonical(request)
	if err != nil {
		return err
	}
	if len(projected) != len(incoming) || len(incoming) == 0 {
		return errDurableTranscriptMismatch
	}
	for index := range incoming {
		if projected[index].Role != incoming[index].Role {
			return errDurableTranscriptMismatch
		}
		// The final user block may contain a transport-only image marker while
		// the UI persists a display-safe label. Every earlier turn is exact.
		lastUserTransportVariant := index == len(incoming)-1 && incoming[index].Role == "user"
		if !lastUserTransportVariant && projected[index].Content != incoming[index].Content {
			return errDurableTranscriptMismatch
		}
	}
	if incoming[len(incoming)-1].Role != "user" {
		return errDurableTranscriptMismatch
	}
	return nil
}

func durableMessagesFromCanonical(messages []types.Message) ([]agent.SessionMessage, error) {
	result := make([]agent.SessionMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		content, err := durableCanonicalText(message.Content)
		if err != nil {
			return nil, errDurableTranscriptMismatch
		}
		if len(content) > maxDurableRunTextBytes {
			return nil, errors.New("durable session message exceeds the safe bound")
		}
		result = append(result, agent.SessionMessage{Role: message.Role, Content: content})
	}
	return result, nil
}

func durableCanonicalText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(block.Text)
	}
	return builder.String(), nil
}

func sameDurableWorkspace(left, right string) bool {
	canonical := func(value string) (string, error) {
		absolute, err := filepath.Abs(strings.TrimSpace(value))
		if err != nil {
			return "", err
		}
		absolute = filepath.Clean(absolute)
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = filepath.Clean(resolved)
		}
		return absolute, nil
	}
	a, errA := canonical(left)
	b, errB := canonical(right)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func cloneDurableSession(session agent.Session) agent.Session {
	copySession := session
	copySession.Messages = cloneDurableMessages(session.Messages)
	if session.Interruption != nil {
		marker := *session.Interruption
		copySession.Interruption = &marker
	}
	if session.LastRunTerminal != nil {
		terminal := *session.LastRunTerminal
		copySession.LastRunTerminal = &terminal
	}
	return copySession
}

func cloneDurableMessages(messages []agent.SessionMessage) []agent.SessionMessage {
	if messages == nil {
		return nil
	}
	encoded, _ := json.Marshal(messages)
	var clone []agent.SessionMessage
	_ = json.Unmarshal(encoded, &clone)
	return clone
}

func durableSessionCommittedEvent(session *agent.Session) agent.Event {
	if session == nil {
		return agent.Event{Type: "durable_session_error", Data: map[string]string{
			"code":    "session_operation_failed",
			"message": "Durable session result is unavailable",
		}}
	}
	return agent.Event{Type: "durable_session", Data: map[string]any{
		"sessionId":         session.ID,
		"revision":          session.Revision,
		"lifecycleStatus":   session.LifecycleStatus,
		"reconcileRequired": session.ReconcileRequired,
	}}
}

func durableSessionErrorCode(err error) string {
	var conflict *agent.SessionRevisionConflictError
	switch {
	case errors.As(err, &conflict):
		return "session_revision_conflict"
	case errors.Is(err, agent.ErrSessionReconcileRequired):
		return "session_reconcile_required"
	case errors.Is(err, agent.ErrSessionLifecycleInvalid):
		return "session_lifecycle_conflict"
	case errors.Is(err, agent.ErrSessionNotFound):
		return "session_not_found"
	default:
		return "session_operation_failed"
	}
}
