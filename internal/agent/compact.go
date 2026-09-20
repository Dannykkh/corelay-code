package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	// Legacy constants remain for callers of ShouldCompact. The main loop now
	// gates on the completed ContextPlan instead of message-only estimates.
	maxOutputReserve = 20000
	compactMargin    = 13000

	maxCompactFailures    = 3
	minMessagesForCompact = 8

	historicalToolResultThreshold = 4 * 1024
	historicalToolPreviewBytes    = 768
	compactionRecentUnits         = 4
	maxCompactionInteractions     = 32
	maxCompactionFiles            = 64
	maxCompactionEvidence         = 16
	maxCompactionUserInstructions = 8
	maxCompactionContextItems     = 12
	maxCompactionNarrativeBytes   = 4 * 1024
	maxCompactionPromptBytes      = 12 * 1024
	compactionProviderTimeout     = 30 * time.Second
)

// CompactConfig is retained for API compatibility. New request planning uses
// ContextPlan and the immutable HarnessProfile selected at run start.
type CompactConfig struct {
	ContextWindow   int
	CompactFailures int
}

// ShouldCompact preserves the legacy helper for non-loop callers. The own-agent
// loop no longer makes safety decisions from this message-only predicate.
func ShouldCompact(cfg CompactConfig, currentTokens int) bool {
	if cfg.CompactFailures >= maxCompactFailures {
		return false
	}
	effective := cfg.ContextWindow - maxOutputReserve
	threshold := effective - compactMargin
	shouldCompact := currentTokens > threshold
	if shouldCompact {
		log.Printf("[Compact] Trigger: %d tokens > %d threshold (window=%d)", currentTokens, threshold, cfg.ContextWindow)
	}
	return shouldCompact
}

// HistoricalToolResultStats contains only counts and content digests. It never
// retains tool inputs or raw result payloads.
type HistoricalToolResultStats struct {
	Replaced    int      `json:"replaced"`
	BeforeBytes int      `json:"beforeBytes"`
	AfterBytes  int      `json:"afterBytes"`
	References  []string `json:"references,omitempty"`
}

// BoundHistoricalToolResults replaces only old, large tool_result content. The
// enclosing block and tool_use_id remain intact, and the most recent history
// units are never modified.
func BoundHistoricalToolResults(
	messages []types.Message,
	store ToolResultStore,
) ([]types.Message, HistoricalToolResultStats) {
	result := cloneMessages(messages)
	stats := HistoricalToolResultStats{}
	if len(result) == 0 {
		return result, stats
	}

	toolNames := toolNamesByUseID(result)
	recentStart := recentHistoryStart(result, compactionRecentUnits)
	for index := 0; index < recentStart; index++ {
		message := result[index]
		if message.Role != "user" {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		changed := false
		for blockIndex := range blocks {
			var blockType string
			if json.Unmarshal(blocks[blockIndex]["type"], &blockType) != nil || blockType != "tool_result" {
				continue
			}
			contentRaw := blocks[blockIndex]["content"]
			var content string
			if json.Unmarshal(contentRaw, &content) != nil {
				content = string(contentRaw)
			}
			if len(content) <= historicalToolResultThreshold {
				continue
			}

			var toolUseID string
			_ = json.Unmarshal(blocks[blockIndex]["tool_use_id"], &toolUseID)
			toolName := toolNames[toolUseID]
			// Never rewrite an orphaned/unresolved result. A verified paired
			// tool_use is required so catalog identity and history semantics stay
			// intact.
			if toolName == "" {
				continue
			}
			digest := sha256String(content)
			reference := ""
			if store != nil {
				if stored, replaced := store.StoreResult(toolName, content); replaced {
					reference = strings.TrimSpace(stored)
				}
			}
			if reference == "" {
				reference = deterministicToolResultReference(toolName, content, digest)
			} else if len(reference) > historicalToolResultThreshold {
				reference = contextUTF8Prefix(reference, historicalToolResultThreshold)
			}
			reference = sanitizeReceiptString(reference)
			if len(reference) > historicalToolResultThreshold {
				reference = contextUTF8Prefix(reference, historicalToolResultThreshold)
			}

			encoded, _ := json.Marshal(reference)
			blocks[blockIndex]["content"] = encoded
			stats.Replaced++
			stats.BeforeBytes += len(content)
			stats.AfterBytes += len(reference)
			stats.References = append(stats.References, digest)
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(blocks)
			if err == nil {
				result[index].Content = encoded
			}
		}
	}
	return result, stats
}

func deterministicToolResultReference(toolName, content, digest string) string {
	head := historicalToolPreviewBytes * 2 / 3
	tail := historicalToolPreviewBytes - head
	preview := contextUTF8Prefix(content, head)
	if len(content) > historicalToolPreviewBytes {
		preview += "\n... [bounded historical preview] ...\n" + contextUTF8Suffix(content, tail)
	}
	preview = sanitizeReceiptString(preview)
	return fmt.Sprintf(
		"[Historical tool result preview; full result was not stored]\ntool=%s\nbytes=%d\ncontent-digest=sha256:%s\nreload=unavailable\npreview:\n%s",
		sanitizeSnapshotText(toolName, 128),
		len(content),
		strings.TrimPrefix(digest, "sha256:"),
		preview,
	)
}

// CompactionState supplies durable run facts that cannot be reconstructed from
// prose alone. Callers may leave optional fields empty.
type CompactionState struct {
	Objective        string
	PlanAnchor       *PlanAnchor
	EditedFiles      []string
	Evidence         *EvidenceLedger
	Decisions        []string
	PendingApprovals []string
	UserInstructions []string
	Context          CompactionContext
}

type CompactionPlanState struct {
	WorkstreamID   string   `json:"workstreamId,omitempty"`
	ID             string   `json:"id,omitempty"`
	Objective      string   `json:"objective"`
	CurrentStep    string   `json:"currentStep,omitempty"`
	RemainingSteps []string `json:"remainingSteps,omitempty"`
	Acceptance     []string `json:"acceptance,omitempty"`
	Revision       uint64   `json:"revision"`
	StateRevision  uint64   `json:"stateRevision,omitempty"`
	StageID        string   `json:"stageId,omitempty"`
}

type CompactionFileState struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
}

