package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aniclew/aniclew/internal/config"
	"github.com/aniclew/aniclew/internal/hooks"
	"github.com/aniclew/aniclew/internal/translate"
	"github.com/aniclew/aniclew/internal/types"
)

// defaultLocalToolBudget caps the tool count for local providers (Ollama,
// SGLang) whose context windows are small. 16 keeps the 8 core file/exec tools
// plus the 8 most task-relevant extras — enough for real coding work while
// roughly halving the tool-definition tokens in the system prompt. Overridable
// via config (localToolBudget) or env (ANICLEW_MAX_TOOLS).
const defaultLocalToolBudget = 16

// defaultLocalTemperature is the sampling temperature for the local agent loop.
// 0 makes tool calling deterministic and reliable (local models otherwise drift
// into prose instead of tool_use). Overridable via config (agentTemperature).
const defaultLocalTemperature = 0.0

// isLocalProvider reports whether the provider is a locally-hosted model server
// that benefits from a trimmed tool list (small context window, degrades when
// handed many tools). Cloud providers keep the full set.
func isLocalProvider(name string) bool {
	return name == "ollama" || name == "sglang"
}

// defaultReadOnlyExploreRounds bounds tool-using rounds for a read-only question
// before the loop forces an answer. Without this, a model — especially a local
// one — crawls the whole tree for a simple "what is this project?", which is
// slow and can exhaust the iteration cap with no answer at all. Overridable via
// config (readOnlyExploreRounds).
const defaultReadOnlyExploreRounds = 5

// Read-only exploration is weighted by what the model actually consumed:
// content reads advance the budget faster than navigation, so listing
// directories doesn't burn the rounds a model needs to read real content.
const (
	contentRoundWeight = 1.0 // a round that read content (Read/Grep/…)
	navRoundWeight     = 0.5 // a navigation-only round (LS/Glob)
)

// isNavTool reports whether a tool only navigates (lists/finds) rather than
// reading file content.
func isNavTool(name string) bool { return name == "LS" || name == "Glob" }

// iterationWeight scores one tool-using round for the read-only budget: a round
// that called any content tool counts full; a navigation-only round counts half.
func iterationWeight(toolUses []toolUseBlock) float64 {
	for _, tu := range toolUses {
		if !isNavTool(tu.Name) {
			return contentRoundWeight
		}
	}
	return navRoundWeight
}

// planModeTools is the read-only tool surface allowed in plan mode: the agent
// explores but cannot change anything, so it produces a plan instead of acting.
var planModeTools = map[string]bool{"Read": true, "Glob": true, "Grep": true, "LS": true}

// filterReadOnlyTools keeps only the read-only tools used by plan mode.
func filterReadOnlyTools(tools []types.ToolDef) []types.ToolDef {
	out := make([]types.ToolDef, 0, len(tools))
	for _, t := range tools {
		if planModeTools[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// editIntentWords mark a request that wants the agent to CHANGE something. Their
// presence disqualifies the read-only guard, so action tasks keep the full loop.
var editIntentWords = []string{
	"rename", "add", "remove", "delete", "fix", "edit", "create", "write",
	"implement", "refactor", "update", "change", "modify", "replace", "install",
	"generate", "migrate", "rewrite", "append", "insert", "convert", "commit",
	"수정", "고쳐", "고치", "만들", "추가", "삭제", "제거", "구현", "변경",
	"바꿔", "바꾸", "작성", "생성", "리팩", "교체", "설치",
}

// readIntentWords mark a question / explanation request (no modification).
var readIntentWords = []string{
	"what", "why", "how", "explain", "describe", "summar", "list", "show",
	"where", "which", "who", "understand", "overview", "tell me", "analyze",
	"뭐", "뭔", "무엇", "무슨", "정체", "설명", "요약", "알려", "어떻게",
	"동작", "분석", "보여", "개요", "이해", "어떤", "왜", "나열", "목록",
	"리스트", "어디", "확인해", "찾아",
}

// isReadOnlyQuestion reports whether the request is a pure question/explanation
// with no intent to modify the codebase. Conservative by design: ANY edit-intent
// word disqualifies it, so action tasks never get the exploration cap (a false
// "read-only" on an edit task would block the edit; a missed read-only just
// stays slow). A question mark or a read-intent word qualifies it.
func isReadOnlyQuestion(text string) bool {
	t := strings.ToLower(text)
	for _, w := range editIntentWords {
		if strings.Contains(t, w) {
			return false
		}
	}
	if strings.Contains(t, "?") || strings.Contains(t, "？") {
		return true
	}
	for _, w := range readIntentWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// flattenToolResults collapses the tool_use/tool_result exchanges in a message
// history into a plain-text digest ("### Read {\"file_path\":…}\n<output>"). The
// read-only guard uses it to answer from gathered context without replaying the
// tool-call pattern (which makes local models keep calling tools). tool_use
// blocks (assistant) supply labels; tool_result blocks (user) supply outputs.
func flattenToolResults(messages []types.Message) string {
	labels := map[string]string{}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" {
				labels[b.ID] = strings.TrimSpace(b.Name + " " + string(b.Input))
			}
		}
	}
	var sb strings.Builder
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // not a tool_result array (e.g. the plain-text question)
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			var s string
			if json.Unmarshal(b.Content, &s) != nil {
				s = string(b.Content)
			}
			label := labels[b.ToolUseID]
			if label == "" {
				label = "result"
			}
			sb.WriteString("### " + label + "\n" + s + "\n\n")
		}
	}
	return sb.String()
}

// heartbeat emits an elapsed-time + output-size signal once per second while
// the agent waits on (and consumes) the provider stream. A slow local model
// (qwen3 on a 16GB GPU offloads to CPU; prompt prefill before the first token
// can be many seconds of pure silence) otherwise looks hung — the agent loop
// emits nothing of its own between the pre-call status and the provider's first
// delta. This is the source of truth every client renders, modeled on Claude
// Code's "Thinking… (Ns · ↑N tokens)" status line.
//
// stop() must be called before the eventCh is closed (it joins the goroutine),
// so callers defer/scope it to a single provider call.
func startHeartbeat(eventCh chan<- Event, outChars *int64) (stop func()) {
	start := time.Now()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				eventCh <- Event{Type: "heartbeat", Data: map[string]interface{}{
					"elapsedMs": time.Since(start).Milliseconds(),
					"chars":     atomic.LoadInt64(outChars),
				}}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
}

