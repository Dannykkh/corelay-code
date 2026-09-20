package approval

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/executionpolicy"
)

const (
	// DefaultTTL bounds an unanswered approval request.
	DefaultTTL            = 5 * time.Minute
	maxIDAttempts         = 16
	maxRedactedInputBytes = 4096
)

var (
	ErrInvalidDraft         = errors.New("invalid approval draft")
	ErrInvalidDecision      = errors.New("invalid approval decision")
	ErrApprovalNotFound     = errors.New("approval not found")
	ErrAlreadyResolved      = errors.New("approval already resolved")
	ErrApprovalExpired      = errors.New("approval expired")
	ErrSessionCanceled      = errors.New("approval session canceled")
	ErrBrokerClosed         = errors.New("approval broker closed")
	ErrIDCollision          = errors.New("approval id collision")
	ErrInvalidFullModeGrant = errors.New("invalid full-mode grant")
)

type request struct {
	pending    Pending
	done       chan struct{}
	resolved   bool
	resolution Resolution
	timer      *time.Timer
}

type fullModeGrant struct {
	pending Pending
	policy  executionpolicy.Snapshot
	timer   *time.Timer
}

// Broker coordinates one-time approval requests. State is process-local and
// bounded by TTL; the broker performs no filesystem or external persistence.
type Broker struct {
	mu               sync.Mutex
	ttl              time.Duration
	requests         map[string]*request
	fullModeGrants   map[string]*fullModeGrant
	canceledSessions map[string]struct{}
	closed           bool
	idGenerator      func() (string, error)
}

var _ Requester = (*Broker)(nil)
var _ FullModeIssuer = (*Broker)(nil)
var _ FullModeGrantConsumer = (*Broker)(nil)

// NewBroker creates a broker. A non-positive ttl selects DefaultTTL.
func NewBroker(ttl time.Duration) *Broker {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Broker{
		ttl:              ttl,
		requests:         make(map[string]*request),
		fullModeGrants:   make(map[string]*fullModeGrant),
		canceledSessions: make(map[string]struct{}),
		idGenerator:      generateID,
	}
}

