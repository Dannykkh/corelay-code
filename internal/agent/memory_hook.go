package agent

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/memory"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// Memory hook — wires internal/memory into the agent loop.
//
// The memory package itself is LLM-free: it produces prompts, parses
// responses, and writes files. The actual model calls live here so the
// memory package stays at the bottom of the dependency graph.
//
// Three hook points plug into loop.go:
//
//  1. BuildMemoryContext  — called once at session start; returns the
//     text to append to the system prompt so the model sees the user's
//     MEMORY.md and the top-N most relevant entries for this query.
//
//  2. ExtractMemoriesAsync — called after a normal session end. Kicks
//     off a background goroutine that asks the model to extract durable
//     memories from the just-finished conversation and writes them.
//     Failures are logged, never surfaced — a user's turn must not
//     hinge on a best-effort background task.
//
//  3. MaybeConsolidateAsync — called after a normal session end. Runs
//     the dream gate (time + entry-count + lock); if ready, kicks off
//     a background consolidation that merges near-duplicates.
//
// All three respect CORELAY_MEMORY (with ANICLEW_MEMORY as a compatibility
// fallback): set it to "off", "0", or "false" to disable the entire memory
// system at runtime. Default on.

// memoryEnabled reports whether long-term memory hooks are active.
func memoryEnabled() bool {
	v := strings.ToLower(renamedAgentEnv("CORELAY_MEMORY", "ANICLEW_MEMORY"))
	return v != "off" && v != "0" && v != "false"
}

// MemoryHeadsUp returns a one-time notice for the user when this workspace has
// no MEMORY.md yet and memory is enabled — so they learn that long-term memory
// files (MEMORY.md, memory/) may be written here on session end, instead of the
// files appearing silently. Returns "" when memory is off or MEMORY.md already
// exists (i.e. not a first run for this workspace).
func MemoryHeadsUp(workDir string) string {
	if !memoryEnabled() {
		return ""
	}
	if _, err := os.Stat(memory.Entrypoint(workDir)); err == nil {
		return ""
	}
	return "[Note] First run here — long-term memory (MEMORY.md, memory/) may be saved to this workspace when the session ends. Disable with CORELAY_MEMORY=off."
}

// BuildMemoryContext returns the memory snippet to append to the system
// prompt for this turn. Uses the most recent user message as the
// relevance query. Returns "" on any error or when memory is disabled
// — the caller appends unconditionally and an empty string is a no-op.
func BuildMemoryContext(workDir string, messages []types.Message) string {
	if !memoryEnabled() {
		return ""
	}
	query := lastUserText(messages)
	ctxText, err := memory.BuildSystemContext(workDir, query, 3)
	if err != nil {
		log.Printf("[memory] build context: %v", err)
		return ""
	}
	if strings.TrimSpace(ctxText) == "" {
		return ""
	}
	return "\n\n" + ctxText
}

