package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	// DefaultContextSafetyMarginTokens is deliberately separate from provider
	// framing overhead and requested output. It absorbs tokenizer drift without
	// silently changing the immutable model/profile limits selected for a run.
	DefaultContextSafetyMarginTokens = 512

	contextPlanVersion = 1
)

// TokenEstimator is the protocol-aware request estimation seam. Implementations
// receive the completed canonical request, including system blocks, tool
// schemas, messages, and all optional request fields. Output reservation and the
// planner safety margin are applied separately by ContextPlan.
type TokenEstimator interface {
	EstimateTokens(ContextEstimateRequest) (TokenEstimate, error)
}

// ContextEstimateRequest is immutable from the planner's perspective. The
// request value supplied to an estimator is a defensive copy.
type ContextEstimateRequest struct {
	Protocol harness.WirePolicy
	Request  types.MessagesRequest
}

// TokenEstimate separates request content from wire framing so the resulting
// ContextPlan can show the exact budget equation without retaining prompt text.
type TokenEstimate struct {
	InputTokens            int    `json:"inputTokens"`
	SystemTokens           int    `json:"systemTokens"`
	MessageTokens          int    `json:"messageTokens"`
	ToolTokens             int    `json:"toolTokens"`
	MetadataTokens         int    `json:"metadataTokens"`
	ProtocolOverheadTokens int    `json:"protocolOverheadTokens"`
	Source                 string `json:"source"`
	Confidence             string `json:"confidence"`
}

// ConservativeTokenEstimator is the deterministic fallback used only when a
// provider-aware estimator was not injected. It counts lexical runs, JSON
// punctuation, non-ASCII runes, content-block framing, and protocol-specific
// envelopes; it is intentionally more conservative than chars/4.
type ConservativeTokenEstimator struct{}

func (ConservativeTokenEstimator) EstimateTokens(input ContextEstimateRequest) (TokenEstimate, error) {
	req := input.Request
	estimate := TokenEstimate{
		SystemTokens:   conservativeRawTokens(req.System),
		MessageTokens:  conservativeMessagesTokens(req.Messages),
		ToolTokens:     conservativeToolsTokens(req.Tools),
		MetadataTokens: conservativeRequestMetadataTokens(req),
		Source:         "deterministic-conservative",
		Confidence:     "conservative",
	}
	estimate.InputTokens = estimate.SystemTokens + estimate.MessageTokens +
		estimate.ToolTokens + estimate.MetadataTokens
	estimate.ProtocolOverheadTokens = conservativeProtocolOverhead(
		input.Protocol,
		req.Messages,
		req.Tools,
	)
	return estimate, nil
}

// ContextSystemSections preserves the existing system-prompt ordering while
// allowing only explicitly optional RAG and recalled-memory sections to shrink.
// CorePrefix and CoreSuffix contain the base prompt, project instructions,
// skills, workstream context, plan mode, profile suffix, and PlanAnchor.
type ContextSystemSections struct {
	CorePrefix    string
	RAGContext    string
	MemoryContext string
	CoreSuffix    string
}

func (s ContextSystemSections) Render() string {
	return s.CorePrefix + s.RAGContext + s.MemoryContext + s.CoreSuffix
}

// OptionalContextReducer allows a memory/RAG implementation to retain its own
// semantic boundaries. The planner validates that its result actually shrinks;
// a non-shrinking result falls back to the deterministic reducer.
type OptionalContextReducer interface {
	ReduceContext(kind string, content string, targetTokens int) string
}

// ToolResultStore is satisfied by SessionMemory and by durable stores that can
// replace a large historical result with a bounded preview/reference.
type ToolResultStore interface {
	StoreResult(toolName string, result string) (reference string, replaced bool)
}

type ContextReductionKind string

