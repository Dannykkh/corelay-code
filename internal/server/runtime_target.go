package server

import (
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/runtimeplane"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func resolveDirectRuntimeProvider(activeProvider types.Provider, activeModel, requestedModel string, telemetry *runtimeplane.TelemetryStore, leases *runtimeplane.LeaseStore, sessionID string) (types.Provider, string, runtimeplane.RuntimeTarget, runtimeplane.SelectionDecision, error) {
	activeProviderName := ""
	activeProviderDisplayName := ""
	if activeProvider != nil {
		activeProviderName = activeProvider.Name()
		activeProviderDisplayName = activeProvider.DisplayName()
	}

	cfg := config.Load()
	target := runtimeplane.ResolveModelTarget(requestedModel, activeProviderName, activeModel)
	providerName, selection := selectDirectRuntimeProviderName(cfg, target, activeProviderName, activeProviderDisplayName, time.Now(), telemetry, leases, sessionID)
	if providerName != "" {
		target.Provider = providerName
	}
	if target.Provider == "" || (activeProvider != nil && target.Provider == activeProvider.Name()) {
		return activeProvider, target.Model, target, selection, nil
	}

	provider, err := createRuntimeProvider(cfg, target.Provider)
	if err != nil {
		return nil, target.Model, target, selection, err
	}
	return provider, target.Model, target, selection, nil
}

func explicitModelOverridesRouter(requestedModel string) bool {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return false
	}
	return runtimeplane.ClassifyModelGroup(requestedModel) != runtimeplane.GroupDefault
}

func selectDirectRuntimeProviderName(cfg config.Config, target runtimeplane.RuntimeTarget, activeProviderName, activeProviderDisplayName string, now time.Time, telemetry *runtimeplane.TelemetryStore, leases *runtimeplane.LeaseStore, sessionID string) (string, runtimeplane.SelectionDecision) {
	accounts := runtimeAccountsFromConfig(cfg, activeProviderName, activeProviderDisplayName, now)
	if telemetry != nil {
		accounts = telemetry.Merge(accounts, now)
	}
	if target.Group == "" || target.Group == runtimeplane.GroupDefault {
		return target.Provider, runtimeplane.SelectionDecision{
			Action:    runtimeplane.DecisionStay,
			AccountID: runtimeCurrentAccountID(accounts, target.Provider),
			Reason:    "default group stays on resolved provider",
		}
	}

	currentAccountID := runtimeCurrentAccountID(accounts, target.Provider)
	if lease, ok := leases.Current(sessionID, target.Group, now); ok {
		currentAccountID = lease.AccountID
		if !lease.NeedsReeval(now, runtimeplane.DefaultLeaseReevalInterval) {
			// Between scheduler passes a bound session stays on its leased
			// account, as long as that account still clears every gate.
			if account, found := runtimeAccountByID(accounts, lease.AccountID); found && runtimeplane.Eligible(account, now, runtimeplane.SchedulerParams{}) {
				if leasedProvider := strings.TrimPrefix(lease.AccountID, "provider:"); leasedProvider != "" {
					return leasedProvider, runtimeplane.SelectionDecision{
						Action:    runtimeplane.DecisionStay,
						AccountID: lease.AccountID,
						Reason:    "leased account inside re-eval interval",
					}
				}
			}
		}
	}
	selection := runtimeplane.SelectAccount(accounts, currentAccountID, target.Group, now, runtimeplane.SchedulerParams{})
	if selection.AccountID == "" {
		return target.Provider, selection
	}
	if account, ok := runtimeAccountByID(accounts, selection.AccountID); ok {
		leases.Upsert(sessionID, "", account, target.Model, now, runtimeplane.DefaultLeaseTTL)
	}
	selectedProvider := strings.TrimPrefix(selection.AccountID, "provider:")
	if selectedProvider == "" {
		return target.Provider, selection
	}
	return selectedProvider, selection
}

func createRuntimeProvider(cfg config.Config, name string) (types.Provider, error) {
	settings := cfg.Providers[strings.TrimSpace(name)]
	return providers.Create(name, &types.ProviderConfig{
		APIKey:  settings.APIKey,
		BaseURL: settings.BaseURL,
	})
}

func runtimeSessionIDFromHeaders(headers map[string]string) string {
	return strings.TrimSpace(headers["x-claude-code-session-id"])
}

func runtimeAccountByID(accounts []runtimeplane.AccountState, accountID string) (runtimeplane.AccountState, bool) {
	for _, account := range accounts {
		if account.ID == accountID {
			return account, true
		}
	}
	return runtimeplane.AccountState{}, false
}