type CompactionToolInteraction struct {
	ToolName     string `json:"toolName"`
	PairID       string `json:"pairId"`
	InputDigest  string `json:"inputDigest"`
	ResultDigest string `json:"resultDigest,omitempty"`
	ResultBytes  int    `json:"resultBytes,omitempty"`
	IsError      bool   `json:"isError"`
}

type CompactionFailureState struct {
	ToolName   string `json:"toolName"`
	PairID     string `json:"pairId"`
	Correction string `json:"correction"`
}

type CompactionEvidenceState struct {
	Source        string `json:"source"`
	Status        string `json:"status"`
	CommandDigest string `json:"commandDigest,omitempty"`
	SummaryDigest string `json:"summaryDigest,omitempty"`
}

type CompactionVerificationState struct {
	Status        string `json:"status,omitempty"`
	Source        string `json:"source,omitempty"`
	SummaryDigest string `json:"summaryDigest,omitempty"`
}

// CompactionSnapshot is deterministic for the same transcript and durable run
// state. It contains digests and redacted bounded fields, never raw tool input,
// result payload, command, credential, or full prompt.
type CompactionSnapshot struct {
	Version                int                          `json:"version"`
	Digest                 string                       `json:"digest"`
	Strategy               string                       `json:"strategy"`
	BeforeMessages         int                          `json:"beforeMessages"`
	AfterMessages          int                          `json:"afterMessages"`
	Objective              string                       `json:"objective"`
	UserInstructions       []string                     `json:"userInstructions,omitempty"`
	WorkstreamID           string                       `json:"workstreamId,omitempty"`
	WorkstreamObjective    string                       `json:"workstreamObjective,omitempty"`
	Constraints            []string                     `json:"constraints,omitempty"`
	Plan                   CompactionPlanState          `json:"plan"`
	Decisions              []string                     `json:"decisions,omitempty"`
	PlanEvidence           []CompactionPlanEvidence     `json:"planEvidence,omitempty"`
	LastVerification       *CompactionVerificationState `json:"lastVerification,omitempty"`
	Files                  []CompactionFileState        `json:"files,omitempty"`
	ToolInteractions       []CompactionToolInteraction  `json:"toolInteractions,omitempty"`
	FailedActions          []CompactionFailureState     `json:"failedActions,omitempty"`
	EvidenceDecision       string                       `json:"evidenceDecision,omitempty"`
	EvidenceMode           string                       `json:"evidenceMode,omitempty"`
	Evidence               []CompactionEvidenceState    `json:"evidence,omitempty"`
	PendingApprovalDigests []string                     `json:"pendingApprovalDigests,omitempty"`
	DurableReferences      []string                     `json:"durableReferences,omitempty"`
	UnresolvedPairDigests  []string                     `json:"unresolvedPairDigests,omitempty"`
	LLMFallbackReason      string                       `json:"llmFallbackReason,omitempty"`
}

type CompactionResult struct {
	Messages     []types.Message
	Snapshot     CompactionSnapshot
	UsedFallback bool
}

type contextCompactionOutcome struct {
	Planned   PlannedContext
	Snapshot  CompactionSnapshot
	Blocked   bool
	BlockCode string
	Err       error
}