const (
	ContextReductionRAGShrink       ContextReductionKind = "rag_shrink"
	ContextReductionRAGRemove       ContextReductionKind = "rag_remove"
	ContextReductionMemoryShrink    ContextReductionKind = "memory_shrink"
	ContextReductionMemoryRemove    ContextReductionKind = "memory_remove"
	ContextReductionToolPrune       ContextReductionKind = "tool_prune"
	ContextReductionToolResultBound ContextReductionKind = "tool_result_preview"
	ContextReductionHistoryCompact  ContextReductionKind = "history_compaction"
)

// ContextReduction contains counts only. It intentionally excludes prompt,
// memory, RAG, tool-input, and tool-result payloads.
type ContextReduction struct {
	Kind         ContextReductionKind `json:"kind"`
	BeforeTokens int                  `json:"beforeTokens"`
	AfterTokens  int                  `json:"afterTokens"`
	RemovedItems int                  `json:"removedItems,omitempty"`
	Detail       string               `json:"detail,omitempty"`
}

// ContextPlan is a deterministic, redacted description of one completed
// request budget. RequiredTokens is the complete window footprint:
// input + protocol framing + requested output + safety margin.
type ContextPlan struct {
	Version                  int                `json:"version"`
	Model                    string             `json:"model"`
	ProfileID                string             `json:"profileId"`
	Protocol                 harness.WirePolicy `json:"protocol"`
	ContextWindowTokens      int                `json:"contextWindowTokens"`
	ModelOutputLimitTokens   int                `json:"modelOutputLimitTokens"`
	RequestedOutputTokens    int                `json:"requestedOutputTokens"`
	SafetyMarginTokens       int                `json:"safetyMarginTokens"`
	ProtocolOverheadTokens   int                `json:"protocolOverheadTokens"`
	ReliableInputLimitTokens int                `json:"reliableInputLimitTokens,omitempty"`
	UsableInputTokens        int                `json:"usableInputTokens"`
	EstimatedInputTokens     int                `json:"estimatedInputTokens"`
	RequiredTokens           int                `json:"requiredTokens"`
	RemainingTokens          int                `json:"remainingTokens"`
	SystemTokens             int                `json:"systemTokens"`
	MessageTokens            int                `json:"messageTokens"`
	ToolTokens               int                `json:"toolTokens"`
	MetadataTokens           int                `json:"metadataTokens"`
	MessageCount             int                `json:"messageCount"`
	ToolCount                int                `json:"toolCount"`
	RAGIncluded              bool               `json:"ragIncluded"`
	MemoryIncluded           bool               `json:"memoryIncluded"`
	EstimateSource           string             `json:"estimateSource"`
	EstimateConfidence       string             `json:"estimateConfidence"`
	Reductions               []ContextReduction `json:"reductions,omitempty"`
	Fits                     bool               `json:"fits"`
	NeedsCompaction          bool               `json:"needsCompaction"`
	Blocked                  bool               `json:"blocked"`
	BlockCode                string             `json:"blockCode,omitempty"`
	CompactionSnapshotDigest string             `json:"compactionSnapshotDigest,omitempty"`
}

// ContextPlanningRequest contains all surfaces used to create the exact
// provider request. SafetyMarginTokens defaults to
// DefaultContextSafetyMarginTokens when zero.
type ContextPlanningRequest struct {
	Profile  harness.HarnessProfile
	Protocol harness.WirePolicy
	Model    string
	System   ContextSystemSections
	Messages []types.Message
	Tools    []types.ToolDef
	// RequiredToolNames are run-control definitions that must survive context
	// reduction. Missing or duplicated required definitions fail closed.
	RequiredToolNames  []string
	MaxTokens          int
	Temperature        *float64
	Estimator          TokenEstimator
	SafetyMarginTokens int
	Task               string
	ContextReducer     OptionalContextReducer
	ToolResultStore    ToolResultStore
	Reductions         []ContextReduction
}

// PlannedContext owns defensive copies of the request surfaces that survived
// deterministic reduction. Request and Plan describe the same completed call.
type PlannedContext struct {
	Request         *types.MessagesRequest
	System          ContextSystemSections
	Messages        []types.Message
	Tools           []types.ToolDef
	Plan            ContextPlan
	NeedsCompaction bool
}

