package kairos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// PRReviewConfig configures the PR auto-reviewer.
type PRReviewConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookSecret string `json:"webhookSecret,omitempty"`
	GitHubToken   string `json:"githubToken,omitempty"`
}

// PREvent represents a GitHub webhook PR event (simplified).
type PREvent struct {
	Action string `json:"action"`
	Number int    `json:"number"`
	PR     struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		DiffURL string `json:"diff_url"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ReviewResult holds the AI review output.
type ReviewResult struct {
	PRURL     string    `json:"prUrl"`
	Summary   string    `json:"summary"`
	Issues    []string  `json:"issues"`
	Approved  bool      `json:"approved"`
	Model     string    `json:"model"`
	Cost      float64   `json:"cost"`
	CreatedAt time.Time `json:"createdAt"`
}

// HandlePRWebhook processes a GitHub PR webhook.
func HandlePRWebhook(
	ctx context.Context,
	body []byte,
	provider types.Provider,
	model string,
) (*ReviewResult, error) {
	var event PREvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("parse webhook: %w", err)
	}

	if event.Action != "opened" && event.Action != "synchronize" {
		return nil, nil // only review new/updated PRs
	}

	log.Printf("[PR-Review] PR #%d: %s (%s → %s)",
		event.Number, event.PR.Title, event.PR.Head.Ref, event.PR.Base.Ref)

	// Fetch diff
	diff, err := fetchDiff(ctx, event.PR.DiffURL)
	if err != nil {
		return nil, fmt.Errorf("fetch diff: %w", err)
	}

	// Truncate long diffs
	if len(diff) > 50000 {
		diff = diff[:50000] + "\n... (truncated)"
	}

	// Ask AI to review
	prompt := fmt.Sprintf(`Review this pull request:

Title: %s
Description: %s
Branch: %s → %s

Diff:
%s

Provide:
1. A brief summary (2-3 sentences)
2. List of issues found (bugs, security, performance, style)
3. Whether you'd approve this PR (yes/no)

Be concise and actionable.`, event.PR.Title, event.PR.Body, event.PR.Head.Ref, event.PR.Base.Ref, diff)

	req := &types.MessagesRequest{
		Model: model,
		Messages: []types.Message{
			{Role: "user", Content: mustJSON(prompt)},
		},
		MaxTokens: 4096,
	}

	ch, err := provider.StreamMessage(ctx, req, nil)
	if err != nil {
		return nil, fmt.Errorf("AI review: %w", err)
	}

	var response strings.Builder
	for event := range ch {
		if event.Delta != nil {
			var delta struct{ Text string `json:"text"` }
			json.Unmarshal(event.Delta, &delta)
			response.WriteString(delta.Text)
		}
		if event.Type == "message_stop" {
			break
		}
	}

	result := &ReviewResult{
		PRURL:     event.PR.HTMLURL,
		Summary:   response.String(),
		Model:     model,
		CreatedAt: time.Now(),
	}

	return result, nil
}

func fetchDiff(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
