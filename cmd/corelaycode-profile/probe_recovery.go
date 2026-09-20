package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// Once the write journal is durable, the interrupted run cannot request a
// second model turn before the event consumer cancels it after the write.
type recoveryProbeProvider struct {
	types.Provider
	writeStarted *atomic.Bool
}

func (p recoveryProbeProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	if p.writeStarted.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.Provider.StreamMessage(ctx, req, opts)
}

func (p lifecycleProbe) recovery(ctx context.Context) observedAgentProbe {
	storePath, err := os.MkdirTemp("", "corelay-recovery-session-")
	if err != nil {
		return lifecycleFailure("recovery_store_unavailable")
	}
	defer os.RemoveAll(storePath)
	store := agent.NewSessionStore(storePath)
	prompt := "Read resume/checkpoint.txt, then use Write exactly once to replace its full contents with state=resumed followed by a newline. Do not perform other mutations."
	session := &agent.Session{Workspace: p.execution.WorkspaceRoot, Provider: p.executor.provider.Name(), Model: p.executor.model,
		Messages: []agent.SessionMessage{{Role: "user", Content: prompt}}}
	if err := store.SaveExpected(session, 0); err != nil {
		return lifecycleFailure("recovery_session_create_failed")
	}
	requester, err := newProbeApprovalRequester(session.Workspace, session.ID, p.fixture.approvedMutations)
	if err != nil {
		return lifecycleFailure("recovery_approval_unavailable")
	}
	options := p.options
	options.SessionID, options.DurableSessionID = session.ID, session.ID
	options.SessionRevision = session.Revision
	options.ApprovalRequester = requester
	var writeStarted atomic.Bool
	var marker agent.SessionInterruption
	options.PreExecutionJournal = func(entry agent.ToolExecutionJournalEntry) error {
		if entry.Name == "Read" {
			return nil
		}
		if entry.Name != "Write" || !writeStarted.CompareAndSwap(false, true) {
			return errors.New("unexpected recovery mutation")
		}
		marker = agent.SessionInterruption{Reason: "profiler cancellation after checkpoint write", At: time.Now().UTC(), RunID: entry.RunID,
			ToolName: entry.Name, ToolCallID: entry.ID, InputDigest: entry.InputDigest,
			SideEffectState: agent.SessionSideEffectStarted, Summary: "Owned fixture write requires reconciliation before resume."}
		guarded, err := store.MarkInterrupted(session.ID, session.Revision, marker)
		if err != nil {
			return err
		}
		session = guarded
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan agent.Event, 64)
	forwarded := make(chan agent.Event, 64)
	var applied atomic.Bool
	message, _ := json.Marshal(prompt)
	started := time.Now()
	go agent.RunLoopWithOptions(runCtx, recoveryProbeProvider{Provider: p.executor.provider, writeStarted: &writeStarted}, p.executor.model,
		[]types.Message{{Role: "user", Content: message}}, session.Workspace, options, events)
	go func() {
		defer close(forwarded)
		for event := range events {
			if event.Type == "tool_result" {
				data := interfaceMap(event.Data)
				if data["name"] == "Write" && data["executed"] == true && data["isError"] == false {
					applied.Store(true)
					cancel()
				} else if writeStarted.Load() && data["name"] == "Write" {
					cancel()
				}
			}
			forwarded <- event
		}
	}()
	first := observeAgentProbe(forwarded, p.fixture, p.execution, started)
	if ctx.Err() != nil || !applied.Load() || !writeStarted.Load() || runCtx.Err() == nil || first.successfulEdits != 1 {
		first.value.Success = false
		first.value.Recovered = false
		first.transportFailed = true
		first.failureCode = "recovery_interruption_not_observed"
		return first
	}
	if _, matches := validateProbeArtifact(p.fixture); !matches {
		return lifecycleFailure("recovery_postimage_mismatch")
	}
	updatedMarker := marker
	updatedMarker.SideEffectState = agent.SessionSideEffectApplied
	session, err = store.UpdateInterruptedRun(session.ID, session.Revision, marker, updatedMarker)
	if err != nil {
		return lifecycleFailure("recovery_journal_update_failed")
	}
	store = agent.NewSessionStore(storePath)
	session, err = store.Get(session.ID)
	if err != nil || !session.ReconcileRequired {
		return lifecycleFailure("recovery_restart_guard_missing")
	}
	if err := store.SaveExpected(session, session.Revision); !errors.Is(err, agent.ErrSessionReconcileRequired) {
		return lifecycleFailure("recovery_unreconciled_save_allowed")
	}
	assessment, err := agent.AssessSessionReconciliation(session)
	if err != nil || assessment.ManualConfirmationRequired || assessment.MatchesPostimage != 1 || assessment.Unavailable != 0 || assessment.Diverged != 0 {
		return lifecycleFailure("recovery_checkpoint_evidence_missing")
	}
	receipt, err := agent.NewSessionReconciliationReceipt(assessment, false, time.Now())
	if err != nil {
		return lifecycleFailure("recovery_receipt_invalid")
	}
	session, err = store.MarkReconciledWithReceipt(session.ID, session.Revision, session.Interruption.RunID, receipt)
	if err != nil || session.ReconcileRequired {
		return lifecycleFailure("recovery_reconciliation_failed")
	}
	resumePrompt := "The previous write was interrupted after it applied. The durable checkpoint was reconciled. Read resume/checkpoint.txt to verify its current state. Do not write or replay the completed operation. Reply with " + p.fixture.marker + " on the first line, followed by the file's trimmed content on the second line."
	session.Messages = append(session.Messages, agent.SessionMessage{Role: "user", Content: resumePrompt})
	if err := store.SaveExpected(session, session.Revision); err != nil {
		return lifecycleFailure("recovery_resume_save_failed")
	}
	options = p.options
	options.SessionID, options.DurableSessionID = session.ID, session.ID
	options.SessionRevision = session.Revision
	options.ApprovalRequester = requester
	var replay atomic.Bool
	options.PreExecutionJournal = func(entry agent.ToolExecutionJournalEntry) error {
		if entry.Name != "Read" {
			replay.Store(true)
			return errors.New("recovery replay forbidden")
		}
		return nil
	}
	messages := make([]types.Message, 0, len(session.Messages))
	for _, item := range session.Messages {
		content, _ := json.Marshal(item.Content)
		messages = append(messages, types.Message{Role: item.Role, Content: content})
	}
	result := p.run(ctx, session.Workspace, messages, p.fixture, options)
	content, readErr := os.ReadFile(filepath.Join(session.Workspace, "resume", "checkpoint.txt"))
	passed := result.value.Success && !result.transportFailed && !replay.Load() && result.successfulReads > 0 && result.successfulEdits == 0 &&
		readErr == nil && string(content) == "state=resumed\n" && strings.TrimSpace(result.finalText) == p.fixture.marker+"\nstate=resumed"
	result.value.Success, result.value.FalseDone, result.value.Recovered = passed, !passed && !result.transportFailed, passed
	result.value.TraceDigest = digestJSON([]string{first.value.TraceDigest, assessment.EvidenceDigest, result.value.TraceDigest})
	result.value.ArtifactDigest = digestJSON(struct{ Evidence, Artifact string }{assessment.EvidenceDigest, digestJSON(content)})
	result.value.Retries += first.value.Retries
	result.value.Malformed = result.value.Malformed || first.value.Malformed
	if first.value.ContextTokens > result.value.ContextTokens {
		result.value.ContextTokens = first.value.ContextTokens
	}
	if first.value.ToolCount > result.value.ToolCount {
		result.value.ToolCount = first.value.ToolCount
	}
	if passed {
		session.Messages = append(session.Messages, agent.SessionMessage{Role: "assistant", Content: result.finalText})
		if err := store.SaveExpected(session, session.Revision); err != nil {
			return lifecycleFailure("recovery_result_save_failed")
		}
		reopened, err := agent.NewSessionStore(storePath).Get(session.ID)
		if err != nil || reopened.ReconcileRequired || reopened.LastReconciliation == nil {
			return lifecycleFailure("recovery_final_state_invalid")
		}
	}
	return result
}