const baseSystemPrompt = `You are AniClew, an expert coding agent. You act by CALLING TOOLS, not by describing actions.

## Acting vs. talking (read this first)
The ONLY way to change the filesystem, run code, or inspect the project is to emit a tool call. Writing code or commands in your reply text does NOTHING — no file is created, nothing runs.
- When the user asks you to create/modify/run/check something, your response MUST contain a tool call. Do not answer with prose or a code block alone.
- NEVER claim you "created", "wrote", "updated", or "ran" anything unless a tool call actually did it in this turn. Saying so without a tool call is a hallucination and is wrong.
- Prefer acting over explaining. Make the tool call first; keep any prose short. After tools report success, give a brief confirmation of what the tools actually did.
- If a task needs several steps, call tools across multiple turns until it is genuinely done.

## Tools: Bash, Read, Write, Edit, Glob, Grep, Git, LS, WebSearch, WebFetch, WebResearch, TaskCreate/Update/List, NotebookRead/Edit, Screenshot, MouseClick, TypeText, OpenApp, FileManager, Clipboard

## Rules
- To create a new file, call Write. To change an existing file, Read it first, then call Edit.
- Use Glob/Grep to find files instead of guessing paths
- Run tests after changes when possible
- For git: use Git tool (not Bash)
- Keep changes minimal and focused
- Be concise`

var langInstructions = map[string]string{
	"ko":   "\n\nIMPORTANT: Always respond in Korean (한국어). Code and file paths stay in English, but all explanations, comments to the user, and descriptions must be in Korean.",
	"en":   "\n\nIMPORTANT: Always respond in English.",
	"ja":   "\n\nIMPORTANT: Always respond in Japanese (日本語). Code and file paths stay in English, but all explanations must be in Japanese.",
	"zh":   "\n\nIMPORTANT: Always respond in Chinese (中文). Code and file paths stay in English, but all explanations must be in Chinese.",
	"auto": "", // no language instruction — let the model follow the user's language
}

func buildSystemPrompt(responseLang string) string {
	instruction := langInstructions[responseLang]
	if instruction == "" {
		instruction = langInstructions["auto"]
	}
	return baseSystemPrompt + instruction
}

// langReminders is a SHORT, native-language directive to append right at the end
// of a generation prompt. Recency matters: when the closing instruction is in
// English, weaker models (observed: gemma4) drift to English even with a Korean
// directive higher up in the system prompt. Empty for en/auto/unknown.
var langReminders = map[string]string{
	"ko": "\n\n한국어로 답하세요.",
	"ja": "\n\n日本語で答えてください。",
	"zh": "\n\n请用中文回答。",
}

// langReminder returns the recency reminder for a response language ("" if none).
func langReminder(responseLang string) string { return langReminders[responseLang] }

// Event is sent to the client via SSE during the agent loop.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// RunLoop executes the agent loop: prompt → LLM → tool_use → execute → repeat.
func RunLoop(
	ctx context.Context,
	provider types.Provider,
	model string,
	userMessages []types.Message,
	workDir string,
	responseLang string,
	eventCh chan<- Event,
) {
	RunLoopWithOptions(ctx, provider, model, userMessages, workDir, RunOptions{ResponseLang: responseLang}, eventCh)
}

