package approval

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTTL bounds an unanswered approval request.
	DefaultTTL            = 5 * time.Minute
	maxIDAttempts         = 16
	maxRedactedInputBytes = 4096
)

var (
	ErrInvalidDraft     = errors.New("invalid approval draft")
	ErrInvalidDecision  = errors.New("invalid approval decision")
	ErrApprovalNotFound = errors.New("approval not found")
	ErrAlreadyResolved  = errors.New("approval already resolved")
	ErrApprovalExpired  = errors.New("approval expired")
	ErrSessionCanceled  = errors.New("approval session canceled")
	ErrBrokerClosed     = errors.New("approval broker closed")
	ErrIDCollision      = errors.New("approval id collision")
)

type request struct {
	pending    Pending
	done       chan struct{}
	resolved   bool
	resolution Resolution
	timer      *time.Timer
}

// Broker coordinates one-time approval requests. State is process-local and
// bounded by TTL; the broker performs no filesystem or external persistence.
type Broker struct {
	mu               sync.Mutex
	ttl              time.Duration
	requests         map[string]*request
	canceledSessions map[string]struct{}
	closed           bool
	idGenerator      func() (string, error)
}

var _ Requester = (*Broker)(nil)

// NewBroker creates a broker. A non-positive ttl selects DefaultTTL.
func NewBroker(ttl time.Duration) *Broker {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Broker{
		ttl:              ttl,
		requests:         make(map[string]*request),
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
		ID:              id,
		SessionID:       draft.SessionID,
		SessionRevision: draft.SessionRevision,
		RunID:           draft.RunID,
		ToolCallID:      draft.ToolCallID,
		ToolName:        draft.ToolName,
		RedactedInput:   draft.RedactedInput,
		InputDigest:     draft.InputDigest,
		DangerLevel:     draft.DangerLevel,
		Scope:           draft.Scope,
		RememberAllowed: draft.RememberAllowed,
		CreatedAt:       now,
		ExpiresAt:       now.Add(b.ttl),
	}
	entry := &request{pending: pending, done: make(chan struct{})}
	b.requests[id] = entry
	entry.timer = time.AfterFunc(b.ttl, func() {
		b.expire(id, entry)
	})
	return pending, nil
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
		if _, exists := b.requests[id]; !exists {
			return id, nil
		}
	}
	return "", ErrIDCollision
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
