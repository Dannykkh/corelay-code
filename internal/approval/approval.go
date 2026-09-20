// Package approval provides the transport-neutral, fail-closed approval
// handshake used before a prepared tool call may execute.
package approval

import (
	"context"
	"time"

	"github.com/Dannykkh/corelay-code/internal/executionpolicy"
)

// ApprovalSource identifies how an execution authorization was obtained.
// Full-mode grants are broker-issued from an explicit user-selected policy;
// they are not user prompt resolutions.
type ApprovalSource string

const (
	ApprovalSourceUser             ApprovalSource = "user-prompt"
	ApprovalSourceUserSelectedFull ApprovalSource = "user-selected-full"
)

// Outcome is the explicit choice supplied by an approval client.
type Outcome string

const (
	// OutcomeAllowOnce permits only the tool call associated with this approval.
	OutcomeAllowOnce Outcome = "allow_once"
	// OutcomeDeny rejects the tool call associated with this approval.
	OutcomeDeny Outcome = "deny"
)

// ResolutionReason records why an approval reached its terminal outcome.
type ResolutionReason string

const (
	ReasonUser            ResolutionReason = "user"
	ReasonExpired         ResolutionReason = "expired"
	ReasonContextCanceled ResolutionReason = "context_canceled"
	ReasonSessionCanceled ResolutionReason = "session_canceled"
	ReasonBrokerShutdown  ResolutionReason = "broker_shutdown"
)

// Draft contains the safe metadata needed to ask for approval. It deliberately
// has no raw input, prompt, environment, credential, or command field.
// RedactedInput must already be safe for display and InputDigest may identify
// the normalized input without retaining it.
type Draft struct {
	SessionID       string
	SessionRevision uint64
	RunID           string
	// ToolCallID binds the approval to the exact model-issued call. Legacy
	// requesters may leave it empty, but execution adapters should preserve it.
	ToolCallID              string
	ToolName                string
	ExecutorID              string
	RedactedInput           string
	InputDigest             string
	ExecutionPolicyRevision uint64
	FullSelectionRevision   uint64
	DangerLevel             string
	Scope                   string
	RememberAllowed         bool
}

// Pending is the immutable approval request returned by Broker.Open.
type Pending struct {
	ID                      string
	SessionID               string
	SessionRevision         uint64
	RunID                   string
	ToolCallID              string
	ToolName                string
	ExecutorID              string
	RedactedInput           string
	InputDigest             string
	ExecutionPolicyRevision uint64
	FullSelectionRevision   uint64
	ApprovalSource          ApprovalSource
	DangerLevel             string
	Scope                   string
	RememberAllowed         bool
	CreatedAt               time.Time
	ExpiresAt               time.Time
}

// Decision is a session-bound, one-time response to a Pending approval.
type Decision struct {
	ApprovalID string
	SessionID  string
	Outcome    Outcome
}

// Resolution is the immutable terminal result observed by Await callers.
// SessionID is intentionally omitted so an error response cannot disclose the
// owning session.
type Resolution struct {
	ApprovalID string
	Outcome    Outcome
	Reason     ResolutionReason
	ResolvedAt time.Time
}

// Requester is the minimal execution-kernel dependency for opening and
// awaiting approvals. Transport adapters and brokers remain outside agent.
type Requester interface {
	Open(Draft) (Pending, error)
	Await(context.Context, string, string) (Resolution, error)
}

// FullModeIssuer issues a no-prompt, one-call grant only for a validated
// user-selected full-mode snapshot. Implementations must retain the grant so
// it can be consumed exactly once by the execution boundary.
type FullModeIssuer interface {
	IssueFullModeGrant(Draft, executionpolicy.Snapshot) (Pending, error)
}

// FullModeGrantConsumer validates and atomically consumes a grant against the
// exact pending metadata, expected tool identity/input, and run policy.
type FullModeGrantConsumer interface {
	ConsumeFullModeGrant(Pending, Draft, executionpolicy.Snapshot) error
}

// Allowed reports whether this resolution grants one execution.
func (r Resolution) Allowed() bool {
	return r.Outcome == OutcomeAllowOnce && r.Reason == ReasonUser
}
