package runtimeplane

import (
	"testing"
	"time"
)

func TestLeaseStorePinsSessionByGroup(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewLeaseStore()
	account := AccountState{ID: "provider:openai-work", Provider: "openai-work", Group: GroupCodex}

	lease := store.Upsert("session-1", "agent-1", account, "gpt-5.5", now, time.Hour)
	if lease.AccountID != "provider:openai-work" || lease.Group != GroupCodex {
		t.Fatalf("unexpected lease: %+v", lease)
	}

	got, ok := store.Current("session-1", GroupCodex, now.Add(30*time.Minute))
	if !ok || got.AccountID != "provider:openai-work" {
		t.Fatalf("expected current lease, got ok=%v lease=%+v", ok, got)
	}
	if _, ok := store.Current("session-1", GroupClaude, now.Add(30*time.Minute)); ok {
		t.Fatal("lease should be isolated by group")
	}
}

func TestAgentLeaseNeedsReeval(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewLeaseStore()
	account := AccountState{ID: "provider:openai", Provider: "openai", Group: GroupCodex}
	lease := store.Upsert("session-1", "", account, "gpt-5.5", now, time.Hour)

	if lease.NeedsReeval(now.Add(30*time.Second), DefaultLeaseReevalInterval) {
		t.Fatal("fresh lease should not need re-eval inside the interval")
	}
	if !lease.NeedsReeval(now.Add(DefaultLeaseReevalInterval), DefaultLeaseReevalInterval) {
		t.Fatal("aged lease should need re-eval")
	}
	if !(AgentLease{}).NeedsReeval(now, DefaultLeaseReevalInterval) {
		t.Fatal("lease without re-eval timestamp must always re-eval")
	}
}

func TestLeaseStoreExpires(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewLeaseStore()
	account := AccountState{ID: "provider:anthropic", Provider: "anthropic", Group: GroupClaude}
	store.Upsert("session-1", "", account, "claude-sonnet", now, time.Minute)

	if _, ok := store.Current("session-1", GroupClaude, now.Add(2*time.Minute)); ok {
		t.Fatal("lease should expire")
	}
	if count := store.Count(now.Add(2 * time.Minute)); count != 0 {
		t.Fatalf("expired lease should not count, got %d", count)
	}
}
