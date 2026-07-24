package runtimeplane

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HeaderQuotaObservation is quota/cooldown information extracted from provider
// response headers. It captures short-window rate limits; long subscription
// quota collectors can be merged into AccountState separately.
type HeaderQuotaObservation struct {
	RateLimit     QuotaWindow
	CooldownUntil time.Time
}

func ParseHeaderQuota(headers http.Header, now time.Time) HeaderQuotaObservation {
	obs := HeaderQuotaObservation{}
	if headers == nil {
		return obs
	}

	if window, ok := parseAnthropicRateLimit(headers, now); ok {
		obs.RateLimit = window
	} else if window, ok := parseOpenAIRateLimit(headers, now); ok {
		obs.RateLimit = window
	}

	if retryAfter := strings.TrimSpace(headers.Get("retry-after")); retryAfter != "" {
		if resetAt, ok := parseResetTime(retryAfter, now); ok {
			obs.CooldownUntil = resetAt
		}
	}
	return obs
}

func parseAnthropicRateLimit(headers http.Header, now time.Time) (QuotaWindow, bool) {
	if window, ok := parseLimitRemainingReset(headers, "anthropic-ratelimit-tokens", now); ok {
		return window, true
	}

	inputLimit, inputRemaining, inputOK := limitRemaining(headers, "anthropic-ratelimit-input-tokens")
	outputLimit, outputRemaining, outputOK := limitRemaining(headers, "anthropic-ratelimit-output-tokens")
	if inputOK && outputOK {
		resetAt := laterReset(headers.Get("anthropic-ratelimit-input-tokens-reset"), headers.Get("anthropic-ratelimit-output-tokens-reset"), now)
		return QuotaWindow{
			Used:    (inputLimit - inputRemaining) + (outputLimit - outputRemaining),
			Limit:   inputLimit + outputLimit,
			ResetAt: resetAt,
		}, true
	}

	return parseLimitRemainingReset(headers, "anthropic-ratelimit-requests", now)
}

func parseOpenAIRateLimit(headers http.Header, now time.Time) (QuotaWindow, bool) {
	if window, ok := parseOpenAIWindow(headers, "tokens", now); ok {
		return window, true
	}
	return parseOpenAIWindow(headers, "requests", now)
}

func parseOpenAIWindow(headers http.Header, metric string, now time.Time) (QuotaWindow, bool) {
	limit, ok := parseFloatHeader(headers, "x-ratelimit-limit-"+metric)
	if !ok || limit <= 0 {
		return QuotaWindow{}, false
	}
	remaining, ok := parseFloatHeader(headers, "x-ratelimit-remaining-"+metric)
	if !ok {
		return QuotaWindow{}, false
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > limit {
		remaining = limit
	}
	resetAt, _ := parseResetTime(headers.Get("x-ratelimit-reset-"+metric), now)
	return QuotaWindow{Used: limit - remaining, Limit: limit, ResetAt: resetAt}, true
}

func parseLimitRemainingReset(headers http.Header, prefix string, now time.Time) (QuotaWindow, bool) {
	limit, remaining, ok := limitRemaining(headers, prefix)
	if !ok {
		return QuotaWindow{}, false
	}
	resetAt, _ := parseResetTime(headers.Get(prefix+"-reset"), now)
	return QuotaWindow{
		Used:    limit - remaining,
		Limit:   limit,
		ResetAt: resetAt,
	}, true
}

func limitRemaining(headers http.Header, prefix string) (float64, float64, bool) {
	limit, ok := parseFloatHeader(headers, prefix+"-limit")
	if !ok || limit <= 0 {
		return 0, 0, false
	}
	remaining, ok := parseFloatHeader(headers, prefix+"-remaining")
	if !ok {
		return 0, 0, false
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > limit {
		remaining = limit
	}
	return limit, remaining, true
}

func parseFloatHeader(headers http.Header, key string) (float64, bool) {
	value := strings.TrimSpace(headers.Get(key))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func laterReset(a, b string, now time.Time) time.Time {
	ra, oka := parseResetTime(a, now)
	rb, okb := parseResetTime(b, now)
	switch {
	case oka && okb && rb.After(ra):
		return rb
	case oka:
		return ra
	case okb:
		return rb
	default:
		return time.Time{}
	}
}

func parseResetTime(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts, true
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return now.Add(duration), true
	}
	return time.Time{}, false
}