// Open registers a bounded approval request.
func (b *Broker) Open(draft Draft) (Pending, error) {
	if err := validateDraft(draft); err != nil {
		return Pending{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Pending{}, ErrBrokerClosed
	}
	if _, canceled := b.canceledSessions[draft.SessionID]; canceled {
		return Pending{}, ErrSessionCanceled
	}

	id, err := b.allocateIDLocked()
	if err != nil {
		return Pending{}, err
	}
	now := time.Now()
	pending := Pending{
		ID:                      id,
		SessionID:               draft.SessionID,
		SessionRevision:         draft.SessionRevision,
		RunID:                   draft.RunID,
		ToolCallID:              draft.ToolCallID,
		ToolName:                draft.ToolName,
		ExecutorID:              draft.ExecutorID,
		ApprovalSource:          ApprovalSourceUser,
		RedactedInput:           draft.RedactedInput,
		InputDigest:             draft.InputDigest,
		ExecutionPolicyRevision: draft.ExecutionPolicyRevision,
		FullSelectionRevision:   draft.FullSelectionRevision,
		DangerLevel:             draft.DangerLevel,
		Scope:                   draft.Scope,
		RememberAllowed:         draft.RememberAllowed,
		CreatedAt:               now,
		ExpiresAt:               now.Add(b.ttl),
	}
	entry := &request{pending: pending, done: make(chan struct{})}
	b.requests[id] = entry
	entry.timer = time.AfterFunc(b.ttl, func() {
		b.expire(id, entry)
	})
	return pending, nil
}

// IssueFullModeGrant creates an internal one-time authorization without
// opening a user-facing approval request. The selected policy and exact call
// identity are stored in the broker and must be presented unchanged to the
// consumer before execution.
func (b *Broker) IssueFullModeGrant(
	draft Draft,
	policy executionpolicy.Snapshot,
) (Pending, error) {
	if err := validateFullModePolicy(policy); err != nil {
		return Pending{}, err
	}
	if err := validateFullModeDraft(draft, policy); err != nil {
		return Pending{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Pending{}, ErrBrokerClosed
	}
	if _, canceled := b.canceledSessions[draft.SessionID]; canceled {
		return Pending{}, ErrSessionCanceled
	}

	id, err := b.allocateIDLocked()
	if err != nil {
		return Pending{}, err
	}
	now := time.Now()
	pending := Pending{
		ID:                      id,
		SessionID:               draft.SessionID,
		SessionRevision:         draft.SessionRevision,
		RunID:                   draft.RunID,
		ToolCallID:              draft.ToolCallID,
		ToolName:                draft.ToolName,
		ExecutorID:              draft.ExecutorID,
		RedactedInput:           draft.RedactedInput,
		InputDigest:             draft.InputDigest,
		ExecutionPolicyRevision: policy.Revision,
		FullSelectionRevision:   policy.FullSelectionRevision,
		ApprovalSource:          ApprovalSourceUserSelectedFull,
		DangerLevel:             draft.DangerLevel,
		Scope:                   draft.Scope,
		RememberAllowed:         false,
		CreatedAt:               now,
		ExpiresAt:               now.Add(b.ttl),
	}
	entry := &fullModeGrant{pending: pending, policy: policy}
	b.fullModeGrants[id] = entry
	entry.timer = time.AfterFunc(b.ttl, func() {
		b.expireFullModeGrant(id, entry)
	})
	return pending, nil
}

// ConsumeFullModeGrant verifies every stored call and policy field before
// atomically deleting the one-time grant. A mismatch leaves the valid grant
// available for the exact intended invocation; expiry, cancellation, replay,
// and broker shutdown all fail closed.
func (b *Broker) ConsumeFullModeGrant(
	grant Pending,
	expected Draft,
	policy executionpolicy.Snapshot,
) error {
	if err := validateFullModePolicy(policy); err != nil {
		return err
	}
	if err := validateFullModeDraft(expected, policy); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBrokerClosed
	}
	entry, ok := b.fullModeGrants[grant.ID]
	if !ok || grant.ID == "" {
		return ErrApprovalNotFound
	}
	if _, canceled := b.canceledSessions[expected.SessionID]; canceled {
		delete(b.fullModeGrants, grant.ID)
		if entry.timer != nil {
			entry.timer.Stop()
		}
		return ErrSessionCanceled
	}
	if !time.Now().Before(entry.pending.ExpiresAt) {
		delete(b.fullModeGrants, grant.ID)
		if entry.timer != nil {
			entry.timer.Stop()
		}
		return ErrApprovalExpired
	}
	if !sameFullModePending(grant, entry.pending) ||
		!fullModeDraftMatchesPending(expected, entry.pending) ||
		policy != entry.policy {
		return ErrInvalidFullModeGrant
	}
	delete(b.fullModeGrants, grant.ID)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	return nil
}

func validateFullModePolicy(policy executionpolicy.Snapshot) error {
	if err := executionpolicy.ValidateSnapshot(policy); err != nil || policy.Mode != executionpolicy.ModeFull ||
		policy.FullSelectionRevision == 0 {
		return ErrInvalidFullModeGrant
	}
	switch policy.Source {
	case executionpolicy.SourceUserSelected:
		if policy.FullSelectionRevision != policy.Revision {
			return ErrInvalidFullModeGrant
		}
	case executionpolicy.SourceInherited, executionpolicy.SourceChildRestriction:
		// A child may inherit only a full authority that has explicit selected
		// provenance in its validated parent snapshot.
		if policy.ParentRevision != policy.Revision {
			return ErrInvalidFullModeGrant
		}
	default:
		return ErrInvalidFullModeGrant
	}
	return nil
}

func validateFullModeDraft(draft Draft, policy executionpolicy.Snapshot) error {
	if validateDraft(draft) != nil ||
		strings.TrimSpace(draft.ToolCallID) == "" ||
		strings.TrimSpace(draft.ExecutorID) == "" ||
		!validFullModeInputDigest(draft.InputDigest) ||
		draft.ExecutionPolicyRevision != policy.Revision ||
		draft.FullSelectionRevision != policy.FullSelectionRevision ||
		draft.RememberAllowed {
		return ErrInvalidFullModeGrant
	}
	return nil
}

func validFullModeInputDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	encoded := strings.TrimPrefix(value, "sha256:")
	if len(encoded) != 64 || strings.ToLower(encoded) != encoded {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}

func fullModeDraftMatchesPending(draft Draft, pending Pending) bool {
	return draft.SessionID == pending.SessionID &&
		draft.SessionRevision == pending.SessionRevision &&
		draft.RunID == pending.RunID &&
		draft.ToolCallID == pending.ToolCallID &&
		draft.ToolName == pending.ToolName &&
		draft.ExecutorID == pending.ExecutorID &&
		draft.RedactedInput == pending.RedactedInput &&
		draft.InputDigest == pending.InputDigest &&
		draft.ExecutionPolicyRevision == pending.ExecutionPolicyRevision &&
		draft.FullSelectionRevision == pending.FullSelectionRevision &&
		draft.DangerLevel == pending.DangerLevel &&
		draft.Scope == pending.Scope &&
		!draft.RememberAllowed && !pending.RememberAllowed
}

func sameFullModePending(left, right Pending) bool {
	leftCreatedAt, rightCreatedAt := left.CreatedAt, right.CreatedAt
	leftExpiresAt, rightExpiresAt := left.ExpiresAt, right.ExpiresAt
	left.CreatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	left.ExpiresAt = time.Time{}
	right.ExpiresAt = time.Time{}
	if left != right {
		return false
	}
	return leftCreatedAt.Equal(rightCreatedAt) && leftExpiresAt.Equal(rightExpiresAt)
}

// Await waits for a terminal resolution. Context cancellation atomically
// denies the approval unless another terminal decision won the race first.
func (b *Broker) Await(ctx context.Context, sessionID, approvalID string) (Resolution, error) {
	if ctx == nil {
		ctx = canceledContext()
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return deniedResolution(approvalID, ReasonBrokerShutdown), ErrBrokerClosed
	}
	entry, ok := b.requests[approvalID]
	if !ok || entry.pending.SessionID != sessionID {
		b.mu.Unlock()
		return Resolution{}, ErrApprovalNotFound
	}
	if !entry.resolved && !time.Now().Before(entry.pending.ExpiresAt) {
		resolveLocked(entry, OutcomeDeny, ReasonExpired)
	}
	if entry.resolved {
		resolution := entry.resolution
		b.mu.Unlock()
		return resolution, resolutionError(resolution)
	}
	done := entry.done
	b.mu.Unlock()

	select {
	case <-done:
		return b.readResolution(entry)
	case <-ctx.Done():
		resolution, err := b.resolveSystem(sessionID, approvalID, ReasonContextCanceled)
		if errors.Is(err, ErrAlreadyResolved) {
			return resolution, resolutionError(resolution)
		}
		if err != nil {
			return resolution, err
		}
		return resolution, ctx.Err()
	}
}

// Resolve consumes one explicit decision. A wrong SessionID is deliberately
// indistinguishable from an unknown ApprovalID.
func (b *Broker) Resolve(decision Decision) (Resolution, error) {
	if decision.Outcome != OutcomeAllowOnce && decision.Outcome != OutcomeDeny {
		return Resolution{}, ErrInvalidDecision
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return deniedResolution(decision.ApprovalID, ReasonBrokerShutdown), ErrBrokerClosed
	}
	entry, ok := b.requests[decision.ApprovalID]
	if !ok || entry.pending.SessionID != decision.SessionID {
		return Resolution{}, ErrApprovalNotFound
	}
	if !entry.resolved && !time.Now().Before(entry.pending.ExpiresAt) {
		return resolveLocked(entry, OutcomeDeny, ReasonExpired), ErrApprovalExpired
	}
	if entry.resolved {
		return entry.resolution, ErrAlreadyResolved
	}
	return resolveLocked(entry, decision.Outcome, ReasonUser), nil
}

// CancelSession permanently tombstones sessionID for this broker and denies
// every unresolved request it owns. Open and CancelSession share the same lock,
// so an Open either precedes cancellation and is denied here, or follows it and
// fails with ErrSessionCanceled. The method has no result count so callers
// cannot use it to probe whether another session owns pending approvals.
func (b *Broker) CancelSession(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || strings.TrimSpace(sessionID) == "" {
		return
	}
	b.canceledSessions[sessionID] = struct{}{}
	for _, entry := range b.requests {
		if !entry.resolved && entry.pending.SessionID == sessionID {
			resolveLocked(entry, OutcomeDeny, ReasonSessionCanceled)
		}
	}
	for id, grant := range b.fullModeGrants {
		if grant.pending.SessionID == sessionID {
			delete(b.fullModeGrants, id)
			if grant.timer != nil {
				grant.timer.Stop()
			}
		}
	}
}

// Shutdown idempotently denies all pending requests and releases broker-owned
// timers. New operations fail closed after shutdown.
func (b *Broker) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, entry := range b.requests {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if !entry.resolved {
			resolveLocked(entry, OutcomeDeny, ReasonBrokerShutdown)
		}
	}
	b.requests = nil
	for _, grant := range b.fullModeGrants {
		if grant.timer != nil {
			grant.timer.Stop()
		}
	}
	b.fullModeGrants = nil
}

