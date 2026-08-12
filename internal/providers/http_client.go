package providers

import "net/http"

// HTTPDoer is the narrow HTTP dependency used by provider implementations.
// Supplying one through CreateWithOptions scopes the override to that provider
// instance; it never mutates http.DefaultClient or other process-wide state.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// CreateOptions contains instance-scoped provider dependencies.
type CreateOptions struct {
	HTTPDoer HTTPDoer
}

func httpDoerOrDefault(doer HTTPDoer) HTTPDoer {
	if doer != nil {
		return doer
	}
	return http.DefaultClient
}
