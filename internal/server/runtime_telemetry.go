package server

import (
	"errors"
	"strconv"
	"time"

	"github.com/Dannykkh/corelay-code/internal/runtimeplane"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func runtimeTargetForProviderModel(providerName, model string) runtimeplane.RuntimeTarget {
	group := runtimeplane.ClassifyModelGroup(model)
	if group == runtimeplane.GroupDefault {
		group = runtimeplane.ClassifyProviderGroup(providerName)
	}
	return runtimeplane.RuntimeTarget{
		Provider: providerName,
		Model:    model,
		Group:    group,
	}
}

func recordRuntimeFailure(store *runtimeplane.TelemetryStore, provider types.Provider, target runtimeplane.RuntimeTarget, err error) {
	if store == nil || provider == nil || err == nil {
		return
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 {
		return
	}
	group := target.Group
	if group == "" {
		group = runtimeTargetForProviderModel(provider.Name(), target.Model).Group
	}
	store.RecordFailure(provider.Name(), group, statusCode, nowUTC())
}

func recordRuntimeResponse(store *runtimeplane.TelemetryStore, provider types.Provider, target runtimeplane.RuntimeTarget, response types.ProviderResponse) {
	if store == nil || provider == nil {
		return
	}
	group := target.Group
	if group == "" {
		group = runtimeTargetForProviderModel(provider.Name(), target.Model).Group
	}
	store.RecordResponse(provider.Name(), group, response.StatusCode, response.Header, nowUTC())
}

func recordRuntimeSuccess(store *runtimeplane.TelemetryStore, provider types.Provider, target runtimeplane.RuntimeTarget, inputTokens, outputTokens int) {
	if store == nil || provider == nil {
		return
	}
	group := target.Group
	if group == "" {
		group = runtimeTargetForProviderModel(provider.Name(), target.Model).Group
	}
	store.RecordSuccess(provider.Name(), group, nowUTC())
	store.RecordUsage(provider.Name(), group, inputTokens, outputTokens, nowUTC())
}

var nowUTC = func() time.Time {
	return time.Now().UTC()
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	message := err.Error()
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, inner := range joined.Unwrap() {
			if code := statusCodeFromError(inner); code != 0 {
				return code
			}
		}
	}
	for i := 0; i < len(message); i++ {
		if message[i] < '0' || message[i] > '9' {
			continue
		}
		j := i
		for j < len(message) && message[j] >= '0' && message[j] <= '9' {
			j++
		}
		if j-i == 3 {
			code, err := strconv.Atoi(message[i:j])
			if err == nil && code >= 100 && code <= 599 {
				return code
			}
		}
		i = j
	}
	return 0
}
