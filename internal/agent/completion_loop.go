package agent

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	maxCompletionPromptEvidenceRefs = 64
	maxCompletionCorrectionAttempts = 2
)

func newRunCompletionContract(
	profile harness.HarnessProfile,
	anchor *PlanAnchor,
	runID string,
	evidenceNotRequired []string,
) (*CompletionContract, error) {
	if profile.PlanAnchorMode() != harness.PlanAnchorStrict {
		return nil, nil
	}
	if anchor == nil {
		return nil, fmt.Errorf("strict completion contract requires a PlanAnchor")
	}
	return NewCompletionContract(CompletionContractSpec{
		RunID:                       runID,
		PlanAnchor:                  *anchor,
		EvidenceNotRequiredCriteria: evidenceNotRequired,
	})
}

func completionRequiredToolNames(contract *CompletionContract) []string {
	if contract == nil {
		return nil
	}
	return []string{reportCompletionToolName}
}

// pinCompletionControlTool keeps the run-control transition available after
// budget pruning, category routing, plan-mode filtering, and exploration
// collapse. A conflicting definition is rejected instead of being replaced.
func pinCompletionControlTool(tools []types.ToolDef, contract *CompletionContract) ([]types.ToolDef, error) {
	if contract == nil {
		return tools, nil
	}
	required := ReportCompletionToolDef()
	found := 0
	for _, tool := range tools {
		if tool.Name != required.Name {
			continue
		}
		found++
		if found > 1 || tool.RuntimeBinding != nil ||
			!bytes.Equal(bytes.TrimSpace(tool.InputSchema), bytes.TrimSpace(required.InputSchema)) {
			return nil, fmt.Errorf("completion control tool definition is not immutable")
		}
	}
	if found == 1 {
		return tools, nil
	}
	return append(tools, required), nil
}

// pruneToolsPreservingRequired applies the existing relevance pruner to the
// non-required surface while reserving room for run-control tools. With no
// required names it delegates directly, preserving legacy ordering/behavior.
func pruneToolsPreservingRequired(
	tools []types.ToolDef,
	task string,
	budget int,
	requiredNames []string,
) ([]types.ToolDef, int, error) {
	if len(requiredNames) == 0 {
		kept, dropped := translate.PruneTools(tools, task, budget)
		return kept, dropped, nil
	}
	required, optional, err := partitionRequiredTools(tools, requiredNames)
	if err != nil {
		return nil, 0, err
	}
	if budget >= len(tools) {
		return append([]types.ToolDef(nil), tools...), 0, nil
	}
	optionalBudget := budget - len(required)
	if optionalBudget < 0 {
		optionalBudget = 0
	}
	var keptOptional []types.ToolDef
	if optionalBudget > 0 {
		keptOptional, _ = translate.PruneTools(optional, task, optionalBudget)
	}
	kept := make([]types.ToolDef, 0, len(keptOptional)+len(required))
	kept = append(kept, keptOptional...)
	kept = append(kept, required...)
	return kept, len(tools) - len(kept), nil
}

func partitionRequiredTools(
	tools []types.ToolDef,
	requiredNames []string,
) (required []types.ToolDef, optional []types.ToolDef, err error) {
	requiredSet := make(map[string]struct{}, len(requiredNames))
	for _, name := range requiredNames {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, nil, fmt.Errorf("required tool name is empty")
		}
		if _, duplicate := requiredSet[name]; duplicate {
			return nil, nil, fmt.Errorf("required tool name %q is duplicated", name)
		}
		requiredSet[name] = struct{}{}
	}
	seen := make(map[string]int, len(tools))
	for _, tool := range tools {
		seen[tool.Name]++
		if seen[tool.Name] > 1 {
			return nil, nil, fmt.Errorf("tool catalog contains duplicate definition %q", tool.Name)
		}
		if _, pinned := requiredSet[tool.Name]; pinned {
			required = append(required, tool)
		} else {
			optional = append(optional, tool)
		}
	}
	for name := range requiredSet {
		if seen[name] != 1 {
			return nil, nil, fmt.Errorf("required tool %q is unavailable", name)
		}
	}
	return required, optional, nil
}