func RunLoopWithOptions(
	ctx context.Context,
	provider types.Provider,
	model string,
	userMessages []types.Message,
	workDir string,
	opts RunOptions,
	eventCh chan<- Event,
) {
	defer close(eventCh)

	responseLang := opts.ResponseLang
	if responseLang == "" {
		responseLang = "auto"
	}

	messages := make([]types.Message, len(userMessages))
	copy(messages, userMessages)

	// /undo: revert the agent's most recent edits, then stop (no LLM call).
	if len(messages) > 0 {
		var last string
		if json.Unmarshal(messages[len(messages)-1].Content, &last) == nil &&
			strings.EqualFold(strings.TrimSpace(last), "/undo") {
			if reverted, ok := undoCheckpoint(workDir); ok {
				eventCh <- Event{Type: "text", Data: "되돌렸습니다:\n- " + strings.Join(reverted, "\n- ")}
			} else {
				eventCh <- Event{Type: "text", Data: "되돌릴 변경이 없습니다."}
			}
			eventCh <- Event{Type: "done", Data: nil}
			return
		}
	}

	tools := AllToolDefs(workDir)
	toolExecOptions := ToolExecutionOptions{
		WorkerID:         opts.WorkerID,
		OwnershipChecker: opts.OwnershipChecker,
	}

	// Plan mode: "/plan <task>" makes the agent explore read-only and produce a
	// step-by-step plan WITHOUT editing — the Claude-Code plan-then-execute flow.
	// We strip the "/plan" prefix, restrict tools to read-only, and route output
	// through the exploration guard so a plan is always produced. The user reviews
	// it and asks to proceed in a follow-up (normal) turn.
	planMode := false
	planTask := ""
	if len(messages) > 0 {
		var last string
		if json.Unmarshal(messages[len(messages)-1].Content, &last) == nil {
			if trimmed := strings.TrimSpace(last); strings.HasPrefix(strings.ToLower(trimmed), "/plan") {
				planMode = true
				planTask = strings.TrimSpace(trimmed[len("/plan"):])
				if planTask == "" {
					planTask = "Plan the requested work."
				}
				messages[len(messages)-1] = types.Message{Role: "user", Content: mustJSON(planTask)}
			}
		}
	}
	if planMode {
		tools = filterReadOnlyTools(tools)
	}

	// @file mentions: "@path" in the message pulls that file's content into
	// context up front, so the model doesn't have to crawl to find it — a focused
	// alternative to exploration, especially helpful for local models.
	if len(messages) > 0 {
		var last string
		if json.Unmarshal(messages[len(messages)-1].Content, &last) == nil && strings.Contains(last, "@") {
			if block, files := expandFileMentions(last, workDir); len(files) > 0 {
				messages[len(messages)-1] = types.Message{Role: "user", Content: mustJSON(block + "\n---\n" + last)}
				eventCh <- Event{Type: "status", Data: fmt.Sprintf("Loaded %d referenced file(s): %s", len(files), strings.Join(files, ", "))}
			}
		}
	}

	// Agent-loop tuning (tool budget + temperature) for local models, resolved
	// from config with built-in fallbacks.
	cfg := config.Load()

	// Resolve agent-loop tuning. Local models (Ollama/SGLang) run with small
	// context windows and degrade with large tool lists or default sampling, so a
	// per-model profile supplies sensible defaults (tool budget + temperature)
	// that explicit config/env still override.
	//   tool budget : env ANICLEW_MAX_TOOLS > config localToolBudget > profile > 16
	//                 (the full ~30-tool list overflows a small context; core
	//                  file/exec tools are always kept, peripherals dropped by
	//                  relevance)
	//   temperature : config agentTemperature > profile (0 — pinning it low makes
	//                 tool calling deterministic; at the provider default local
	//                 models drift into prose instead of emitting tool_use)
	// Cloud models keep the full toolset and provider default unless
	// ANICLEW_MAX_TOOLS is set.
	toolBudget := translate.ToolBudget()
	var agentTemp *float64
	if isLocalProvider(provider.Name()) {
		prof, matched := profileFor(model)
		if toolBudget == 0 {
			if toolBudget = cfg.LocalToolBudget; toolBudget == 0 {
				toolBudget = prof.toolBudget
			}
		}
		if cfg.AgentTemperature != nil {
			agentTemp = cfg.AgentTemperature
		} else {
			t := prof.temperature
			agentTemp = &t
		}
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Model profile: %s (tools=%d, temp=%.1f)", prof.name, toolBudget, *agentTemp)}
		log.Printf("[Agent] profile=%q matched=%v budget=%d temp=%.2f model=%s", prof.name, matched, toolBudget, *agentTemp, model)
	}
	if toolBudget > 0 {
		var dropped int
		tools, dropped = translate.PruneTools(tools, lastUserText(messages), toolBudget)
		if dropped > 0 {
			log.Printf("[Agent] tool budget %d: kept %d, dropped %d (provider=%s)", toolBudget, len(tools), dropped, provider.Name())
		}
	}

	// Read-only over-exploration guard: a pure question ("what is this project?")
	// doesn't need the whole tree read. After a few exploration rounds the loop
	// drops the tools so the model must answer from what it has already read,
	// instead of crawling every file until it exhausts the iteration cap and
	// ends with "Max iterations reached" and no answer. Edit/action tasks are
	// exempt (isReadOnlyQuestion returns false for them).
	readOnly := planMode || isReadOnlyQuestion(lastUserText(messages))
	readOnlyRounds := defaultReadOnlyExploreRounds
	if cfg.ReadOnlyExploreRounds > 0 {
		readOnlyRounds = cfg.ReadOnlyExploreRounds
	}
	if readOnly {
		log.Printf("[Agent] read-only question — exploration capped at %d rounds", readOnlyRounds)
	}

	// Model capability check (Ollama): the agent loop is built on tool calling, so
	// warn up front when the selected model can't do it — otherwise an agentic
	// request silently does nothing (the model just chats). devstral, for example,
	// advertises no "tools" capability through Ollama's generic API. Best-effort
	// and non-blocking: unknown capabilities skip the check.
	if provider.Name() == "ollama" {
		if caps := detectOllamaCapabilities(model); len(caps) > 0 {
			eventCh <- Event{Type: "status", Data: "Model capabilities: " + strings.Join(caps, ", ")}
			if !hasCapability(caps, "tools") {
				warn := fmt.Sprintf("[Warning] %s does not support tool calling — it can only chat, so file edits and other agent actions will not work. Switch to a tools-capable model (e.g. qwen3-coder).", model)
				eventCh <- Event{Type: "status", Data: warn}
				log.Printf("[Agent] WARNING: model %s lacks 'tools' capability (caps=%v)", model, caps)
			}
		}
	}

	maxIterations := 25

	// Reflection guard: stop if the model keeps producing tool calls that all
	// fail for several rounds in a row (e.g. repeating an edit the lint gate
	// rejects). Prevents burning iterations on a stuck self-correction loop.
	consecutiveErrorRounds := 0
	const maxErrorRounds = 3

	// ── Hook system: load from project + skill source ──
	hookRegistry := hooks.NewRegistry()
	hookRegistry.Load(workDir, "") // "" = all sources
	hookRegistry.Execute(hooks.HookSessionStart, map[string]string{"WORK_DIR": workDir})

	// ── Permission snapshot (immutable for this session) ──
	permissions := hooks.CapturePermissions(workDir)
	_ = permissions // used in tool execution below

	// ── Compaction config ──
	compactCfg := CompactConfig{ContextWindow: 200000}

	// ── Detect project type ──
	project := DetectProject(workDir)
	projectPrompt := project.ToPrompt()
	eventCh <- Event{Type: "status", Data: fmt.Sprintf("Project: %s (%s, %d files)", project.Name, project.Type, project.FileCount)}

	// ── Load project context (CLAUDE.md, AGENTS.md, skills) ──
	projectCtx := LoadProjectContext(workDir)
	skills := LoadSkills(workDir)
	mcpConfig := LoadMCPConfig(workDir)

	// ── Long-term memory: load index + top relevant entries for this turn ──
	// Computed once before the iteration loop — the snippet only depends
	// on the first user message, so recomputing per-iteration would just
	// defeat the prompt cache.
	memoryContext := BuildMemoryContext(workDir, messages)
	workstreamContext := ""
	if strings.TrimSpace(opts.WorkstreamContext) != "" {
		workstreamContext = "\n\n" + strings.TrimSpace(opts.WorkstreamContext)
	}
	if opts.Recorder != nil {
		opts.Recorder.RunStarted()
	}

	// ── Process slash commands ──
	if len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		var lastText string
		json.Unmarshal(lastMsg.Content, &lastText)
		if IsSlashCommand(lastText) {
			processed, err := ProcessSlashCommand(lastText, skills)
			if err != nil {
				failRun(opts.Recorder, err.Error())
				eventCh <- Event{Type: "error", Data: err.Error()}
				return
			}
			// Direct output commands — don't send to LLM
			if processed == "[CLEAR_CHAT]" || processed == "[SHOW_MODEL_SELECTOR]" {
				eventCh <- Event{Type: "command", Data: processed}
				return
			}
			if processed == "[COMPACT_CONTEXT]" {
				eventCh <- Event{Type: "status", Data: "Compressing context..."}
			}
			// /help → return directly, no LLM needed
			if strings.HasPrefix(lastText, "/help") {
				eventCh <- Event{Type: "text", Data: processed}
				eventCh <- Event{Type: "done", Data: nil}
				return
			}
			// Replace last message with processed skill prompt
			messages[len(messages)-1] = types.Message{
				Role:    "user",
				Content: mustJSON(processed),
			}
			eventCh <- Event{Type: "status", Data: "Skill loaded: " + lastText}
		}
	}

	// ── Connect MCP servers ──
	if mcpConfig != "" {
		count, _ := ConnectMCPServers(workDir)
		if count > 0 {
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Connected to %d MCP servers", count)}
		}
	}

	// Mention skills as a single pointer line — do NOT enumerate them and do
	// NOT inline their content. Two separate failures were observed driving an
	// open model (Qwen3 via Ollama/SGLang) and confirmed by replaying captured
	// requests directly against the backend:
	//   1. Inlining every SKILL.md balloons the prompt to ~700KB (~180K tokens),
	//      overflowing a local model's ~32K context so it loses the task/tools.
	//   2. Even a compact name+description index of ~100 skills SUPPRESSES tool
	//      calling: the model reads the list as a menu and answers with prose
	//      ("here is the code…") instead of emitting a tool_use — and then
	//      hallucinates that it created the file. Dropping the enumeration and
	//      keeping a one-line pointer restores reliable tool calls.
	// Skills stay fully usable: the user invokes one with /<name> and
	// ProcessSlashCommand (above) expands its full prompt before the LLM runs,
	// so the model never needs to see the catalog to use it.
	skillText := ""
	if len(skills) > 0 {
		skillText = fmt.Sprintf("\n\n## Skills\n%d task skills are available; the user invokes one by typing /<name>. Skills are not tools — never try to call them. Just do the work with the tools above.", len(skills))
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Loaded %d skills", len(skills))}
	}
	if projectCtx != "" {
		eventCh <- Event{Type: "status", Data: "Project context loaded (CLAUDE.md)"}
	}
	if mcpConfig != "" {
		eventCh <- Event{Type: "status", Data: "MCP config detected"}
	}

	// First-run heads-up: surface the agent-managed files this workspace will get
	// (long-term memory + remembered permissions) instead of creating them
	// silently. The permission file (.claude/settings.json) is only created if a
	// tool gets auto-allowed, so it is announced at creation time below.
	if msg := MemoryHeadsUp(workDir); msg != "" {
		eventCh <- Event{Type: "status", Data: msg}
	}
	claudeSettings := filepath.Join(workDir, ".claude", "settings.json")
	permFileExisted := fileExists(claudeSettings)
	permFileNotified := false

	// exploreScore weights the read-only guard by what the model actually read:
	// content reads (Read/Grep) count full, navigation (LS/Glob) counts half — so
	// a model that lists a lot still gets enough rounds to read real content
	// before being forced to answer.
	var exploreScore float64

	// Auto-verify state: whether the model edited any file this session, and how
	// many times we've already asked it to fix failing tests.
	didEdit := false
	verifyAttempts := 0
	checkpointStarted := false // clear the undo buffer on this turn's first edit
	var editedFiles []string   // files changed this session, for the completion summary
	testResult := ""           // auto-verify outcome for the summary ("passed"/"failed"/"")

	for i := 0; i < maxIterations; i++ {
		// ── Read-only over-exploration guard ──
		// Once a pure question has explored enough rounds, collapse the
		// tool_use/tool_result history into a single "here is what I read"
		// message and drop tools. Removing the tool-call pattern is essential:
		// local models (qwen3-coder via Ollama) keep emitting tool calls that the
		// backend parses even when the tools field is empty, as long as the
		// conversation still shows the pattern. With a clean digest + no tools the
		// model answers in text and the loop ends — instead of crawling the whole
		// tree to the iteration cap and returning "Max iterations reached" with no
		// answer. Edit/action tasks are exempt (readOnly is false for them).
		if readOnly && exploreScore >= float64(readOnlyRounds) {
			digest := flattenToolResults(messages)
			if len(digest) > 12000 {
				digest = digest[:12000] + "\n…(truncated)"
			}
			collapseQuestion := lastUserText(userMessages)
			collapseClosing := "\n\nAnswer the question above directly and concisely from this context. Do not request any more files."
			if planMode {
				collapseQuestion = planTask
				collapseClosing = "\n\nNow produce the step-by-step implementation plan from this context (which files to change and what to do in each). Do NOT make changes; do not request more files."
			}
			collapsed := collapseQuestion +
				"\n\n## Context I gathered from the codebase\n" + digest +
				collapseClosing + langReminder(responseLang)
			messages = []types.Message{{Role: "user", Content: mustJSON(collapsed)}}
			tools = nil
			readOnly = false // collapse once; the next pass produces the answer
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Read-only question — explored %d rounds, answering now", i)}
		}

		// ── Context compression ──
		tokenEstimate := EstimateMessageTokens(messages)
		if ShouldCompact(compactCfg, tokenEstimate) && len(messages) >= minMessagesForCompact {
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Compacting context (~%dk tokens, %d messages)...", tokenEstimate/1000, len(messages))}

			// Try LLM-based compaction first
			compacted, err := CompactMessages(ctx, provider, model, messages)
			if err != nil {
				compactCfg.CompactFailures++
				log.Printf("[Compact] LLM compact failed (%d/%d): %v — falling back to snip", compactCfg.CompactFailures, maxCompactFailures, err)

				// Snip fallback: keep first 2 + last 4, summarize middle inline
				if len(messages) > 8 {
					var middleSummary string
					for _, m := range messages[2 : len(messages)-4] {
						var text string
						json.Unmarshal(m.Content, &text)
						if len(text) > 100 {
							text = text[:100] + "..."
						}
						if text != "" {
							middleSummary += fmt.Sprintf("[%s] %s\n", m.Role, text)
						}
					}
					snipped := make([]types.Message, 0)
					snipped = append(snipped, messages[:2]...)
					snipped = append(snipped, types.Message{Role: "user", Content: mustJSON("[Context Summary]\n" + middleSummary)})
					snipped = append(snipped, messages[len(messages)-4:]...)
					messages = snipped
				}
			} else {
				messages = compacted
				compactCfg.CompactFailures = 0
			}
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Compacted to %d messages", len(messages))}
		}

		// Normalize messages before API call
		messages = NormalizeMessages(messages)

		// RAG: search project for relevant context based on last user message
		ragContext := ""
		if i == 0 && len(messages) > 0 { // only on first iteration
			lastUser := ""
			for j := len(messages) - 1; j >= 0; j-- {
				if messages[j].Role == "user" {
					json.Unmarshal(messages[j].Content, &lastUser)
					break
				}
			}
			if lastUser != "" {
				ragResults := RAGSearch(workDir, lastUser, 3)
				ragContext = FormatRAGContext(ragResults)
			}
		}

		// Build request with full context
		sysPrompt := buildSystemPrompt(responseLang) + projectPrompt + projectCtx + skillText + ragContext + memoryContext + workstreamContext
		if planMode {
			sysPrompt += "\n\n## PLAN MODE\nYou are in plan mode. Explore the codebase with the read-only tools and produce a concrete, step-by-step implementation plan (which files to change and what to do in each). You have NO edit tools — do not attempt to make changes. End with the plan; the user will review it and ask you to proceed."
		}
		req := &types.MessagesRequest{
			Model:       model,
			System:      mustJSON([]map[string]string{{"type": "text", "text": sysPrompt}}),
			Messages:    messages,
			Tools:       tools,
			MaxTokens:   8192,
			Temperature: agentTemp,
		}

		// Call LLM (with retry)
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Thinking... (iteration %d/%d, ~%dk tokens)", i+1, maxIterations, tokenEstimate/1000)}
		finishMakerSpan := startRunSpan(opts.Recorder, fmt.Sprintf("maker_%02d", i+1), "maker.llm_call", map[string]string{
			"iteration":      fmt.Sprintf("%d", i+1),
			"provider":       provider.Name(),
			"model":          model,
			"tokenEstimate":  fmt.Sprintf("%d", tokenEstimate),
			"messageCount":   fmt.Sprintf("%d", len(messages)),
			"toolDefinition": fmt.Sprintf("%d", len(tools)),
		})

		// Liveness: heartbeat elapsed-time + output size every second. Started
		// BEFORE StreamMessage on purpose — for a slow/cold local model the
		// longest dead air is inside StreamMessage itself (Ollama blocks the
		// HTTP response until a 23GB model finishes loading + prefill), well
		// before the first delta. outChars stays 0 during load, so the client
		// shows "Ns · 0 chars" — exactly the proof-of-life we want. Idempotent
		// stop, called on every exit path before eventCh closes.
		var outChars int64
		stopHeartbeat := startHeartbeat(eventCh, &outChars)

		var ch <-chan types.SSEEvent
		var err error
		for retry := 0; retry < 3; retry++ {
			ch, err = provider.StreamMessage(ctx, req, nil)
			if err == nil {
				break
			}
			if retry < 2 {
				eventCh <- Event{Type: "status", Data: fmt.Sprintf("Retrying... (%d/3): %s", retry+1, err.Error())}
				select {
				case <-ctx.Done():
					stopHeartbeat()
					finishMakerSpan("failed", map[string]string{"error": ctx.Err().Error()})
					failRun(opts.Recorder, ctx.Err().Error())
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
		if err != nil {
			stopHeartbeat()
			finishMakerSpan("failed", map[string]string{"error": err.Error()})
			failRun(opts.Recorder, fmt.Sprintf("Failed after 3 retries: %s", err.Error()))
			eventCh <- Event{Type: "error", Data: fmt.Sprintf("Failed after 3 retries: %s", err.Error())}
			return
		}

		// Collect response
		var textContent string
		var toolUses []toolUseBlock
		currentText := ""
		var currentTool *toolUseBlock
		stopReason := ""

		for event := range ch {
			switch event.Type {
			case "content_block_start":
				var block struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				json.Unmarshal(event.ContentBlock, &block)

				if block.Type == "thinking" {
					// Thinking block — stream to UI
					eventCh <- Event{Type: "status", Data: "Thinking..."}
				} else if block.Type == "text" {
					currentText = ""
				} else if block.Type == "tool_use" {
					currentTool = &toolUseBlock{ID: block.ID, Name: block.Name}
					eventCh <- Event{Type: "tool_start", Data: map[string]string{
						"id": block.ID, "name": block.Name,
					}}
				}

			case "content_block_delta":
				var delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				}
				json.Unmarshal(event.Delta, &delta)

				if delta.Type == "thinking_delta" {
					// Stream thinking to UI as dimmed text
					var thinkDelta struct {
						Thinking string `json:"thinking"`
					}
					json.Unmarshal(event.Delta, &thinkDelta)
					if thinkDelta.Thinking != "" {
						atomic.AddInt64(&outChars, int64(len(thinkDelta.Thinking)))
						eventCh <- Event{Type: "thinking", Data: thinkDelta.Thinking}
					}
				} else if delta.Type == "text_delta" {
					currentText += delta.Text
					atomic.AddInt64(&outChars, int64(len(delta.Text)))
					eventCh <- Event{Type: "text", Data: delta.Text}
				} else if delta.Type == "input_json_delta" && currentTool != nil {
					currentTool.InputRaw += delta.PartialJSON
				}

			case "content_block_stop":
				if currentTool != nil {
					currentTool.Input = json.RawMessage(currentTool.InputRaw)
					toolUses = append(toolUses, *currentTool)
					currentTool = nil
				} else if currentText != "" {
					textContent += currentText
				}

			case "message_delta":
				var d struct {
					StopReason string `json:"stop_reason"`
				}
				if json.Unmarshal(event.Delta, &d) == nil && d.StopReason != "" {
					stopReason = d.StopReason
				}

			case "message_stop":
				// done with this LLM call
			}
		}
		finishMakerSpan("ok", map[string]string{
			"textChars": fmt.Sprintf("%d", len(textContent)),
			"toolCalls": fmt.Sprintf("%d", len(toolUses)),
		})

		// Generation finished for this iteration — stop the liveness ticker
		// before tool execution (tools emit their own progress) and before any
		// return path that would close eventCh.
		stopHeartbeat()

		// Recover tool calls the model leaked as plain text instead of the
		// parseable format the backend expects (observed with qwen via Ollama) —
		// so a formatting slip doesn't silently end the turn with nothing done.
		if len(toolUses) == 0 {
			if recovered, cleaned := recoverLeakedToolCalls(textContent); len(recovered) > 0 {
				var valid []toolUseBlock
				for _, c := range recovered {
					for _, t := range tools {
						if t.Name == c.Name {
							valid = append(valid, c)
							break
						}
					}
				}
				if len(valid) > 0 {
					log.Printf("[Agent] recovered %d leaked tool call(s) from text", len(valid))
					toolUses = valid
					textContent = cleaned
					for _, c := range valid {
						eventCh <- Event{Type: "status", Data: "Recovered a tool call written as text: " + c.Name}
						eventCh <- Event{Type: "tool_start", Data: map[string]string{"id": c.ID, "name": c.Name}}
					}
				}
			}
		}

		// Context-exhaustion guard: a "max_tokens" stop with almost no output
		// means the prompt filled the model's context window (Ollama defaults to a
		// small one — 8K), leaving no room to generate. That is the silent failure
		// that looks like the model "doing nothing" (it emits ~1 token and stops).
		// Surface it with the actionable fix instead of leaving the user puzzled.
		if looksContextExhausted(stopReason, atomic.LoadInt64(&outChars), len(toolUses)) {
			eventCh <- Event{Type: "status", Data: "[Warning] The model hit its token limit with almost no output — the prompt likely fills the context window. Increase Ollama's Context length (Settings → Context length) or shorten the request."}
			log.Printf("[Agent] context-exhaustion suspected: stop=max_tokens outChars=%d", atomic.LoadInt64(&outChars))
		}

		// ── No tool calls → done ──
		if len(toolUses) == 0 {
			// ── Auto-verify: before declaring done, run the project's tests if
			//    the model edited files (the Claude-Code "edit → test → fix"
			//    loop). On failure, feed the output back so the model fixes it —
			//    bounded by maxVerifyAttempts so an unrelated/pre-existing failure
			//    cannot loop forever. Skips silently when there is no test runner.
			if didEdit && autoVerifyEnabled() && verifyAttempts < maxVerifyAttempts {
				eventCh <- Event{Type: "status", Data: "Auto-verify: running tests after edits…"}
				finishCheckSpan := startRunSpan(opts.Recorder, fmt.Sprintf("checker_%02d_%02d", i+1, verifyAttempts+1), "checker.auto_verify", map[string]string{
					"iteration": fmt.Sprintf("%d", i+1),
					"attempt":   fmt.Sprintf("%d", verifyAttempts+1),
				})
				vout, vfailed, vran := runAutoVerify(workDir)
				if vran && vfailed {
					testResult = "failed"
					finishCheckSpan("failed", map[string]string{"outputChars": fmt.Sprintf("%d", len(vout))})
					verifyAttempts++
					eventCh <- Event{Type: "status", Data: fmt.Sprintf("Auto-verify: tests failed — asking the model to fix (attempt %d/%d)", verifyAttempts, maxVerifyAttempts)}
					finishRepairSpan := startRunSpan(opts.Recorder, fmt.Sprintf("repair_%02d_%02d", i+1, verifyAttempts), "repair.feedback", map[string]string{
						"iteration": fmt.Sprintf("%d", i+1),
						"attempt":   fmt.Sprintf("%d", verifyAttempts),
					})
					if textContent != "" {
						messages = append(messages, types.Message{Role: "assistant", Content: mustJSON([]map[string]interface{}{{"type": "text", "text": textContent}})})
					}
					messages = append(messages, types.Message{Role: "user", Content: mustJSON(
						"Automated verification ran the tests after your changes and they FAILED:\n\n" + vout +
							"\n\nIf these failures are caused by your edits, fix them and we'll re-verify. If they are pre-existing and unrelated to your change, briefly say so and stop.")})
					finishRepairSpan("ok", map[string]string{"next": "model_repair"})
					continue
				}
				if vran {
					testResult = "passed"
					finishCheckSpan("ok", map[string]string{"result": "passed", "outputChars": fmt.Sprintf("%d", len(vout))})
					eventCh <- Event{Type: "status", Data: "Auto-verify: tests passed"}
				} else {
					finishCheckSpan("skipped", map[string]string{})
				}
			}

			// Completion summary: a one-line recap of what changed this session,
			// appended after the model's final answer (persists in both clients).
			if didEdit {
				files := uniqueStrings(editedFiles)
				summary := fmt.Sprintf("바꾼 파일 %d개", len(files))
				if len(files) > 0 {
					summary += ": " + strings.Join(files, ", ")
				}
				if testResult != "" {
					summary += " · 테스트 " + testResultLabel(testResult)
				}
				summary += fmt.Sprintf(" · %d회 반복", i+1)
				eventCh <- Event{Type: "text", Data: "\n\n---\n[요약] " + summary}
			}

			receiptPath := ""
			if didEdit || planMode {
				receipt := AgentReceipt{
					Provider:     provider.Name(),
					Model:        model,
					ProjectType:  project.Type,
					PlanMode:     planMode,
					Iterations:   i + 1,
					EditedFiles:  uniqueStrings(editedFiles),
					Verification: receiptVerification(testResult),
				}
				if path, err := writeAgentReceipt(workDir, receipt); err != nil {
					log.Printf("[Agent] receipt write failed: %v", err)
				} else {
					receiptPath = path
					eventCh <- Event{Type: "status", Data: "Receipt saved: " + path}
					if opts.Recorder != nil {
						opts.Recorder.ReceiptWritten(path, receipt)
					}
				}
			}

			// ── Memory hooks: extract durable memories from this
			//    conversation and (maybe) consolidate. Both run in
			//    background goroutines and their failures are logged,
			//    never surfaced to the user. We only hook normal
			//    termination — the max-iterations branch below is a
			//    failure mode that would feed noisy data to extraction.
			ExtractMemoriesAsync(ctx, provider, model, workDir, messages)
			MaybeConsolidateAsync(ctx, provider, model, workDir)
			// Auto-skill creation (opt-in via ANICLEW_AUTOSKILL): if this was
			// a complex, repeatable workflow, author a reusable SKILL.md in
			// the background. Gated and best-effort like the memory hooks.
			CreateSkillAsync(ctx, provider, model, workDir, messages)

			if planMode {
				eventCh <- Event{Type: "status", Data: "Plan ready — review it above, then reply to proceed with implementation."}
			}

			eventCh <- Event{Type: "done", Data: map[string]interface{}{
				"iterations":    i + 1,
				"tokenEstimate": tokenEstimate,
				"project":       project.Type,
				"planMode":      planMode,
				"receipt":       receiptPath,
			}}
			if opts.Recorder != nil {
				opts.Recorder.RunCompleted(RunSummary{
					Provider:     provider.Name(),
					Model:        model,
					ProjectType:  project.Type,
					PlanMode:     planMode,
					Iterations:   i + 1,
					EditedFiles:  uniqueStrings(editedFiles),
					Verification: receiptVerification(testResult),
					ReceiptPath:  receiptPath,
				})
			}
			return
		}

		// Advance the read-only exploration budget, weighting content reads above
		// navigation (see exploreScore / iterationWeight).
		exploreScore += iterationWeight(toolUses)

		// Note file edits so auto-verify knows to run tests at completion.
		for _, tu := range toolUses {
			if tu.Name == "Write" || tu.Name == "Edit" {
				didEdit = true
			}
		}

		// ── Build assistant message with tool_use blocks ──
		var assistantContent []map[string]interface{}
		if textContent != "" {
			assistantContent = append(assistantContent, map[string]interface{}{
				"type": "text", "text": textContent,
			})
		}
		for _, tu := range toolUses {
			assistantContent = append(assistantContent, map[string]interface{}{
				"type": "tool_use", "id": tu.ID, "name": tu.Name, "input": json.RawMessage(tu.InputRaw),
			})
		}
		messages = append(messages, types.Message{
			Role:    "assistant",
			Content: mustJSON(assistantContent),
		})

		// ── Partition tools into concurrent-safe vs serial ──
		var concurrentTools, serialTools []toolUseBlock
		for _, tu := range toolUses {
			inputMap := make(map[string]interface{})
			json.Unmarshal(tu.Input, &inputMap)
			if IsConcurrencySafe(tu.Name, inputMap) {
				concurrentTools = append(concurrentTools, tu)
			} else {
				serialTools = append(serialTools, tu)
			}
		}
		if len(concurrentTools) > 1 {
			log.Printf("[Agent] Parallel: %d concurrent + %d serial", len(concurrentTools), len(serialTools))
		}

		// ── Execute tools and collect results ──
		var toolResults []map[string]interface{}
		// First: run concurrent-safe tools in parallel
		if len(concurrentTools) > 1 {
			type toolResultEntry struct {
				idx    int
				result map[string]interface{}
				event  Event
			}
			resultCh := make(chan toolResultEntry, len(concurrentTools))

			for idx, tu := range concurrentTools {
				go func(i int, t toolUseBlock) {
					hookRegistry.Execute(hooks.HookPreToolUse, map[string]string{
						"TOOL_NAME": t.Name, "WORK_DIR": workDir,
					})
					r, isErr := ExecuteToolWithOptions(t.Name, t.Input, workDir, toolExecOptions)
					hookRegistry.Execute(hooks.HookPostToolUse, map[string]string{
						"TOOL_NAME": t.Name, "WORK_DIR": workDir,
						"TOOL_ERROR": fmt.Sprintf("%v", isErr),
					})
					resultCh <- toolResultEntry{
						idx: i,
						result: map[string]interface{}{
							"type": "tool_result", "tool_use_id": t.ID,
							"content": r, "is_error": isErr,
						},
						event: Event{Type: "tool_result", Data: map[string]interface{}{
							"id": t.ID, "name": t.Name, "result": truncateStr(r, 2000), "isError": isErr,
						}},
					}
				}(idx, tu)
			}

			// Collect parallel results
			collected := make([]toolResultEntry, len(concurrentTools))
			for i := 0; i < len(concurrentTools); i++ {
				entry := <-resultCh
				collected[entry.idx] = entry
			}
			for _, entry := range collected {
				eventCh <- entry.event
				toolResults = append(toolResults, entry.result)
			}
		} else {
			// Run single concurrent tool normally (falls through to serial loop)
			serialTools = append(concurrentTools, serialTools...)
		}

		// Then: run serial tools one by one
		for _, tu := range serialTools {
			log.Printf("[Agent] Executing: %s", tu.Name)

			// ── Pre-tool hook ──
			hookRegistry.Execute(hooks.HookPreToolUse, map[string]string{
				"TOOL_NAME": tu.Name, "WORK_DIR": workDir,
			})

			// ── Permission check (snapshot + legacy) ──
			permDecision := permissions.Decide(tu.Name, string(tu.InputRaw))

			permCfg := DefaultPermissionConfig()
			permCfg.AutoApprove = "moderate"
			allowed, permReason, dangerLevel := CheckPermission(tu.Name, tu.Input, workDir, permCfg)

			// Snapshot decision overrides if explicit
			if permDecision == "deny" {
				allowed = false
				permReason = "Denied by permission rule"
			} else if permDecision == "allow" {
				allowed = true
			} else if permDecision == "ask" && allowed {
				// Tool was allowed by legacy check but snapshot says "ask"
				// Persist this as an allow rule for future sessions
				hooks.PersistAllowRule(workDir, tu.Name, "")
				// Announce the first time we create .claude/settings.json so the
				// user knows a permission file was written to their workspace.
				if !permFileExisted && !permFileNotified && fileExists(claudeSettings) {
					permFileNotified = true
					eventCh <- Event{Type: "status", Data: "[Note] Created .claude/settings.json in this workspace to remember allowed tool permissions."}
				}
			}

			// Show tool input to client
			var inputPreview interface{}
			json.Unmarshal(tu.Input, &inputPreview)
			eventCh <- Event{Type: "tool_input", Data: map[string]interface{}{
				"id": tu.ID, "name": tu.Name, "input": inputPreview,
				"danger": string(dangerLevel),
			}}

			// Plan mode hard-enforces read-only: block any non-read-only tool the
			// model emitted (the backend can parse a tool call that was withheld
			// from the tools list), so plan mode never edits or runs commands.
			if planMode && !planModeTools[tu.Name] {
				allowed = false
				permReason = "Plan mode is read-only — describe this change in your plan instead of editing or running commands"
			}

			if !allowed {
				eventCh <- Event{Type: "tool_result", Data: map[string]interface{}{
					"id": tu.ID, "name": tu.Name,
					"result": fmt.Sprintf("[BLOCKED] %s", permReason), "isError": true,
				}}
				toolResults = append(toolResults, map[string]interface{}{
					"type": "tool_result", "tool_use_id": tu.ID,
					"content": fmt.Sprintf("Permission denied: %s", permReason), "is_error": true,
				})
				continue
			}

			// Capture pre-edit state so the user can be shown a diff of the change
			// (Write reads the old file now; Edit uses its old_string).
			var diffFile, diffBefore string
			if tu.Name == "Edit" || tu.Name == "Write" {
				diffFile = editFilePath(tu.Input)
				diffBefore = editFileBefore(tu.Name, tu.Input, workDir)
				// Snapshot the file's prior state for /undo (new generation on the
				// turn's first edit).
				if !checkpointStarted {
					startCheckpoint(workDir)
					checkpointStarted = true
				}
				checkpointFile(workDir, diffFile, resolvePath(diffFile, workDir))
			}

			result, isError := ExecuteToolWithOptions(tu.Name, tu.Input, workDir, toolExecOptions)

			// ── Post-tool hook ──
			hookRegistry.Execute(hooks.HookPostToolUse, map[string]string{
				"TOOL_NAME": tu.Name, "WORK_DIR": workDir,
				"TOOL_RESULT": truncateStr(result, 500),
				"TOOL_ERROR":  fmt.Sprintf("%v", isError),
			})

			// Send result to client
			eventCh <- Event{Type: "tool_result", Data: map[string]interface{}{
				"id": tu.ID, "name": tu.Name, "result": truncateStr(result, 2000), "isError": isError,
			}}

			// Show the edit as a before/after diff so the user sees exactly what
			// changed — the Claude-Code-style edit preview.
			if diffFile != "" && !isError {
				if d := unifiedLineDiff(diffBefore, editFileAfter(tu.Name, tu.Input)); d != "" {
					eventCh <- Event{Type: "diff", Data: map[string]string{"file": diffFile, "diff": d}}
				}
				editedFiles = append(editedFiles, diffFile) // deduped in the summary
			}

			toolResults = append(toolResults, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": tu.ID,
				"content":     result,
				"is_error":    isError,
			})
		}

		// ── Add tool results as user message ──
		messages = append(messages, types.Message{
			Role:    "user",
			Content: mustJSON(toolResults),
		})

		// ── Reflection guard ──
		// A round where every tool errored counts as a failed round; any
		// success resets the counter. Too many failed rounds in a row means
		// the model is stuck (e.g. repeating an edit the lint gate rejects),
		// so stop instead of looping all the way to maxIterations.
		if allToolsErrored(toolResults) {
			consecutiveErrorRounds++
		} else {
			consecutiveErrorRounds = 0
		}
		if consecutiveErrorRounds >= maxErrorRounds {
			msg := fmt.Sprintf(
				"Stopped after %d consecutive failed tool rounds — the model appears "+
					"stuck repeating a failing action. Try rephrasing the request.",
				consecutiveErrorRounds)
			eventCh <- Event{Type: "error", Data: msg}
			failRun(opts.Recorder, msg)
			hookRegistry.Execute(hooks.HookSessionEnd, map[string]string{"WORK_DIR": workDir})
			return
		}

		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Iteration %d/%d — %d tools executed", i+1, maxIterations, len(toolUses))}
	}

	hookRegistry.Execute(hooks.HookSessionEnd, map[string]string{"WORK_DIR": workDir})
	eventCh <- Event{Type: "error", Data: "Max iterations reached"}
	failRun(opts.Recorder, "Max iterations reached")
}

type toolUseBlock struct {
	ID       string
	Name     string
	InputRaw string
	Input    json.RawMessage
}

// allToolsErrored reports whether every tool result in a round is an error
// (and there is at least one). Used by the reflection guard in RunLoop.
func allToolsErrored(toolResults []map[string]interface{}) bool {
	if len(toolResults) == 0 {
		return false
	}
	for _, r := range toolResults {
		if e, ok := r["is_error"].(bool); !ok || !e {
			return false
		}
	}
	return true
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func testResultLabel(testResult string) string {
	switch testResult {
	case "passed":
		return "통과"
	case "failed":
		return "실패"
	default:
		return testResult
	}
}

func mustJSON(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