// compactPlannedContext owns one actual structured-compaction attempt. It
// preflights the deterministic fallback before the optional summarizer call,
// then recalculates the complete request with the already selected RAG, memory,
// tools, protocol, output reservation, and safety margin.
func compactPlannedContext(
	ctx context.Context,
	provider types.Provider,
	hookRunner compactionHookRunner,
	base ContextPlanningRequest,
	planned PlannedContext,
	state CompactionState,
) contextCompactionOutcome {
	outcome := contextCompactionOutcome{}
	hookEnv := map[string]string{
		"MODEL":           base.Model,
		"MESSAGE_COUNT":   fmt.Sprintf("%d", len(planned.Messages)),
		"ESTIMATED_INPUT": fmt.Sprintf("%d", planned.Plan.EstimatedInputTokens),
		"COMPACT_TRIGGER": "auto",
	}

	withCompactionHooks(ctx, hookRunner, hookEnv, func() {
		fallback := BuildDeterministicCompaction(planned.Messages, state)
		if len(fallback.Snapshot.UnresolvedPairDigests) > 0 {
			outcome.Planned = planned
			outcome.Planned.Plan.Blocked = true
			outcome.Planned.Plan.BlockCode = "unresolved_tool_pair"
			outcome.Planned.Plan.NeedsCompaction = false
			outcome.Snapshot = fallback.Snapshot
			outcome.Blocked = true
			outcome.BlockCode = "unresolved_tool_pair"
			hookEnv["RESULT"] = "blocked"
			hookEnv["BLOCK_CODE"] = outcome.BlockCode
			return
		}
		fallbackInput := base
		fallbackInput.System = planned.System
		fallbackInput.Messages = fallback.Messages
		fallbackInput.Tools = planned.Tools
		fallbackInput.Reductions = append([]ContextReduction(nil), planned.Plan.Reductions...)

		fallbackPlan, _, err := CalculateContextPlan(fallbackInput)
		if err != nil {
			outcome.Snapshot = fallback.Snapshot
			outcome.Blocked = true
			outcome.BlockCode = contextBudgetErrorCode(err, "compaction_recalc_failed")
			outcome.Err = err
			hookEnv["RESULT"] = "blocked"
			hookEnv["BLOCK_CODE"] = outcome.BlockCode
			return
		}

		candidate := fallback
		candidatePlan := fallbackPlan
		if fallbackPlan.Fits {
			candidate = TryLLMCompaction(
				ctx,
				provider,
				base.Model,
				planned.Messages,
				state,
				fallback,
			)
			candidateInput := fallbackInput
			candidateInput.Messages = candidate.Messages
			candidatePlan, _, err = CalculateContextPlan(candidateInput)
			if err != nil {
				outcome.Snapshot = candidate.Snapshot
				outcome.Blocked = true
				outcome.BlockCode = contextBudgetErrorCode(err, "compaction_recalc_failed")
				outcome.Err = err
				hookEnv["RESULT"] = "blocked"
				hookEnv["BLOCK_CODE"] = outcome.BlockCode
				return
			}
			if !candidatePlan.Fits {
				// A verbose or invalid LLM narrative never displaces the safe,
				// already-preflighted deterministic candidate.
				candidate = fallback
				candidate.Snapshot.LLMFallbackReason = "llm_candidate_overflow"
				candidate.Snapshot.Digest = compactionSnapshotDigest(candidate.Snapshot)
				candidate.Messages = buildCompactedMessages(
					planned.Messages,
					candidate.Snapshot,
					"Older history was replaced by the deterministic structured state below.",
				)
				candidatePlan = fallbackPlan
			}
		}

		reduction := ContextReduction{
			Kind:         ContextReductionHistoryCompact,
			BeforeTokens: planned.Plan.EstimatedInputTokens,
			AfterTokens:  candidatePlan.EstimatedInputTokens,
			RemovedItems: maxInt(0, len(planned.Messages)-len(candidate.Messages)),
			Detail:       candidate.Snapshot.Strategy,
		}
		finalInput := fallbackInput
		finalInput.Messages = candidate.Messages
		finalInput.Reductions = append(
			append([]ContextReduction(nil), planned.Plan.Reductions...),
			reduction,
		)
		finalPlan, finalRequest, err := CalculateContextPlan(finalInput)
		if err != nil {
			outcome.Snapshot = candidate.Snapshot
			outcome.Blocked = true
			outcome.BlockCode = contextBudgetErrorCode(err, "compaction_recalc_failed")
			outcome.Err = err
			hookEnv["RESULT"] = "blocked"
			hookEnv["BLOCK_CODE"] = outcome.BlockCode
			return
		}
		finalPlan.CompactionSnapshotDigest = candidate.Snapshot.Digest
		finalPlan.NeedsCompaction = !finalPlan.Fits
		outcome.Planned = PlannedContext{
			Request:         finalRequest,
			System:          planned.System,
			Messages:        cloneMessages(candidate.Messages),
			Tools:           cloneToolDefs(planned.Tools),
			Plan:            finalPlan,
			NeedsCompaction: !finalPlan.Fits,
		}
		outcome.Snapshot = candidate.Snapshot
		outcome.Blocked = !finalPlan.Fits
		if outcome.Blocked {
			outcome.BlockCode = "context_overflow_after_compaction"
			outcome.Planned.Plan.Blocked = true
			outcome.Planned.Plan.BlockCode = outcome.BlockCode
		}
		hookEnv["RESULT"] = "compacted"
		if outcome.Blocked {
			hookEnv["RESULT"] = "blocked"
			hookEnv["BLOCK_CODE"] = outcome.BlockCode
		}
		hookEnv["STRATEGY"] = candidate.Snapshot.Strategy
		hookEnv["SNAPSHOT_DIGEST"] = candidate.Snapshot.Digest
		hookEnv["AFTER_MESSAGES"] = fmt.Sprintf("%d", len(candidate.Messages))
	})
	return outcome
}

func contextBudgetErrorCode(err error, fallback string) string {
	if typed, ok := err.(*ContextBudgetError); ok && typed.Code != "" {
		return typed.Code
	}
	return fallback
}

// BuildDeterministicCompaction creates the structured fallback before any LLM
// call. The loop can therefore preflight the complete post-compaction request
// and avoid a pointless provider call when even the fallback cannot fit.
func BuildDeterministicCompaction(
	messages []types.Message,
	state CompactionState,
) CompactionResult {
	snapshot := buildCompactionSnapshot(messages, state)
	snapshot.Strategy = "deterministic-fallback"
	compacted := buildCompactedMessages(
		messages,
		snapshot,
		"Older history was replaced by the deterministic structured state below.",
	)
	snapshot.AfterMessages = len(compacted)
	snapshot.Digest = compactionSnapshotDigest(snapshot)
	// Re-render after the final message count and digest are known.
	compacted = buildCompactedMessages(
		messages,
		snapshot,
		"Older history was replaced by the deterministic structured state below.",
	)
	return CompactionResult{Messages: compacted, Snapshot: snapshot, UsedFallback: true}
}

