package translate

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// coreTools are always kept regardless of budget — the minimal file/exec
// surface an agent needs to do real work. A weak model that loses these can do
// nothing useful, so they never get pruned.
var coreTools = map[string]bool{
	"Read": true, "Write": true, "Edit": true, "Bash": true,
	"Glob": true, "Grep": true, "LS": true, "Git": true,
}

// ToolBudget returns the configured maximum tool count from CORELAY_MAX_TOOLS.
// ANICLEW_MAX_TOOLS remains a compatibility fallback.
// Returns 0 (disabled) when unset, invalid, or <= 0 — so the feature is opt-in
// and never alters the toolset unless explicitly enabled.
func ToolBudget() int {
	value := strings.TrimSpace(os.Getenv("CORELAY_MAX_TOOLS"))
	if value == "" {
		value = strings.TrimSpace(os.Getenv("ANICLEW_MAX_TOOLS"))
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// PruneTools trims a large tool list down to `budget` entries so a weak local
// model is not overwhelmed (observed: Qwen3 hallucinates tool names at ~90
// tools but works at ~32). Selection priority:
//  1. core tools (always kept)
//  2. non-MCP tools, ranked by relevance to the last user message
//  3. MCP (mcp__*) tools, ranked by relevance — demoted, filled last
//
// Tool names are never modified; whole defs are kept or dropped. The input
// slice is not mutated. Kept tools are returned in their original order for
// stability. Returns the (possibly) pruned slice and the number dropped.
func PruneTools(tools []types.ToolDef, lastUserText string, budget int) (kept []types.ToolDef, dropped int) {
	if budget <= 0 || len(tools) <= budget {
		return tools, 0
	}

	query := tokenize(lastUserText)

	type ranked struct {
		idx   int
		score int
	}
	keep := make(map[int]bool, budget)

	// 1. Core tools always kept (they are few; counted against the budget).
	for i, t := range tools {
		if coreTools[t.Name] {
			keep[i] = true
		}
	}

	// Partition the rest into non-MCP and MCP, each ranked by relevance.
	var nonMCP, mcp []ranked
	for i, t := range tools {
		if keep[i] {
			continue
		}
		r := ranked{idx: i, score: relevance(t.Name+" "+t.Description, query)}
		if strings.HasPrefix(t.Name, "mcp__") {
			mcp = append(mcp, r)
		} else {
			nonMCP = append(nonMCP, r)
		}
	}
	// Higher score first; stable on ties (preserve original order via idx).
	byScore := func(s []ranked) {
		sort.SliceStable(s, func(a, b int) bool {
			if s[a].score != s[b].score {
				return s[a].score > s[b].score
			}
			return s[a].idx < s[b].idx
		})
	}
	byScore(nonMCP)
	byScore(mcp)

	// 2 & 3. Fill remaining budget: non-MCP first, then demoted MCP.
	for _, group := range [][]ranked{nonMCP, mcp} {
		for _, r := range group {
			if len(keep) >= budget {
				break
			}
			keep[r.idx] = true
		}
	}

	// Emit kept tools in original order.
	kept = make([]types.ToolDef, 0, len(keep))
	for i, t := range tools {
		if keep[i] {
			kept = append(kept, t)
		}
	}
	return kept, len(tools) - len(kept)
}

// relevance scores how well a tool's text matches the query tokens. Simple
// token-containment count — cheap and good enough to surface obviously
// task-relevant tools.
func relevance(toolText string, query map[string]bool) int {
	if len(query) == 0 {
		return 0
	}
	text := strings.ToLower(toolText)
	score := 0
	for w := range query {
		if strings.Contains(text, w) {
			score++
		}
	}
	return score
}

// tokenize lowercases and splits text into a set of word tokens >= 3 chars,
// dropping common English stopwords so they do not inflate every tool's score.
func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(raw) < 3 || stopwords[raw] {
			continue
		}
		out[raw] = true
	}
	return out
}

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "you": true,
	"this": true, "that": true, "use": true, "using": true, "your": true,
	"can": true, "create": true, "make": true, "file": true, "named": true,
	"please": true, "into": true, "from": true, "are": true, "was": true,
}

// lastUserMessageText extracts the most recent user message's text (best effort)
// from an Anthropic message list, for relevance scoring. Content may be a plain
// string or an array of content blocks.
func lastUserMessageText(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		raw := messages[i].Content
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) == nil {
			var b strings.Builder
			for _, bl := range blocks {
				if bl.Text != "" {
					b.WriteString(bl.Text)
					b.WriteByte(' ')
				}
			}
			return b.String()
		}
		return ""
	}
	return ""
}
