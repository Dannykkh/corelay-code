package runtimeplane

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	telemetryFiveHourWindow = 5 * time.Hour
	telemetrySevenDayWindow = 7 * 24 * time.Hour

	// transientRecoverySuccesses is how many consecutive successes an
	// escalated account needs before it rejoins rotation at full trust.
	transientRecoverySuccesses = 2
)

// transientAvoidLadder escalates soft avoidance for repeated transient
// failures (5xx/timeouts) so a flapping upstream backs off progressively
// instead of being retried at a fixed cadence.
var transientAvoidLadder = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

type usageSample struct {
	At    time.Time
	Units float64
}

type telemetryAccount struct {
	ID         string
	Provider   string
	Group      ProviderGroup
	Status     AccountStatus
	ObservedAt time.Time
	// SnapshotAt is the ObservedAt of the last accepted external quota
	// snapshot; older snapshots arriving out of order are dropped.
	SnapshotAt time.Time
	// QuotaCooldownUntil is a hard quota cooldown (429/Retry-After). It only
	// expires with time; a later success does not clear it.
	QuotaCooldownUntil time.Time
	// SoftAvoidUntil is transient-failure avoidance driven by the escalation
	// ladder; it clears through the success hysteresis below.
	SoftAvoidUntil time.Time
	TransientLevel int
	SuccessStreak  int
	FiveHour       QuotaWindow
	SevenDay       QuotaWindow
	RateLimit      QuotaWindow
	Samples        []usageSample
}

// TelemetryStore keeps live proxy-observed account state. It intentionally
// records local observations only; provider quota collectors can be merged later.
type TelemetryStore struct {
	mu       sync.RWMutex
	accounts map[string]*telemetryAccount
}

func NewTelemetryStore() *TelemetryStore {
	return &TelemetryStore{accounts: map[string]*telemetryAccount{}}
}

func (s *TelemetryStore) RecordUsage(provider string, group ProviderGroup, inputTokens, outputTokens int, now time.Time) {
	if s == nil || provider == "" {
		return
	}
	units := float64(inputTokens + outputTokens)
	if units < 0 {
		units = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.ensureAccountLocked(provider, group, now)
	// Usage is accounting only; recovery state moves through RecordSuccess so
	// the success streak is not double-counted when both are recorded.
	account.Status = AccountHealthy
	account.ObservedAt = now
	if units > 0 {
		account.Samples = append(account.Samples, usageSample{At: now, Units: units})
	}
	account.Samples = pruneUsageSamples(account.Samples, now)
}

func (s *TelemetryStore) RecordSuccess(provider string, group ProviderGroup, now time.Time) {
	if s == nil || provider == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.ensureAccountLocked(provider, group, now)
	account.ObservedAt = now
	account.Samples = pruneUsageSamples(account.Samples, now)
	applySuccessState(account)
}

func (s *TelemetryStore) RecordFailure(provider string, group ProviderGroup, statusCode int, now time.Time) {
	if s == nil || provider == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.ensureAccountLocked(provider, group, now)
	account.ObservedAt = now
	account.Samples = pruneUsageSamples(account.Samples, now)
	applyFailureState(account, statusCode, now)
}

func (s *TelemetryStore) RecordResponse(provider string, group ProviderGroup, statusCode int, headers http.Header, now time.Time) {
	if s == nil || provider == "" {
		return
	}
	observation := ParseHeaderQuota(headers, now)

	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.ensureAccountLocked(provider, group, now)
	account.ObservedAt = now
	account.Samples = pruneUsageSamples(account.Samples, now)
	if observation.RateLimit.Limit > 0 {
		account.RateLimit = observation.RateLimit
	}
	if observation.CooldownUntil.After(now) {
		account.QuotaCooldownUntil = maxTime(account.QuotaCooldownUntil, observation.CooldownUntil)
	}
	applyFailureState(account, statusCode, now)
}

func (s *TelemetryStore) RecordAccountSnapshot(account AccountState, now time.Time) {
	if s == nil || account.Provider == "" {
		return
	}
	if account.ID == "" {
		account.ID = "provider:" + account.Provider
	}
	if account.Group == "" {
		account.Group = ClassifyProviderGroup(account.Provider)
	}
	if account.Status == "" {
		account.Status = AccountHealthy
	}
	if account.ObservedAt.IsZero() {
		account.ObservedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.ensureAccountLocked(account.Provider, account.Group, account.ObservedAt)
	if account.ObservedAt.Before(stored.SnapshotAt) {
		// A slower collector finished after a fresher snapshot was already
		// applied; dropping the out-of-order write keeps observations monotonic.
		return
	}
	stored.SnapshotAt = account.ObservedAt
	stored.ID = account.ID
	stored.Provider = account.Provider
	stored.Group = account.Group
	if account.Status == AccountUnhealthy || stored.Status != AccountUnhealthy {
		// Fail-closed: an external quota snapshot cannot prove credentials
		// recovered, so only a live success clears an unhealthy account.
		stored.Status = account.Status
	}
	stored.ObservedAt = maxTime(stored.ObservedAt, account.ObservedAt)
	stored.QuotaCooldownUntil = maxTime(stored.QuotaCooldownUntil, account.CooldownUntil)
	stored.FiveHour = account.FiveHour
	stored.SevenDay = account.SevenDay
	stored.RateLimit = account.RateLimit
	stored.Samples = pruneUsageSamples(stored.Samples, now)
}

func (s *TelemetryStore) Merge(accounts []AccountState, now time.Time) []AccountState {
	if s == nil {
		return accounts
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]AccountState, len(accounts))
	copy(out, accounts)
	byID := make(map[string]int, len(out))
	for i := range out {
		byID[out[i].ID] = i
	}

	for _, telemetry := range s.accounts {
		fiveHourUsed, sevenDayUsed := telemetryUsage(telemetry.Samples, now)
		fiveHour := telemetry.FiveHour
		if fiveHour.Limit <= 0 && fiveHour.Used <= 0 {
			fiveHour = QuotaWindow{Used: fiveHourUsed}
		}
		sevenDay := telemetry.SevenDay
		if sevenDay.Limit <= 0 && sevenDay.Used <= 0 {
			sevenDay = QuotaWindow{Used: sevenDayUsed}
		}
		idx, ok := byID[telemetry.ID]
		if !ok {
			out = append(out, AccountState{
				ID:         telemetry.ID,
				Provider:   telemetry.Provider,
				Group:      telemetry.Group,
				Status:     telemetryStatus(telemetry.Status),
				ObservedAt: telemetry.ObservedAt,
				AllowStale: true,
				FiveHour:   fiveHour,
				SevenDay:   sevenDay,
				RateLimit:  telemetry.RateLimit,
			})
			idx = len(out) - 1
			byID[telemetry.ID] = idx
		}

		account := out[idx]
		account.Provider = firstNonEmpty(account.Provider, telemetry.Provider)
		if account.Group == "" || account.Group == GroupDefault {
			account.Group = telemetry.Group
		}
		account.Status = telemetryStatus(telemetry.Status)
		effectiveCooldown := maxTime(telemetry.QuotaCooldownUntil, telemetry.SoftAvoidUntil)
		if effectiveCooldown.After(now) {
			account.CooldownUntil = maxTime(account.CooldownUntil, effectiveCooldown)
		} else if !account.CooldownUntil.IsZero() && !account.CooldownUntil.After(now) {
			account.CooldownUntil = time.Time{}
		}
		if telemetry.ObservedAt.After(account.ObservedAt) {
			account.ObservedAt = telemetry.ObservedAt
		}
		if fiveHour.Limit > 0 || fiveHour.Used > 0 {
			account.FiveHour = fiveHour
		}
		if sevenDay.Limit > 0 || sevenDay.Used > 0 {
			account.SevenDay = sevenDay
		}
		if telemetry.RateLimit.Limit > 0 {
			account.RateLimit = telemetry.RateLimit
		}
		out[idx] = account
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *TelemetryStore) HasData() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts) > 0
}