// ContextBudgetError is safe to expose: it contains only typed budget metadata,
// never raw request content.
type ContextBudgetError struct {
	Code string
	Plan ContextPlan
	Err  error
}

func (e *ContextBudgetError) Error() string {
	if e == nil {
		return "context budget error"
	}
	if code := strings.TrimSpace(e.Code); code != "" {
		return code
	}
	// Estimators and optional reducers are injectable boundaries. Their raw
	// errors may contain endpoint, credential, prompt, or provider response
	// fragments, so the user-facing error string is deliberately typed only.
	// Unwrap retains the original cause for in-process classification without
	// copying it into SSE events, receipts, or recorder summaries.
	return "context_budget_error"
}

func (e *ContextBudgetError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PlanContextRequest applies the fixed reduction order and recalculates the
// complete request after every change. Structured history compaction is left to
// the loop because it owns the provider call and compact hooks.
func PlanContextRequest(input ContextPlanningRequest) (PlannedContext, error) {
	state, err := newContextPlanningState(input)
	if err != nil {
		return PlannedContext{}, err
	}

	plan, err := state.calculate()
	if err != nil {
		return PlannedContext{}, err
	}
	if plan.Fits {
		return state.result(plan), nil
	}

	// 1. Optional first-turn RAG: bounded shrink, then removal.
	if state.system.RAGContext != "" {
		before := plan
		state.system.RAGContext = shrinkOptionalContext(
			"rag",
			state.system.RAGContext,
			state.reducer,
		)
		plan, err = state.calculate()
		if err != nil {
			return PlannedContext{}, err
		}
		state.addReduction(ContextReductionRAGShrink, before, plan, 0, "bounded optional RAG")
		if plan.Fits {
			return state.result(plan), nil
		}
		if state.system.RAGContext != "" {
			before = plan
			state.system.RAGContext = ""
			plan, err = state.calculate()
			if err != nil {
				return PlannedContext{}, err
			}
			state.addReduction(ContextReductionRAGRemove, before, plan, 1, "optional RAG removed")
			if plan.Fits {
				return state.result(plan), nil
			}
		}
	}

	// 2. Optional recalled memory/context seam: bounded shrink, then removal.
	if state.system.MemoryContext != "" {
		before := plan
		state.system.MemoryContext = shrinkOptionalContext(
			"memory",
			state.system.MemoryContext,
			state.reducer,
		)
		plan, err = state.calculate()
		if err != nil {
			return PlannedContext{}, err
		}
		state.addReduction(ContextReductionMemoryShrink, before, plan, 0, "bounded recalled memory")
		if plan.Fits {
			return state.result(plan), nil
		}
		if state.system.MemoryContext != "" {
			before = plan
			state.system.MemoryContext = ""
			plan, err = state.calculate()
			if err != nil {
				return PlannedContext{}, err
			}
			state.addReduction(ContextReductionMemoryRemove, before, plan, 1, "optional recalled memory removed")
			if plan.Fits {
				return state.result(plan), nil
			}
		}
	}

	// 3. Whole-definition, relevance-ranked tool pruning. The same returned
	// slice is sent to the model and used as the dispatch allow-list.
	for !plan.Fits && len(state.tools) > 0 {
		beforeTools := len(state.tools)
		kept, dropped, pruneErr := pruneToolsPreservingRequired(
			state.tools,
			state.task,
			beforeTools-1,
			state.requiredTools,
		)
		if pruneErr != nil {
			return PlannedContext{}, &ContextBudgetError{Code: "required_tool_unavailable", Err: pruneErr}
		}
		if dropped <= 0 || len(kept) >= beforeTools {
			break
		}
		before := plan
		state.tools = cloneToolDefs(kept)
		plan, err = state.calculate()
		if err != nil {
			return PlannedContext{}, err
		}
		state.addReduction(ContextReductionToolPrune, before, plan, dropped, "whole schema definitions")
	}
	if plan.Fits {
		return state.result(plan), nil
	}

	// 4. Large historical tool results retain their tool_use/tool_result IDs,
	// but replace raw output with a bounded preview and digest/reference.
	bounded, stats := BoundHistoricalToolResults(state.messages, state.toolResultStore)
	if stats.Replaced > 0 {
		before := plan
		state.messages = bounded
		plan, err = state.calculate()
		if err != nil {
			return PlannedContext{}, err
		}
		state.addReduction(
			ContextReductionToolResultBound,
			before,
			plan,
			stats.Replaced,
			"paired historical results replaced with preview/reference",
		)
	}
	if plan.Fits {
		return state.result(plan), nil
	}

	// 5. The only remaining legal step is structured history compaction.
	plan.NeedsCompaction = true
	return state.result(plan), nil
}

// CalculateContextPlan recalculates an already selected completed request. It
// performs no reductions and is used for compaction preflight and final gates.
func CalculateContextPlan(input ContextPlanningRequest) (ContextPlan, *types.MessagesRequest, error) {
	state, err := newContextPlanningState(input)
	if err != nil {
		return ContextPlan{}, nil, err
	}
	plan, err := state.calculate()
	if err != nil {
		return ContextPlan{}, nil, err
	}
	return plan, state.buildRequest(), nil
}

type contextPlanningState struct {
	profile         harness.HarnessProfile
	protocol        harness.WirePolicy
	model           string
	system          ContextSystemSections
	messages        []types.Message
	tools           []types.ToolDef
	requiredTools   []string
	maxTokens       int
	temperature     *float64
	estimator       TokenEstimator
	safetyMargin    int
	task            string
	reducer         OptionalContextReducer
	toolResultStore ToolResultStore
	reductions      []ContextReduction
}

func newContextPlanningState(input ContextPlanningRequest) (*contextPlanningState, error) {
	if !input.Profile.Valid() {
		return nil, &ContextBudgetError{Code: "invalid_profile", Err: fmt.Errorf("unresolved HarnessProfile")}
	}
	protocol := input.Protocol
	if protocol == "" {
		protocol = input.Profile.WirePolicy()
	}
	if !protocol.Valid() {
		return nil, &ContextBudgetError{Code: "invalid_protocol", Err: fmt.Errorf("unsupported wire policy %q", protocol)}
	}
	if input.MaxTokens <= 0 {
		return nil, &ContextBudgetError{Code: "invalid_output_reserve", Err: fmt.Errorf("requested output must be positive")}
	}
	if input.MaxTokens > input.Profile.OutputReserve() {
		return nil, &ContextBudgetError{
			Code: "invalid_output_reserve",
			Err: fmt.Errorf(
				"requested output %d exceeds model/profile limit %d",
				input.MaxTokens,
				input.Profile.OutputReserve(),
			),
		}
	}
	safetyMargin := input.SafetyMarginTokens
	if safetyMargin == 0 {
		safetyMargin = DefaultContextSafetyMarginTokens
	}
	if safetyMargin < 0 {
		return nil, &ContextBudgetError{Code: "invalid_safety_margin", Err: fmt.Errorf("safety margin must be non-negative")}
	}
	estimator := input.Estimator
	if estimator == nil {
		estimator = ConservativeTokenEstimator{}
	}
	if len(input.RequiredToolNames) > 0 {
		if _, _, err := partitionRequiredTools(input.Tools, input.RequiredToolNames); err != nil {
			return nil, &ContextBudgetError{Code: "required_tool_unavailable", Err: err}
		}
	}
	return &contextPlanningState{
		profile:         input.Profile,
		protocol:        protocol,
		model:           input.Model,
		system:          input.System,
		messages:        cloneMessages(input.Messages),
		tools:           cloneToolDefs(input.Tools),
		requiredTools:   append([]string(nil), input.RequiredToolNames...),
		maxTokens:       input.MaxTokens,
		temperature:     cloneFloat64(input.Temperature),
		estimator:       estimator,
		safetyMargin:    safetyMargin,
		task:            input.Task,
		reducer:         input.ContextReducer,
		toolResultStore: input.ToolResultStore,
		reductions:      append([]ContextReduction(nil), input.Reductions...),
	}, nil
}

func (s *contextPlanningState) buildRequest() *types.MessagesRequest {
	return &types.MessagesRequest{
		Model:       s.model,
		System:      mustJSON([]map[string]string{{"type": "text", "text": s.system.Render()}}),
		Messages:    cloneMessages(s.messages),
		Tools:       cloneToolDefs(s.tools),
		MaxTokens:   s.maxTokens,
		Temperature: cloneFloat64(s.temperature),
	}
}

func (s *contextPlanningState) calculate() (ContextPlan, error) {
	req := s.buildRequest()
	estimateRequest := ContextEstimateRequest{
		Protocol: s.protocol,
		Request:  *cloneMessagesRequest(req),
	}
	estimate, err := s.estimator.EstimateTokens(estimateRequest)
	if err != nil {
		return ContextPlan{}, &ContextBudgetError{Code: "token_estimation_failed", Err: err}
	}
	if err := validateTokenEstimate(estimate); err != nil {
		return ContextPlan{}, &ContextBudgetError{Code: "invalid_token_estimate", Err: err}
	}
	if estimate.Source == "" {
		estimate.Source = "injected"
	}
	if estimate.Confidence == "" {
		estimate.Confidence = "unknown"
	}

	usable := s.profile.ContextWindow() - s.maxTokens - s.safetyMargin - estimate.ProtocolOverheadTokens
	if reliable := s.profile.ReliableInputTokens(); reliable > 0 && reliable < usable {
		usable = reliable
	}
	required := estimate.InputTokens + estimate.ProtocolOverheadTokens + s.maxTokens + s.safetyMargin
	plan := ContextPlan{
		Version:                  contextPlanVersion,
		Model:                    strings.TrimSpace(s.model),
		ProfileID:                s.profile.ID(),
		Protocol:                 s.protocol,
		ContextWindowTokens:      s.profile.ContextWindow(),
		ModelOutputLimitTokens:   s.profile.OutputReserve(),
		RequestedOutputTokens:    s.maxTokens,
		SafetyMarginTokens:       s.safetyMargin,
		ProtocolOverheadTokens:   estimate.ProtocolOverheadTokens,
		ReliableInputLimitTokens: s.profile.ReliableInputTokens(),
		UsableInputTokens:        usable,
		EstimatedInputTokens:     estimate.InputTokens,
		RequiredTokens:           required,
		RemainingTokens:          s.profile.ContextWindow() - required,
		SystemTokens:             estimate.SystemTokens,
		MessageTokens:            estimate.MessageTokens,
		ToolTokens:               estimate.ToolTokens,
		MetadataTokens:           estimate.MetadataTokens,
		MessageCount:             len(s.messages),
		ToolCount:                len(s.tools),
		RAGIncluded:              strings.TrimSpace(s.system.RAGContext) != "",
		MemoryIncluded:           strings.TrimSpace(s.system.MemoryContext) != "",
		EstimateSource:           estimate.Source,
		EstimateConfidence:       estimate.Confidence,
		Reductions:               append([]ContextReduction(nil), s.reductions...),
		Fits:                     usable > 0 && estimate.InputTokens <= usable,
	}
	if usable <= 0 {
		plan.Blocked = true
		plan.BlockCode = "reservations_exhaust_window"
		return plan, &ContextBudgetError{
			Code: "reservations_exhaust_window",
			Plan: plan,
			Err: fmt.Errorf(
				"context window %d is exhausted by output %d, protocol %d, and safety %d",
				s.profile.ContextWindow(),
				s.maxTokens,
				estimate.ProtocolOverheadTokens,
				s.safetyMargin,
			),
		}
	}
	return plan, nil
}

func (s *contextPlanningState) addReduction(
	kind ContextReductionKind,
	before ContextPlan,
	after ContextPlan,
	removed int,
	detail string,
) {
	s.reductions = append(s.reductions, ContextReduction{
		Kind:         kind,
		BeforeTokens: before.EstimatedInputTokens,
		AfterTokens:  after.EstimatedInputTokens,
		RemovedItems: removed,
		Detail:       detail,
	})
	after.Reductions = append([]ContextReduction(nil), s.reductions...)
}

func (s *contextPlanningState) result(plan ContextPlan) PlannedContext {
	plan.Reductions = append([]ContextReduction(nil), s.reductions...)
	return PlannedContext{
		Request:         s.buildRequest(),
		System:          s.system,
		Messages:        cloneMessages(s.messages),
		Tools:           cloneToolDefs(s.tools),
		Plan:            plan,
		NeedsCompaction: plan.NeedsCompaction,
	}
}

func validateTokenEstimate(estimate TokenEstimate) error {
	values := map[string]int{
		"input":             estimate.InputTokens,
		"system":            estimate.SystemTokens,
		"messages":          estimate.MessageTokens,
		"tools":             estimate.ToolTokens,
		"metadata":          estimate.MetadataTokens,
		"protocol overhead": estimate.ProtocolOverheadTokens,
	}
	for name, value := range values {
		if value < 0 {
			return fmt.Errorf("%s token estimate must be non-negative", name)
		}
	}
	known := estimate.SystemTokens + estimate.MessageTokens + estimate.ToolTokens + estimate.MetadataTokens
	if estimate.InputTokens < known {
		return fmt.Errorf("input token estimate %d is below component total %d", estimate.InputTokens, known)
	}
	return nil
}

func shrinkOptionalContext(kind, content string, reducer OptionalContextReducer) string {
	if content == "" {
		return ""
	}
	targetTokens := conservativeTextTokens(content) / 2
	if targetTokens < 128 {
		targetTokens = 128
	}
	var reduced string
	if reducer != nil {
		reduced = reducer.ReduceContext(kind, content, targetTokens)
	}
	if len(reduced) >= len(content) || strings.TrimSpace(reduced) == "" {
		reduced = deterministicContextPreview(kind, content, targetTokens)
	}
	if len(reduced) >= len(content) {
		return ""
	}
	return reduced
}

func deterministicContextPreview(kind, content string, targetTokens int) string {
	maxBytes := targetTokens * 2
	if maxBytes < 256 {
		maxBytes = 256
	}
	if len(content) <= maxBytes {
		return content
	}
	marker := fmt.Sprintf("\n\n[%s context shortened deterministically]\n\n", kind)
	head := (maxBytes - len(marker)) * 2 / 3
	if head < 0 {
		head = 0
	}
	tail := maxBytes - len(marker) - head
	return contextUTF8Prefix(content, head) + marker + contextUTF8Suffix(content, tail)
}

func conservativeMessagesTokens(messages []types.Message) int {
	total := 0
	for _, message := range messages {
		total += conservativeTextTokens(message.Role)
		total += conservativeRawTokens(message.Content)
	}
	return total
}

func conservativeToolsTokens(tools []types.ToolDef) int {
	total := 0
	for _, tool := range tools {
		total += conservativeTextTokens(tool.Name)
		total += conservativeTextTokens(tool.Description)
		total += conservativeRawTokens(tool.InputSchema)
	}
	return total
}

func conservativeRequestMetadataTokens(req types.MessagesRequest) int {
	total := conservativeTextTokens(req.Model)
	for _, raw := range []json.RawMessage{
		req.ToolChoice,
		req.Thinking,
		req.Metadata,
	} {
		total += conservativeRawTokens(raw)
	}
	for _, beta := range req.Betas {
		total += conservativeTextTokens(beta)
	}
	total += conservativeTextTokens(req.Speed)
	if req.Temperature != nil {
		total += 2
	}
	if req.Stream != nil {
		total++
	}
	return total
}

func conservativeProtocolOverhead(
	protocol harness.WirePolicy,
	messages []types.Message,
	tools []types.ToolDef,
) int {
	base, perMessage, perTool, perBlock := 64, 8, 20, 4
	switch protocol {
	case harness.WireAnthropicMessages:
		base, perMessage, perTool, perBlock = 32, 6, 14, 3
	case harness.WireOpenAIChatCompletions:
		base, perMessage, perTool, perBlock = 40, 7, 16, 4
	case harness.WireOpenAIResponses:
		base, perMessage, perTool, perBlock = 48, 8, 18, 4
	case harness.WireGemini:
		base, perMessage, perTool, perBlock = 40, 7, 16, 4
	case harness.WireACP:
		base, perMessage, perTool, perBlock = 64, 8, 20, 4
	case harness.WireAuto:
		// Unknown adapters use the largest supported envelope.
	}
	blocks := 0
	for _, message := range messages {
		blocks += contentBlockCount(message.Content)
	}
	return base + perMessage*len(messages) + perTool*len(tools) + perBlock*blocks
}

func contentBlockCount(raw json.RawMessage) int {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		return len(blocks)
	}
	if len(raw) > 0 {
		return 1
	}
	return 0
}

