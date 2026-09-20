package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// decision exercises the production structured compactor and then asks the
// model to recover a changed historical decision. The final question supplies
// neither the old nor the current answer, and no answer-bearing file is read.
func (p lifecycleProbe) decision(ctx context.Context) observedAgentProbe {
	limit := 2 + int((p.execution.Case.Seed+int64(p.execution.Attempt))%5)
	current := fmt.Sprintf("CURRENT approved decision: retry_limit=%d; authorization=required. This replaces the earlier retry_limit=19 and authorization=optional decision.", limit)
	message := func(role, text string) types.Message {
		content, _ := json.Marshal(text)
		return types.Message{Role: role, Content: content}
	}
	history := []types.Message{
		message("user", "Initial decision: retry_limit=19; authorization=optional."),
		message("assistant", "Recorded the initial decision."),
		message("user", current),
		message("assistant", "Recorded the replacement decision; the initial choice is superseded."),
	}
	for index := 0; index < 32; index++ {
		history = append(history,
			message("user", fmt.Sprintf("Archive unrelated progress note %d. %s", index, strings.Repeat("Historical fixture observation with no policy change. ", 32))),
			message("assistant", "Archived without changing the approved decision."),
		)
	}
	question := fmt.Sprintf("Recall the latest approved retry and authorization decision from the compacted history. Do not read files or use tools. Answer with %s on the first line, then one JSON object with keys retry_limit (number) and authorization (string). Output no other text.", p.fixture.marker)
	compacted := agent.BuildDeterministicCompaction(history, agent.CompactionState{
		Objective: "Retain the most recently approved decision across history compaction.",
		Decisions: []string{current},
	})
	fixture := p.fixture
	fixture.prompt = question
	followup := append(compacted.Messages, message("user", question))
	observation := p.run(ctx, p.execution.WorkspaceRoot, followup, fixture, p.options)
	accepted := decisionProbeAnswerMatches(observation.finalText, fixture.marker, limit)
	compactedHistory := compacted.Snapshot.AfterMessages < compacted.Snapshot.BeforeMessages && compacted.Snapshot.Digest != ""
	passed := observation.value.Success && !observation.transportFailed && !observation.value.Malformed && observation.toolStarts == 0 && accepted && compactedHistory
	observation.value.Success = passed
	observation.value.FalseDone = !observation.transportFailed && !passed
	observation.value.ArtifactDigest = digestJSON(struct {
		Snapshot string
		Answer   string
		Accepted bool
	}{compacted.Snapshot.Digest, digestJSON(observation.finalText), accepted})
	observation.value.TraceDigest = digestJSON(struct {
		Run           string
		Snapshot      string
		Before, After int
		Accepted      bool
	}{observation.value.TraceDigest, compacted.Snapshot.Digest, compacted.Snapshot.BeforeMessages, compacted.Snapshot.AfterMessages, accepted})
	return observation
}

func decisionProbeAnswerMatches(text, marker string, retryLimit int) bool {
	lines := strings.SplitN(strings.TrimSpace(text), "\n", 2)
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != marker {
		return false
	}
	var answer map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(lines[1])), &answer) != nil || len(answer) != 2 {
		return false
	}
	var limit int
	var authorization string
	return json.Unmarshal(answer["retry_limit"], &limit) == nil &&
		json.Unmarshal(answer["authorization"], &authorization) == nil &&
		limit == retryLimit && authorization == "required"
}
