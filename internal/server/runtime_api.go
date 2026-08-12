package server

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/runtimeplane"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type runtimeStatusResponse struct {
	GeneratedAt     time.Time                      `json:"generatedAt"`
	Active          runtimeActiveTarget            `json:"active"`
	RouterEnabled   bool                           `json:"routerEnabled"`
	Scheduler       runtimeSchedulerPolicy         `json:"scheduler"`
	Accounts        []runtimeAccountInfo           `json:"accounts"`
	Selection       runtimeplane.SelectionDecision `json:"selection"`
	Leases          []runtimeplane.AgentLease      `json:"leases,omitempty"`
	Providers       []runtimeProviderInfo          `json:"providers"`
	QuotaCollectors []runtimeQuotaCollectorInfo    `json:"quotaCollectors,omitempty"`
	QuotaSource     string                         `json:"quotaSource"`
}

type runtimeActiveTarget struct {
	Provider            string                     `json:"provider"`
	ProviderDisplayName string                     `json:"providerDisplayName,omitempty"`
	Model               string                     `json:"model"`
	ModelGroup          runtimeplane.ProviderGroup `json:"modelGroup"`
	ProviderGroup       runtimeplane.ProviderGroup `json:"providerGroup"`
	SelectionGroup      runtimeplane.ProviderGroup `json:"selectionGroup"`
}

type runtimeSchedulerPolicy struct {
	FiveHourMax       float64 `json:"fiveHourMax"`
	SevenDayMax       float64 `json:"sevenDayMax"`
	UrgencyMax        float64 `json:"urgencyMax"`
	SwitchMargin      float64 `json:"switchMargin"`
	StaleAfterSeconds int64   `json:"staleAfterSeconds"`
}

type runtimeProviderInfo struct {
	Name        string                     `json:"name"`
	DisplayName string                     `json:"displayName"`
	Group       runtimeplane.ProviderGroup `json:"group"`
	Models      []runtimeModelInfo         `json:"models"`
}

type runtimeAccountInfo struct {
	ID            string                     `json:"id"`
	DisplayName   string                     `json:"displayName,omitempty"`
	Provider      string                     `json:"provider"`
	Group         runtimeplane.ProviderGroup `json:"group"`
	Status        runtimeplane.AccountStatus `json:"status"`
	FiveHour      runtimeQuotaWindow         `json:"fiveHour"`
	SevenDay      runtimeQuotaWindow         `json:"sevenDay"`
	RateLimit     runtimeQuotaWindow         `json:"rateLimit,omitempty"`
	CooldownUntil *time.Time                 `json:"cooldownUntil,omitempty"`
	ObservedAt    *time.Time                 `json:"observedAt,omitempty"`
	AllowStale    bool                       `json:"allowStale,omitempty"`
}

type runtimeQuotaWindow struct {
	Used    float64    `json:"used"`
	Limit   float64    `json:"limit"`
	ResetAt *time.Time `json:"resetAt,omitempty"`
}

type runtimeQuotaCollectorInfo struct {
	Index           int      `json:"index"`
	Name            string   `json:"name,omitempty"`
	Type            string   `json:"type"`
	Path            string   `json:"path,omitempty"`
	URL             string   `json:"url,omitempty"`
	HeaderNames     []string `json:"headerNames,omitempty"`
	IntervalSeconds int      `json:"intervalSeconds,omitempty"`
	TimeoutSeconds  int      `json:"timeoutSeconds,omitempty"`
	Enabled         bool     `json:"enabled"`
}

type runtimeModelInfo struct {
	ID            string                     `json:"id"`
	DisplayName   string                     `json:"displayName"`
	Group         runtimeplane.ProviderGroup `json:"group"`
	ContextWindow int                        `json:"contextWindow,omitempty"`
	MaxOutput     int                        `json:"maxOutput,omitempty"`
}

