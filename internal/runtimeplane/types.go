package runtimeplane

import "time"

// ProviderGroup is the runtime routing bucket selected before a concrete
// provider/account is leased. It deliberately stays coarser than provider names:
// several accounts can compete inside one group.
type ProviderGroup string

const (
	GroupDefault ProviderGroup = "default"
	GroupClaude  ProviderGroup = "claude"
	GroupCodex   ProviderGroup = "codex"
	GroupLocal   ProviderGroup = "local"
)

// RuntimeTarget is the concrete provider/model pair chosen for a request.
type RuntimeTarget struct {
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Group    ProviderGroup `json:"group"`
}

// ModelCapability describes model behavior the harness must know before it can
// safely route work across providers.
type ModelCapability struct {
	ContextWindow       int  `json:"contextWindow,omitempty"`
	MaxOutput           int  `json:"maxOutput,omitempty"`
	ToolCalls           bool `json:"toolCalls"`
	Vision              bool `json:"vision"`
	Thinking            bool `json:"thinking"`
	StructuredOutput    bool `json:"structuredOutput"`
	PromptCacheFriendly bool `json:"promptCacheFriendly"`
}

// QuotaWindow is a provider/account quota window expressed in normalized units.
// Used and Limit may represent tokens, request counts, or provider-specific
// usage points, as long as both values use the same unit.
type QuotaWindow struct {
	Used    float64   `json:"used"`
	Limit   float64   `json:"limit"`
	ResetAt time.Time `json:"resetAt,omitempty"`
}

// Utilization returns Used / Limit. A zero or negative limit means the window is
// unknown/unbounded and therefore has zero utilization pressure.
func (w QuotaWindow) Utilization() float64 {
	if w.Limit <= 0 {
		return 0
	}
	return w.Used / w.Limit
}

// Headroom returns the remaining usage fraction before a ceiling is reached.
func (w QuotaWindow) Headroom(maxUtilization float64) float64 {
	return clamp01(maxUtilization - w.Utilization())
}

// AccountStatus describes whether an account can currently serve requests.
type AccountStatus string

const (
	AccountHealthy   AccountStatus = "healthy"
	AccountUnhealthy AccountStatus = "unhealthy"
)

// AccountState is the scheduler snapshot for one account. It contains no
// secrets, so it is safe to expose in dashboards and traces.
type AccountState struct {
	ID            string        `json:"id"`
	DisplayName   string        `json:"displayName,omitempty"`
	Provider      string        `json:"provider"`
	Group         ProviderGroup `json:"group"`
	Status        AccountStatus `json:"status"`
	FiveHour      QuotaWindow   `json:"fiveHour"`
	SevenDay      QuotaWindow   `json:"sevenDay"`
	RateLimit     QuotaWindow   `json:"rateLimit,omitempty"`
	CooldownUntil time.Time     `json:"cooldownUntil,omitempty"`
	ObservedAt    time.Time     `json:"observedAt,omitempty"`
	AllowStale    bool          `json:"allowStale,omitempty"`
}

// AgentLease pins a session/agent to a runtime account. The scheduler may choose
// a different account for the next lease, but an active stream should not move
// mid-request.
type AgentLease struct {
	ID           string        `json:"id"`
	SessionID    string        `json:"sessionId"`
	AgentID      string        `json:"agentId,omitempty"`
	AccountID    string        `json:"accountId"`
	Provider     string        `json:"provider"`
	Model        string        `json:"model"`
	Group        ProviderGroup `json:"group"`
	CreatedAt    time.Time     `json:"createdAt"`
	LastReevalAt time.Time     `json:"lastReevalAt,omitempty"`
	ExpiresAt    time.Time     `json:"expiresAt,omitempty"`
}

// NewAgentLease creates a stable lease record for a selected account.
func NewAgentLease(id, sessionID, agentID string, account AccountState, model string, now time.Time, ttl time.Duration) AgentLease {
	lease := AgentLease{
		ID:           id,
		SessionID:    sessionID,
		AgentID:      agentID,
		AccountID:    account.ID,
		Provider:     account.Provider,
		Model:        model,
		Group:        account.Group,
		CreatedAt:    now,
		LastReevalAt: now,
	}
	if ttl > 0 {
		lease.ExpiresAt = now.Add(ttl)
	}
	return lease
}

// NeedsReeval reports whether the lease is due for a fresh scheduler pass. A
// bound session stays on its account between passes so near-margin scores do
// not flap it between accounts on every request.
func (l AgentLease) NeedsReeval(now time.Time, interval time.Duration) bool {
	if interval <= 0 || l.LastReevalAt.IsZero() {
		return true
	}
	return now.Sub(l.LastReevalAt) >= interval
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
