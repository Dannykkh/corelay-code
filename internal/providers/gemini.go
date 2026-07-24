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

	"github.com/aniclew/aniclew/internal/translate"
	"github.com/aniclew/aniclew/internal/types"
)

type GeminiProvider struct {
	apiKey  string
	baseURL string
}

func NewGemini(cfg *types.ProviderConfig) types.Provider {
	return &GeminiProvider{
		apiKey:  coalesce(cfg.APIKey, os.Getenv("GEMINI_API_KEY")),
		baseURL: coalesce(cfg.BaseURL, "https://generativelanguage.googleapis.com"),
	}
}

func (p *GeminiProvider) Name() string        { return "gemini" }
func (p *GeminiProvider) DisplayName() string { return "Google Gemini" }
func (p *GeminiProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{
		{ID: "gemini-3-pro-preview", DisplayName: "Gemini 3 Pro (최신 플래그십)", ContextWindow: 1048576},
		{ID: "gemini-3-flash-preview", DisplayName: "Gemini 3 Flash (최신 빠름)", ContextWindow: 1048576},
		{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", ContextWindow: 1048576},
		{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", ContextWindow: 1048576},
		{ID: "gemini-2.5-flash-lite", DisplayName: "Gemini 2.5 Flash Lite (최저가)", ContextWindow: 1048576},
	}
}
func (p *GeminiProvider) Validate() error {
	if p.apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is required")
	}
	return nil
}

func (p *GeminiProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	geminiReq := buildGeminiRequest(req)
	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, err
	}

	// Auth: configured key first, then passthrough incoming header
	apiKey := p.apiKey
	if apiKey == "" && opts != nil && opts.IncomingHeaders != nil {
		if v := opts.IncomingHeaders["x-goog-api-key"]; v != "" {
			apiKey = v
		} else if v := opts.IncomingHeaders["authorization"]; v != "" {
			apiKey = strings.TrimPrefix(v, "Bearer ")
		}
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", p.baseURL, req.Model, apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini connection failed: %w", err)
	}
	opts.ObserveResponse(resp.StatusCode, resp.Header)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("gemini API error %d", resp.StatusCode)
	}

	ch := make(chan types.SSEEvent, 64)
	go p.translateStream(ctx, resp, req.Model, ch)
	return ch, nil
}

func (p *GeminiProvider) translateStream(ctx context.Context, resp *http.Response, model string, ch chan<- types.SSEEvent) {
	defer resp.Body.Close()
	defer close(ch)

	msgID := translate.NewTranslator(model) // just for the ID
	_ = msgID

	if !sendSSEEvent(ctx, ch, types.SSEEvent{
		Type: "message_start",
		Message: &types.SSEMessage{
			ID: "msg_gemini", Type: "message", Role: "assistant", Model: model,
			Content: json.RawMessage(`[]`),
			Usage:   &types.SSEUsage{},
		},
	}) {
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	blockIndex := 0
	textOpen := false
	outputTokens := 0
	stopReason := "end_turn"

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []json.RawMessage `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}

		if chunk.UsageMetadata != nil {
			outputTokens = chunk.UsageMetadata.CandidatesTokenCount
		}

		if len(chunk.Candidates) == 0 {
			continue
		}
		cand := chunk.Candidates[0]

		for _, partRaw := range cand.Content.Parts {
			var textPart struct {
				Text string `json:"text"`
			}
			var fnPart struct {
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			}

			if json.Unmarshal(partRaw, &textPart) == nil && textPart.Text != "" {
				if !textOpen {
					idx := blockIndex
					if !sendSSEEvent(ctx, ch, types.SSEEvent{
						Type: "content_block_start", Index: &idx,
						ContentBlock: mustJSON(map[string]string{"type": "text", "text": ""}),
					}) {
						return
					}
					textOpen = true
				}
				idx := blockIndex
				if !sendSSEEvent(ctx, ch, types.SSEEvent{
					Type: "content_block_delta", Index: &idx,
					Delta: mustJSON(map[string]string{"type": "text_delta", "text": textPart.Text}),
				}) {
					return
				}
			} else if json.Unmarshal(partRaw, &fnPart) == nil && fnPart.FunctionCall != nil {
				if textOpen {
					idx := blockIndex
					if !sendSSEEvent(ctx, ch, types.SSEEvent{Type: "content_block_stop", Index: &idx}) {
						return
					}
					blockIndex++
					textOpen = false
				}
				idx := blockIndex
				if !sendSSEEvent(ctx, ch, types.SSEEvent{
					Type: "content_block_start", Index: &idx,
					ContentBlock: mustJSON(map[string]any{
						"type": "tool_use", "id": "toolu_gemini", "name": fnPart.FunctionCall.Name, "input": "",
					}),
				}) {
					return
				}
				if !sendSSEEvent(ctx, ch, types.SSEEvent{
					Type: "content_block_delta", Index: &idx,
					Delta: mustJSON(map[string]string{"type": "input_json_delta", "partial_json": string(fnPart.FunctionCall.Args)}),
				}) {
					return
				}
				if !sendSSEEvent(ctx, ch, types.SSEEvent{Type: "content_block_stop", Index: &idx}) {
					return
				}
				blockIndex++
				stopReason = "tool_use"
			}
		}

		if cand.FinishReason == "STOP" {
			stopReason = "end_turn"
		}
	}

	if textOpen {
		idx := blockIndex
		if !sendSSEEvent(ctx, ch, types.SSEEvent{Type: "content_block_stop", Index: &idx}) {
			return
		}
	}

	if !sendSSEEvent(ctx, ch, types.SSEEvent{
		Type:  "message_delta",
		Delta: mustJSON(map[string]any{"stop_reason": stopReason, "stop_sequence": nil}),
		Usage: &types.SSEUsage{OutputTokens: outputTokens},
	}) {
		return
	}
	sendSSEEvent(ctx, ch, types.SSEEvent{Type: "message_stop"})
}

func buildGeminiRequest(req *types.MessagesRequest) map[string]any {
	result := map[string]any{}

	// System
	if len(req.System) > 0 {
		sysMsg := translate.SystemToOAI(req.System)
		if sysMsg != nil {
			var text string
			json.Unmarshal(sysMsg.Content, &text)
			result["systemInstruction"] = map[string]any{
				"parts": []map[string]string{{"text": text}},
			}
		}
	}

	// Contents (simplified — text only for now)
	var contents []map[string]any
	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}

		var text string
		json.Unmarshal(msg.Content, &text)
		if text == "" {
			// Try blocks
			var blocks []struct{ Type, Text string }
			json.Unmarshal(msg.Content, &blocks)
			for _, b := range blocks {
				if b.Text != "" {
					text += b.Text
				}
			}
		}
		if text != "" {
			contents = append(contents, map[string]any{
				"role": role, "parts": []map[string]string{{"text": text}},
			})
		}
	}
	result["contents"] = contents

	// Tools
	if len(req.Tools) > 0 {
		var decls []map[string]any
		for _, t := range req.Tools {
			decls = append(decls, map[string]any{
				"name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.InputSchema),
			})
		}
		result["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}

	// Generation config
	genCfg := map[string]any{"maxOutputTokens": req.MaxTokens}
	if req.Temperature != nil {
		genCfg["temperature"] = *req.Temperature
	}
	result["generationConfig"] = genCfg

	return result
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