func conservativeRawTokens(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	return conservativeTextTokens(string(raw))
}

func conservativeTextTokens(value string) int {
	if value == "" {
		return 0
	}
	tokens := 0
	wordRun := 0
	flushWord := func() {
		if wordRun == 0 {
			return
		}
		tokens += (wordRun + 2) / 3
		wordRun = 0
	}
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			wordRun++
			continue
		}
		flushWord()
		if unicode.IsSpace(r) {
			continue
		}
		if r > unicode.MaxASCII {
			// Local-model tokenizers often split CJK and arbitrary Unicode more
			// aggressively than Latin text.
			tokens += 2
			continue
		}
		// JSON delimiters, escapes, and punctuation each consume capacity in
		// at least one supported wire format.
		tokens++
	}
	flushWord()
	if tokens == 0 {
		return 1
	}
	return tokens
}

func cloneMessagesRequest(req *types.MessagesRequest) *types.MessagesRequest {
	if req == nil {
		return &types.MessagesRequest{}
	}
	clone := *req
	clone.System = append(json.RawMessage(nil), req.System...)
	clone.Messages = cloneMessages(req.Messages)
	clone.Tools = cloneToolDefs(req.Tools)
	clone.ToolChoice = append(json.RawMessage(nil), req.ToolChoice...)
	clone.Thinking = append(json.RawMessage(nil), req.Thinking...)
	clone.Metadata = append(json.RawMessage(nil), req.Metadata...)
	clone.Betas = append([]string(nil), req.Betas...)
	clone.Temperature = cloneFloat64(req.Temperature)
	if req.Stream != nil {
		value := *req.Stream
		clone.Stream = &value
	}
	return &clone
}

func cloneMessages(messages []types.Message) []types.Message {
	if messages == nil {
		return nil
	}
	clone := make([]types.Message, len(messages))
	for index, message := range messages {
		clone[index] = message
		clone[index].Content = append(json.RawMessage(nil), message.Content...)
	}
	return clone
}

func cloneToolDefs(tools []types.ToolDef) []types.ToolDef {
	if tools == nil {
		return nil
	}
	clone := make([]types.ToolDef, len(tools))
	for index, tool := range tools {
		clone[index] = tool
		clone[index].InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
	}
	return clone
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func contextUTF8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	prefix := value[:maxBytes]
	for !utf8.ValidString(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}

func contextUTF8Suffix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	suffix := value[len(value)-maxBytes:]
	for !utf8.ValidString(suffix) && len(suffix) > 0 {
		suffix = suffix[1:]
	}
	return suffix
}