func (s *TelemetryStore) ensureAccountLocked(provider string, group ProviderGroup, now time.Time) *telemetryAccount {
	id := "provider:" + provider
	account := s.accounts[id]
	if account == nil {
		account = &telemetryAccount{
			ID:         id,
			Provider:   provider,
			Group:      group,
			Status:     AccountHealthy,
			ObservedAt: now,
		}
		s.accounts[id] = account
	}
	if group != "" && group != GroupDefault {
		account.Group = group
	} else if account.Group == "" {
		account.Group = ClassifyProviderGroup(provider)
	}
	if account.Provider == "" {
		account.Provider = provider
	}
	return account
}

func applyFailureState(account *telemetryAccount, statusCode int, now time.Time) {
	switch {
	case statusCode == 401 || statusCode == 403:
		account.Status = AccountUnhealthy
		account.SuccessStreak = 0
	case statusCode == 429:
		account.Status = AccountHealthy
		account.SuccessStreak = 0
		account.QuotaCooldownUntil = maxTime(account.QuotaCooldownUntil, now.Add(5*time.Minute))
	case statusCode >= 500:
		account.Status = AccountHealthy
		account.SuccessStreak = 0
		if account.TransientLevel < len(transientAvoidLadder) {
			account.TransientLevel++
		}
		account.SoftAvoidUntil = maxTime(account.SoftAvoidUntil, now.Add(transientAvoidLadder[account.TransientLevel-1]))
	}
}

// applySuccessState records one successful upstream call. Level-1 transient
// avoidance clears on the first success; escalated levels need a success
// streak so a flapping upstream cannot bounce straight back into rotation.
// Hard quota cooldowns are deliberately left to expire on their own.
func applySuccessState(account *telemetryAccount) {
	account.Status = AccountHealthy
	if account.TransientLevel == 0 {
		return
	}
	account.SuccessStreak++
	if account.TransientLevel <= 1 || account.SuccessStreak >= transientRecoverySuccesses {
		account.TransientLevel = 0
		account.SuccessStreak = 0
		account.SoftAvoidUntil = time.Time{}
	}
}

func pruneUsageSamples(samples []usageSample, now time.Time) []usageSample {
	cutoff := now.Add(-telemetrySevenDayWindow)
	kept := samples[:0]
	for _, sample := range samples {
		if sample.At.After(cutoff) || sample.At.Equal(cutoff) {
			kept = append(kept, sample)
		}
	}
	return kept
}

func telemetryUsage(samples []usageSample, now time.Time) (float64, float64) {
	fiveHourCutoff := now.Add(-telemetryFiveHourWindow)
	sevenDayCutoff := now.Add(-telemetrySevenDayWindow)
	var fiveHourUsed float64
	var sevenDayUsed float64
	for _, sample := range samples {
		if sample.At.After(sevenDayCutoff) || sample.At.Equal(sevenDayCutoff) {
			sevenDayUsed += sample.Units
		}
		if sample.At.After(fiveHourCutoff) || sample.At.Equal(fiveHourCutoff) {
			fiveHourUsed += sample.Units
		}
	}
	return fiveHourUsed, sevenDayUsed
}

func telemetryStatus(status AccountStatus) AccountStatus {
	if status == "" {
		return AccountHealthy
	}
	return status
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
