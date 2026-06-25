package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aniclew/aniclew/internal/types"
)

// ChronosConfig controls the autonomous loop.
type ChronosConfig struct {
	MaxIterations     int           // overall iteration cap (default 50)
	MaxCycles         int           // FIND→FIX→VERIFY cycle cap (default 10)
	CycleTimeout      time.Duration // max time per cycle (default 5 min)
	TotalTimeout      time.Duration // max total time (default 30 min)
	VerifyCommand     string        // command to verify (e.g., "npm test", "go test ./...")
	CompletionCheck   string        // how to determine completion
	AutoFix           bool          // automatically attempt fixes on verify failure
	WorkstreamContext string        // durable workstream context rendered by the server
}

// DefaultChronosConfig returns standard settings.
func DefaultChronosConfig() ChronosConfig {
	return ChronosConfig{
		MaxIterations: 50,
		MaxCycles:     10,
		CycleTimeout:  5 * time.Minute,
		TotalTimeout:  30 * time.Minute,
		AutoFix:       true,
	}
}

// ChronosState tracks the autonomous loop progress.
type ChronosState struct {
	Cycle         int       `json:"cycle"`
	Phase         string    `json:"phase"` // "find", "fix", "verify", "complete", "failed"
	TotalTools    int       `json:"totalTools"`
	TotalTokens   int       `json:"totalTokens"`
	Findings      []string  `json:"findings"`
	Fixes         []string  `json:"fixes"`
	VerifyResults []string  `json:"verifyResults"`
	StartedAt     time.Time `json:"startedAt"`
	LastCycleAt   time.Time `json:"lastCycleAt"`
}