// ExtractMemoriesAsync spawns a background goroutine that extracts
// durable memories from the conversation and writes them. The caller
// returns immediately; any failure is logged only.
//
// No-ops when memory is disabled, the conversation is trivial (<2
// messages), or the provider is nil. The context passed in is NOT
// propagated — the goroutine uses a fresh 2-minute context so it is
// not torn down by the request lifecycle ending.
func ExtractMemoriesAsync(_ context.Context, provider types.Provider, model, workDir string, messages []types.Message) {
	if !memoryEnabled() || provider == nil || len(messages) < 2 {
		return
	}
	// Snapshot the slice so later mutation in the caller cannot race us.
	snapshot := make([]types.Message, len(messages))
	copy(snapshot, messages)

	memoryWG.Add(1)
	go func() {
		defer memoryWG.Done()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		conversation := renderConversationForExtraction(snapshot)
		prompt := memory.BuildExtractPrompt(workDir, conversation)

		resp, err := callModelCollect(ctx, provider, model, prompt, "Extract durable memories now.")
		if err != nil {
			log.Printf("[memory] extract call: %v", err)
			return
		}

		entries, err := memory.ParseExtractedEntries(resp)
		if err != nil {
			log.Printf("[memory] parse extracted: %v", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		paths, trunc, err := memory.SaveExtracted(workDir, entries)
		if err != nil {
			log.Printf("[memory] save extracted: %v", err)
			return
		}
		log.Printf("[memory] extracted %d entries (%d files written)", len(entries), len(paths))
		if trunc.LineCapped || trunc.ByteCapped {
			log.Printf("[memory] WARNING MEMORY.md hit caps (lines=%d bytes=%d)", trunc.LineCount, trunc.ByteCount)
		}
	}()
}

// MaybeConsolidateAsync kicks off a background consolidation if the
// dream gate says so. Safe to call on every session end — the gate
// short-circuits almost every time.
func MaybeConsolidateAsync(_ context.Context, provider types.Provider, model, workDir string) {
	if !memoryEnabled() || provider == nil {
		return
	}

	cfg := memory.DefaultDreamConfig()
	g, err := memory.CheckDreamGate(workDir, cfg)
	if err != nil {
		log.Printf("[memory] gate check: %v", err)
		return
	}
	if !g.Ready {
		return
	}

	release, err := memory.TryAcquireDreamLock(workDir, cfg)
	if err != nil {
		// Another process beat us to it — that is fine.
		return
	}

	memoryWG.Add(1)
	go func() {
		defer memoryWG.Done()
		defer release()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		prompt, err := memory.BuildConsolidatePrompt(workDir)
		if err != nil {
			log.Printf("[memory] build consolidate prompt: %v", err)
			return
		}
		resp, err := callModelCollect(ctx, provider, model, prompt, "Consolidate memories now.")
		if err != nil {
			log.Printf("[memory] consolidate call: %v", err)
			return
		}
		entries, err := memory.ParseExtractedEntries(resp)
		if err != nil {
			log.Printf("[memory] parse consolidation: %v", err)
			return
		}
		if err := memory.ApplyConsolidation(workDir, entries); err != nil {
			log.Printf("[memory] apply consolidation: %v", err)
			return
		}
		log.Printf("[memory] consolidated to %d entries", len(entries))
	}()
}

// memoryWG tracks in-flight background memory tasks so a graceful
// server shutdown can drain them.
var memoryWG sync.WaitGroup

// WaitForMemoryTasks blocks until all background memory tasks finish
// or the timeout elapses, whichever comes first. Intended to be called
// by the server's graceful-shutdown path. Returns true if all tasks
// completed, false if the timeout fired with work still pending.
func WaitForMemoryTasks(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		memoryWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ----- helpers -----

// lastUserText returns the text of the most recent user message, or ""
// if there is none. Used as the relevance query for BuildSystemContext.
// Tool-result user messages (JSON arrays) are skipped by the type
// assertion below since they unmarshal as an array, not a string.
func lastUserText(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		var text string
		if err := json.Unmarshal(messages[i].Content, &text); err == nil && text != "" {
			return text
		}
	}
	return ""
}

// renderConversationForExtraction serializes user and assistant text
// turns into a single plain-text block the extraction prompt can embed
// without choking on nested JSON. Tool results and non-string content
// are intentionally dropped — they are too noisy for memory extraction
// and the model already saw them during the live turn.
func renderConversationForExtraction(messages []types.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil {
			continue // not a plain string — skip (tool_use blocks etc.)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// callModelCollect makes a single non-streaming-from-the-caller-view
// call to the provider, concatenating text deltas into one string.
// Used for the extract and consolidate prompts where we just want the
// full response text, not an incremental UI stream.
func callModelCollect(ctx context.Context, provider types.Provider, model, systemPrompt, userText string) (string, error) {
	sysBlock, _ := json.Marshal([]map[string]string{{"type": "text", "text": systemPrompt}})
	userBlock, _ := json.Marshal(userText)
	req := &types.MessagesRequest{
		Model:     model,
		System:    sysBlock,
		Messages:  []types.Message{{Role: "user", Content: userBlock}},
		MaxTokens: 4096,
	}

	ch, err := provider.StreamMessage(ctx, req, nil)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for ev := range ch {
		if ev.Type != "content_block_delta" {
			continue
		}
		var delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Delta, &delta); err != nil {
			continue
		}
		if delta.Type == "text_delta" {
			out.WriteString(delta.Text)
		}
	}
	return out.String(), nil
}
