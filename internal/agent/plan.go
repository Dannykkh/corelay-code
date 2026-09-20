package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ── Context Compression ──

// CompressContext summarizes a conversation to reduce token usage.
func CompressContext(messages []map[string]string) string {
	if len(messages) < 6 {
		return "" // no need to compress short conversations
	}

	var summary strings.Builder
	summary.WriteString("## Conversation Summary\n\n")

	toolsUsed := make(map[string]int)
	filesModified := []string{}
	keyDecisions := []string{}

	for _, m := range messages {
		role := m["role"]
		content := m["content"]

		if role == "tool" {
			name := m["toolName"]
			toolsUsed[name]++
			if name == "Write" || name == "Edit" {
				var args struct {
					FilePath string `json:"file_path"`
				}
				json.Unmarshal([]byte(content), &args)
				if args.FilePath != "" {
					filesModified = append(filesModified, args.FilePath)
				}
			}
		}

		// Extract key decisions from assistant messages
		if role == "assistant" && len(content) > 100 {
			// Take first sentence as key point
			sentence := content
			if idx := strings.Index(content, ". "); idx > 0 && idx < 200 {
				sentence = content[:idx+1]
			}
			if len(sentence) > 200 {
				sentence = sentence[:200] + "..."
			}
			keyDecisions = append(keyDecisions, sentence)
		}
	}

	// Tools summary
	if len(toolsUsed) > 0 {
		summary.WriteString("**Tools used**: ")
		for name, count := range toolsUsed {
			summary.WriteString(fmt.Sprintf("%s(%d) ", name, count))
		}
		summary.WriteString("\n")
	}

	// Files modified
	if len(filesModified) > 0 {
		unique := uniqueStrings(filesModified)
		summary.WriteString("**Files modified**: " + strings.Join(unique, ", ") + "\n")
	}

	// Key decisions (last 5)
	if len(keyDecisions) > 5 {
		keyDecisions = keyDecisions[len(keyDecisions)-5:]
	}
	if len(keyDecisions) > 0 {
		summary.WriteString("\n**Key points**:\n")
		for _, d := range keyDecisions {
			summary.WriteString("- " + d + "\n")
		}
	}

	return summary.String()
}

// EstimateTokens roughly estimates token count from text.
func EstimateTokens(text string) int {
	return len(text) / 4
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
