package runtimeplane

import (
	"net/http"
	"testing"
	"time"
)

func TestParseHeaderQuotaAnthropicTokens(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Minute).Format(time.RFC3339)
	headers := http.Header{
		"Anthropic-Ratelimit-Tokens-Limit":     []string{"1000"},
		"Anthropic-Ratelimit-Tokens-Remaining": []string{"250"},
		"Anthropic-Ratelimit-Tokens-Reset":     []string{reset},
	}

	obs := ParseHeaderQuota(headers, now)
	if obs.RateLimit.Limit != 1000 || obs.RateLimit.Used != 750 {
		t.Fatalf("unexpected rate limit: %+v", obs.RateLimit)
	}
	if !obs.RateLimit.ResetAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("unexpected reset: %v", obs.RateLimit.ResetAt)
	}
}

func TestParseHeaderQuotaOpenAIDurationResetAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	headers := http.Header{
		"X-Ratelimit-Limit-Tokens":     []string{"1000"},
		"X-Ratelimit-Remaining-Tokens": []string{"900"},
		"X-Ratelimit-Reset-Tokens":     []string{"6m0s"},
		"Retry-After":                  []string{"10"},
	}

	obs := ParseHeaderQuota(headers, now)
	if obs.RateLimit.Limit != 1000 || obs.RateLimit.Used != 100 {
		t.Fatalf("unexpected rate limit: %+v", obs.RateLimit)
	}
	if !obs.RateLimit.ResetAt.Equal(now.Add(6 * time.Minute)) {
		t.Fatalf("unexpected reset: %v", obs.RateLimit.ResetAt)
	}
	if !obs.CooldownUntil.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("unexpected cooldown: %v", obs.CooldownUntil)
	}
}

func TestSelectAccountGatesDepletedRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	gated := testAccount("gated-rate", 0.10, 0.10, time.Hour)
	gated.ObservedAt = now
	gated.RateLimit = QuotaWindow{Used: 95, Limit: 100, ResetAt: now.Add(time.Minute)}
	usable := testAccount("usable", 0.10, 0.10, time.Hour)
	usable.ObservedAt = now
	usable.RateLimit = QuotaWindow{Used: 20, Limit: 100, ResetAt: now.Add(time.Minute)}

	decision := SelectAccount([]AccountState{gated, usable}, "", GroupClaude, now, SchedulerParams{})
	if decision.AccountID != "usable" {
		t.Fatalf("expected rate-limit usable account, got %+v", decision)
	}
}