// TryLLMCompaction adds an optional narrative to the deterministic snapshot.
// Stream failures, empty output, and invalid text all return a stable fallback.
func TryLLMCompaction(
	ctx context.Context,
	provider types.Provider,
	model string,
	messages []types.Message,
	state CompactionState,
	fallback CompactionResult,
) CompactionResult {
	if provider == nil {
		fallback.Snapshot.LLMFallbackReason = "provider_unavailable"
		fallback.Snapshot.Digest = compactionSnapshotDigest(fallback.Snapshot)
		fallback.Messages = buildCompactedMessages(
			messages,
			fallback.Snapshot,
			"Older history was replaced by the deterministic structured state below.",
		)
		return fallback
	}
	summary, reason := requestCompactionNarrative(ctx, provider, model, messages, fallback.Snapshot)
	if reason != "" {
		fallback.Snapshot.LLMFallbackReason = reason
		fallback.Snapshot.Digest = compactionSnapshotDigest(fallback.Snapshot)
		fallback.Messages = buildCompactedMessages(
			messages,
			fallback.Snapshot,
			"Older history was replaced by the deterministic structured state below.",
		)
		return fallback
	}

	snapshot := fallback.Snapshot
	snapshot.Strategy = "llm-narrative"
	snapshot.LLMFallbackReason = ""
	snapshot.Digest = compactionSnapshotDigest(snapshot)
	compacted := buildCompactedMessages(messages, snapshot, summary)
	snapshot.AfterMessages = len(compacted)
	snapshot.Digest = compactionSnapshotDigest(snapshot)
	compacted = buildCompactedMessages(messages, snapshot, summary)
	return CompactionResult{Messages: compacted, Snapshot: snapshot, UsedFallback: false}
}

// CompactMessages preserves the legacy API. New loop code uses the structured
// preflight/TryLLMCompaction path so overflow remains fail closed.
func CompactMessages(
	ctx context.Context,
	provider types.Provider,
	model string,
	messages []types.Message,
) ([]types.Message, error) {
	if len(messages) <= 2 {
		return messages, nil
	}
	fallback := BuildDeterministicCompaction(messages, CompactionState{})
	summary, reason := requestCompactionNarrative(ctx, provider, model, messages, fallback.Snapshot)
	if reason != "" {
		return messages, fmt.Errorf("compact summary failed: %s", reason)
	}
	result := fallback
	result.Snapshot.Strategy = "llm-narrative"
	result.Snapshot.Digest = compactionSnapshotDigest(result.Snapshot)
	result.Messages = buildCompactedMessages(messages, result.Snapshot, summary)
	log.Printf("[Compact] Reduced %d -> %d messages", len(messages), len(result.Messages))
	return result.Messages, nil
}

// EstimateMessageTokens is retained for compatibility and diagnostics. Safety
// gates use TokenEstimator over the completed request, not this helper.
func EstimateMessageTokens(messages []types.Message) int {
	return conservativeMessagesTokens(messages)
}

type compactionHookRunner interface {
	ExecuteContext(context.Context, hooks.HookType, map[string]string) []hooks.HookResult
}

// withCompactionHooks guarantees one registry invocation on each side of one
// actual attempt. Hook exit status and output are observational only and cannot
// bypass the context safety decision made inside attempt.
func withCompactionHooks(
	ctx context.Context,
	runner compactionHookRunner,
	env map[string]string,
	attempt func(),
) {
	if runner != nil {
		runner.ExecuteContext(ctx, hooks.HookPreCompact, cloneStringMap(env))
	}
	attempt()
	if runner != nil {
		runner.ExecuteContext(ctx, hooks.HookPostCompact, cloneStringMap(env))
	}
}

