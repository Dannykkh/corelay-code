package runtimeplane

import (
	"net/http"
	"testing"
	"time"
)

func TestTelemetryStoreMergesRollingUsage(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordUsage("openai", GroupCodex, 10, 5, now.Add(-time.Hour))
	store.RecordUsage("openai", GroupCodex, 20, 7, now.Add(-6*time.Hour))

	accounts := store.Merge([]AccountState{{
		ID:         "provider:openai",
		Provider:   "openai",
		Group:      GroupCodex,
		Status:     AccountHealthy,
		AllowStale: true,
	}}, now)

	if len(accounts) != 1 {
		t.Fatalf("expected one account, got %+v", accounts)
	}
	if accounts[0].FiveHour.Used != 15 {
		t.Fatalf("five hour usage = %v, want 15", accounts[0].FiveHour.Used)
	}
	if accounts[0].SevenDay.Used != 42 {
		t.Fatalf("seven day usage = %v, want 42", accounts[0].SevenDay.Used)
	}
}

func TestTelemetryStoreFailureStates(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()

	store.RecordFailure("anthropic", GroupClaude, 401, now)
	authAccounts := store.Merge(nil, now)
	if len(authAccounts) != 1 || authAccounts[0].Status != AccountUnhealthy {
		t.Fatalf("auth failure should mark unhealthy, got %+v", authAccounts)
	}

	store.RecordSuccess("anthropic", GroupClaude, now.Add(time.Minute))
	healthyAccounts := store.Merge(nil, now.Add(time.Minute))
	if healthyAccounts[0].Status != AccountHealthy {
		t.Fatalf("success should restore healthy status, got %+v", healthyAccounts[0])
	}

	store.RecordFailure("anthropic", GroupClaude, 429, now.Add(2*time.Minute))
	coolingAccounts := store.Merge(nil, now.Add(2*time.Minute))
	if !coolingAccounts[0].CooldownUntil.After(now.Add(2 * time.Minute)) {
		t.Fatalf("rate limit should set cooldown, got %+v", coolingAccounts[0])
	}
}

func TestTelemetryStoreEscalatesTransientAvoidance(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()

	store.RecordFailure("openai", GroupCodex, 503, now)
	first := store.Merge(nil, now)
	if !first[0].CooldownUntil.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("first transient failure should avoid for 30s, got %+v", first[0])
	}

	later := now.Add(time.Minute)
	store.RecordFailure("openai", GroupCodex, 503, later)
	second := store.Merge(nil, later)
	if !second[0].CooldownUntil.Equal(later.Add(2 * time.Minute)) {
		t.Fatalf("second transient failure should escalate to 2m, got %+v", second[0])
	}
}

func TestTelemetryStoreEscalatedRecoveryNeedsSuccessStreak(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordFailure("openai", GroupCodex, 503, now)
	store.RecordFailure("openai", GroupCodex, 503, now.Add(time.Second))

	oneSuccess := now.Add(2 * time.Second)
	store.RecordSuccess("openai", GroupCodex, oneSuccess)
	if accounts := store.Merge(nil, oneSuccess); !accounts[0].CooldownUntil.After(oneSuccess) {
		t.Fatalf("one success must not clear escalated avoidance, got %+v", accounts[0])
	}

	store.RecordFailure("openai", GroupCodex, 503, now.Add(3*time.Second))
	store.RecordSuccess("openai", GroupCodex, now.Add(4*time.Second))
	if accounts := store.Merge(nil, now.Add(4*time.Second)); !accounts[0].CooldownUntil.After(now.Add(4 * time.Second)) {
		t.Fatalf("failure must reset the success streak, got %+v", accounts[0])
	}

	store.RecordSuccess("openai", GroupCodex, now.Add(5*time.Second))
	if accounts := store.Merge(nil, now.Add(5*time.Second)); !accounts[0].CooldownUntil.IsZero() {
		t.Fatalf("success streak should clear escalated avoidance, got %+v", accounts[0])
	}
}