func validateDraft(draft Draft) error {
	switch {
	case strings.TrimSpace(draft.SessionID) == "":
		return fmt.Errorf("%w: session id is required", ErrInvalidDraft)
	case strings.TrimSpace(draft.RunID) == "":
		return fmt.Errorf("%w: run id is required", ErrInvalidDraft)
	case strings.TrimSpace(draft.ToolName) == "":
		return fmt.Errorf("%w: tool name is required", ErrInvalidDraft)
	case len(draft.ToolCallID) > 512:
		return fmt.Errorf("%w: tool call id is too large", ErrInvalidDraft)
	case len(draft.RedactedInput) > maxRedactedInputBytes:
		return fmt.Errorf("%w: redacted input exceeds %d bytes", ErrInvalidDraft, maxRedactedInputBytes)
	default:
		return nil
	}
}

func generateID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate approval id: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random)
	return "apr_" + strings.ToLower(encoded), nil
}

func (b *Broker) allocateIDLocked() (string, error) {
	generator := b.idGenerator
	if generator == nil {
		generator = generateID
	}
	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		id, err := generator()
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("generate approval id: empty id")
		}
		_, existsRequest := b.requests[id]
		_, existsGrant := b.fullModeGrants[id]
		if !existsRequest && !existsGrant {
			return id, nil
		}
	}
	return "", ErrIDCollision
}

