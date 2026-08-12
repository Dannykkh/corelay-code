package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestToolExecutionJournalRunsBeforeStartEventAndExecutor(t *testing.T) {
	call := dispatchTestCall("call-journal", "Read", map[string]interface{}{"file_path": "safe.txt"})
	var order []string
	var entry ToolExecutionJournalEntry
	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          t.TempDir(),
		AllowedTools:     dispatchAllowedTools("Read"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		RunID:            "run_journal",
		PreExecutionJournal: func(value ToolExecutionJournalEntry) error {
			entry = value
			order = append(order, "journal")
			return nil
		},
		Emit: func(event Event) {
			if event.Type == "tool_execution_start" {
				order = append(order, "start")
			}
		},
		Execute: func(toolUseBlock) (string, bool) {
			order = append(order, "execute")
			return "ok", false
		},
	})
	if len(results) != 1 || !results[0].Executed || results[0].IsError {
		t.Fatalf("results = %#v", results)
	}
	if strings.Join(order, ",") != "journal,start,execute" {
		t.Fatalf("order = %q", strings.Join(order, ","))
	}
	if entry.ID != call.ID || entry.Name != call.Name || entry.RunID != "run_journal" ||
		entry.InputDigest != toolInputDigest(call) || strings.Contains(entry.InputDigest, "safe.txt") {
		t.Fatalf("journal entry = %#v", entry)
	}
}

func TestToolExecutionJournalFailureSuppressesStartExecutorAndUnderlyingError(t *testing.T) {
	const secret = "raw-command-and-storage-path-C:/private/session.json"
	call := dispatchTestCall("call-journal-fail", "Read", map[string]interface{}{"file_path": "safe.txt"})
	executed := 0
	startEvents := 0
	results := dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          t.TempDir(),
		AllowedTools:     dispatchAllowedTools("Read"),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		RunID:            "run_journal_failure",
		PreExecutionJournal: func(ToolExecutionJournalEntry) error {
			return errors.New(secret)
		},
		Emit: func(event Event) {
			if event.Type == "tool_execution_start" {
				startEvents++
			}
		},
		Execute: func(toolUseBlock) (string, bool) {
			executed++
			return "unexpected", false
		},
	})
	if len(results) != 1 || !results[0].Synthetic || !results[0].IsError || results[0].Executed {
		t.Fatalf("results = %#v", results)
	}
	if executed != 0 || startEvents != 0 {
		t.Fatalf("journal failure reached start/executor: starts=%d execute=%d", startEvents, executed)
	}
	if strings.Contains(results[0].Content, secret) || strings.Contains(results[0].Display, secret) ||
		!strings.Contains(results[0].Content, "Tool execution journal failed") {
		t.Fatalf("journal failure result leaked details: %#v", results[0])
	}
}
