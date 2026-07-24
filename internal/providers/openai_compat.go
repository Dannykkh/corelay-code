package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aniclew/aniclew/internal/stream"
	"github.com/aniclew/aniclew/internal/translate"
	"github.com/aniclew/aniclew/internal/types"
)

// OpenAICompat is the base for all OpenAI-compatible providers.
type OpenAICompat struct {
	ProviderName string
	ProviderDisp string
	ModelList    []types.ModelInfo
	BaseURL      string
	AuthHeader   func() (string, string) // returns (headerName, headerValue)
}

func (p *OpenAICompat) Name() string              { return p.ProviderName }
func (p *OpenAICompat) DisplayName() string       { return p.ProviderDisp }
func (p *OpenAICompat) Models() []types.ModelInfo { return p.ModelList }
func (p *OpenAICompat) Validate() error           { return nil }

func (p *OpenAICompat) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	oaiReq := translate.ToOpenAI(req, req.Model)

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.BaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Auth: try configured key first, then passthrough incoming header
	authed := false
	if p.AuthHeader != nil {
		k, v := p.AuthHeader()
		if k != "" && v != "" && v != "Bearer " {
			httpReq.Header.Set(k, v)
			authed = true
		}
	}
	if !authed && opts != nil && opts.IncomingHeaders != nil {
		if v := opts.IncomingHeaders["authorization"]; v != "" {
			httpReq.Header.Set("Authorization", v)
		}
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s connection failed: %w", p.ProviderName, err)
	}
	opts.ObserveResponse(resp.StatusCode, resp.Header)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s API error %d", p.ProviderName, resp.StatusCode)
	}

	ch := make(chan types.SSEEvent, 64)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		translator := translate.NewTranslator(req.Model)
		if !sendSSEEvent(ctx, ch, translator.Start()) {
			return
		}

		chunks := make(chan types.OAIStreamChunk, 64)
		go stream.ReadOpenAISSE(ctx, resp.Body, chunks)

		finished := false
		for chunk := range chunks {
			for _, event := range translator.Translate(chunk) {
				if !sendSSEEvent(ctx, ch, event) {
					return
				}
				if event.Type == "message_stop" {
					finished = true
				}
			}
		}
		if !finished {
			for _, event := range translator.End() {
				if !sendSSEEvent(ctx, ch, event) {
					return
				}
			}
		}
	}()

	return ch, nil
}
