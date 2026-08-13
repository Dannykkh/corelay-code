package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// defaultLocalToolBudget caps the tool count for local providers (Ollama,
// SGLang) whose context windows are small. 16 keeps the 8 core file/exec tools
// plus the 8 most task-relevant extras — enough for real coding work while
// roughly halving the tool-definition tokens in the system prompt. Overridable
// via config (localToolBudget) or env (CORELAY_MAX_TOOLS; legacy
// ANICLEW_MAX_TOOLS remains supported).
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
func isNavTool(name string) bool { return name == "LS" || name == "Glob" || name == "RepoMap" }

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
var planModeTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true, "RepoMap": true,
	loadToolResultToolName: true, reportCompletionToolName: true,
}

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

func renderToolResultInventory(references []ToolResultReference) string {
	if len(references) == 0 {
		return ""
	}
	verified := make([]ToolResultReference, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		digest := normalizeResultID(reference.ID)
		if !validResultDigest(digest) || reference.Digest != "sha256:"+digest || reference.Size <= 0 {
			continue
		}
		id := "result_" + digest
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		reference.ID = id
		verified = append(verified, reference)
	}
	if len(verified) == 0 {
		return ""
	}
	sort.Slice(verified, func(i, j int) bool { return verified[i].ID < verified[j].ID })
	var builder strings.Builder
	builder.WriteString("\n\n## Available durable tool results\n")
	builder.WriteString("These IDs are referenced by the committed session transcript and verified in this workspace. Use LoadToolResult with offset/limit to read bounded chunks.\n")
	for _, reference := range verified {
		fmt.Fprintf(&builder, "- %s (%d bytes, %s)\n", reference.ID, reference.Size, reference.Digest)
	}
	return strings.TrimRight(builder.String(), "\n")
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

const baseSystemPrompt = `You are Corelay Code, an expert coding agent. You act by CALLING TOOLS, not by describing actions.

## Acting vs. talking (read this first)
The ONLY way to change the filesystem, run code, or inspect the project is to emit a tool call. Writing code or commands in your reply text does NOTHING — no file is created, nothing runs.
- When the user asks you to create/modify/run/check something, your response MUST contain a tool call. Do not answer with prose or a code block alone.
- NEVER claim you "created", "wrote", "updated", or "ran" anything unless a tool call actually did it in this turn. Saying so without a tool call is a hallucination and is wrong.
- Prefer acting over explaining. Make the tool call first; keep any prose short. After tools report success, give a brief confirmation of what the tools actually did.
- If a task needs several steps, call tools across multiple turns until it is genuinely done.

## Tools: Bash, Read, Write, Edit, Glob, Grep, Git, LS, RepoMap, WebSearch, WebFetch, WebResearch, TaskCreate/Update/List, NotebookRead/Edit, Screenshot, MouseClick, TypeText, OpenApp, FileManager, Clipboard

## Rules
- To create a new file, call Write. To change an existing file, Read it first, then call Edit.
- Use RepoMap for a bounded structural overview and Glob/Grep to find files instead of guessing paths
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

type resolvedRunHarness struct {
	Profile harness.HarnessProfile
	Label   string
	Matched bool
}

func resolveRunHarness(
	provider types.Provider,
	model string,
	override *harness.HarnessProfile,
	cfg config.Config,
	environmentToolBudget int,
) (resolvedRunHarness, error) {
	return resolveRunHarnessWithCapability(
		provider,
		model,
		override,
		cfg,
		environmentToolBudget,
		nil,
		time.Now(),
	)
}

func resolveRunHarnessWithCapability(
	provider types.Provider,
	model string,
	override *harness.HarnessProfile,
	cfg config.Config,
	environmentToolBudget int,
	empirical *capabilityprofile.AutomaticSelection,
	now time.Time,
) (resolvedRunHarness, error) {
	if override != nil {
		profile := *override
		if !profile.Valid() {
			return resolvedRunHarness{}, fmt.Errorf("explicit HarnessProfile is unresolved")
		}
		return resolvedRunHarness{
			Profile: profile,
			Label:   profile.ID(),
			Matched: true,
		}, nil
	}

	local := isLocalProvider(provider.Name())
	var base harness.HarnessProfile
	label := "provider-default"
	matched := false
	if local {
		legacy, legacyMatched := profileFor(model)
		base = legacy.HarnessProfile()
		label = legacy.name
		matched = legacyMatched
	} else {
		id := strings.TrimSpace(provider.Name())
		if id == "" {
			id = "provider"
		}
		var err error
		base, err = harness.ResolveProfile(harness.ProfileSpec{
			ID: id + "-default",
		})
		if err != nil {
			return resolvedRunHarness{}, err
		}
		label = base.ID()
	}

	explicitToolBudget := 0
	if environmentToolBudget > 0 {
		explicitToolBudget = environmentToolBudget
	} else if local && cfg.LocalToolBudget > 0 {
		explicitToolBudget = cfg.LocalToolBudget
	}

	explicitTemperature := harness.OptionalFloat64{}
	if local && cfg.AgentTemperature != nil {
		explicitTemperature = harness.SomeFloat64(*cfg.AgentTemperature)
	}

	info, hasModelInfo := providerModelInfo(provider, model)
	modelContextWindow := 0
	modelOutputLimit := 0
	if hasModelInfo {
		if info.ContextWindow > 0 {
			modelContextWindow = info.ContextWindow
		}
		if info.MaxOutput > 0 {
			modelOutputLimit = info.MaxOutput
		}
	}
	composition, err := harness.ComposeProfile(harness.CompositionInput{
		Base:                base,
		ProviderName:        strings.TrimSpace(provider.Name()),
		Model:               strings.TrimSpace(model),
		Now:                 now,
		ModelContextWindow:  modelContextWindow,
		ModelOutputLimit:    modelOutputLimit,
		Empirical:           empirical,
		ExplicitToolBudget:  explicitToolBudget,
		ExplicitTemperature: explicitTemperature,
	})
	if err != nil {
		return resolvedRunHarness{}, fmt.Errorf("resolve run HarnessProfile: %w", err)
	}
	if composition.EmpiricalApplied {
		label = "empirical:" + composition.EmpiricalProfileID
		matched = true
	}
	return resolvedRunHarness{Profile: composition.Profile, Label: label, Matched: matched}, nil
}

func providerModelInfo(provider types.Provider, model string) (types.ModelInfo, bool) {
	for _, info := range provider.Models() {
		if strings.EqualFold(strings.TrimSpace(info.ID), strings.TrimSpace(model)) {
			return info, true
		}
	}
	return types.ModelInfo{}, false
}

func renderRunPlanAnchor(
	profile harness.HarnessProfile,
	anchor *PlanAnchor,
) (string, error) {
	if profile.PlanAnchorMode() == harness.PlanAnchorOff {
		return "", nil
	}
	if anchor == nil {
		return "", fmt.Errorf(
			"HarnessProfile %q requires a PlanAnchor",
			profile.ID(),
		)
	}
	return anchor.Render(profile.PlanAnchorMode())
}

func appendPromptPolicy(base, suffix, planAnchor string) string {
	for _, section := range []string{suffix, planAnchor} {
		if section = strings.TrimSpace(section); section != "" {
			base += "\n\n" + section
		}
	}
	return base
}

func requestMaxTokens(profile harness.HarnessProfile) int {
	const existingDefault = 8192
	if reserve := profile.OutputReserve(); reserve > 0 && reserve < existingDefault {
		return reserve
	}
	return existingDefault
}

func shouldCompactForHarness(
	profile harness.HarnessProfile,
	compactFailures int,
	currentTokens int,
) bool {
	if !profile.Valid() || compactFailures >= maxCompactFailures {
		return false
	}
	usable, err := profile.UsableInputTokens(0, 0)
	if err != nil {
		return true
	}
	margin := compactMargin
	if margin >= usable {
		margin = usable / 10
	}
	threshold := usable - margin
	return currentTokens > threshold
}

func estimateRequestTokens(
	messages []types.Message,
	systemPrompt string,
	tools []types.ToolDef,
) int {
	estimate, err := (ConservativeTokenEstimator{}).EstimateTokens(ContextEstimateRequest{
		Protocol: harness.WireAuto,
		Request: types.MessagesRequest{
			System:   mustJSON([]map[string]string{{"type": "text", "text": systemPrompt}}),
			Messages: messages,
			Tools:    tools,
		},
	})
	if err != nil {
		return 0
	}
	return estimate.InputTokens + estimate.ProtocolOverheadTokens
}

// Event is sent to the client via SSE during the agent loop.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// ContextBlockedEvent is the typed compatibility event emitted before the
// common terminal done frame. Plan contains budget metadata only; Message is
// sanitized and bounded before it reaches clients/recorders.
type ContextBlockedEvent struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Plan    ContextPlan `json:"plan"`
}

func emitContextBlocked(
	eventCh chan<- Event,
	terminal *runTerminalFinalizer,
	recorder RunRecorder,
	plan ContextPlan,
	code string,
	message string,
) {
	if code == "" {
		code = "context_blocked"
	}
	message = sanitizeSnapshotText(message, 1000)
	if message == "" {
		message = "The completed request cannot fit the selected model context window."
	}
	plan.Blocked = true
	plan.BlockCode = code
	plan.Fits = false
	plan.NeedsCompaction = false
	recordContextPlan(recorder, plan)
	eventCh <- Event{Type: "context_blocked", Data: ContextBlockedEvent{
		Code:    code,
		Message: message,
		Plan:    plan,
	}}
	eventCh <- Event{Type: "error", Data: message}
	terminal.Fail(
		RunTerminalContextBlocked,
		code,
		message,
		map[string]interface{}{"contextCode": code},
	)
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

// isExplicitUndoRequest defines the consent boundary for /undo. Only an exact
// top-level role=user slash command qualifies. Team workers are excluded
// because their role=user prompt is generated by the orchestrator/model, and
// assistant or tool-result messages cannot trigger this pre-loop branch.
func isExplicitUndoRequest(messages []types.Message, opts RunOptions) bool {
	if len(messages) == 0 || strings.TrimSpace(opts.WorkerID) != "" || opts.RunMode != nil {
		return false
	}
	lastMessage := messages[len(messages)-1]
	if lastMessage.Role != "user" {
		return false
	}
	var command string
	return json.Unmarshal(lastMessage.Content, &command) == nil &&
		strings.EqualFold(strings.TrimSpace(command), "/undo")
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
	terminal := newRunTerminalFinalizer(eventCh, opts.Recorder)
	defer terminal.Finalize()

	responseLang := opts.ResponseLang
	if responseLang == "" {
		responseLang = "auto"
	}

	messages := make([]types.Message, len(userMessages))
	copy(messages, userMessages)
	interactiveDirectives := strings.TrimSpace(opts.WorkerID) == "" && opts.RunMode == nil

	// /undo is an explicit user-consent command. It is handled before any model
	// call, but only after the role/worker boundary above has rejected messages
	// that a model, tool result, or team orchestrator could have produced.
	if isExplicitUndoRequest(messages, opts) {
		reverted, ok, err := undoCheckpointSecure(workDir)
		if err != nil {
			log.Printf("[Agent] /undo safety validation failed: %v", err)
			if ok {
				eventCh <- Event{Type: "text", Data: "되돌렸지만 체크포인트 정리에 실패했습니다:\n- " + strings.Join(reverted, "\n- ")}
			} else {
				eventCh <- Event{Type: "text", Data: "안전 검증에 실패하여 변경을 되돌리지 않았습니다."}
			}
		} else if ok {
			eventCh <- Event{Type: "text", Data: "되돌렸습니다:\n- " + strings.Join(reverted, "\n- ")}
		} else {
			eventCh <- Event{Type: "text", Data: "되돌릴 변경이 없습니다."}
		}
		terminal.Complete(
			RunTerminalCommand,
			"undo",
			RunTerminalDurableNone,
			nil,
			RunSummary{},
		)
		return
	}

	cfg := config.Load()
	runHarness, err := resolveRunHarnessWithCapability(
		provider,
		model,
		opts.HarnessProfile,
		cfg,
		translate.ToolBudget(),
		opts.CapabilityProfile,
		time.Now(),
	)
	if err != nil {
		message := "HarnessProfile resolution failed: " + err.Error()
		terminal.Fail(RunTerminalFailed, "harness_resolution_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	compiledHarness := runHarness.Profile
	runModeSystemSuffix, err := resolveRunMode(opts.RunMode)
	if err != nil {
		message := "Run mode resolution failed: " + err.Error()
		terminal.Fail(RunTerminalFailed, "run_mode_resolution_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	maxIterations, err := effectiveRunIterationLimit(compiledHarness, opts.IterationLimit)
	if err != nil {
		message := "Run mode resolution failed: " + err.Error()
		terminal.Fail(RunTerminalFailed, "iteration_limit_resolution_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	sandboxRunner, sandboxPolicy, err := resolveRunSandboxExecution(opts, workDir)
	if err != nil {
		message := "Sandbox configuration failed: " + err.Error()
		terminal.Fail(RunTerminalFailed, "sandbox_configuration_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	planAnchorPrompt, err := renderRunPlanAnchor(compiledHarness, opts.PlanAnchor)
	if err != nil {
		message := "PlanAnchor resolution failed: " + err.Error()
		terminal.Fail(RunTerminalFailed, "plan_anchor_resolution_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	activeRunID := newActiveRunID()
	evidence := NewEvidenceLedger(lastUserText(userMessages), opts.EvidencePolicy)
	completionContract, err := newRunCompletionContract(
		compiledHarness,
		opts.PlanAnchor,
		activeRunID,
		opts.CompletionEvidenceNotRequiredCriteria(),
	)
	if err != nil {
		message := "CompletionContract resolution failed: " + sanitizeSnapshotText(err.Error(), 1000)
		terminal.Fail(RunTerminalFailed, "completion_contract_resolution_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	if completionContract != nil {
		if bindErr := evidence.BindCompletionRun(activeRunID); bindErr != nil {
			message := "Completion evidence binding failed"
			terminal.Fail(RunTerminalFailed, "completion_evidence_binding_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
	}
	activePlanStep := ""
	if opts.PlanAnchor != nil {
		activePlanStep = opts.PlanAnchor.CurrentStep()
	}

	// Connect before catalog construction so the first model request sees the
	// exact MCP definitions bound to this run's live executor generation. No
	// process-global client registry participates in this composition.
	mcpConfigDetected := false
	mcpExecution := DefaultMCPExecutionOptions(ctx, workDir)
	if opts.MCPExecution != nil {
		mcpExecution = *opts.MCPExecution
		mcpExecution.Context = ctx
	}
	var mcpReportsMu sync.Mutex
	var mcpReports []processsupervisor.Report
	configuredObserver := mcpExecution.ObserveStart
	mcpExecution.ObserveStart = func(report processsupervisor.Report) {
		if configuredObserver != nil {
			configuredObserver(report)
		}
		mcpReportsMu.Lock()
		mcpReports = append(mcpReports, report)
		mcpReportsMu.Unlock()
		recordMCPExecution(opts.Recorder, report)
		eventCh <- Event{Type: "mcp_process", Data: report}
	}
	var runMCP = opts.MCPRuntime
	ownedRunMCP := false
	mcpGeneration := ""
	var mcpServers []MCPServerSpec
	var mcpErr error
	if runMCP != nil {
		if opts.MCPServers != nil || !opts.DisableWorkspaceMCP {
			message := "MCP configuration failed: borrowed runtime cannot be merged with another MCP authority"
			terminal.Fail(RunTerminalFailed, "mcp_configuration_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		mcpConfigDetected = true
	} else {
		var configured bool
		mcpServers, configured, mcpErr = resolveRunMCPServerSpecs(opts, workDir)
		mcpConfigDetected = configured
		if mcpErr != nil {
			message := "MCP configuration failed: " + mcpErr.Error()
			terminal.Fail(RunTerminalFailed, "mcp_configuration_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
	}
	if runMCP == nil && len(mcpServers) > 0 {
		factory := opts.MCPRuntimeFactory
		if factory == nil {
			factory = NewMCPRuntime
		}
		runMCP, mcpErr = factory(ctx, workDir, cloneMCPServerSpecs(mcpServers), mcpExecution)
		if mcpErr != nil || runMCP == nil {
			// Runtime factories receive secret-bearing process specs. Never copy
			// their error text into an event, recorder, receipt, or trace.
			message := "MCP connection failed: secure stdio runtime unavailable"
			terminal.Fail(RunTerminalFailed, "mcp_connection_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		ownedRunMCP = true
	}
	if runMCP != nil {
		if !runMCP.Healthy() {
			if ownedRunMCP {
				runMCP.Close()
			}
			message := "MCP connection failed: runtime generation is not healthy"
			terminal.Fail(RunTerminalFailed, "mcp_connection_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		if ownedRunMCP {
			defer runMCP.Close()
		} else {
			for _, report := range runMCP.Reports() {
				mcpReportsMu.Lock()
				mcpReports = append(mcpReports, report)
				mcpReportsMu.Unlock()
				recordMCPExecution(opts.Recorder, report)
				eventCh <- Event{Type: "mcp_process", Data: report}
			}
		}
		mcpGeneration = runMCP.Generation()
		eventCh <- Event{Type: "mcp_runtime", Data: map[string]interface{}{
			"generation": mcpGeneration,
			"servers":    runMCP.ServerCount(),
			"borrowed":   !ownedRunMCP,
		}}
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Connected to %d MCP servers", runMCP.ServerCount())}
	}

	baseTools := staticToolDefsWithResultReader(workDir, opts.ToolResultReader)
	if runMCP != nil {
		baseTools = append(baseTools, runMCP.ToolDefs()...)
	}
	pluginTools, pluginErr := resolveRunPluginToolDefs(opts, workDir, sandboxRunner)
	if pluginErr != nil {
		detail := sanitizeSnapshotText(pluginErr.Error(), 1000)
		if explicitRunPluginConfiguration(opts) {
			message := "Executable plugin configuration failed: " + detail
			terminal.Fail(RunTerminalFailed, "plugin_configuration_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		// Default discovery is optional. Reject the whole plugin set and keep the
		// base catalog; never downgrade to an unconfined plugin runner.
		log.Printf("[Agent] executable plugins not advertised: %s", detail)
		eventCh <- Event{Type: "status", Data: "Executable plugins not advertised: " + detail}
	} else if len(pluginTools) > 0 {
		baseTools = append(baseTools, pluginTools...)
		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Loaded %d executable plugin tool(s)", len(pluginTools))}
	}
	baseTools, err = pinCompletionControlTool(baseTools, completionContract)
	if err != nil {
		message := "Tool catalog construction failed: " + sanitizeSnapshotText(err.Error(), 1000)
		terminal.Fail(RunTerminalFailed, "tool_catalog_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	// Plugin definitions enter the same immutable catalog, pruning and routing
	// pipeline as built-ins and MCP. No parallel plugin-only loop exists.
	tools := applyEditPolicyToToolDefs(baseTools, compiledHarness.EditPolicy())
	toolExecOptions := ToolExecutionOptions{
		WorkerID:                   opts.WorkerID,
		OwnershipChecker:           opts.OwnershipChecker,
		ExpectedSessionID:          opts.SessionID,
		ExpectedRunID:              activeRunID,
		Context:                    ctx,
		SandboxRunner:              sandboxRunner,
		SandboxPolicy:              sandboxPolicy,
		EditPolicy:                 compiledHarness.EditPolicy(),
		ToolResultReader:           opts.ToolResultReader,
		CompletionContract:         completionContract,
		CompletionEvidenceResolver: evidence.ResolveCompletionEvidence,
	}
	var sandboxReports sync.Map
	var sandboxRecordsMu sync.Mutex
	var sandboxRecords []SandboxExecutionRecord

	// Plan mode: "/plan <task>" makes the agent explore read-only and produce a
	// step-by-step plan WITHOUT editing — the Claude-Code plan-then-execute flow.
	// We strip the "/plan" prefix, restrict tools to read-only, and route output
	// through the exploration guard so a plan is always produced. The user reviews
	// it and asks to proceed in a follow-up (normal) turn.
	planMode := false
	planTask := ""
	if interactiveDirectives && len(messages) > 0 {
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
	tools, err = pinCompletionControlTool(tools, completionContract)
	if err != nil {
		message := "Tool catalog construction failed: " + sanitizeSnapshotText(err.Error(), 1000)
		terminal.Fail(RunTerminalFailed, "tool_catalog_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	allowedTools := toolCatalogNamesForRun(tools)
	if _, catalogErr := snapshotForAllowedTools(allowedTools); catalogErr != nil {
		message := "Tool catalog construction failed: " + sanitizeSnapshotText(catalogErr.Error(), 1000)
		terminal.Fail(RunTerminalFailed, "tool_catalog_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}

	// @file mentions: "@path" in the message pulls that file's content into
	// context up front, so the model doesn't have to crawl to find it — a focused
	// alternative to exploration, especially helpful for local models.
	if interactiveDirectives && len(messages) > 0 {
		var last string
		if json.Unmarshal(messages[len(messages)-1].Content, &last) == nil && strings.Contains(last, "@") {
			if block, files := expandFileMentions(last, workDir); len(files) > 0 {
				messages[len(messages)-1] = types.Message{Role: "user", Content: mustJSON(block + "\n---\n" + last)}
				eventCh <- Event{Type: "status", Data: fmt.Sprintf("Loaded %d referenced file(s): %s", len(files), strings.Join(files, ", "))}
			}
		}
	}

	// The Compiled Harness was resolved once at run start. No config, model
	// metadata, or override is re-read inside the iteration loop.
	toolBudget := compiledHarness.ToolBudget()
	var agentTemp *float64
	if temperature, configured := compiledHarness.Temperature(); configured {
		value := temperature
		agentTemp = &value
	}
	if isLocalProvider(provider.Name()) && agentTemp != nil {
		eventCh <- Event{Type: "status", Data: fmt.Sprintf(
			"Model profile: %s (tools=%d, temp=%.1f)",
			runHarness.Label,
			toolBudget,
			*agentTemp,
		)}
	}
	log.Printf(
		"[Agent] harness=%q label=%q matched=%v budget=%d context=%d reserve=%d model=%s",
		compiledHarness.ID(),
		runHarness.Label,
		runHarness.Matched,
		toolBudget,
		compiledHarness.ContextWindow(),
		compiledHarness.OutputReserve(),
		model,
	)
	if toolBudget > 0 {
		var dropped int
		var pruneErr error
		tools, dropped, pruneErr = pruneToolsPreservingRequired(
			tools,
			lastUserText(messages),
			toolBudget,
			completionRequiredToolNames(completionContract),
		)
		if pruneErr != nil {
			message := "Tool budget construction failed: " + sanitizeSnapshotText(pruneErr.Error(), 1000)
			terminal.Fail(RunTerminalFailed, "tool_budget_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		if dropped > 0 {
			log.Printf("[Agent] tool budget %d: kept %d, dropped %d (provider=%s)", toolBudget, len(tools), dropped, provider.Name())
		}
	}
	routing, err := newToolRoutingState(compiledHarness, tools, lastUserText(messages))
	if err != nil {
		message := "Tool routing configuration failed: " + err.Error()
		terminal.Fail(RunTerminalFailed, "tool_routing_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	tools, err = pinCompletionControlTool(routing.tools(), completionContract)
	if err != nil {
		message := "Tool routing configuration failed: " + sanitizeSnapshotText(err.Error(), 1000)
		terminal.Fail(RunTerminalFailed, "tool_routing_failed", message, nil)
		eventCh <- Event{Type: "error", Data: message}
		return
	}
	if compiledHarness.ToolRouting() != harness.ToolRoutingDirect {
		record := routing.record()
		record.Exposed = len(tools)
		eventCh <- Event{Type: "tool_route", Data: record}
		log.Printf(
			"[Agent] tool routing policy=%s phase=%s category=%s exposed=%d/%d confidence=%.2f fallback=%v",
			record.Policy,
			record.Phase,
			record.Category,
			record.Exposed,
			record.Total,
			record.Confidence,
			record.Fallback,
		)
	}
	if opts.RunMode != nil {
		directive, modeErr := opts.RunMode.Start(time.Now())
		if modeErr != nil || validateRunModeDirective(directive, true) != nil {
			message := "Run mode start failed"
			terminal.Fail(RunTerminalFailed, "run_mode_start_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		for _, status := range directive.Status {
			eventCh <- Event{Type: "status", Data: status}
		}
		messages = appendRunModeDirective(messages, directive)
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

	// Reflection guard: stop if the model keeps producing tool calls that all
	// fail for several rounds in a row (e.g. repeating an edit the lint gate
	// rejects). Prevents burning iterations on a stuck self-correction loop.
	consecutiveErrorRounds := 0
	maxErrorRounds := compiledHarness.MaxErrorRounds()

	// ── Hook system: load from project + skill source ──
	hookRegistry := opts.HookRegistry
	if hookRegistry == nil {
		hookRegistry = hooks.NewRegistry()
	}
	_ = hookRegistry.Load(workDir, "") // "" = all sources; failures remain quarantined
	var hookResultsMu sync.Mutex
	var hookResults []hooks.HookResult
	executeHooksWithContext := func(hookCtx context.Context, hookType hooks.HookType, env map[string]string) []hooks.HookResult {
		results := hookRegistry.ExecuteContext(hookCtx, hookType, env)
		hookResultsMu.Lock()
		hookResults = append(hookResults, results...)
		recordHookResults(opts.Recorder, results)
		hookResultsMu.Unlock()
		return results
	}
	executeHooks := func(hookType hooks.HookType, env map[string]string) []hooks.HookResult {
		return executeHooksWithContext(ctx, hookType, env)
	}
	terminal.BeginHookSession(
		func() {
			executeHooks(hooks.HookSessionStart, map[string]string{"WORK_DIR": workDir})
		},
		func() {
			// SessionEnd is cleanup, so it must still run after the request
			// context is cancelled. Registry runner timeouts remain authoritative.
			executeHooksWithContext(
				context.WithoutCancel(ctx),
				hooks.HookSessionEnd,
				map[string]string{"WORK_DIR": workDir},
			)
		},
		func() []hooks.HookResult {
			hookResultsMu.Lock()
			defer hookResultsMu.Unlock()
			return append([]hooks.HookResult(nil), hookResults...)
		},
	)

	// ── Permission snapshot (immutable for this session) ──
	permissions := hooks.CapturePermissions(workDir)
	readLedger := NewReadLedger(workDir)
	runGuard := NewRunGuard(compiledHarness.RepeatLimit())

	// ── Detect project type ──
	project := DetectProject(workDir)
	projectPrompt := project.ToPrompt()
	eventCh <- Event{Type: "status", Data: fmt.Sprintf("Project: %s (%s, %d files)", project.Name, project.Type, project.FileCount)}

	// ── Load project context (CLAUDE.md, AGENTS.md, skills) ──
	projectCtx := LoadProjectContext(workDir)
	skills := LoadSkills(workDir)

	// ── Long-term memory: load index + top relevant entries for this turn ──
	// Computed once before the iteration loop — the snippet only depends
	// on the first user message, so recomputing per-iteration would just
	// defeat the prompt cache.
	memoryContext := BuildMemoryContext(workDir, messages)
	memoryContext += renderToolResultInventory(opts.ToolResultReferences)
	workstreamContext := ""
	if strings.TrimSpace(opts.WorkstreamContext) != "" {
		workstreamContext = "\n\n" + strings.TrimSpace(opts.WorkstreamContext)
	}
	// ── Process slash commands ──
	if interactiveDirectives && len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		var lastText string
		json.Unmarshal(lastMsg.Content, &lastText)
		if IsSlashCommand(lastText) {
			processed, err := ProcessSlashCommand(lastText, skills)
			if err != nil {
				terminal.Fail(RunTerminalFailed, "slash_command_failed", err.Error(), nil)
				eventCh <- Event{Type: "error", Data: err.Error()}
				return
			}
			// Direct output commands — don't send to LLM
			if processed == "[CLEAR_CHAT]" || processed == "[SHOW_MODEL_SELECTOR]" {
				eventCh <- Event{Type: "command", Data: processed}
				terminal.Complete(
					RunTerminalCommand,
					"ui_command",
					RunTerminalDurableNone,
					nil,
					RunSummary{Provider: provider.Name(), Model: model, ProjectType: project.Type},
				)
				return
			}
			if processed == "[COMPACT_CONTEXT]" {
				eventCh <- Event{Type: "status", Data: "Compressing context..."}
			}
			// /help → return directly, no LLM needed
			if strings.HasPrefix(lastText, "/help") {
				eventCh <- Event{Type: "text", Data: processed}
				terminal.Complete(
					RunTerminalCommand,
					"help",
					RunTerminalDurableNone,
					nil,
					RunSummary{Provider: provider.Name(), Model: model, ProjectType: project.Type},
				)
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
	if mcpConfigDetected {
		eventCh <- Event{Type: "status", Data: "MCP config detected"}
	}

	// First-run heads-up: surface agent-managed memory files before creation.
	// Permission ask decisions are never persisted implicitly.
	if msg := MemoryHeadsUp(workDir); msg != "" {
		eventCh <- Event{Type: "status", Data: msg}
	}

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
	recoverySnapshot := func() *RunGuardSnapshot {
		snapshot := runGuard.Snapshot()
		if snapshot.Denied == 0 {
			return nil
		}
		return &snapshot
	}
	writeRecoveryReceipt := func(iterations int, recovery *RunGuardSnapshot) string {
		if recovery == nil {
			return ""
		}
		verification := evidence.ApplyToReceipt(receiptVerification(testResult))
		verification.TerminalState = EvidenceTerminalBlocked
		verification.Gate = EvidenceGateBlock
		receipt := AgentReceipt{
			Provider:     provider.Name(),
			Model:        model,
			ProjectType:  project.Type,
			PlanMode:     planMode,
			Iterations:   iterations,
			EditedFiles:  uniqueStrings(editedFiles),
			Verification: verification,
			Recovery:     recovery,
		}
		path, receiptErr := terminal.WriteReceipt(workDir, receipt)
		if receiptErr != nil {
			log.Printf("[Agent] recovery receipt write failed: %v", receiptErr)
			return ""
		}
		eventCh <- Event{Type: "status", Data: "Receipt saved: " + path}
		return path
	}
	systemCorePrefix := buildSystemPrompt(responseLang) +
		projectPrompt +
		projectCtx +
		skillText
	systemCoreSuffix := workstreamContext
	if runModeSystemSuffix != "" {
		systemCoreSuffix += "\n\n" + runModeSystemSuffix
	}
	if planMode {
		systemCoreSuffix += "\n\n## PLAN MODE\nYou are in plan mode. Explore the codebase with the read-only tools and produce a concrete, step-by-step implementation plan (which files to change and what to do in each). You have NO edit tools — do not attempt to make changes. End with the plan; the user will review it and ask you to proceed."
	}
	systemCoreSuffix = appendPromptPolicy(
		systemCoreSuffix,
		compiledHarness.PromptSuffix(),
		planAnchorPrompt,
	)

	responseCorrections := newToolResponseCorrectionState()
	selectorCorrections := 0
	completionCorrections := 0
	compactionAttempts := 0
	runModeStopReason := ""
	runModeTerminalText := ""
	runModeTotalTools := 0
	runModeEstimatedTokens := 0
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
			tools, err = pinCompletionControlTool(nil, completionContract)
			if err != nil {
				message := "Tool catalog construction failed: " + sanitizeSnapshotText(err.Error(), 1000)
				terminal.Fail(RunTerminalFailed, "tool_catalog_failed", message, nil)
				eventCh <- Event{Type: "error", Data: message}
				return
			}
			readOnly = false // collapse once; the next pass produces the answer
			eventCh <- Event{Type: "status", Data: fmt.Sprintf("Read-only question — explored %d rounds, answering now", i)}
		}

		// Normalize before planning so the estimate describes the exact message
		// shape handed to the provider, including any role framing repairs.
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

		// Build and reduce the completed request. Every calculation includes the
		// full system prompt, schemas, messages, first-turn RAG, recalled memory,
		// output reservation, protocol framing, and safety margin.
		iterationSystemCoreSuffix := systemCoreSuffix
		if completionContract != nil {
			completionPrompt, promptErr := renderRunCompletionContract(
				completionContract,
				evidence.CompletionEvidenceSnapshot(),
			)
			if promptErr != nil {
				message := "CompletionContract prompt rendering failed"
				terminal.Fail(RunTerminalFailed, "completion_contract_render_failed", message, nil)
				eventCh <- Event{Type: "error", Data: message}
				return
			}
			iterationSystemCoreSuffix = appendPromptPolicy(iterationSystemCoreSuffix, completionPrompt, "")
		}

		planningInput := ContextPlanningRequest{
			Profile:  compiledHarness,
			Protocol: compiledHarness.WirePolicy(),
			Model:    model,
			System: ContextSystemSections{
				CorePrefix:    systemCorePrefix,
				RAGContext:    ragContext,
				MemoryContext: memoryContext,
				CoreSuffix:    iterationSystemCoreSuffix,
			},
			Messages:           messages,
			Tools:              tools,
			RequiredToolNames:  completionRequiredToolNames(completionContract),
			MaxTokens:          requestMaxTokens(compiledHarness),
			Temperature:        agentTemp,
			Estimator:          opts.TokenEstimator,
			SafetyMarginTokens: DefaultContextSafetyMarginTokens,
			Task:               lastUserText(userMessages),
			ContextReducer:     opts.ContextReducer,
			ToolResultStore:    opts.ToolResultStore,
		}
		planned, planErr := PlanContextRequest(planningInput)
		if planErr != nil {
			code := "context_planning_failed"
			plan := ContextPlan{}
			if typed, ok := planErr.(*ContextBudgetError); ok {
				code = typed.Code
				plan = typed.Plan
			}
			emitContextBlocked(eventCh, terminal, opts.Recorder, plan, code, planErr.Error())
			return
		}

		if planned.NeedsCompaction {
			if compactionAttempts >= maxCompactFailures {
				emitContextBlocked(
					eventCh,
					terminal,
					opts.Recorder,
					planned.Plan,
					"compaction_limit_reached",
					"Context remains over budget after the bounded compaction attempt limit.",
				)
				return
			}
			compactionAttempts++
			eventCh <- Event{Type: "status", Data: fmt.Sprintf(
				"Compacting completed request (~%dk input tokens, %d messages)...",
				planned.Plan.EstimatedInputTokens/1000,
				len(planned.Messages),
			)}
			outcome := compactPlannedContext(
				ctx,
				provider,
				hookRegistry,
				planningInput,
				planned,
				CompactionState{
					Objective:   lastUserText(userMessages),
					PlanAnchor:  opts.PlanAnchor,
					EditedFiles: uniqueStrings(editedFiles),
					Evidence:    evidence,
				},
			)
			if outcome.Snapshot.Version != 0 {
				recordCompaction(opts.Recorder, outcome.Snapshot)
				eventCh <- Event{Type: "compaction_snapshot", Data: outcome.Snapshot}
			}
			if outcome.Blocked || outcome.Err != nil {
				message := "The completed request remains over the selected model context budget after structured compaction."
				if outcome.Err != nil {
					message = "Structured context compaction could not produce a valid request: " + outcome.Err.Error()
				}
				emitContextBlocked(
					eventCh,
					terminal,
					opts.Recorder,
					outcome.Planned.Plan,
					outcome.BlockCode,
					message,
				)
				return
			}
			planned = outcome.Planned
			eventCh <- Event{Type: "status", Data: fmt.Sprintf(
				"Compacted to %d messages (%s)",
				len(planned.Messages),
				outcome.Snapshot.Strategy,
			)}
		}

		// Commit exactly the catalog/messages/system that were budgeted. The tool
		// dispatch allow-list below derives from this same tools slice.
		messages = planned.Messages
		tools = planned.Tools
		allowedTools = toolCatalogNamesForRun(tools)
		if _, catalogErr := snapshotForAllowedTools(allowedTools); catalogErr != nil {
			message := "Tool catalog construction failed: " + sanitizeSnapshotText(catalogErr.Error(), 1000)
			terminal.Fail(RunTerminalFailed, "tool_catalog_failed", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}
		memoryContext = planned.System.MemoryContext
		req := planned.Request
		tokenEstimate := planned.Plan.EstimatedInputTokens
		addRunModeTokenEstimate(&runModeEstimatedTokens, tokenEstimate)
		recordContextPlan(opts.Recorder, planned.Plan)
		eventCh <- Event{Type: "context_plan", Data: planned.Plan}

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
					cancelReason := "cancelled"
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						cancelReason = "deadline_exceeded"
					}
					terminal.Fail(RunTerminalCancelled, cancelReason, ctx.Err().Error(), nil)
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
		if err != nil {
			stopHeartbeat()
			finishMakerSpan("failed", map[string]string{"error": err.Error()})
			message := fmt.Sprintf("Failed after 3 retries: %s", err.Error())
			terminal.Fail(RunTerminalFailed, "provider_retry_exhausted", message, nil)
			eventCh <- Event{Type: "error", Data: message}
			return
		}

		// Collect response
		var textContent string
		var toolUses []toolUseBlock
		currentText := ""
		var currentTool *toolUseBlock
		var reasoningContent boundedReasoningBuffer
		stopReason := ""
		sawProviderEvent := false

		for event := range ch {
			sawProviderEvent = true
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
						reasoningContent.Append(thinkDelta.Thinking)
						eventCh <- Event{Type: "thinking", Data: thinkDelta.Thinking}
					}
				} else if delta.Type == "text_delta" {
					currentText += delta.Text
					atomic.AddInt64(&outChars, int64(len(delta.Text)))
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			cancelReason := "cancelled"
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				cancelReason = "deadline_exceeded"
			}
			terminal.Fail(RunTerminalCancelled, cancelReason, ctxErr.Error(), nil)
			return
		}
		if !sawProviderEvent {
			message := "Provider stream closed without a terminal event"
			eventCh <- Event{Type: "error", Data: message}
			terminal.Fail(RunTerminalNoTerminal, "provider_stream_closed", message, nil)
			return
		}

		hadNativeCalls := len(toolUses) > 0
		responseDecision := applyToolResponsePolicy(
			compiledHarness.ResponsePolicy(),
			textContent,
			reasoningContent.String(),
			toolUses,
			tools,
		)
		recordToolParseResult(eventCh, opts.Recorder, fmt.Sprintf("tool_parse_%02d", i+1), responseDecision.Parse)
		toolUses = responseDecision.Calls
		textContent = responseDecision.VisibleText

		if responseDecision.NeedsCorrection {
			correction, terminalErr := responseCorrections.next(responseDecision.Parse)
			if terminalErr != nil {
				terminal.Fail(RunTerminalFailed, "parser_correction_exhausted", terminalErr.Error(), nil)
				eventCh <- Event{Type: "error", Data: terminalErr}
				return
			}
			messages = append(messages, types.Message{Role: "user", Content: mustJSON(correction)})
			eventCh <- Event{Type: "status", Data: fmt.Sprintf(
				"Tool response correction requested (%d/%d): %s",
				responseCorrections.attempts,
				responseCorrections.limit,
				responseDecision.Parse.Reason,
			)}
			continue
		}

		if !hadNativeCalls && len(toolUses) > 0 {
			log.Printf("[Agent] recovered %d tool call(s) with response policy %s", len(toolUses), compiledHarness.ResponsePolicy())
			for _, call := range toolUses {
				eventCh <- Event{Type: "tool_start", Data: map[string]string{"id": call.ID, "name": call.Name}}
			}
		}
		selectorBypass := routing.awaitingSelector() && completionControlOnlyCalls(toolUses)
		if handled, assistantMessage, resultMessage, routeErr := routing.consumeSelector(
			func() []toolUseBlock {
				if selectorBypass {
					return nil
				}
				return toolUses
			}(),
			textContent,
		); handled {
			if routeErr != nil {
				selectorCorrections++
				if selectorCorrections > defaultToolResponseCorrectionLimit {
					message := "Tool routing selector failed after bounded correction attempts"
					terminal.Fail(RunTerminalFailed, "selector_correction_exhausted", message, nil)
					eventCh <- Event{Type: "error", Data: message}
					return
				}
				messages = append(messages, types.Message{Role: "user", Content: mustJSON(routing.selectorCorrection())})
				eventCh <- Event{Type: "status", Data: fmt.Sprintf(
					"Tool routing selector correction requested (%d/%d)",
					selectorCorrections,
					defaultToolResponseCorrectionLimit,
				)}
				continue
			}
			if textContent != "" && completionContract == nil {
				eventCh <- Event{Type: "text", Data: textContent}
			}
			messages = append(messages, assistantMessage, resultMessage)
			tools, err = pinCompletionControlTool(routing.tools(), completionContract)
			if err != nil {
				message := "Tool routing configuration failed"
				terminal.Fail(RunTerminalFailed, "tool_routing_failed", message, nil)
				eventCh <- Event{Type: "error", Data: message}
				return
			}
			allowedTools = toolCatalogNamesForRun(tools)
			record := routing.record()
			record.Exposed = len(tools)
			eventCh <- Event{Type: "tool_route", Data: record}
			continue
		}
		strictNoToolResponse := completionContract != nil && len(toolUses) == 0
		if textContent != "" && completionContract == nil {
			eventCh <- Event{Type: "text", Data: textContent}
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
			if opts.RunMode != nil {
				directive, modeErr := opts.RunMode.Advance(RunModeTurn{
					Text:      textContent,
					Iteration: i + 1,
					Now:       time.Now(),
				})
				if modeErr != nil || validateRunModeDirective(directive, false) != nil {
					message := "Run mode transition failed"
					terminal.Fail(RunTerminalFailed, "run_mode_transition_failed", message, nil)
					eventCh <- Event{Type: "error", Data: message}
					return
				}
				for _, status := range directive.Status {
					eventCh <- Event{Type: "status", Data: status}
				}
				if directive.Continue {
					if textContent != "" {
						messages = append(messages, types.Message{
							Role: "assistant",
							Content: mustJSON([]map[string]interface{}{{
								"type": "text", "text": textContent,
							}}),
						})
					}
					messages = appendRunModeDirective(messages, directive)
					continue
				}
				runModeStopReason = directive.StopReason
				runModeTerminalText = directive.TerminalText
			}
			// ── Auto-verify: before declaring done, run the project's tests if
			//    the model edited files (the Claude-Code "edit → test → fix"
			//    loop). On failure, feed the output back so the model fixes it —
			//    bounded by maxVerifyAttempts so an unrelated/pre-existing failure
			//    cannot loop forever. Skips silently when there is no test runner.
			contractBlockedBeforeVerify := completionContract != nil &&
				completionContract.Status() == CompletionStatusBlocked
			if !contractBlockedBeforeVerify && didEdit && autoVerifyEnabled() && verifyAttempts < maxVerifyAttempts {
				eventCh <- Event{Type: "status", Data: "Auto-verify: running tests after edits…"}
				finishCheckSpan := startRunSpan(opts.Recorder, fmt.Sprintf("checker_%02d_%02d", i+1, verifyAttempts+1), "checker.auto_verify", map[string]string{
					"iteration": fmt.Sprintf("%d", i+1),
					"attempt":   fmt.Sprintf("%d", verifyAttempts+1),
				})
				vout, vfailed, vran := runAutoVerify(workDir)
				evidence.ObserveAutoVerify(vout, vfailed, vran)
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

			var completionSnapshot *CompletionContractSnapshot
			completionBlocked := false
			completionIncompleteExhausted := false
			if completionContract != nil {
				snapshot, snapshotErr := completionContract.Snapshot()
				if snapshotErr != nil {
					message := "CompletionContract snapshot failed"
					terminal.Fail(RunTerminalFailed, "completion_contract_snapshot_failed", message, nil)
					eventCh <- Event{Type: "error", Data: message}
					return
				}
				completionSnapshot = &snapshot
				switch snapshot.Status {
				case CompletionStatusIncomplete:
					if completionCorrections < maxCompletionCorrectionAttempts {
						completionCorrections++
						if textContent != "" {
							messages = append(messages, types.Message{
								Role: "assistant",
								Content: mustJSON([]map[string]interface{}{{
									"type": "text", "text": textContent,
								}}),
							})
						}
						messages = append(messages, types.Message{
							Role:    "user",
							Content: mustJSON(completionCorrectionMessage(snapshot)),
						})
						eventCh <- Event{Type: "status", Data: fmt.Sprintf(
							"Completion contract correction requested (%d/%d)",
							completionCorrections,
							maxCompletionCorrectionAttempts,
						)}
						continue
					}
					completionBlocked = true
					completionIncompleteExhausted = true
				case CompletionStatusBlocked:
					completionBlocked = true
				case CompletionStatusComplete:
					// The machine-readable completion contract is satisfied. The
					// independent evidence policy below still owns verification.
				default:
					message := "CompletionContract status is invalid"
					terminal.Fail(RunTerminalFailed, "completion_contract_status_invalid", message, nil)
					eventCh <- Event{Type: "error", Data: message}
					return
				}
			}

			gate := evidence.Evaluate()
			if !completionBlocked && gate.Decision == EvidenceGateBlock {
				evidence.MarkStopBlock()
				eventCh <- Event{Type: "status", Data: "Evidence gate blocked completion: verification evidence required"}
				if textContent != "" {
					messages = append(messages, types.Message{Role: "assistant", Content: mustJSON([]map[string]interface{}{{"type": "text", "text": textContent}})})
				}
				messages = append(messages, types.Message{Role: "user", Content: mustJSON(
					"Evidence gate blocked completion: this deep task changed files but no successful verification evidence was recorded.\n\nRun the narrowest relevant verification command now, or explain why no verification is applicable. If verification fails because of this change, fix it before stopping.")})
				continue
			}
			if gate.Decision != EvidenceGateAllow {
				eventCh <- Event{Type: "status", Data: fmt.Sprintf("Evidence gate: %s — %s", gate.Decision, gate.Summary)}
			}
			completionVerification := evidence.ApplyToReceipt(receiptVerification(testResult))
			if completionBlocked {
				completionVerification.TerminalState = EvidenceTerminalBlocked
				eventCh <- Event{Type: "status", Data: "Completion contract blocked the run terminal"}
			}
			var runModeSnapshot *RunModeSnapshot
			if opts.RunMode != nil {
				sandboxRecordsMu.Lock()
				modeSandbox := append([]SandboxExecutionRecord(nil), sandboxRecords...)
				sandboxRecordsMu.Unlock()
				snapshot := opts.RunMode.Snapshot(RunModeMetrics{
					TotalTools:      runModeTotalTools,
					EstimatedTokens: runModeEstimatedTokens,
					Sandbox:         modeSandbox,
				})
				if snapshotErr := validateRunModeSnapshot(opts.RunMode, snapshot); snapshotErr != nil {
					message := "Run mode snapshot failed"
					terminal.Fail(RunTerminalFailed, "run_mode_snapshot_failed", message, nil)
					eventCh <- Event{Type: "error", Data: message}
					return
				}
				runModeSnapshot = &snapshot
			}
			if strictNoToolResponse && textContent != "" {
				if !completionIncompleteExhausted {
					eventCh <- Event{Type: "text", Data: textContent}
				}
			}
			if completionIncompleteExhausted {
				eventCh <- Event{Type: "text", Data: fmt.Sprintf(
					"Completion blocked: the Definition of Done remained incomplete after %d correction attempts.",
					maxCompletionCorrectionAttempts,
				)}
			}
			if !completionBlocked && runModeTerminalText != "" {
				eventCh <- Event{Type: "text", Data: runModeTerminalText}
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
			recovery := recoverySnapshot()
			if didEdit || planMode || completionSnapshot != nil || recovery != nil {
				receipt := AgentReceipt{
					Provider:     provider.Name(),
					Model:        model,
					ProjectType:  project.Type,
					PlanMode:     planMode,
					Iterations:   i + 1,
					EditedFiles:  uniqueStrings(editedFiles),
					Verification: completionVerification,
					Completion:   completionSnapshot,
					Recovery:     recovery,
				}
				if path, err := terminal.WriteReceipt(workDir, receipt); err != nil {
					log.Printf("[Agent] receipt write failed: %v", err)
				} else {
					receiptPath = path
					eventCh <- Event{Type: "status", Data: "Receipt saved: " + path}
				}
			}

			// ── Memory hooks: extract durable memories from this
			//    conversation and (maybe) consolidate. Both run in
			//    background goroutines and their failures are logged,
			//    never surfaced to the user. We only hook normal
			//    termination — the max-iterations branch below is a
			//    failure mode that would feed noisy data to extraction.
			if !completionBlocked {
				ExtractMemoriesAsync(ctx, provider, model, workDir, messages)
				MaybeConsolidateAsync(ctx, provider, model, workDir)
			}
			// Auto-skill creation (opt-in via CORELAY_AUTOSKILL): if this was
			// a complex, repeatable workflow, author a reusable SKILL.md in
			// the background. Gated and best-effort like the memory hooks.
			if !completionBlocked {
				CreateSkillAsync(ctx, provider, model, workDir, messages)
			}

			if planMode {
				eventCh <- Event{Type: "status", Data: "Plan ready — review it above, then reply to proceed with implementation."}
			}

			terminalStopReason := stopReason
			if runModeStopReason != "" {
				terminalStopReason = runModeStopReason
			}
			if completionBlocked {
				terminalStopReason = "completion_blocked"
			}
			doneData := map[string]interface{}{
				"iterations":    i + 1,
				"tokenEstimate": tokenEstimate,
				"project":       project.Type,
				"planMode":      planMode,
				"receipt":       receiptPath,
				"stopReason":    terminalStopReason,
				"terminalState": completionVerification.TerminalState,
			}
			for key, value := range completionDoneMetadata(completionSnapshot) {
				doneData[key] = value
			}
			if runModeSnapshot != nil {
				doneData["runMode"] = *runModeSnapshot
			}
			if recovery != nil {
				doneData["recovery"] = *recovery
			}
			sandboxRecordsMu.Lock()
			runSandboxRecords := append([]SandboxExecutionRecord(nil), sandboxRecords...)
			sandboxRecordsMu.Unlock()
			mcpReportsMu.Lock()
			runMCPReports := append([]processsupervisor.Report(nil), mcpReports...)
			mcpReportsMu.Unlock()
			runSummary := RunSummary{
				Provider:      provider.Name(),
				Model:         model,
				ProjectType:   project.Type,
				PlanMode:      planMode,
				Iterations:    i + 1,
				EditedFiles:   uniqueStrings(editedFiles),
				Verification:  completionVerification,
				ReceiptPath:   receiptPath,
				Sandbox:       runSandboxRecords,
				MCP:           runMCPReports,
				MCPGeneration: mcpGeneration,
				Completion:    completionSnapshot,
				RunMode:       runModeSnapshot,
				Recovery:      recovery,
			}
			switch {
			case completionBlocked:
				terminal.Fail(
					RunTerminalFailed,
					"completion_blocked",
					"Completion contract blocked the run terminal",
					doneData,
				)
			case runModeStopReason == "max_cycles":
				terminal.Fail(
					RunTerminalMaxCycles,
					"max_cycles",
					"Run mode reached its maximum cycle limit",
					doneData,
				)
			default:
				terminal.Complete(
					RunTerminalCompleted,
					terminalStopReason,
					RunTerminalDurableCommit,
					doneData,
					runSummary,
				)
			}
			return
		}

		// Advance the read-only exploration budget, weighting content reads above
		// navigation (see exploreScore / iterationWeight).
		exploreScore += iterationWeight(toolUses)
		runModeTotalTools += len(toolUses)

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

		permissionConfig := DefaultPermissionConfig()
		permissionConfig.AutoApprove = "moderate"
		dispatchResults := dispatchToolCalls(toolUses, toolDispatchOptions{
			Context:           ctx,
			WorkDir:           workDir,
			AllowedTools:      allowedTools,
			PlanMode:          planMode,
			PermissionConfig:  permissionConfig,
			ApprovalRequester: opts.ApprovalRequester,
			SessionID:         opts.SessionID,
			RunID:             activeRunID,
			ReadBeforeWrite:   compiledHarness.ReadBeforeWrite(),
			ReadLedger:        readLedger,
			RunGuard:          runGuard,
			PlanStep:          runModePlanStep(opts.RunMode, activePlanStep),
			SnapshotDecision: func(call toolUseBlock) string {
				return permissions.Decide(call.Name, call.InputRaw)
			},
			ScopeCheck: func(call toolUseBlock) (bool, string) {
				checker := opts.OwnershipChecker
				if checker == nil {
					checker = FileOwnershipChecker
				}
				workerID := strings.TrimSpace(opts.WorkerID)
				if workerID == "" {
					workerID = activeWorkerID
				}
				if checker == nil || workerID == "" ||
					(call.Name != "Write" && call.Name != "Edit") {
					return true, ""
				}
				return checker(workerID, dispatchFilePath(call.Input))
			},
			PreHook: func(call toolUseBlock) (bool, string) {
				for _, result := range executeHooks(
					hooks.HookPreToolUse,
					map[string]string{
						"TOOL_NAME": call.Name,
						"WORK_DIR":  workDir,
					},
				) {
					if result.Blocked {
						reason := result.Output
						if reason == "" {
							reason = result.Error
						}
						if reason == "" {
							reason = "Project hook rejected the tool call"
						}
						return true, reason
					}
				}
				return false, ""
			},
			PostHook: func(call toolUseBlock, result string, isError bool) {
				executeHooks(hooks.HookPostToolUse, map[string]string{
					"TOOL_NAME":   call.Name,
					"WORK_DIR":    workDir,
					"TOOL_RESULT": result,
					"TOOL_ERROR":  fmt.Sprintf("%v", isError),
				})
			},
			BeforeExecute: func(call toolUseBlock) toolMutationPreview {
				if call.Name != "Write" && call.Name != "Edit" {
					return toolMutationPreview{}
				}
				file := editFilePath(call.Input)
				preview := toolMutationPreview{
					File:   file,
					Before: editFileBefore(call.Name, call.Input, workDir),
				}
				if !checkpointStarted {
					startCheckpoint(workDir)
					checkpointStarted = true
				}
				checkpointFile(workDir, file, resolvePath(file, workDir))
				return preview
			},
			PreExecutionJournal: opts.PreExecutionJournal,
			Execute: func(call toolUseBlock) (string, bool) {
				log.Printf("[Agent] Executing: %s", call.Name)
				execOptions := toolExecOptions
				execOptions.ObserveSandbox = func(report sandbox.Report) {
					sandboxReports.Store(call.ID, report)
				}
				content, isError := ExecuteToolWithOptions(
					call.Name,
					call.Input,
					workDir,
					execOptions,
				)
				if !isError && call.Name != loadToolResultToolName && opts.ToolResultStore != nil {
					content, isError = persistSuccessfulToolResult(opts.ToolResultStore, call.Name, content)
				}
				return content, isError
			},
			Emit: func(event Event) {
				eventCh <- event
			},
		})

		concurrentExecuted := 0
		var toolResults []map[string]interface{}
		for _, result := range dispatchResults {
			if result.Concurrent && result.Executed {
				concurrentExecuted++
			}
			evidence.ObserveToolResult(
				result.Tool.Name,
				result.Tool.Input,
				result.Content,
				result.IsError,
			)
			if completionContract != nil {
				if _, evidenceErr := evidence.ObserveCompletionToolOutcome(CompletionToolOutcome{
					ToolName:  result.Tool.Name,
					Input:     result.Tool.Input,
					Result:    result.Content,
					IsError:   result.IsError,
					Executed:  result.Executed,
					Synthetic: result.Synthetic,
					Denied:    !result.Executed,
				}); evidenceErr != nil {
					message := "Completion evidence indexing failed"
					terminal.Fail(RunTerminalFailed, "completion_evidence_index_failed", message, nil)
					eventCh <- Event{Type: "error", Data: message}
					return
				}
			}
			toolEvent := map[string]interface{}{
				"id":       result.Tool.ID,
				"name":     result.Tool.Name,
				"result":   truncateStr(result.Display, 2000),
				"isError":  result.IsError,
				"executed": result.Executed,
			}
			if value, ok := sandboxReports.LoadAndDelete(result.Tool.ID); ok {
				report := value.(sandbox.Report)
				record := SandboxExecutionRecord{
					ToolID:   result.Tool.ID,
					ToolName: result.Tool.Name,
					Report:   report,
				}
				toolEvent["sandbox"] = report
				recordSandboxExecution(opts.Recorder, record)
				sandboxRecordsMu.Lock()
				sandboxRecords = append(sandboxRecords, record)
				sandboxRecordsMu.Unlock()
			}
			eventCh <- Event{Type: "tool_result", Data: toolEvent}

			if result.Executed && !result.IsError && isEditTool(result.Tool.Name) {
				didEdit = true
				file := result.Mutation.File
				if file == "" {
					file = dispatchFilePath(result.Tool.Input)
				}
				if file != "" {
					editedFiles = append(editedFiles, file)
				}
				if result.Mutation.File != "" {
					if diff := unifiedLineDiff(
						result.Mutation.Before,
						editFileAfter(result.Tool.Name, result.Tool.Input),
					); diff != "" {
						eventCh <- Event{Type: "diff", Data: map[string]string{
							"file": result.Mutation.File,
							"diff": diff,
						}}
					}
				}
			}

			toolResults = append(toolResults, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": result.Tool.ID,
				"content":     result.Content,
				"is_error":    result.IsError,
			})
		}
		if concurrentExecuted > 1 {
			log.Printf("[Agent] Parallel: %d authorized tools", concurrentExecuted)
		}
		if routing.observeDispatch(completionExecutionResults(dispatchResults)) {
			tools, err = pinCompletionControlTool(routing.tools(), completionContract)
			if err != nil {
				message := "Tool routing configuration failed"
				terminal.Fail(RunTerminalFailed, "tool_routing_failed", message, nil)
				eventCh <- Event{Type: "error", Data: message}
				return
			}
			record := routing.record()
			record.Exposed = len(tools)
			eventCh <- Event{Type: "tool_route", Data: record}
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
			recovery := recoverySnapshot()
			data := map[string]interface{}{}
			if recovery != nil {
				data["recovery"] = *recovery
				data["receipt"] = writeRecoveryReceipt(i+1, recovery)
			}
			eventCh <- Event{Type: "error", Data: msg}
			terminal.Fail(RunTerminalFailed, "consecutive_tool_failures", msg, data)
			return
		}

		eventCh <- Event{Type: "status", Data: fmt.Sprintf("Iteration %d/%d — %d tools executed", i+1, maxIterations, len(toolUses))}
	}

	recovery := recoverySnapshot()
	data := map[string]interface{}{}
	if recovery != nil {
		data["recovery"] = *recovery
		data["receipt"] = writeRecoveryReceipt(maxIterations, recovery)
	}
	eventCh <- Event{Type: "error", Data: "Max iterations reached"}
	terminal.Fail(RunTerminalMaxIterations, "max_iterations", "Max iterations reached", data)
}

const toolResultPersistenceFailureMessage = "tool result unavailable: durable persistence failed"

// persistSuccessfulToolResult runs inside the dispatch Execute callback, so
// its output is the first form visible to post-hooks, events, recorders, the
// durable observer, or the next provider request. Any failed large write is
// replaced with a fixed tool error; the raw payload is never returned.
func persistSuccessfulToolResult(store ToolResultStore, toolName, content string) (string, bool) {
	if checked, ok := store.(CheckedToolResultStore); ok {
		reference, disposition, err := checked.StoreResultChecked(toolName, content)
		if err != nil {
			return toolResultPersistenceFailureMessage, true
		}
		if disposition == ToolResultPersisted {
			return reference, false
		}
		if len(content) > maxInlineToolResultBytes {
			return toolResultPersistenceFailureMessage, true
		}
		return content, false
	}

	reference, replaced := store.StoreResult(toolName, content)
	if replaced {
		return reference, false
	}
	if len(content) > maxInlineToolResultBytes {
		return toolResultPersistenceFailureMessage, true
	}
	return content, false
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