func buildCompactionSnapshot(messages []types.Message, state CompactionState) CompactionSnapshot {
	objective := strings.TrimSpace(state.Objective)
	if objective == "" {
		objective = latestObjectiveText(messages)
	}
	userInstructions := state.UserInstructions
	if len(userInstructions) == 0 {
		userInstructions = userInstructionTexts(messages)
	}

	snapshot := CompactionSnapshot{
		Version:             1,
		BeforeMessages:      len(messages),
		Objective:           sanitizeSnapshotText(objective, 1200),
		UserInstructions:    sanitizeUserInstructionList(userInstructions),
		WorkstreamID:        sanitizeSnapshotText(state.Context.WorkstreamID, 256),
		WorkstreamObjective: sanitizeSnapshotText(state.Context.WorkstreamObjective, 1200),
		Constraints:         sanitizeRecentSnapshotList(state.Context.Constraints, maxCompactionContextItems, 300),
		Decisions:           sanitizeRecentSnapshotList(append(append([]string(nil), state.Context.Decisions...), state.Decisions...), maxCompactionContextItems, 300),
		PlanEvidence:        sanitizeCompactionPlanEvidence(state.Context.PlanEvidence),
	}
	verification := CompactionVerificationState{
		Status: sanitizeSnapshotText(state.Context.LastVerificationStatus, 64),
		Source: sanitizeSnapshotText(state.Context.LastVerificationSource, 128),
	}
	if summary := sanitizeSnapshotText(state.Context.LastVerificationSummary, 600); summary != "" {
		verification.SummaryDigest = sha256String(summary)
	}
	if verification.Status != "" || verification.Source != "" || verification.SummaryDigest != "" {
		snapshot.LastVerification = &verification
	}
	if state.PlanAnchor != nil && state.PlanAnchor.Valid() {
		snapshot.Plan = CompactionPlanState{
			WorkstreamID:   sanitizeSnapshotText(state.Context.WorkstreamID, 256),
			ID:             sanitizeSnapshotText(state.Context.PlanID, 256),
			Objective:      sanitizeSnapshotText(state.PlanAnchor.Objective(), 1200),
			CurrentStep:    sanitizeSnapshotText(state.PlanAnchor.CurrentStep(), 400),
			RemainingSteps: sanitizeSnapshotList(state.PlanAnchor.RemainingSteps(), 32, 400),
			Acceptance:     sanitizeSnapshotList(state.PlanAnchor.DefinitionOfDone(), 32, 400),
			Revision:       uint64(state.PlanAnchor.Revision()),
			StateRevision:  state.Context.PlanStateRevision,
			StageID:        sanitizeSnapshotText(state.Context.StageID, 256),
		}
		if state.Context.PlanRevision != 0 {
			snapshot.Plan.Revision = state.Context.PlanRevision
		}
	} else {
		snapshot.Plan = CompactionPlanState{
			WorkstreamID:  sanitizeSnapshotText(state.Context.WorkstreamID, 256),
			ID:            sanitizeSnapshotText(state.Context.PlanID, 256),
			Objective:     snapshot.Objective,
			Revision:      state.Context.PlanRevision,
			StateRevision: state.Context.PlanStateRevision,
			StageID:       sanitizeSnapshotText(state.Context.StageID, 256),
		}
	}

	pairs := collectToolPairs(messages)
	start := 0
	if len(pairs) > maxCompactionInteractions {
		start = len(pairs) - maxCompactionInteractions
	}
	fileRevisions := make(map[string]string)
	fileOrder := make([]string, 0, len(state.EditedFiles))
	for _, file := range state.EditedFiles {
		file = sanitizeSnapshotText(file, 500)
		if file == "" {
			continue
		}
		if _, exists := fileRevisions[file]; !exists {
			fileOrder = append(fileOrder, file)
		}
		fileRevisions[file] = "observed"
	}
	for _, pair := range pairs[start:] {
		interaction := CompactionToolInteraction{
			ToolName:     sanitizeSnapshotText(pair.Name, 128),
			PairID:       shortDigest(pair.ID),
			InputDigest:  sha256String(string(pair.Input)),
			ResultDigest: sha256String(pair.Result),
			ResultBytes:  len(pair.Result),
			IsError:      pair.IsError,
		}
		snapshot.ToolInteractions = append(snapshot.ToolInteractions, interaction)
		if pair.IsError {
			snapshot.FailedActions = append(snapshot.FailedActions, CompactionFailureState{
				ToolName:   interaction.ToolName,
				PairID:     interaction.PairID,
				Correction: "preserve failure and subsequent correction evidence",
			})
		}
		if isEditTool(pair.Name) {
			if path := compactEditPath(pair.Input); path != "" {
				path = sanitizeSnapshotText(path, 500)
				if _, exists := fileRevisions[path]; !exists {
					fileOrder = append(fileOrder, path)
				}
				fileRevisions[path] = interaction.ResultDigest
			}
		}
		if reference := embeddedToolResultReference(pair.Result); reference != "" {
			snapshot.DurableReferences = append(snapshot.DurableReferences, reference)
		}
	}
	if len(fileOrder) > maxCompactionFiles {
		fileOrder = fileOrder[len(fileOrder)-maxCompactionFiles:]
	}
	for _, file := range fileOrder {
		snapshot.Files = append(snapshot.Files, CompactionFileState{
			Path:     file,
			Revision: fileRevisions[file],
		})
	}
	snapshot.DurableReferences = uniqueSortedStrings(snapshot.DurableReferences)

	if state.Evidence != nil {
		gate, records := compactionEvidenceSnapshot(state.Evidence)
		snapshot.EvidenceDecision = gate.Decision
		snapshot.EvidenceMode = gate.Mode
		if len(records) > maxCompactionEvidence {
			records = records[len(records)-maxCompactionEvidence:]
		}
		for _, record := range records {
			snapshot.Evidence = append(snapshot.Evidence, CompactionEvidenceState{
				Source:        sanitizeSnapshotText(record.Source, 128),
				Status:        sanitizeSnapshotText(record.Status, 64),
				CommandDigest: sha256String(record.Command),
				SummaryDigest: sha256String(record.Summary),
			})
		}
	}
	for _, approval := range tailStrings(state.PendingApprovals, maxCompactionContextItems) {
		if approval = strings.TrimSpace(approval); approval != "" {
			snapshot.PendingApprovalDigests = append(snapshot.PendingApprovalDigests, sha256String(approval))
		}
	}
	snapshot.PendingApprovalDigests = uniqueSortedStrings(snapshot.PendingApprovalDigests)
	snapshot.UnresolvedPairDigests = unresolvedHistoricalPairDigests(messages)
	snapshot.Digest = compactionSnapshotDigest(snapshot)
	return snapshot
}

func buildCompactedMessages(
	messages []types.Message,
	snapshot CompactionSnapshot,
	narrative string,
) []types.Message {
	objectiveIndex := firstObjectiveIndex(messages)
	recentStart := recentHistoryStart(messages, compactionRecentUnits)
	result := make([]types.Message, 0, len(messages)-recentStart+3)
	if objectiveIndex >= 0 && objectiveIndex < recentStart {
		result = append(result, cloneMessages(messages[objectiveIndex:objectiveIndex+1])...)
	}

	narrative = sanitizeSnapshotText(narrative, maxCompactionNarrativeBytes)
	snapshotJSON, _ := json.Marshal(snapshot)
	summaryText := "[Structured Conversation State]\n" + string(snapshotJSON)
	if narrative != "" {
		summaryText += "\n\nNarrative:\n" + narrative
	}
	result = appendAlternatingMessage(result, types.Message{
		Role:    "assistant",
		Content: mustJSON(summaryText),
	})

	for index := recentStart; index < len(messages); index++ {
		if index == objectiveIndex && objectiveIndex < recentStart {
			continue
		}
		result = appendAlternatingMessage(result, messages[index])
	}
	return result
}

func appendAlternatingMessage(messages []types.Message, next types.Message) []types.Message {
	if len(messages) > 0 && messages[len(messages)-1].Role == next.Role {
		role := "assistant"
		if next.Role == "assistant" {
			role = "user"
		}
		messages = append(messages, types.Message{Role: role, Content: mustJSON("(preserved context continues)")})
	}
	next.Content = append(json.RawMessage(nil), next.Content...)
	return append(messages, next)
}

