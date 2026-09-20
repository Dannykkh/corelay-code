package acpbridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/approval"
)

type approvalRequester struct {
	client             acp.Client
	executionSessionID string
	wireSessionID      string
	sessionRevision    uint64
	ttl                time.Duration

	mu      sync.Mutex
	pending map[string]*approvalRequest
	broker  *approval.Broker
}

type approvalRequest struct {
	pending approval.Pending
	waiting bool
}

func newApprovalRequester(
	client acp.Client,
	executionSessionID string,
	wireSessionID string,
	sessionRevision uint64,
	ttl time.Duration,
) *approvalRequester {
	return &approvalRequester{
		client:             client,
		executionSessionID: executionSessionID,
		wireSessionID:      wireSessionID,
		sessionRevision:    sessionRevision,
		ttl:                ttl,
		pending:            make(map[string]*approvalRequest),
		broker:             approval.NewBroker(ttl),
	}
}

func (r *approvalRequester) Open(draft approval.Draft) (approval.Pending, error) {
	if draft.SessionID != r.executionSessionID || draft.SessionID == "" || r.wireSessionID == "" || draft.ToolName == "" || r.client == nil {
		return approval.Pending{}, errors.New("approval request is invalid")
	}
	id, err := newOpaqueID("approval_")
	if err != nil {
		return approval.Pending{}, errors.New("approval request could not be created")
	}
	now := time.Now().UTC()
	pending := approval.Pending{
		ID:              id,
		SessionID:       draft.SessionID,
		SessionRevision: r.sessionRevision,
		RunID:           draft.RunID,
		ToolCallID:      draft.ToolCallID,
		ToolName:        draft.ToolName,
		ExecutorID:      draft.ExecutorID,
		// This value is retained only to satisfy the execution-kernel
		// handshake. It is never projected into the ACP permission payload.
		RedactedInput:           draft.RedactedInput,
		InputDigest:             draft.InputDigest,
		ExecutionPolicyRevision: draft.ExecutionPolicyRevision,
		FullSelectionRevision:   draft.FullSelectionRevision,
		ApprovalSource:          approval.ApprovalSourceUser,
		DangerLevel:             draft.DangerLevel,
		Scope:                   draft.Scope,
		RememberAllowed:         draft.RememberAllowed,
		CreatedAt:               now,
		ExpiresAt:               now.Add(r.ttl),
	}
	r.mu.Lock()
	r.pending[id] = &approvalRequest{pending: pending}
	r.mu.Unlock()
	return pending, nil
}

func (r *approvalRequester) IssueFullModeGrant(
	draft approval.Draft,
	policy agent.ExecutionPolicySnapshot,
) (approval.Pending, error) {
	if r == nil || r.broker == nil {
		return approval.Pending{}, errors.New("approval broker is unavailable")
	}
	return r.broker.IssueFullModeGrant(draft, policy)
}

func (r *approvalRequester) ConsumeFullModeGrant(
	grant approval.Pending,
	expected approval.Draft,
	policy agent.ExecutionPolicySnapshot,
) error {
	if r == nil || r.broker == nil {
		return errors.New("approval broker is unavailable")
	}
	return r.broker.ConsumeFullModeGrant(grant, expected, policy)
}

func (r *approvalRequester) Shutdown() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.pending = make(map[string]*approvalRequest)
	r.mu.Unlock()
	if r.broker != nil {
		r.broker.Shutdown()
	}
}

func (r *approvalRequester) Await(ctx context.Context, sessionID, approvalID string) (approval.Resolution, error) {
	r.mu.Lock()
	request := r.pending[approvalID]
	if request == nil || sessionID != r.executionSessionID || request.pending.SessionID != sessionID || request.waiting {
		r.mu.Unlock()
		return approval.Resolution{}, errors.New("approval request is unavailable")
	}
	if !request.pending.ExpiresAt.After(time.Now()) {
		delete(r.pending, approvalID)
		r.mu.Unlock()
		return approval.Resolution{}, errors.New("approval request expired")
	}
	request.waiting = true
	pending := request.pending
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, approvalID)
		r.mu.Unlock()
	}()

	deadlineCtx, cancel := context.WithDeadline(ctx, pending.ExpiresAt)
	defer cancel()
	toolCallID := pending.ToolCallID
	if toolCallID == "" {
		toolCallID = pending.ID
	}
	response, err := r.client.RequestPermission(deadlineCtx, acp.RequestPermissionRequest{
		SessionID: r.wireSessionID,
		ToolCall: acp.PermissionToolCall{
			ToolCallID: toolCallID,
			Kind:       toolKind(pending.ToolName),
			Status:     acp.ToolPending,
			Title:      "Permission required: " + sanitizeText(pending.ToolName, 220),
		},
		Options: []acp.PermissionOption{
			{OptionID: "allow_once", Name: "Allow once", Kind: acp.PermissionAllowOnce},
			{OptionID: "deny", Name: "Deny", Kind: acp.PermissionRejectOnce},
		},
	})
	if err != nil {
		return approval.Resolution{}, errors.New("approval request was denied or cancelled")
	}
	if !pending.ExpiresAt.After(time.Now()) {
		return approval.Resolution{}, errors.New("approval request expired")
	}
	outcome := approval.OutcomeDeny
	if response.Outcome.Outcome == "selected" && response.Outcome.OptionID == "allow_once" {
		outcome = approval.OutcomeAllowOnce
	}
	return approval.Resolution{
		ApprovalID: pending.ID,
		Outcome:    outcome,
		Reason:     approval.ReasonUser,
		ResolvedAt: time.Now().UTC(),
	}, nil
}
