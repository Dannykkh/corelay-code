package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// MultiModelResult holds the response from one model.
type MultiModelResult struct {
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Response string        `json:"response"`
	Latency  time.Duration `json:"latencyMs"`
	Tokens   int           `json:"tokens"`
	Error    string        `json:"error,omitempty"`
}

// MultiModelQuery sends the same prompt to multiple models simultaneously.
func MultiModelQuery(
	ctx context.Context,
	prompt string,
	targets []struct{ Provider, Model string },
) []MultiModelResult {
	var wg sync.WaitGroup
	results := make([]MultiModelResult, len(targets))

	for i, target := range targets {
		wg.Add(1)
		go func(idx int, prov, model string) {
			defer wg.Done()

			result := MultiModelResult{Provider: prov, Model: model}
			start := time.Now()

			provider, err := providers.Create(prov, &types.ProviderConfig{})
			if err != nil {
				result.Error = err.Error()
				results[idx] = result
				return
			}

			userContent, _ := json.Marshal(prompt)
			req := &types.MessagesRequest{
				Model: model,
				Messages: []types.Message{
					{Role: "user", Content: userContent},
				},
				MaxTokens: 2000,
			}

			ch, err := provider.StreamMessage(ctx, req, nil)
			if err != nil {
				result.Error = err.Error()
				result.Latency = time.Since(start)
				results[idx] = result
				return
			}

			var response string
			for event := range ch {
				if event.Type == "content_block_delta" && event.Delta != nil {
					var delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					json.Unmarshal(event.Delta, &delta)
					if delta.Type == "text_delta" {
						response += delta.Text
					}
				}
				if event.Usage != nil {
					result.Tokens = event.Usage.OutputTokens
				}
				if event.Type == "message_stop" {
					break
				}
			}

			result.Response = response
			result.Latency = time.Since(start)
			results[idx] = result
		}(i, target.Provider, target.Model)
	}

	wg.Wait()
	return results
}
