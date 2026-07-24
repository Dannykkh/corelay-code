package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aniclew/aniclew/internal/types"
)

type AnthropicProvider struct {
	apiKey  string
	baseURL string
}

func NewAnthropic(cfg *types.ProviderConfig) types.Provider {
	return &AnthropicProvider{
		apiKey:  coalesce(cfg.APIKey, os.Getenv("ANTHROPIC_API_KEY")),
		baseURL: coalesce(cfg.BaseURL, "https://api.anthropic.com"),
	}
}

func (p *AnthropicProvider) Name() string        { return "anthropic" }
func (p *AnthropicProvider) DisplayName() string { return "Anthropic (passthrough)" }
func (p *AnthropicProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{
		{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8 (최신 최상급)", ContextWindow: 1000000},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6 (균형)", ContextWindow: 1000000},
		{ID: "claude-haiku-4-5-20251001", DisplayName: "Claude Haiku 4.5 (빠름/저가)", ContextWindow: 200000},
		{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7 (legacy)", ContextWindow: 1000000},
		{ID: "claude-opus-4-6-20250205", DisplayName: "Claude Opus 4.6 (legacy)", ContextWindow: 1000000},
		{ID: "claude-sonnet-4-6-20250217", DisplayName: "Claude Sonnet 4.6 dated (legacy)", ContextWindow: 1000000},
		{ID: "claude-opus-4-20250514", DisplayName: "Claude Opus 4 (legacy)", ContextWindow: 200000},
		{ID: "claude-sonnet-4-20250514", DisplayName: "Claude Sonnet 4 (legacy)", ContextWindow: 200000},
	}
}
func (p *AnthropicProvider) Validate() error { return nil }

func (p *AnthropicProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	// Build request body — set stream=true
	reqCopy := *req
	t := true
	reqCopy.Stream = &t
	betas := reqCopy.Betas
	reqCopy.Betas = nil // betas go in header

	body, err := json.Marshal(reqCopy)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	// ── Auth passthrough: forward original client headers ──
	incoming := map[string]string{}
	if opts != nil && opts.IncomingHeaders != nil {
		incoming = opts.IncomingHeaders
	}

	if v := incoming["authorization"]; v != "" {
		httpReq.Header.Set("Authorization", v)
	} else if v := incoming["x-api-key"]; v != "" {
		httpReq.Header.Set("x-api-key", v)
	} else if p.apiKey != "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}

	// Forward useful headers
	for _, h := range []string{"anthropic-beta", "x-app", "user-agent", "x-claude-code-session-id"} {
		if v := incoming[h]; v != "" {
			httpReq.Header.Set(h, v)
		}
	}
	if httpReq.Header.Get("anthropic-beta") == "" && len(betas) > 0 {
		httpReq.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic connection failed: %w", err)
	}
	opts.ObserveResponse(resp.StatusCode, resp.Header)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic API error %d", resp.StatusCode)
	}

	ch := make(chan types.SSEEvent, 64)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "event:") {
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				var event types.SSEEvent
				if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
					continue
				}
				if !sendSSEEvent(ctx, ch, event) {
					return
				}
				if event.Type == "message_stop" {
					return
				}
			}
		}
	}()

	return ch, nil
}