func completionControlOnlyCalls(calls []toolUseBlock) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		if call.Name != reportCompletionToolName {
			return false
		}
	}
	return true
}

func completionExecutionResults(results []toolDispatchResult) []toolDispatchResult {
	filtered := make([]toolDispatchResult, 0, len(results))
	for _, result := range results {
		if result.Tool.Name != reportCompletionToolName {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func renderRunCompletionContract(
	contract *CompletionContract,
	evidence CompletionEvidenceSnapshot,
) (string, error) {
	if contract == nil {
		return "", nil
	}
	snapshot, err := contract.Snapshot()
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	fmt.Fprintf(
		&builder,
		"<completion-contract version=%q plan-revision=%q revision=%q status=%q>\n",
		fmt.Sprintf("%d", snapshot.Version),
		fmt.Sprintf("%d", snapshot.PlanRevision),
		fmt.Sprintf("%d", snapshot.Revision),
		snapshot.Status,
	)
	builder.WriteString("Completion is accepted only through ReportCompletion; prose does not change this state.\n")
	fmt.Fprintf(&builder, "Call ReportCompletion with expectedRevision=%d and exact criterion IDs.\n", snapshot.Revision)
	builder.WriteString("Criteria:\n")
	for _, criterion := range snapshot.Criteria {
		fmt.Fprintf(
			&builder,
			"- id=%s state=%s evidenceRequired=%t text=%q\n",
			criterion.ID,
			criterion.State,
			criterion.EvidenceRequired,
			criterion.Text,
		)
	}

	successful := make([]CompletionEvidenceRef, 0, len(evidence.Refs))
	for _, ref := range evidence.Refs {
		if ref.Succeeded {
			successful = append(successful, ref)
		}
	}
	if len(successful) > maxCompletionPromptEvidenceRefs {
		successful = successful[len(successful)-maxCompletionPromptEvidenceRefs:]
	}
	if len(successful) == 0 {
		builder.WriteString("Successful evidence digests: none recorded in this run.\n")
	} else {
		builder.WriteString("Successful evidence digests (use exact digest values):\n")
		for _, ref := range successful {
			fmt.Fprintf(
				&builder,
				"- evidence-digest=%s sequence=%d source=%s tool=%s\n",
				ref.Digest,
				ref.Sequence,
				ref.Source,
				ref.ToolName,
			)
		}
	}
	builder.WriteString("For evidenceRequired=false, provide a bounded assertion. If a criterion cannot be completed, report state=blocked with a bounded summary.\n")
	builder.WriteString("</completion-contract>")
	return builder.String(), nil
}

func completionCorrectionMessage(snapshot CompletionContractSnapshot) string {
	pending, satisfied, blocked := completionSnapshotCounts(snapshot)
	return fmt.Sprintf(
		"Completion was not accepted from prose. The run contract is %s at revision %d (%d pending, %d satisfied, %d blocked). Continue the work, then call ReportCompletion with the exact current revision and criterion IDs. Do not claim completion in prose alone.",
		snapshot.Status,
		snapshot.Revision,
		pending,
		satisfied,
		blocked,
	)
}

func completionSnapshotCounts(snapshot CompletionContractSnapshot) (pending, satisfied, blocked int) {
	for _, criterion := range snapshot.Criteria {
		switch criterion.State {
		case CompletionClaimSatisfied:
			satisfied++
		case CompletionClaimBlocked:
			blocked++
		default:
			pending++
		}
	}
	return pending, satisfied, blocked
}

func completionDoneMetadata(snapshot *CompletionContractSnapshot) map[string]interface{} {
	if snapshot == nil {
		return nil
	}
	_, satisfied, blocked := completionSnapshotCounts(*snapshot)
	return map[string]interface{}{
		"completionStatus":    snapshot.Status,
		"completionRevision":  snapshot.Revision,
		"completionCriteria":  len(snapshot.Criteria),
		"completionSatisfied": satisfied,
		"completionBlocked":   blocked,
	}
}