func (b *Broker) expireFullModeGrant(id string, expected *fullModeGrant) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if grant, ok := b.fullModeGrants[id]; ok && grant == expected {
		delete(b.fullModeGrants, id)
	}
}

func resolveLocked(entry *request, outcome Outcome, reason ResolutionReason) Resolution {
	resolution := Resolution{
		ApprovalID: entry.pending.ID,
		Outcome:    outcome,
		Reason:     reason,
		ResolvedAt: time.Now(),
	}
	entry.resolution = resolution
	entry.resolved = true
	close(entry.done)
	return resolution
}

func (b *Broker) resolveSystem(sessionID, approvalID string, reason ResolutionReason) (Resolution, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return deniedResolution(approvalID, ReasonBrokerShutdown), ErrBrokerClosed
	}
	entry, ok := b.requests[approvalID]
	if !ok || entry.pending.SessionID != sessionID {
		return Resolution{}, ErrApprovalNotFound
	}
	if entry.resolved {
		return entry.resolution, ErrAlreadyResolved
	}
	return resolveLocked(entry, OutcomeDeny, reason), nil
}

func (b *Broker) readResolution(entry *request) (Resolution, error) {
	b.mu.Lock()
	resolution := entry.resolution
	b.mu.Unlock()
	return resolution, resolutionError(resolution)
}

func (b *Broker) expire(id string, expected *request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.requests[id]
	if !ok || entry != expected {
		return
	}
	if entry.resolved {
		delete(b.requests, id)
		return
	}
	resolveLocked(entry, OutcomeDeny, ReasonExpired)
	entry.timer = time.AfterFunc(b.ttl, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.requests[id] == entry {
			delete(b.requests, id)
		}
	})
}

func resolutionError(resolution Resolution) error {
	switch resolution.Reason {
	case ReasonExpired:
		return ErrApprovalExpired
	case ReasonContextCanceled:
		return context.Canceled
	case ReasonSessionCanceled:
		return ErrSessionCanceled
	case ReasonBrokerShutdown:
		return ErrBrokerClosed
	default:
		return nil
	}
}

func deniedResolution(approvalID string, reason ResolutionReason) Resolution {
	return Resolution{
		ApprovalID: approvalID,
		Outcome:    OutcomeDeny,
		Reason:     reason,
		ResolvedAt: time.Now(),
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
