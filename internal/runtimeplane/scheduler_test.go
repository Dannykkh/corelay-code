package runtimeplane

import (
	"testing"
	"time"
)

func testAccount(id string, u5, u7 float64, resetIn time.Duration) AccountState {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	return AccountState{
		ID:         id,
		Provider:   "anthropic",
		Group:      GroupClaude,
		Status:     AccountHealthy,
		FiveHour:   QuotaWindow{Used: u5, Limit: 1, ResetAt: now.Add(5 * time.Hour)},
		SevenDay:   QuotaWindow{Used: u7, Limit: 1, ResetAt: now.Add(resetIn)},
		ObservedAt: now,
	}
}

func TestClassifyModelGroup(t *testing.T) {
	cases := map[string]ProviderGroup{
		"claude-sonnet-4-20250514": GroupClaude,
		"sonnet":                   GroupClaude,
		"gpt-5.5":                  GroupCodex,
		"codex":                    GroupCodex,
		"o3":                       GroupCodex,
		"qwen3:14b":                GroupLocal,
		"custom-model":             GroupDefault,
	}
	for model, want := range cases {
		if got := ClassifyModelGroup(model); got != want {
			t.Fatalf("ClassifyModelGroup(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestClassifyProviderGroup(t *testing.T) {
	cases := map[string]ProviderGroup{
		"anthropic":       GroupClaude,
		"claude-main":     GroupClaude,
		"openai":          GroupCodex,
		"codex-weekend":   GroupCodex,
		"ollama-office":   GroupLocal,
		"sglang":          GroupLocal,
		"github-copilot":  GroupDefault,
		"unknown-service": GroupDefault,
	}
	for provider, want := range cases {
		if got := ClassifyProviderGroup(provider); got != want {
			t.Fatalf("ClassifyProviderGroup(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestSelectAccountBurnsSoonToResetQuota(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	accounts := []AccountState{
		testAccount("fresh-week", 0.10, 0.20, 6*24*time.Hour),
		testAccount("expires-soon", 0.10, 0.40, 2*time.Hour),
	}
	decision := SelectAccount(accounts, "", GroupClaude, now, SchedulerParams{})
	if decision.AccountID != "expires-soon" || decision.Action != DecisionSwitch {
		t.Fatalf("expected expires-soon switch, got %+v", decision)
	}
}

func TestSelectAccountDoesNotChaseFiveHourGatedQuota(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	accounts := []AccountState{
		testAccount("urgent-but-gated", 0.95, 0.20, time.Hour),
		testAccount("usable", 0.30, 0.60, 24*time.Hour),
	}
	decision := SelectAccount(accounts, "", GroupClaude, now, SchedulerParams{})
	if decision.AccountID != "usable" {
		t.Fatalf("expected usable account, got %+v", decision)
	}
}

func TestSelectAccountKeepsStickyCurrentWithinMargin(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	accounts := []AccountState{
		testAccount("current", 0.10, 0.40, 12*time.Hour),
		testAccount("slightly-better", 0.09, 0.40, 11*time.Hour),
	}
	decision := SelectAccount(accounts, "current", GroupClaude, now, SchedulerParams{})
	if decision.Action != DecisionStay || decision.AccountID != "current" {
		t.Fatalf("expected sticky current, got %+v", decision)
	}
}

func TestSelectAccountSwitchesWhenBestBeatsMargin(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	accounts := []AccountState{
		testAccount("current", 0.80, 0.80, 6*24*time.Hour),
		testAccount("much-better", 0.10, 0.50, 2*time.Hour),
	}
	decision := SelectAccount(accounts, "current", GroupClaude, now, SchedulerParams{})
	if decision.Action != DecisionSwitch || decision.AccountID != "much-better" {
		t.Fatalf("expected switch to much-better, got %+v", decision)
	}
}

func TestSelectAccountGatesCooldownAuthQuotaAndStaleness(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	stale := testAccount("stale", 0.10, 0.10, time.Hour)
	stale.ObservedAt = now.Add(-30 * time.Minute)
	unhealthy := testAccount("unhealthy", 0.10, 0.10, time.Hour)
	unhealthy.Status = AccountUnhealthy
	cooling := testAccount("cooling", 0.10, 0.10, time.Hour)
	cooling.CooldownUntil = now.Add(time.Hour)
	gated := testAccount("weekly-gated", 0.10, 0.995, time.Hour)
	usable := testAccount("usable", 0.10, 0.10, 48*time.Hour)

	decision := SelectAccount([]AccountState{stale, unhealthy, cooling, gated, usable}, "", GroupClaude, now, SchedulerParams{})
	if decision.AccountID != "usable" {
		t.Fatalf("expected only usable account, got %+v", decision)
	}
}

func TestSelectAccountRotatesWhenAllAccountsStale(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	stale := func(id string) AccountState {
		account := testAccount(id, 0.10, 0.10, time.Hour)
		account.ObservedAt = now.Add(-30 * time.Minute)
		return account
	}
	accounts := []AccountState{stale("stale-a"), stale("stale-b"), stale("stale-c")}

	first := SelectAccount(accounts, "", GroupClaude, now, SchedulerParams{})
	if first.Action != DecisionSwitch || first.AccountID != "stale-a" {
		t.Fatalf("expected first stale account, got %+v", first)
	}
	second := SelectAccount(accounts, "stale-a", GroupClaude, now, SchedulerParams{})
	if second.Action != DecisionSwitch || second.AccountID != "stale-b" {
		t.Fatalf("expected rotation to stale-b, got %+v", second)
	}
	wrapped := SelectAccount(accounts, "stale-c", GroupClaude, now, SchedulerParams{})
	if wrapped.Action != DecisionSwitch || wrapped.AccountID != "stale-a" {
		t.Fatalf("expected wrap-around to stale-a, got %+v", wrapped)
	}
	only := SelectAccount(accounts[:1], "stale-a", GroupClaude, now, SchedulerParams{})
	if only.Action != DecisionStay || only.AccountID != "stale-a" {
		t.Fatalf("expected lone stale current to stay, got %+v", only)
	}
}

func TestSelectAccountStaleRotationExcludesHardGatedAccounts(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	unhealthy := testAccount("unhealthy", 0.10, 0.10, time.Hour)
	unhealthy.Status = AccountUnhealthy
	cooling := testAccount("cooling", 0.10, 0.10, time.Hour)
	cooling.CooldownUntil = now.Add(time.Hour)

	decision := SelectAccount([]AccountState{unhealthy, cooling}, "", GroupClaude, now, SchedulerParams{})
	if decision.Action != DecisionNone {
		t.Fatalf("hard-gated accounts must not enter stale rotation, got %+v", decision)
	}
}

func TestNewAgentLeaseCopiesSelectedRuntimeTarget(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	account := testAccount("acct-1", 0.10, 0.10, time.Hour)
	lease := NewAgentLease("lease-1", "session-1", "agent-1", account, "claude-sonnet", now, time.Hour)
	if lease.AccountID != "acct-1" || lease.Provider != "anthropic" || lease.Group != GroupClaude {
		t.Fatalf("lease did not copy account runtime target: %+v", lease)
	}
	if lease.ExpiresAt.Sub(now) != time.Hour {
		t.Fatalf("lease TTL mismatch: %+v", lease)
	}
}
