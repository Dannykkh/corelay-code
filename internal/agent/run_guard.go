package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
)

const DefaultRunGuardRepeatLimit = 3

// ActionFingerprintInput contains only state that determines whether a tool
// attempt is equivalent to an earlier attempt in the same run. TargetRevision
// is essential: the same call after an artifact changes is progress, not a
// repeated action.
type ActionFingerprintInput struct {
	ToolName       string
	Arguments      json.RawMessage
	TargetRevision string
	FailureCode    string
	PlanStep       string
}

type RunGuardReason string

const (
	RunGuardWithinLimit    RunGuardReason = "within_limit"
	RunGuardRepeatedAction RunGuardReason = "repeated_action"
)

// RunGuardDecision is returned before execution. Allowed becomes false only
// after the configured number of equivalent attempts has been observed.
type RunGuardDecision struct {
	Allowed     bool
	Fingerprint string
	Occurrences int
	Limit       int
	Reason      RunGuardReason
}

// RunGuardSnapshot is the content-free audit record for repetition decisions.
// Fingerprints are one-way SHA-256 digests of normalized actions; raw tool
// names, arguments, paths, and results are deliberately excluded.
type RunGuardSnapshot struct {
	RepeatLimit           int            `json:"repeatLimit"`
	Observations          int            `json:"observations"`
	Denied                int            `json:"denied"`
	LastDeniedReason      RunGuardReason `json:"lastDeniedReason,omitempty"`
	LastDeniedFingerprint string         `json:"lastDeniedFingerprint,omitempty"`
	LastDeniedOccurrences int            `json:"lastDeniedOccurrences,omitempty"`
}

// RunGuard is intentionally scoped to one run. It is safe for concurrent tool
// preparation, but it must not be shared as durable cross-run state.
type RunGuard struct {
	mu          sync.Mutex
	repeatLimit int
	seen        map[string]int
	// successfulResults remembers the last content digest for an equivalent
	// action. A first or changed successful result is progress; replaying the
	// same successful result is not and must keep consuming the repeat budget.
	successfulResults map[string]string
	observations      int
	denied            int
	lastDenied        RunGuardDecision
}

func NewRunGuard(repeatLimit int) *RunGuard {
	if repeatLimit <= 0 {
		repeatLimit = DefaultRunGuardRepeatLimit
	}
	return &RunGuard{
		repeatLimit:       repeatLimit,
		seen:              make(map[string]int),
		successfulResults: make(map[string]string),
	}
}

// Observe records an attempted action and decides whether its retry budget is
// exhausted. For a limit of N, observations 1..N are allowed and N+1 is denied.
func (g *RunGuard) Observe(input ActionFingerprintInput) RunGuardDecision {
	fingerprint := CanonicalActionFingerprint(input)
	g.mu.Lock()
	defer g.mu.Unlock()

	g.seen[fingerprint]++
	g.observations++
	occurrences := g.seen[fingerprint]
	decision := RunGuardDecision{
		Allowed:     occurrences <= g.repeatLimit,
		Fingerprint: fingerprint,
		Occurrences: occurrences,
		Limit:       g.repeatLimit,
		Reason:      RunGuardWithinLimit,
	}
	if !decision.Allowed {
		decision.Reason = RunGuardRepeatedAction
		g.denied++
		g.lastDenied = decision
	}
	return decision
}

// Snapshot returns an immutable, content-free audit view. Explicit progress
// and approval reset the active retry budget but intentionally do not erase
// the fact that a denial occurred earlier in this run.
func (g *RunGuard) Snapshot() RunGuardSnapshot {
	if g == nil {
		return RunGuardSnapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return RunGuardSnapshot{
		RepeatLimit:           g.repeatLimit,
		Observations:          g.observations,
		Denied:                g.denied,
		LastDeniedReason:      g.lastDenied.Reason,
		LastDeniedFingerprint: g.lastDenied.Fingerprint,
		LastDeniedOccurrences: g.lastDenied.Occurrences,
	}
}

// Reset marks explicit progress or user-authorized retry and starts a fresh
// repetition budget for the remainder of this run.
func (g *RunGuard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = make(map[string]int)
	g.successfulResults = make(map[string]string)
}

// ObserveResult records the outcome of an action previously passed to
// Observe. It returns true only when a successful result is new or changed for
// that exact normalized action. Progress resets occurrence counts while
// retaining the successful-result digest, so an identical successful replay
// starts consuming the budget again instead of resetting forever.
func (g *RunGuard) ObserveResult(input ActionFingerprintInput, result string, isError bool) bool {
	if g == nil || isError {
		return false
	}
	fingerprint := CanonicalActionFingerprint(input)
	digest := sha256.Sum256([]byte(result))
	resultDigest := hex.EncodeToString(digest[:])

	g.mu.Lock()
	defer g.mu.Unlock()
	if previous, exists := g.successfulResults[fingerprint]; exists && previous == resultDigest {
		return false
	}
	g.successfulResults[fingerprint] = resultDigest
	g.seen = make(map[string]int)
	return true
}

// CanonicalActionFingerprint returns a stable, non-reversible SHA-256 digest.
// Valid JSON arguments are normalized for insignificant whitespace and object
// key order. Invalid JSON remains deterministic but is namespaced separately.
func CanonicalActionFingerprint(input ActionFingerprintInput) string {
	payload := struct {
		ToolName       string `json:"tool_name"`
		Arguments      string `json:"arguments"`
		TargetRevision string `json:"target_revision"`
		FailureCode    string `json:"failure_code"`
		PlanStep       string `json:"plan_step"`
	}{
		ToolName:       input.ToolName,
		Arguments:      canonicalFingerprintArguments(input.Arguments),
		TargetRevision: input.TargetRevision,
		FailureCode:    input.FailureCode,
		PlanStep:       input.PlanStep,
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func canonicalFingerprintArguments(arguments json.RawMessage) string {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 {
		return "valid:{}"
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "invalid:" + string(trimmed)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "invalid:" + string(trimmed)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "invalid:" + string(trimmed)
	}
	return "valid:" + string(canonical)
}