func requestCompactionNarrative(
	ctx context.Context,
	provider types.Provider,
	model string,
	messages []types.Message,
	snapshot CompactionSnapshot,
) (string, string) {
	if provider == nil {
		return "", "provider_unavailable"
	}
	prompt := "Write a concise narrative for the structured state below. Preserve decisions, corrections, and next action. Do not invent facts or repeat credentials.\n\n"
	snapshotJSON, _ := json.Marshal(snapshot)
	prompt += string(snapshotJSON) + "\n\nBounded prior excerpts:\n" + compactionNarrativeSource(messages)
	if len(prompt) > maxCompactionPromptBytes {
		prompt = contextUTF8Prefix(prompt, maxCompactionPromptBytes)
	}
	req := &types.MessagesRequest{
		Model: model,
		Messages: []types.Message{{
			Role:    "user",
			Content: mustJSON(prompt),
		}},
		MaxTokens: 1024,
	}
	compactCtx, cancel := context.WithTimeout(ctx, compactionProviderTimeout)
	defer cancel()
	type streamResult struct {
		stream <-chan types.SSEEvent
		err    error
	}
	started := make(chan streamResult, 1)
	go func() {
		stream, err := provider.StreamMessage(compactCtx, req, nil)
		started <- streamResult{stream: stream, err: err}
	}()
	var stream <-chan types.SSEEvent
	select {
	case <-compactCtx.Done():
		return "", "timeout"
	case result := <-started:
		if result.err != nil {
			return "", "stream_error"
		}
		stream = result.stream
	}
	var summary strings.Builder
	for {
		var event types.SSEEvent
		var ok bool
		select {
		case <-compactCtx.Done():
			return "", "timeout"
		case event, ok = <-stream:
			if !ok {
				value := strings.TrimSpace(summary.String())
				if value == "" {
					return "", "empty_summary"
				}
				return value, ""
			}
		}
		if event.Type == "content_block_delta" && event.Delta != nil {
			var delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Delta, &delta) == nil && delta.Text != "" {
				remaining := maxCompactionNarrativeBytes - summary.Len()
				if remaining > 0 {
					summary.WriteString(contextUTF8Prefix(delta.Text, remaining))
				}
			}
		}
		if event.Type == "message_stop" {
			value := strings.TrimSpace(summary.String())
			if value == "" {
				return "", "empty_summary"
			}
			return value, ""
		}
	}
}

func compactionNarrativeSource(messages []types.Message) string {
	units := historyUnits(messages)
	selected := make([][]string, 0, len(units))
	selectedBytes := 0
	limit := maxCompactionPromptBytes / 2
	for unitIndex := len(units) - 1; unitIndex >= 0; unitIndex-- {
		unit := units[unitIndex]
		lines := make([]string, 0, unit.end-unit.start)
		unitBytes := 0
		for index := unit.start; index < unit.end; index++ {
			message := messages[index]
			text := sanitizeSnapshotText(messageTextForCompaction(message), 400)
			if text == "" {
				continue
			}
			line := fmt.Sprintf("[%s] %s\n", message.Role, text)
			lines = append(lines, line)
			unitBytes += len(line)
		}
		if len(lines) == 0 {
			continue
		}
		if selectedBytes+unitBytes > limit {
			break
		}
		selected = append(selected, lines)
		selectedBytes += unitBytes
	}
	var builder strings.Builder
	for unitIndex := len(selected) - 1; unitIndex >= 0; unitIndex-- {
		for _, line := range selected[unitIndex] {
			builder.WriteString(line)
		}
	}
	return builder.String()
}

func messageTextForCompaction(message types.Message) string {
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return text
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(message.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		var blockType string
		_ = json.Unmarshal(block["type"], &blockType)
		switch blockType {
		case "text":
			var value string
			_ = json.Unmarshal(block["text"], &value)
			if value != "" {
				parts = append(parts, value)
			}
		case "tool_use":
			var name string
			_ = json.Unmarshal(block["name"], &name)
			parts = append(parts, "tool_use "+sanitizeSnapshotText(name, 128))
		case "tool_result":
			parts = append(parts, "tool_result "+shortDigest(string(block["tool_use_id"])))
		}
	}
	return strings.Join(parts, "; ")
}

type compactToolPair struct {
	ID        string
	Name      string
	Input     json.RawMessage
	Result    string
	IsError   bool
	HasResult bool
}

func collectToolPairs(messages []types.Message) []compactToolPair {
	order := make([]string, 0)
	pairs := make(map[string]*compactToolPair)
	for _, message := range messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			var blockType string
			_ = json.Unmarshal(block["type"], &blockType)
			switch blockType {
			case "tool_use":
				var id, name string
				_ = json.Unmarshal(block["id"], &id)
				_ = json.Unmarshal(block["name"], &name)
				if id == "" {
					continue
				}
				if _, exists := pairs[id]; !exists {
					order = append(order, id)
					pairs[id] = &compactToolPair{ID: id}
				}
				pairs[id].Name = name
				pairs[id].Input = append(json.RawMessage(nil), block["input"]...)
			case "tool_result":
				var id string
				_ = json.Unmarshal(block["tool_use_id"], &id)
				if id == "" {
					continue
				}
				if _, exists := pairs[id]; !exists {
					order = append(order, id)
					pairs[id] = &compactToolPair{ID: id}
				}
				if json.Unmarshal(block["content"], &pairs[id].Result) != nil {
					pairs[id].Result = string(block["content"])
				}
				_ = json.Unmarshal(block["is_error"], &pairs[id].IsError)
				pairs[id].HasResult = true
			}
		}
	}
	result := make([]compactToolPair, 0, len(order))
	for _, id := range order {
		pair := pairs[id]
		if pair == nil || pair.Name == "" || !pair.HasResult {
			continue
		}
		result = append(result, *pair)
	}
	return result
}