func (s *Server) handleRuntimeStatus(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()

	s.mu.RLock()
	activeProvider := s.activeProvider
	activeModel := s.activeModel
	routerEnabled := s.router != nil
	telemetry := s.runtimeTelemetry
	leases := s.runtimeLeases
	quotaCollectorCount := 0
	if s.runtimeQuota != nil {
		quotaCollectorCount = s.runtimeQuota.EnabledSourceCount()
	}
	s.mu.RUnlock()

	activeProviderName := ""
	activeProviderDisplayName := ""
	if activeProvider != nil {
		activeProviderName = activeProvider.Name()
		activeProviderDisplayName = activeProvider.DisplayName()
	}

	modelGroup := runtimeplane.ClassifyModelGroup(activeModel)
	providerGroup := runtimeplane.ClassifyProviderGroup(activeProviderName)
	selectionGroup := modelGroup
	if selectionGroup == runtimeplane.GroupDefault {
		selectionGroup = providerGroup
	}

	cfg := config.Load()
	accounts := runtimeAccountsFromConfig(cfg, activeProviderName, activeProviderDisplayName, now)
	if telemetry != nil {
		accounts = telemetry.Merge(accounts, now)
	}
	currentAccountID := runtimeCurrentAccountID(accounts, activeProviderName)
	params := runtimeplane.SchedulerParams{}.Normalized()
	selection := runtimeplane.SelectAccount(accounts, currentAccountID, selectionGroup, now, params)

	writeJSON(w, runtimeStatusResponse{
		GeneratedAt: now,
		Active: runtimeActiveTarget{
			Provider:            activeProviderName,
			ProviderDisplayName: activeProviderDisplayName,
			Model:               activeModel,
			ModelGroup:          modelGroup,
			ProviderGroup:       providerGroup,
			SelectionGroup:      selectionGroup,
		},
		RouterEnabled:   routerEnabled,
		Scheduler:       runtimeSchedulerFromParams(params),
		Accounts:        runtimeAccountInfos(accounts),
		Selection:       selection,
		Leases:          runtimeLeaseSnapshot(leases, now),
		Providers:       runtimeKnownProviders(),
		QuotaCollectors: runtimeQuotaCollectorInfos(cfg.RuntimeQuotaSources),
		QuotaSource:     runtimeQuotaSource(telemetry, quotaCollectorCount),
	})
}

func runtimeLeaseSnapshot(leases *runtimeplane.LeaseStore, now time.Time) []runtimeplane.AgentLease {
	if leases == nil {
		return nil
	}
	return leases.Snapshot(now)
}

func runtimeQuotaSource(telemetry *runtimeplane.TelemetryStore, collectorCount int) string {
	collector := "subscription quota collector not connected"
	if collectorCount > 0 {
		collector = "subscription quota collector polling " + pluralizeSources(collectorCount)
	}
	if telemetry != nil && telemetry.HasData() {
		return "configured-accounts + live proxy telemetry + provider response headers; " + collector
	}
	return "configured-accounts; live proxy telemetry and provider response headers attached; " + collector
}

func pluralizeSources(count int) string {
	if count == 1 {
		return "1 source"
	}
	return fmt.Sprintf("%d sources", count)
}

func runtimeQuotaCollectorInfos(sources []config.RuntimeQuotaSource) []runtimeQuotaCollectorInfo {
	if len(sources) == 0 {
		return nil
	}
	infos := make([]runtimeQuotaCollectorInfo, 0, len(sources))
	for idx, source := range sources {
		infos = append(infos, runtimeQuotaCollectorInfo{
			Index:           idx,
			Name:            source.Name,
			Type:            source.Type,
			Path:            source.Path,
			URL:             source.URL,
			HeaderNames:     runtimeQuotaHeaderNames(source.Headers),
			IntervalSeconds: source.IntervalSeconds,
			TimeoutSeconds:  source.TimeoutSeconds,
			Enabled:         !source.Disabled,
		})
	}
	return infos
}

func runtimeSchedulerFromParams(params runtimeplane.SchedulerParams) runtimeSchedulerPolicy {
	params = params.Normalized()
	return runtimeSchedulerPolicy{
		FiveHourMax:       params.FiveHourMax,
		SevenDayMax:       params.SevenDayMax,
		UrgencyMax:        params.UrgencyMax,
		SwitchMargin:      params.SwitchMargin,
		StaleAfterSeconds: int64(params.StaleAfter.Seconds()),
	}
}