// RunChronos executes the FIND → FIX → VERIFY autonomous loop.
func RunChronos(
	ctx context.Context,
	provider types.Provider,
	model string,
	task string,
	workDir string,
	cfg ChronosConfig,
	eventCh chan<- Event,
) {
	defer close(eventCh)

	state := &ChronosState{
		Phase:     "find",
		StartedAt: time.Now(),
	}

	totalCtx, cancel := context.WithTimeout(ctx, cfg.TotalTimeout)
	defer cancel()

	log.Printf("[Chronos] Starting autonomous loop: %s", truncateStr(task, 100))
	eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos: starting autonomous loop (max %d cycles)", cfg.MaxCycles)}

	// Detect project for context
	project := DetectProject(workDir)
	projectCtx := LoadProjectContext(workDir)

	// Build system prompt for Chronos mode
	sysPrompt := buildChronosSystemPrompt(task, cfg)

	messages := []types.Message{
		{Role: "user", Content: mustJSON(task)},
	}

	for cycle := 1; cycle <= cfg.MaxCycles; cycle++ {
		if totalCtx.Err() != nil {
			eventCh <- Event{Type: "error", Data: "Chronos: total timeout reached"}
			return
		}

		state.Cycle = cycle
		state.LastCycleAt = time.Now()

		// ── FIND phase ──
		state.Phase = "find"
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos cycle %d/%d — FIND", cycle, cfg.MaxCycles)}

		findPrompt := fmt.Sprintf(`[Chronos Cycle %d — FIND Phase]
Analyze the current state. What needs to be fixed or changed?
Project: %s (%s)
%s

Report findings concisely. If everything looks good, respond with [COMPLETE].`, cycle, project.Name, project.Type, projectCtx)

		if cycle > 1 {
			// Add previous cycle results
			findPrompt += fmt.Sprintf("\n\nPrevious findings: %s\nPrevious fixes: %s\nVerify result: %s",
				strings.Join(state.Findings, "; "),
				strings.Join(state.Fixes, "; "),
				strings.Join(state.VerifyResults, "; "))
		}

		messages = append(messages, types.Message{
			Role:    "user",
			Content: mustJSON(findPrompt),
		})

		findResult := runAgentIteration(totalCtx, provider, model, sysPrompt, messages, workDir, eventCh, state)

		// Check for completion
		if strings.Contains(strings.ToUpper(findResult), "[COMPLETE]") {
			state.Phase = "complete"
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos: COMPLETE after %d cycles", cycle)}
			eventCh <- Event{Type: "text", Data: fmt.Sprintf("\n\n---\nChronos completed in %d cycles (%.0fs)\nFindings: %d, Fixes: %d\n",
				cycle, time.Since(state.StartedAt).Seconds(), len(state.Findings), len(state.Fixes))}
			eventCh <- Event{Type: "done", Data: nil}
			return
		}

		state.Findings = append(state.Findings, truncateStr(findResult, 200))

		// ── FIX phase ──
		if !cfg.AutoFix {
			// In non-autofix mode, report findings and stop
			eventCh <- Event{Type: "status", Data: "Chronos: findings reported (auto-fix disabled)"}
			eventCh <- Event{Type: "done", Data: nil}
			return
		}

		state.Phase = "fix"
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos cycle %d/%d — FIX", cycle, cfg.MaxCycles)}

		fixPrompt := fmt.Sprintf(`[Chronos Cycle %d — FIX Phase]
Based on the findings above, make the necessary fixes.
Use tools to edit files, run commands, etc.
Be precise and minimal — only fix what was found.`, cycle)

		messages = append(messages, types.Message{
			Role:    "user",
			Content: mustJSON(fixPrompt),
		})

		fixResult := runAgentIteration(totalCtx, provider, model, sysPrompt, messages, workDir, eventCh, state)
		state.Fixes = append(state.Fixes, truncateStr(fixResult, 200))

		// ── VERIFY phase ──
		state.Phase = "verify"
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos cycle %d/%d — VERIFY", cycle, cfg.MaxCycles)}

		var verifyPrompt string
		if cfg.VerifyCommand != "" {
			verifyPrompt = fmt.Sprintf(`[Chronos Cycle %d — VERIFY Phase]
Run the verification command: %s
If it passes, respond with [COMPLETE].
If it fails, analyze the failures for the next FIND phase.`, cycle, cfg.VerifyCommand)
		} else {
			verifyPrompt = fmt.Sprintf(`[Chronos Cycle %d — VERIFY Phase]
Verify that the fixes are correct:
1. Read the modified files to confirm changes
2. Run any relevant tests or checks
3. If everything looks good, respond with [COMPLETE]
4. If issues remain, describe them for the next cycle`, cycle)
		}

		messages = append(messages, types.Message{
			Role:    "user",
			Content: mustJSON(verifyPrompt),
		})

		verifyResult := runAgentIteration(totalCtx, provider, model, sysPrompt, messages, workDir, eventCh, state)
		state.VerifyResults = append(state.VerifyResults, truncateStr(verifyResult, 200))

		// Check if verify passed
		if strings.Contains(strings.ToUpper(verifyResult), "[COMPLETE]") {
			state.Phase = "complete"
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos: VERIFIED & COMPLETE after %d cycles", cycle)}
			eventCh <- Event{Type: "text", Data: fmt.Sprintf("\n\n---\nChronos completed in %d cycles (%.0fs)\nFindings: %d, Fixes: %d, Verified\n",
				cycle, time.Since(state.StartedAt).Seconds(), len(state.Findings), len(state.Fixes))}
			eventCh <- Event{Type: "done", Data: nil}
			return
		}

		// Compress context if getting large
		if len(messages) > 20 {
			messages = compressChronosMessages(messages)
			eventCh <- Event{Type: "status", Data: "Chronos: context compressed"}
		}
	}

	state.Phase = "failed"
	eventCh <- Event{Type: "status", Data: fmt.Sprintf("Chronos: max cycles (%d) reached without completion", cfg.MaxCycles)}
	eventCh <- Event{Type: "text", Data: fmt.Sprintf("\n\n---\nChronos stopped after %d cycles (%.0fs)\nLast findings: %s\n",
		cfg.MaxCycles, time.Since(state.StartedAt).Seconds(), strings.Join(state.Findings, "; "))}
	eventCh <- Event{Type: "done", Data: nil}
}