type messageRange struct {
	start int
	end   int
}

func historyUnits(messages []types.Message) []messageRange {
	units := make([]messageRange, 0, len(messages))
	for index := 0; index < len(messages); {
		end := index + 1
		if index+1 < len(messages) && messages[index].Role == "assistant" && messages[index+1].Role == "user" {
			uses := toolUseIDs(messages[index].Content)
			results := toolResultIDs(messages[index+1].Content)
			if len(uses) > 0 && sameStringSet(uses, results) {
				end = index + 2
			}
		}
		units = append(units, messageRange{start: index, end: end})
		index = end
	}
	return units
}

func recentHistoryStart(messages []types.Message, keepUnits int) int {
	units := historyUnits(messages)
	if len(units) <= keepUnits {
		return 0
	}
	return units[len(units)-keepUnits].start
}

func unresolvedHistoricalPairDigests(messages []types.Message) []string {
	cutoff := recentHistoryStart(messages, compactionRecentUnits)
	if cutoff <= 0 {
		return nil
	}
	useIndex := make(map[string]int)
	resultIndex := make(map[string]int)
	paired := make(map[string]bool)
	for index, message := range messages {
		for _, id := range toolUseIDs(message.Content) {
			if _, exists := useIndex[id]; !exists {
				useIndex[id] = index
			}
		}
		for _, id := range toolResultIDs(message.Content) {
			if _, exists := resultIndex[id]; !exists {
				resultIndex[id] = index
			}
		}
		if index+1 < len(messages) && message.Role == "assistant" && messages[index+1].Role == "user" {
			uses := toolUseIDs(message.Content)
			results := toolResultIDs(messages[index+1].Content)
			if len(uses) > 0 && sameStringSet(uses, results) {
				for _, id := range uses {
					paired[id] = true
				}
			}
		}
	}
	all := make(map[string]struct{}, len(useIndex)+len(resultIndex))
	for id := range useIndex {
		all[id] = struct{}{}
	}
	for id := range resultIndex {
		all[id] = struct{}{}
	}
	var digests []string
	for id := range all {
		if paired[id] {
			continue
		}
		useAt, hasUse := useIndex[id]
		resultAt, hasResult := resultIndex[id]
		if (hasUse && useAt < cutoff) || (hasResult && resultAt < cutoff) {
			digests = append(digests, shortDigest(id))
		}
	}
	sort.Strings(digests)
	return digests
}

func toolUseIDs(content json.RawMessage) []string {
	return contentBlockIDs(content, "tool_use", "id")
}

func toolResultIDs(content json.RawMessage) []string {
	return contentBlockIDs(content, "tool_result", "tool_use_id")
}

func contentBlockIDs(content json.RawMessage, wantedType, idField string) []string {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var ids []string
	for _, block := range blocks {
		var blockType, id string
		_ = json.Unmarshal(block["type"], &blockType)
		_ = json.Unmarshal(block[idField], &id)
		if blockType == wantedType && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func toolNamesByUseID(messages []types.Message) map[string]string {
	result := make(map[string]string)
	for _, pair := range collectToolPairs(messages) {
		result[pair.ID] = pair.Name
	}
	return result
}

func firstObjectiveIndex(messages []types.Message) int {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != "user" || len(toolResultIDs(message.Content)) > 0 {
			continue
		}
		if strings.TrimSpace(userInstructionText(message)) != "" {
			return index
		}
	}
	return -1
}

func latestObjectiveText(messages []types.Message) string {
	if index := firstObjectiveIndex(messages); index >= 0 {
		return userInstructionText(messages[index])
	}
	return ""
}

func userInstructionTexts(messages []types.Message) []string {
	var instructions []string
	for _, message := range messages {
		if message.Role == "assistant" {
			if previous, ok := priorCompactionUserInstructions(message); ok {
				instructions = append([]string(nil), previous...)
				continue
			}
		}
		if message.Role != "user" || len(toolResultIDs(message.Content)) > 0 {
			continue
		}
		if text := strings.TrimSpace(userInstructionText(message)); text != "" {
			instructions = appendRecentInstruction(instructions, text)
		}
	}
	return boundUserInstructionCount(instructions)
}

func priorCompactionUserInstructions(message types.Message) ([]string, bool) {
	text := messageTextForCompaction(message)
	const prefix = "[Structured Conversation State]\n"
	if !strings.HasPrefix(text, prefix) {
		return nil, false
	}
	payload := strings.TrimPrefix(text, prefix)
	if structured, _, found := strings.Cut(payload, "\n\nNarrative:\n"); found {
		payload = structured
	}
	var snapshot CompactionSnapshot
	if json.Unmarshal([]byte(payload), &snapshot) != nil ||
		snapshot.Version != 1 || snapshot.Digest == "" || compactionSnapshotDigest(snapshot) != snapshot.Digest {
		return nil, false
	}
	return append([]string(nil), snapshot.UserInstructions...), true
}

func appendRecentInstruction(instructions []string, text string) []string {
	for index, previous := range instructions {
		if previous == text {
			copy(instructions[index:], instructions[index+1:])
			instructions = instructions[:len(instructions)-1]
			break
		}
	}
	return append(instructions, text)
}

func renderRecentUserInstructions(messages []types.Message, latestOverride string) string {
	instructions := userInstructionTexts(messages)
	if latestOverride = strings.TrimSpace(latestOverride); latestOverride != "" {
		if len(instructions) == 0 {
			instructions = append(instructions, latestOverride)
		} else {
			instructions[len(instructions)-1] = latestOverride
		}
	}
	instructions = sanitizeUserInstructionList(instructions)
	if len(instructions) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("## User instructions in conversation order\n")
	builder.WriteString("Apply later corrections when earlier user instructions conflict.\n")
	for _, instruction := range instructions {
		builder.WriteString("- ")
		builder.WriteString(instruction)
		builder.WriteByte('\n')
	}
	return strings.TrimSpace(builder.String())
}

func userInstructionText(message types.Message) string {
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return meaningfulInstructionText(text)
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(message.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		var blockType string
		if json.Unmarshal(block["type"], &blockType) != nil || blockType != "text" {
			continue
		}
		var value string
		if json.Unmarshal(block["text"], &value) == nil && strings.TrimSpace(value) != "" {
			parts = append(parts, strings.TrimSpace(value))
		}
	}
	return meaningfulInstructionText(strings.Join(parts, "\n"))
}

func meaningfulInstructionText(value string) string {
	value = strings.TrimSpace(value)
	if value == "(preserved context continues)" {
		return ""
	}
	return value
}

func compactEditPath(input json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}
	for _, name := range []string{"file_path", "path", "file"} {
		var value string
		if json.Unmarshal(fields[name], &value) == nil && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var toolResultReferencePattern = regexp.MustCompile(`(?i)tool-result://sha256:([a-f0-9]{64})`)

func embeddedToolResultReference(content string) string {
	match := toolResultReferencePattern.FindStringSubmatch(content)
	if len(match) != 2 {
		return ""
	}
	return "sha256:" + strings.ToLower(match[1])
}

func compactionEvidenceSnapshot(ledger *EvidenceLedger) (EvidenceGateResult, []EvidenceRecord) {
	if ledger == nil {
		return EvidenceGateResult{}, nil
	}
	gate := ledger.Evaluate()
	ledger.mu.Lock()
	records := append([]EvidenceRecord(nil), ledger.Records...)
	ledger.mu.Unlock()
	return gate, records
}

func sanitizeSnapshotText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes > 0 && len(value) > maxBytes {
		value = contextUTF8Prefix(value, maxBytes)
	}
	return sanitizeReceiptString(value)
}

func sanitizeSnapshotList(values []string, maxItems, maxBytes int) []string {
	if len(values) == 0 || maxItems <= 0 {
		return nil
	}
	if len(values) > maxItems {
		values = values[:maxItems]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if sanitized := sanitizeSnapshotText(value, maxBytes); sanitized != "" {
			result = append(result, sanitized)
		}
	}
	return result
}

func sanitizeRecentSnapshotList(values []string, maxItems, maxBytes int) []string {
	values = tailStrings(values, maxItems)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if sanitized := sanitizeRecentSnapshotText(value, maxBytes); sanitized != "" {
			result = append(result, sanitized)
		}
	}
	return result
}