func runtimeAccountsFromConfig(cfg config.Config, activeProviderName, activeProviderDisplayName string, now time.Time) []runtimeplane.AccountState {
	seen := map[string]bool{}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	accounts := make([]runtimeplane.AccountState, 0, len(names)+1)
	for _, name := range names {
		settings := cfg.Providers[name]
		account := runtimeAccountFromProvider(name, runtimeProviderDisplayName(name, settings, activeProviderName, activeProviderDisplayName), now)
		accounts = append(accounts, account)
		seen[name] = true
	}

	if activeProviderName != "" && !seen[activeProviderName] {
		accounts = append(accounts, runtimeAccountFromProvider(activeProviderName, activeProviderDisplayName, now))
	}

	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Group != accounts[j].Group {
			return accounts[i].Group < accounts[j].Group
		}
		return accounts[i].ID < accounts[j].ID
	})
	return accounts
}

func runtimeAccountInfos(accounts []runtimeplane.AccountState) []runtimeAccountInfo {
	infos := make([]runtimeAccountInfo, 0, len(accounts))
	for _, account := range accounts {
		infos = append(infos, runtimeAccountInfo{
			ID:            account.ID,
			DisplayName:   account.DisplayName,
			Provider:      account.Provider,
			Group:         account.Group,
			Status:        account.Status,
			FiveHour:      runtimeQuotaWindowInfo(account.FiveHour),
			SevenDay:      runtimeQuotaWindowInfo(account.SevenDay),
			RateLimit:     runtimeQuotaWindowInfo(account.RateLimit),
			CooldownUntil: optionalRuntimeTime(account.CooldownUntil),
			ObservedAt:    optionalRuntimeTime(account.ObservedAt),
			AllowStale:    account.AllowStale,
		})
	}
	return infos
}

func runtimeQuotaWindowInfo(window runtimeplane.QuotaWindow) runtimeQuotaWindow {
	return runtimeQuotaWindow{
		Used:    window.Used,
		Limit:   window.Limit,
		ResetAt: optionalRuntimeTime(window.ResetAt),
	}
}

func optionalRuntimeTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func runtimeAccountFromProvider(providerName, displayName string, now time.Time) runtimeplane.AccountState {
	return runtimeplane.AccountState{
		ID:          "provider:" + providerName,
		DisplayName: displayName,
		Provider:    providerName,
		Group:       runtimeplane.ClassifyProviderGroup(providerName),
		Status:      runtimeplane.AccountHealthy,
		ObservedAt:  now,
		AllowStale:  true,
	}
}

func runtimeProviderDisplayName(name string, settings config.ProviderSettings, activeName, activeDisplay string) string {
	if name == activeName && activeDisplay != "" {
		return activeDisplay
	}
	p, err := providers.Create(name, &types.ProviderConfig{APIKey: settings.APIKey, BaseURL: settings.BaseURL})
	if err != nil {
		return name
	}
	return p.DisplayName()
}

func runtimeCurrentAccountID(accounts []runtimeplane.AccountState, providerName string) string {
	if providerName == "" {
		return ""
	}
	want := "provider:" + providerName
	for _, account := range accounts {
		if account.ID == want {
			return want
		}
	}
	return ""
}

func runtimeKnownProviders() []runtimeProviderInfo {
	infos := make([]runtimeProviderInfo, 0, len(providers.ProviderOrder))
	seen := map[string]bool{}
	for _, name := range providers.ProviderOrder {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		p, err := providers.Create(name, nil)
		if err != nil {
			continue
		}
		models := p.Models()
		outModels := make([]runtimeModelInfo, 0, len(models))
		for _, model := range models {
			outModels = append(outModels, runtimeModelFromProvider(model))
		}
		infos = append(infos, runtimeProviderInfo{
			Name:        name,
			DisplayName: runtimeProviderInventoryDisplayName(name, p),
			Group:       runtimeplane.ClassifyProviderGroup(name),
			Models:      outModels,
		})
	}
	return infos
}

func runtimeProviderInventoryDisplayName(name string, provider types.Provider) string {
	displayName := provider.DisplayName()
	if provider.Name() != "" && provider.Name() != name {
		return name + " (" + displayName + ")"
	}
	return displayName
}

func runtimeModelFromProvider(model types.ModelInfo) runtimeModelInfo {
	return runtimeModelInfo{
		ID:            model.ID,
		DisplayName:   model.DisplayName,
		Group:         runtimeplane.ClassifyModelGroup(model.ID),
		ContextWindow: model.ContextWindow,
		MaxOutput:     model.MaxOutput,
	}
}
