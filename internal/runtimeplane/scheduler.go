package runtimeplane

import (
	"fmt"
	"sort"
	"time"
)

const (
	defaultFiveHourMax  = 0.90
	defaultSevenDayMax  = 0.99
	defaultUrgencyMax   = 4.0
	defaultSwitchMargin = 0.25
	defaultStaleAfter   = 15 * time.Minute
	sevenDayPeriod      = 7 * 24 * time.Hour
	DecisionStay        = "stay"
	DecisionSwitch      = "switch"
	DecisionNone        = "none"
)

// SchedulerParams controls pure account selection.
type SchedulerParams struct {
	FiveHourMax  float64
	SevenDayMax  float64
	UrgencyMax   float64
	SwitchMargin float64
	StaleAfter   time.Duration
}

// Normalized fills default scheduler values.
func (p SchedulerParams) Normalized() SchedulerParams {
	if p.FiveHourMax <= 0 {
		p.FiveHourMax = defaultFiveHourMax
	}
	if p.SevenDayMax <= 0 {
		p.SevenDayMax = defaultSevenDayMax
	}
	if p.UrgencyMax <= 0 {
		p.UrgencyMax = defaultUrgencyMax
	}
	if p.SwitchMargin < 0 {
		p.SwitchMargin = defaultSwitchMargin
	}
	if p.SwitchMargin == 0 {
		p.SwitchMargin = defaultSwitchMargin
	}
	if p.StaleAfter <= 0 {
		p.StaleAfter = defaultStaleAfter
	}
	return p
}

// SelectionDecision explains the scheduler choice.
type SelectionDecision struct {
	Action    string  `json:"action"`
	AccountID string  `json:"accountId,omitempty"`
	Score     float64 `json:"score,omitempty"`
	Reason    string  `json:"reason"`
}

// SelectAccount chooses the best account from a snapshot. It is deterministic:
// ties are broken by lower 5h utilization, then earlier 7d reset, then account ID.
func SelectAccount(accounts []AccountState, currentID string, group ProviderGroup, now time.Time, params SchedulerParams) SelectionDecision {
	params = params.Normalized()
	candidates := make([]scoredAccount, 0, len(accounts))
	for _, account := range accounts {
		if group != "" && group != GroupDefault && account.Group != group {
			continue
		}
		if reason := ineligibleReason(account, now, params); reason != "" {
			continue
		}
		candidates = append(candidates, scoredAccount{
			account: account,
			score:   AccountScore(account, now, params),
		})
	}
	if len(candidates) == 0 {
		return staleRotation(accounts, currentID, group, now, params)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return rankLess(candidates[i], candidates[j])
	})

	best := candidates[0]
	if currentID != "" {
		for _, candidate := range candidates {
			if candidate.account.ID != currentID {
				continue
			}
			if best.account.ID == currentID {
				return SelectionDecision{
					Action:    DecisionStay,
					AccountID: currentID,
					Score:     candidate.score,
					Reason:    "current account remains highest ranked",
				}
			}
			threshold := candidate.score * (1 + params.SwitchMargin)
			if best.score <= threshold {
				return SelectionDecision{
					Action:    DecisionStay,
					AccountID: currentID,
					Score:     candidate.score,
					Reason:    fmt.Sprintf("sticky current within %.0f%% switch margin", params.SwitchMargin*100),
				}
			}
			break
		}
	}

	return SelectionDecision{
		Action:    DecisionSwitch,
		AccountID: best.account.ID,
		Score:     best.score,
		Reason:    "highest usable burst multiplied by quota perishability",
	}
}

// AccountScore is usable burst now multiplied by weekly quota perishability.
func AccountScore(account AccountState, now time.Time, params SchedulerParams) float64 {
	params = params.Normalized()
	servableNow := account.FiveHour.Headroom(params.FiveHourMax)
	if account.RateLimit.Limit > 0 {
		rateHeadroom := account.RateLimit.Headroom(params.FiveHourMax)
		if rateHeadroom < servableNow {
			servableNow = rateHeadroom
		}
	}
	sevenHeadroom := account.SevenDay.Headroom(params.SevenDayMax)
	if sevenHeadroom < servableNow {
		servableNow = sevenHeadroom
	}
	return servableNow * urgency(account.SevenDay.ResetAt, now, params.UrgencyMax)
}

