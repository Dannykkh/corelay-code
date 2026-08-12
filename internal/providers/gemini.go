package providers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type GeminiProvider struct {
	apiKey   string
	baseURL  string
	httpDoer HTTPDoer
}

func NewGemini(cfg *types.ProviderConfig) types.Provider {
	return NewGeminiWithOptions(cfg, CreateOptions{})
}

func NewGeminiWithOptions(cfg *types.ProviderConfig, opts CreateOptions) types.Provider {
	if cfg == nil {
		cfg = &types.ProviderConfig{}
	}
	return &GeminiProvider{
		apiKey:   coalesce(cfg.APIKey, os.Getenv("GEMINI_API_KEY")),
		baseURL:  coalesce(cfg.BaseURL, "https://generativelanguage.googleapis.com"),
		httpDoer: httpDoerOrDefault(opts.HTTPDoer),
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
	geminiReq, err := buildGeminiRequest(req)
	if err != nil {
		return nil, err
	}
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

	url := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", p.baseURL, req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini request construction failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		// Official Gemini REST authentication uses x-goog-api-key. Keeping the
		// credential out of the URL prevents proxy/error logs from capturing it.
		httpReq.Header.Set("x-goog-api-key", apiKey)
	}

	resp, err := httpDoerOrDefault(p.httpDoer).Do(httpReq)
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
	sawToolCall := false

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
					ID   string          `json:"id"`
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
				callID := fnPart.FunctionCall.ID
				if callID == "" {
					callID = newGeminiToolCallID()
				}
				args := fnPart.FunctionCall.Args
				if len(args) == 0 || string(args) == "null" {
					args = json.RawMessage(`{}`)
				}
				if !sendSSEEvent(ctx, ch, types.SSEEvent{
					Type: "content_block_start", Index: &idx,
					ContentBlock: mustJSON(map[string]any{
						"type": "tool_use", "id": callID, "name": fnPart.FunctionCall.Name, "input": map[string]any{},
					}),
				}) {
					return
				}
				if !sendSSEEvent(ctx, ch, types.SSEEvent{
					Type: "content_block_delta", Index: &idx,
					Delta: mustJSON(map[string]string{"type": "input_json_delta", "partial_json": string(args)}),
				}) {
					return
				}
				if !sendSSEEvent(ctx, ch, types.SSEEvent{Type: "content_block_stop", Index: &idx}) {
					return
				}
				blockIndex++
				stopReason = "tool_use"
				sawToolCall = true
			}
		}

		if cand.FinishReason == "STOP" && !sawToolCall {
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

// Gemini history shape follows the official GenerateContent Part contract.
// In particular, a prior model functionCall must be paired with a later user
// functionResponse rather than flattened into text.
// https://ai.google.dev/api/generate-content
// https://ai.google.dev/gemini-api/docs/function-calling
func buildGeminiRequest(req *types.MessagesRequest) (map[string]any, error) {
	result := map[string]any{}

	// System
	if len(req.System) > 0 {
		sysMsg := translate.SystemToOAI(req.System)
		if sysMsg != nil {
			var text string
			if err := json.Unmarshal(sysMsg.Content, &text); err != nil {
				return nil, fmt.Errorf("gemini system instruction: %w", err)
			}
			result["systemInstruction"] = map[string]any{
				"parts": []map[string]string{{"text": text}},
			}
		}
	}

	// Build an ID-to-function map while walking history so function_result
	// blocks can retain both the call ID and Gemini's required function name.
	var contents []map[string]any
	callNames := make(map[string]string)
	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}

		parts, err := geminiPartsForMessage(msg, callNames)
		if err != nil {
			return nil, err
		}
		if len(parts) > 0 {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
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

	return result, nil
}

func geminiPartsForMessage(msg types.Message, callNames map[string]string) ([]map[string]any, error) {
	var text string
	if err := json.Unmarshal(msg.Content, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []map[string]any{{"text": text}}, nil
	}

	var blocks []types.ContentBlockParam
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("gemini message content: %w", err)
	}
	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, map[string]any{"text": block.Text})
			}
		case "tool_use":
			if msg.Role != "assistant" || block.ID == "" || block.Name == "" {
				return nil, fmt.Errorf("gemini function call requires assistant role, id, and name")
			}
			if _, duplicate := callNames[block.ID]; duplicate {
				return nil, fmt.Errorf("gemini duplicate function call id")
			}
			var args map[string]any
			if err := json.Unmarshal(block.Input, &args); err != nil || args == nil {
				return nil, fmt.Errorf("gemini function call arguments must be a JSON object")
			}
			callNames[block.ID] = block.Name
			parts = append(parts, map[string]any{"functionCall": map[string]any{"id": block.ID, "name": block.Name, "args": args}})
		case "tool_result":
			if msg.Role != "user" || block.ToolUseID == "" {
				return nil, fmt.Errorf("gemini function response requires user role and tool_use_id")
			}
			name, ok := callNames[block.ToolUseID]
			if !ok {
				return nil, fmt.Errorf("gemini function response references an unknown call id")
			}
			response, err := geminiFunctionResponse(block.Content, block.IsError)
			if err != nil {
				return nil, err
			}
			parts = append(parts, map[string]any{"functionResponse": map[string]any{"id": block.ToolUseID, "name": name, "response": response}})
		default:
			return nil, fmt.Errorf("gemini unsupported history block type %q", block.Type)
		}
	}
	return parts, nil
}

func geminiFunctionResponse(raw json.RawMessage, isError *bool) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("gemini function response content is required")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("gemini function response content: %w", err)
	}
	key := "output"
	if isError != nil && *isError {
		key = "error"
	}
	if object, ok := value.(map[string]any); ok && key == "output" {
		return object, nil
	}
	return map[string]any{key: value}, nil
}

func newGeminiToolCallID() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		counter := geminiFallbackID.Add(1)
		return fmt.Sprintf("toolu_gemini_%d_%d", time.Now().UTC().UnixNano(), counter)
	}
	return "toolu_gemini_" + base64.RawURLEncoding.EncodeToString(b)
}

var geminiFallbackID atomic.Uint64

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
