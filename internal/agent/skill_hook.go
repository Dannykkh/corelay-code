package agent

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/skills"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// Auto-skill creation hook — wires internal/skills into the agent loop,
// mirroring memory_hook.go's ExtractMemoriesAsync. After a normal session
// end, if the session was a complex, repeatable workflow, a background
// goroutine asks the model to author a reusable SKILL.md and writes it under
// the project skills dir (workDir/.claude/skills).
//
// Opt-in: set CORELAY_AUTOSKILL to "1", "on", "true", or "yes" to enable.
// ANICLEW_AUTOSKILL remains a compatibility fallback. Default OFF — unlike
// memory, this writes new skill files into the user's project, so it should be
// a deliberate choice. Failures are logged, never surfaced — a user's turn
// must not hinge on a best-effort background task.

// autoSkillEnabled reports whether the auto-skill hook is active.
func autoSkillEnabled() bool {
	v := strings.ToLower(renamedAgentEnv("CORELAY_AUTOSKILL", "ANICLEW_AUTOSKILL"))
	return v == "1" || v == "on" || v == "true" || v == "yes"
}

// CreateSkillAsync spawns a background goroutine that may author a SKILL.md
// from the finished conversation. No-ops when disabled, the provider is nil,
// the conversation is trivial, or the session is not complex enough. The
// passed context is NOT propagated — the goroutine uses a fresh 2-minute
// context so it is not torn down by the request lifecycle ending.
func CreateSkillAsync(_ context.Context, provider types.Provider, model, workDir string, messages []types.Message) {
	if !autoSkillEnabled() || provider == nil || len(messages) < 2 {
		return
	}
	stats := analyzeTaskStats(messages)
	if !stats.Eligible() {
		return
	}
	// Snapshot so later mutation in the caller cannot race us.
	snapshot := make([]types.Message, len(messages))
	copy(snapshot, messages)

	// Reuse memoryWG so the server's graceful-shutdown drain
	// (WaitForMemoryTasks) also waits on in-flight skill creation.
	memoryWG.Add(1)
	go func() {
		defer memoryWG.Done()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		skillsDir := filepath.Join(workDir, ".claude", "skills")
		existing := skills.ExistingSkillNames(skillsDir)
		conversation := renderConversationForExtraction(snapshot)
		prompt := skills.BuildSkillPrompt(existing, stats, conversation)

		resp, err := callModelCollect(ctx, provider, model, prompt, "Author the skill now, or return create:false.")
		if err != nil {
			log.Printf("[autoskill] call: %v", err)
			return
		}
		sk, err := skills.ParseSkill(resp)
		if err != nil {
			log.Printf("[autoskill] parse: %v", err)
			return
		}
		if sk == nil {
			return // model declined — not skill-worthy
		}
		path, err := skills.SaveSkill(skillsDir, sk)
		if err != nil {
			log.Printf("[autoskill] save: %v", err)
			return
		}
		log.Printf("[autoskill] created skill %q at %s", sk.Name, path)
	}()
}

// analyzeTaskStats walks the conversation counting tool_use blocks (by name)
// and tool_result errors, mirroring Claude Code's analyzeTaskComplexity.
// Error recovery = at least one tool error AND tool calls overall.
func analyzeTaskStats(messages []types.Message) skills.TaskStats {
	counts := map[string]int{}
	hadError := false
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			var blocks []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, blk := range blocks {
					if blk.Type == "tool_use" && blk.Name != "" {
						counts[blk.Name]++
					}
				}
			}
		case "user":
			var blocks []struct {
				Type    string `json:"type"`
				IsError bool   `json:"is_error"`
			}
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, blk := range blocks {
					if blk.Type == "tool_result" && blk.IsError {
						hadError = true
					}
				}
			}
		}
	}

	total := 0
	tools := make([]string, 0, len(counts))
	for name, c := range counts {
		total += c
		tools = append(tools, name)
	}
	sort.Strings(tools) // stable order for the prompt header and tests

	return skills.TaskStats{
		ToolCalls:        total,
		UniqueTools:      tools,
		HadErrorRecovery: hadError && total > 0,
	}
}