type scoredAccount struct {
	account AccountState
	score   float64
}

func rankLess(a, b scoredAccount) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	aFive := a.account.FiveHour.Utilization()
	bFive := b.account.FiveHour.Utilization()
	if aFive != bFive {
		return aFive < bFive
	}
	aReset := a.account.SevenDay.ResetAt
	bReset := b.account.SevenDay.ResetAt
	if !aReset.Equal(bReset) {
		if aReset.IsZero() {
			return false
		}
		if bReset.IsZero() {
			return true
		}
		return aReset.Before(bReset)
	}
	return a.account.ID < b.account.ID
}

// Eligible reports whether the account passes every scheduler gate right now.
func Eligible(account AccountState, now time.Time, params SchedulerParams) bool {
	return ineligibleReason(account, now, params.Normalized()) == ""
}

// staleRotation resolves the all-stale deadlock: when every otherwise-usable
// account is gated only by stale usage data there is no fresh signal to rank
// on, so rotate deterministically to the account after the current one instead
// of pinning a single unknown-usage account forever.
func staleRotation(accounts []AccountState, currentID string, group ProviderGroup, now time.Time, params SchedulerParams) SelectionDecision {
	stale := make([]AccountState, 0, len(accounts))
	for _, account := range accounts {
		if group != "" && group != GroupDefault && account.Group != group {
			continue
		}
		// "usage stale" is the last gate checked, so this reason implies the
		// account is healthy, off cooldown, and inside every quota ceiling.
		if ineligibleReason(account, now, params) == "usage stale" {
			stale = append(stale, account)
		}
	}
	if len(stale) == 0 {
		return SelectionDecision{Action: DecisionNone, Reason: "no eligible account"}
	}
	sort.SliceStable(stale, func(i, j int) bool { return stale[i].ID < stale[j].ID })
	next := stale[0]
	for i, account := range stale {
		if account.ID == currentID {
			next = stale[(i+1)%len(stale)]
			break
		}
	}
	if next.ID == currentID {
		return SelectionDecision{
			Action:    DecisionStay,
			AccountID: currentID,
			Reason:    "all usable accounts stale; only unknown-usage candidate",
		}
	}
	return SelectionDecision{
		Action:    DecisionSwitch,
		AccountID: next.ID,
		Reason:    "all usable accounts stale; rotating unknown-usage accounts",
	}
}

func ineligibleReason(account AccountState, now time.Time, params SchedulerParams) string {
	if account.ID == "" {
		return "missing account id"
	}
	if account.Status == AccountUnhealthy {
		return "auth unhealthy"
	}
	if !account.CooldownUntil.IsZero() && account.CooldownUntil.After(now) {
		return "cooldown"
	}
	if account.FiveHour.Utilization() > params.FiveHourMax {
		return "5h quota gated"
	}
	if account.RateLimit.Limit > 0 && account.RateLimit.Utilization() > params.FiveHourMax {
		return "rate limit gated"
	}
	if account.SevenDay.Utilization() > params.SevenDayMax {
		return "7d quota gated"
	}
	if !account.AllowStale && !account.ObservedAt.IsZero() && now.Sub(account.ObservedAt) > params.StaleAfter {
		return "usage stale"
	}
	return ""
}

func urgency(resetAt time.Time, now time.Time, max float64) float64 {
	if resetAt.IsZero() {
		return 1
	}
	if resetAt.Before(now) || resetAt.Equal(now) {
		return max
	}
	remaining := resetAt.Sub(now)
	if remaining >= sevenDayPeriod {
		return 1
	}
	ratio := 1 - (float64(remaining) / float64(sevenDayPeriod))
	value := 1 + ratio*(max-1)
	if value > max {
		return max
	}
	if value < 1 {
		return 1
	}
	return value
}
