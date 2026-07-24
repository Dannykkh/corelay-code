package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/aniclew/aniclew/internal/runtimeplane"
)

type runtimeTelemetryUpdate struct {
	Provider      string                     `json:"provider"`
	DisplayName   string                     `json:"displayName,omitempty"`
	Group         runtimeplane.ProviderGroup `json:"group,omitempty"`
	Status        runtimeplane.AccountStatus `json:"status,omitempty"`
	FiveHour      runtimeQuotaWindow         `json:"fiveHour,omitempty"`
	SevenDay      runtimeQuotaWindow         `json:"sevenDay,omitempty"`
	RateLimit     runtimeQuotaWindow         `json:"rateLimit,omitempty"`
	CooldownUntil *time.Time                 `json:"cooldownUntil,omitempty"`
	ObservedAt    *time.Time                 `json:"observedAt,omitempty"`
	AllowStale    bool                       `json:"allowStale,omitempty"`
}

func (s *Server) handleRuntimeTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var update runtimeTelemetryUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if update.Provider == "" {
		writeError(w, http.StatusBadRequest, "provider required")
		return
	}

	now := time.Now().UTC()
	observedAt := now
	if update.ObservedAt != nil {
		observedAt = *update.ObservedAt
	}
	group := update.Group
	if group == "" {
		group = runtimeplane.ClassifyProviderGroup(update.Provider)
	}
	status := update.Status
	if status == "" {
		status = runtimeplane.AccountHealthy
	}

	account := runtimeplane.AccountState{
		ID:          "provider:" + update.Provider,
		DisplayName: update.DisplayName,
		Provider:    update.Provider,
		Group:       group,
		Status:      status,
		FiveHour:    quotaWindowFromRuntime(update.FiveHour),
		SevenDay:    quotaWindowFromRuntime(update.SevenDay),
		RateLimit:   quotaWindowFromRuntime(update.RateLimit),
		ObservedAt:  observedAt,
		AllowStale:  update.AllowStale,
	}
	if update.CooldownUntil != nil {
		account.CooldownUntil = *update.CooldownUntil
	}

	s.mu.RLock()
	telemetry := s.runtimeTelemetry
	s.mu.RUnlock()
	if telemetry == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime telemetry unavailable")
		return
	}
	telemetry.RecordAccountSnapshot(account, now)
	writeJSON(w, map[string]any{
		"ok":        true,
		"provider":  update.Provider,
		"accountId": account.ID,
	})
}

func quotaWindowFromRuntime(window runtimeQuotaWindow) runtimeplane.QuotaWindow {
	out := runtimeplane.QuotaWindow{
		Used:  window.Used,
		Limit: window.Limit,
	}
	if window.ResetAt != nil {
		out.ResetAt = *window.ResetAt
	}
	return out
}
