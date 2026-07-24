package runtimeplane

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	DefaultLeaseTTL = 5 * time.Hour

	// DefaultLeaseReevalInterval bounds how long a leased session may skip the
	// scheduler before its account choice is reconsidered.
	DefaultLeaseReevalInterval = time.Minute
)

// LeaseStore keeps session-to-account stickiness in memory. It is intentionally
// ephemeral: a proxy restart may rebalance sessions from current quota state.
type LeaseStore struct {
	mu     sync.RWMutex
	leases map[string]AgentLease
}

func NewLeaseStore() *LeaseStore {
	return &LeaseStore{leases: map[string]AgentLease{}}
}

func (s *LeaseStore) Current(sessionID string, group ProviderGroup, now time.Time) (AgentLease, bool) {
	if s == nil || sessionID == "" || group == "" {
		return AgentLease{}, false
	}
	s.mu.RLock()
	lease, ok := s.leases[leaseKey(sessionID, group)]
	s.mu.RUnlock()
	if !ok || leaseExpired(lease, now) {
		if ok {
			s.drop(sessionID, group)
		}
		return AgentLease{}, false
	}
	return lease, true
}

func (s *LeaseStore) Upsert(sessionID, agentID string, account AccountState, model string, now time.Time, ttl time.Duration) AgentLease {
	if s == nil || sessionID == "" || account.ID == "" {
		return AgentLease{}
	}
	if account.Group == "" {
		account.Group = ClassifyProviderGroup(account.Provider)
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	lease := NewAgentLease(fmt.Sprintf("lease:%s:%s", account.Group, sessionID), sessionID, agentID, account, model, now, ttl)
	s.mu.Lock()
	s.leases[leaseKey(sessionID, account.Group)] = lease
	s.mu.Unlock()
	return lease
}

func (s *LeaseStore) Snapshot(now time.Time) []AgentLease {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]AgentLease, 0, len(s.leases))
	for _, lease := range s.leases {
		if !leaseExpired(lease, now) {
			out = append(out, lease)
		}
	}
	s.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *LeaseStore) Count(now time.Time) int {
	return len(s.Snapshot(now))
}

func (s *LeaseStore) drop(sessionID string, group ProviderGroup) {
	s.mu.Lock()
	delete(s.leases, leaseKey(sessionID, group))
	s.mu.Unlock()
}

func leaseKey(sessionID string, group ProviderGroup) string {
	return string(group) + "\x00" + sessionID
}

func leaseExpired(lease AgentLease, now time.Time) bool {
	return !lease.ExpiresAt.IsZero() && !lease.ExpiresAt.After(now)
}
