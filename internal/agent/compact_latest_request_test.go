package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestDeterministicCompactionRetainsLatestPendingUserRequest(t *testing.T) {
	var history []types.Message
	message := func(role, text string) types.Message {
		encoded, _ := json.Marshal(text)
		return types.Message{Role: role, Content: encoded}
	}
	for index := 0; index < 30; index++ {
		history = append(history, message("user", fmt.Sprintf("Previous task %d", index)), message("assistant", "Earlier task completed."))
	}
	const latest = "Stop the earlier task. Inspect only the new regression in parser.go and report its cause."
	history = append(history, message("user", latest))
	for pass := 0; pass < 2; pass++ {
		result := BuildDeterministicCompaction(history, CompactionState{})
		if len(result.Messages) == 0 {
			t.Fatal("compaction discarded all history")
		}
		last := result.Messages[len(result.Messages)-1]
		var text string
		if err := json.Unmarshal(last.Content, &text); err != nil {
			t.Fatal(err)
		}
		if last.Role != "user" || text != latest {
			t.Fatalf("pass %d lost pending request: last role=%q text=%q", pass, last.Role, text)
		}
		count := 0
		for _, item := range result.Messages {
			if item.Role != "user" {
				continue
			}
			var content string
			if json.Unmarshal(item.Content, &content) == nil {
				count += strings.Count(content, latest)
			}
		}
		if count != 1 {
			t.Fatalf("pass %d latest request occurrences in user messages=%d", pass, count)
		}
		history = result.Messages
	}
}
