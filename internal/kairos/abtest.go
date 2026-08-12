package kairos

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ABTestConfig configures model A/B testing.
type ABTestConfig struct {
	Enabled   bool   `json:"enabled"`
	ModelA    string `json:"modelA"`
	ProviderA string `json:"providerA"`
	ModelB    string `json:"modelB"`
	ProviderB string `json:"providerB"`
}

// ABTestResult records the result of one A/B test.
type ABTestResult struct {
	ID        string    `json:"id"`
	Prompt    string    `json:"prompt"`
	ModelA    string    `json:"modelA"`
	ModelB    string    `json:"modelB"`
	ResponseA string    `json:"responseA"`
	ResponseB string    `json:"responseB"`
	LatencyA  int64     `json:"latencyA_ms"`
	LatencyB  int64     `json:"latencyB_ms"`
	TokensA   int       `json:"tokensA"`
	TokensB   int       `json:"tokensB"`
	Winner    string    `json:"winner"` // "A", "B", or "tie"
	CreatedAt time.Time `json:"createdAt"`
}

// ABTester runs parallel model comparisons.
type ABTester struct {
	mu       sync.RWMutex
	config   ABTestConfig
	results  []ABTestResult
}

func NewABTester(cfg ABTestConfig) *ABTester {
	return &ABTester{
		config:  cfg,
		results: make([]ABTestResult, 0),
	}
}

// RunTest sends the same prompt to both models and compares results.
func (t *ABTester) RunTest(
	ctx context.Context,
	prompt string,
	providerA types.Provider,
	modelA string,
	providerB types.Provider,
	modelB string,
) *ABTestResult {
	log.Printf("[A/B Test] %s vs %s", modelA, modelB)

	req := &types.MessagesRequest{
		Messages: []types.Message{
			{Role: "user", Content: mustJSON(prompt)},
		},
		MaxTokens: 2048,
	}

	var wg sync.WaitGroup
	var responseA, responseB string
	var latencyA, latencyB time.Duration
	var tokensA, tokensB int

	// Run A
	wg.Add(1)
	go func() {
		defer wg.Done()
		reqA := *req
		reqA.Model = modelA
		start := time.Now()
		responseA, tokensA = collectResponse(ctx, providerA, &reqA)
		latencyA = time.Since(start)
	}()

	// Run B
	wg.Add(1)
	go func() {
		defer wg.Done()
		reqB := *req
		reqB.Model = modelB
		start := time.Now()
		responseB, tokensB = collectResponse(ctx, providerB, &reqB)
		latencyB = time.Since(start)
	}()

	wg.Wait()

	// Determine winner (simple heuristic)
	winner := determineWinner(responseA, responseB, latencyA, latencyB)

	result := &ABTestResult{
		ID:        time.Now().Format("20060102-150405"),
		Prompt:    truncate(prompt, 200),
		ModelA:    modelA,
		ModelB:    modelB,
		ResponseA: truncate(responseA, 500),
		ResponseB: truncate(responseB, 500),
		LatencyA:  latencyA.Milliseconds(),
		LatencyB:  latencyB.Milliseconds(),
		TokensA:   tokensA,
		TokensB:   tokensB,
		Winner:    winner,
		CreatedAt: time.Now(),
	}

	t.mu.Lock()
	t.results = append(t.results, *result)
	if len(t.results) > 100 {
		t.results = t.results[len(t.results)-50:]
	}
	t.mu.Unlock()

	log.Printf("[A/B Test] Winner: %s (A=%dms B=%dms)", winner, latencyA.Milliseconds(), latencyB.Milliseconds())
	return result
}

// GetResults returns recent test results.
func (t *ABTester) GetResults(limit int) []ABTestResult {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if limit <= 0 || limit > len(t.results) {
		limit = len(t.results)
	}
	start := len(t.results) - limit
	result := make([]ABTestResult, limit)
	copy(result, t.results[start:])
	return result
}

func collectResponse(ctx context.Context, provider types.Provider, req *types.MessagesRequest) (string, int) {
	ch, err := provider.StreamMessage(ctx, req, nil)
	if err != nil {
		return "ERROR: " + err.Error(), 0
	}

	var resp strings.Builder
	tokens := 0
	for event := range ch {
		if event.Delta != nil {
			var delta struct{ Text string `json:"text"` }
			json.Unmarshal(event.Delta, &delta)
			resp.WriteString(delta.Text)
		}
		if event.Usage != nil {
			tokens = event.Usage.OutputTokens
		}
		if event.Type == "message_stop" {
			break
		}
	}
	return resp.String(), tokens
}

func determineWinner(respA, respB string, latA, latB time.Duration) string {
	scoreA, scoreB := 0, 0

	// Length (more detail = better, up to a point)
	if len(respA) > len(respB)*2 {
		scoreA++
	} else if len(respB) > len(respA)*2 {
		scoreB++
	}

	// Speed
	if latA < latB {
		scoreA++
	} else if latB < latA {
		scoreB++
	}

	// Non-empty
	if respA != "" && respB == "" {
		return "A"
	}
	if respB != "" && respA == "" {
		return "B"
	}

	if scoreA > scoreB {
		return "A"
	} else if scoreB > scoreA {
		return "B"
	}
	return "tie"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