// runAgentIteration executes one LLM call with tool use within the Chronos loop.
func runAgentIteration(
	ctx context.Context,
	provider types.Provider,
	model string,
	sysPrompt string,
	messages []types.Message,
	workDir string,
	eventCh chan<- Event,
	state *ChronosState,
) string {
	tools := AllToolDefs(workDir)

	req := &types.MessagesRequest{
		Model:     model,
		System:    mustJSON([]map[string]string{{"type": "text", "text": sysPrompt}}),
		Messages:  messages,
		Tools:     tools,
		MaxTokens: 8192,
	}

	// Inner tool loop (max 15 iterations per phase)
	var finalText string
	for iter := 0; iter < 15; iter++ {
		if ctx.Err() != nil {
			return "Timeout"
		}

		ch, err := provider.StreamMessage(ctx, req, nil)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}

		var textContent string
		var toolUses []toolUseBlock

		for event := range ch {
			switch event.Type {
			case "content_block_start":
				var block struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				json.Unmarshal(event.ContentBlock, &block)
				if block.Type == "tool_use" {
					toolUses = append(toolUses, toolUseBlock{ID: block.ID, Name: block.Name})
				}
			case "content_block_delta":
				var delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					Thinking    string `json:"thinking"`
				}
				json.Unmarshal(event.Delta, &delta)
				if delta.Type == "text_delta" {
					textContent += delta.Text
					eventCh <- Event{Type: "text", Data: delta.Text}
				}
				if delta.Type == "thinking_delta" && delta.Thinking != "" {
					// Stream thinking but don't add to final text
					eventCh <- Event{Type: "thinking", Data: delta.Thinking}
				}
				if delta.Type == "input_json_delta" && len(toolUses) > 0 {
					toolUses[len(toolUses)-1].InputRaw += delta.PartialJSON
				}
			}
		}

		if len(toolUses) == 0 {
			finalText = textContent
			// Add assistant response to messages
			messages = append(messages, types.Message{
				Role:    "assistant",
				Content: mustJSON(textContent),
			})
			break
		}

		// Execute tools
		var assistantContent []map[string]interface{}
		if textContent != "" {
			assistantContent = append(assistantContent, map[string]interface{}{"type": "text", "text": textContent})
		}

		var toolResults []map[string]interface{}
		for _, tu := range toolUses {
			state.TotalTools++
			tu.Input = json.RawMessage(tu.InputRaw)

			result, isError := ExecuteTool(tu.Name, tu.Input, workDir)
			eventCh <- Event{Type: "tool_result", Data: map[string]interface{}{
				"id": tu.ID, "name": tu.Name, "result": truncateStr(result, 1000), "isError": isError,
			}}

			assistantContent = append(assistantContent, map[string]interface{}{
				"type": "tool_use", "id": tu.ID, "name": tu.Name, "input": json.RawMessage(tu.InputRaw),
			})
			toolResults = append(toolResults, map[string]interface{}{
				"type": "tool_result", "tool_use_id": tu.ID,
				"content": result, "is_error": isError,
			})
		}

		messages = append(messages,
			types.Message{Role: "assistant", Content: mustJSON(assistantContent)},
			types.Message{Role: "user", Content: mustJSON(toolResults)},
		)
		req.Messages = messages
		finalText = textContent
	}

	return finalText
}

func buildChronosSystemPrompt(task string, cfg ChronosConfig) string {
	verify := "Review your changes carefully."
	if cfg.VerifyCommand != "" {
		verify = fmt.Sprintf("Use this command to verify: %s", cfg.VerifyCommand)
	}

	prompt := fmt.Sprintf(`You are AniClew Chronos — an autonomous coding agent that works in cycles until the task is complete.

## Mode: FIND → FIX → VERIFY

Each cycle:
1. FIND: Analyze the codebase, identify issues or remaining work
2. FIX: Make the necessary changes using tools
3. VERIFY: %s

## Rules
- Work autonomously without asking the user
- Be precise and minimal in changes
- When everything is done, respond with [COMPLETE]
- If stuck after multiple attempts, explain why and respond with [COMPLETE]
- Use Read/Grep to understand code before editing
- Run tests after changes when possible

## Task
%s

## Constraints
- Max %d cycles, %v total timeout
- Focus on the task, don't over-engineer`, verify, task, cfg.MaxCycles, cfg.TotalTimeout)
	if contextBlock := strings.TrimSpace(cfg.WorkstreamContext); contextBlock != "" {
		prompt += "\n\n" + contextBlock
	}
	return prompt
}

// compressChronosMessages keeps the first 2 and last 6 messages, summarizing the middle.
func compressChronosMessages(messages []types.Message) []types.Message {
	if len(messages) <= 8 {
		return messages
	}
	head := messages[:2]
	tail := messages[len(messages)-6:]

	// Summarize middle
	var middleSummary string
	for _, m := range messages[2 : len(messages)-6] {
		var text string
		json.Unmarshal(m.Content, &text)
		if len(text) > 100 {
			text = text[:100] + "..."
		}
		if text != "" {
			middleSummary += fmt.Sprintf("[%s] %s\n", m.Role, text)
		}
	}

	summaryMsg := types.Message{
		Role:    "user",
		Content: mustJSON("[Previous conversation summary]\n" + middleSummary),
	}

	compressed := make([]types.Message, 0, len(head)+1+len(tail))
	compressed = append(compressed, head...)
	compressed = append(compressed, summaryMsg)
	compressed = append(compressed, tail...)
	return compressed
}