func TestTelemetryStoreKeepsQuotaCooldownAfterSuccess(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordFailure("anthropic", GroupClaude, 429, now)
	store.RecordSuccess("anthropic", GroupClaude, now.Add(time.Second))

	accounts := store.Merge(nil, now.Add(time.Second))
	if !accounts[0].CooldownUntil.After(now.Add(time.Second)) {
		t.Fatalf("hard quota cooldown must survive a success, got %+v", accounts[0])
	}
}

func TestTelemetryStoreRecordsResponseHeaders(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordResponse("openai", GroupCodex, 200, http.Header{
		"X-Ratelimit-Limit-Tokens":     []string{"1000"},
		"X-Ratelimit-Remaining-Tokens": []string{"250"},
		"X-Ratelimit-Reset-Tokens":     []string{"1m"},
	}, now)

	accounts := store.Merge(nil, now)
	if len(accounts) != 1 {
		t.Fatalf("expected response header account, got %+v", accounts)
	}
	if accounts[0].RateLimit.Limit != 1000 || accounts[0].RateLimit.Used != 750 {
		t.Fatalf("rate limit headers not merged: %+v", accounts[0])
	}
	if !accounts[0].RateLimit.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("rate limit reset not parsed: %+v", accounts[0].RateLimit)
	}
}

func TestTelemetryStoreRecordsAccountSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordAccountSnapshot(AccountState{
		Provider: "anthropic",
		Group:    GroupClaude,
		Status:   AccountHealthy,
		FiveHour: QuotaWindow{Used: 80, Limit: 100, ResetAt: now.Add(time.Hour)},
		SevenDay: QuotaWindow{Used: 200, Limit: 1000, ResetAt: now.Add(24 * time.Hour)},
	}, now)

	accounts := store.Merge(nil, now)
	if len(accounts) != 1 {
		t.Fatalf("expected snapshot account, got %+v", accounts)
	}
	if accounts[0].FiveHour.Used != 80 || accounts[0].FiveHour.Limit != 100 {
		t.Fatalf("five hour snapshot not merged: %+v", accounts[0].FiveHour)
	}
	if accounts[0].SevenDay.Used != 200 || accounts[0].SevenDay.Limit != 1000 {
		t.Fatalf("seven day snapshot not merged: %+v", accounts[0].SevenDay)
	}
}

func TestTelemetryStoreDropsOutOfOrderSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordAccountSnapshot(AccountState{
		Provider:   "anthropic",
		Group:      GroupClaude,
		FiveHour:   QuotaWindow{Used: 80, Limit: 100},
		ObservedAt: now,
	}, now)
	store.RecordAccountSnapshot(AccountState{
		Provider:   "anthropic",
		Group:      GroupClaude,
		FiveHour:   QuotaWindow{Used: 10, Limit: 100},
		ObservedAt: now.Add(-time.Minute),
	}, now)

	accounts := store.Merge(nil, now)
	if accounts[0].FiveHour.Used != 80 {
		t.Fatalf("out-of-order snapshot overwrote newer data: %+v", accounts[0].FiveHour)
	}
}

func TestTelemetryStoreSnapshotCannotClearUnhealthy(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	store := NewTelemetryStore()
	store.RecordFailure("anthropic", GroupClaude, 401, now)
	store.RecordAccountSnapshot(AccountState{
		Provider:   "anthropic",
		Group:      GroupClaude,
		Status:     AccountHealthy,
		ObservedAt: now.Add(time.Second),
	}, now.Add(time.Second))

	if accounts := store.Merge(nil, now.Add(time.Second)); accounts[0].Status != AccountUnhealthy {
		t.Fatalf("snapshot must not clear unhealthy status, got %+v", accounts[0])
	}

	store.RecordSuccess("anthropic", GroupClaude, now.Add(2*time.Second))
	if accounts := store.Merge(nil, now.Add(2*time.Second)); accounts[0].Status != AccountHealthy {
		t.Fatalf("live success should clear unhealthy status, got %+v", accounts[0])
	}
}