// boundUserInstructionCount keeps the original request and the most recent
// corrections within a fixed-size envelope. The middle is intentionally
// bounded when a conversation contains more than the snapshot limit.
func boundUserInstructionCount(values []string) []string {
	if len(values) <= maxCompactionUserInstructions {
		return append([]string(nil), values...)
	}
	result := make([]string, 0, maxCompactionUserInstructions)
	result = append(result, values[0])
	result = append(result, values[len(values)-(maxCompactionUserInstructions-1):]...)
	return result
}

func sanitizeUserInstructionList(values []string) []string {
	return sanitizeRecentSnapshotList(boundUserInstructionCount(values), maxCompactionUserInstructions, 600)
}

func sanitizeRecentSnapshotText(value string, maxBytes int) string {
	value = sanitizeReceiptString(strings.TrimSpace(value))
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	const marker = "\n...[middle omitted]...\n"
	if maxBytes <= len(marker) {
		return contextUTF8Suffix(value, maxBytes)
	}
	available := maxBytes - len(marker)
	headBytes := available * 2 / 3
	tailBytes := available - headBytes
	return contextUTF8Prefix(value, headBytes) + marker + contextUTF8Suffix(value, tailBytes)
}

func sanitizeCompactionPlanEvidence(values []CompactionPlanEvidence) []CompactionPlanEvidence {
	if len(values) > maxCompactionContextItems {
		values = values[len(values)-maxCompactionContextItems:]
	}
	result := make([]CompactionPlanEvidence, 0, len(values))
	for _, value := range values {
		result = append(result, CompactionPlanEvidence{
			StageID:             sanitizeSnapshotText(value.StageID, 256),
			StageStatus:         sanitizeSnapshotText(value.StageStatus, 64),
			AttemptStatus:       sanitizeSnapshotText(value.AttemptStatus, 64),
			AttemptPlanRevision: value.AttemptPlanRevision,
			ReceiptDigest:       sanitizePlanEvidenceDigest(value.ReceiptDigest),
			CriteriaDigest:      sanitizePlanEvidenceDigest(value.CriteriaDigest),
			VerificationStatus:  sanitizeSnapshotText(value.VerificationStatus, 64),
			CompletionStatus:    sanitizeSnapshotText(value.CompletionStatus, 64),
		})
	}
	return result
}

func sanitizePlanEvidenceDigest(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	hexDigest := strings.TrimPrefix(value, "sha256:")
	if len(hexDigest) != 64 {
		return ""
	}
	for _, char := range hexDigest {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return ""
		}
	}
	return "sha256:" + hexDigest
}

func tailStrings(values []string, limit int) []string {
	if len(values) == 0 || limit <= 0 {
		return nil
	}
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return append([]string(nil), values...)
}

func compactionSnapshotDigest(snapshot CompactionSnapshot) string {
	snapshot.Digest = ""
	encoded, _ := json.Marshal(snapshot)
	return sha256String(string(encoded))
}

func shortDigest(value string) string {
	digest := strings.TrimPrefix(sha256String(value), "sha256:")
	if len(digest) > 16 {
		digest = digest[:16]
	}
	return digest
}

func uniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
