package types

import (
	"context"
	"net/http"
)

type ModelInfo struct {
	ID            string               `json:"id"`
	DisplayName   string               `json:"displayName"`
	ContextWindow int                  `json:"contextWindow,omitempty"`
	MaxOutput     int                  `json:"maxOutput,omitempty"`
	ImageInput    ImageInputCapability `json:"imageInput,omitempty"`
}

// ImageInputCapability describes whether this Corelay provider/model path can
// pass canonical image blocks through to the model. Empty is Unknown; the
// transport alone being able to encode images is not enough to claim support.
type ImageInputCapability string

const (
	ImageInputUnknown     ImageInputCapability = ""
	ImageInputUnsupported ImageInputCapability = "unsupported"
	ImageInputSupported   ImageInputCapability = "supported"
)

type ProviderConfig struct {
	APIKey  string `json:"apiKey,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
}

type StreamOptions struct {
	IncomingHeaders map[string]string
	OnResponse      func(ProviderResponse)
}

type ProviderResponse struct {
	StatusCode int
	Header     http.Header
}

func (opts *StreamOptions) ObserveResponse(statusCode int, header http.Header) {
	if opts == nil || opts.OnResponse == nil {
		return
	}
	opts.OnResponse(ProviderResponse{
		StatusCode: statusCode,
		Header:     header.Clone(),
	})
}

type Provider interface {
	Name() string
	DisplayName() string
	Models() []ModelInfo
	Validate() error
	StreamMessage(ctx context.Context, req *MessagesRequest, opts *StreamOptions) (<-chan SSEEvent, error)
}
