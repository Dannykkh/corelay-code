package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestSubAgentRunModeContractAndBoundedSnapshot(t *testing.T) {
	now := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	task := &SubAgentTask{
		ID:          "sub-17",
		Name:        "parser-worker",
		Instruction: "Repair the parser and report the outcome.",
		Files:       []string{"private-file-sentinel.go"},
	}
	mode := newSubAgentRunMode(task)

	if mode.Name() != "subagent" || mode.CurrentStep() != task.ID {
		t.Fatalf("mode identity = name %q step %q", mode.Name(), mode.CurrentStep())
	}
	suffix := mode.SystemPromptSuffix()
	for _, want := range []string{
		"/no_think",
		"Task: parser-worker",
		"Instruction: " + task.Instruction,
		"Write/Edit targets are restricted",
	} {
		if !strings.Contains(suffix, want) {
			t.Errorf("SystemPromptSuffix() missing %q:\n%s", want, suffix)
		}
	}
	for _, unwanted := range []string{task.Files[0], "You have access to tools"} {
		if strings.Contains(suffix, unwanted) {
			t.Errorf("SystemPromptSuffix() contains unsupported detail %q:\n%s", unwanted, suffix)
		}
	}

	if _, err := mode.Advance(RunModeTurn{Text: "premature", Now: now}); err == nil {
		t.Fatal("Advance() before Start() error = nil")
	}
	start, err := mode.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Continue || start.UserPrompt != task.Instruction {
		t.Fatalf("Start() = %+v, want original instruction", start)
	}
	if _, err := mode.Start(now); err == nil {
		t.Fatal("second Start() error = nil")
	}

	terminal, err := mode.Advance(RunModeTurn{
		Text:      "final tool-less response",
		Iteration: 3,
		Now:       now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Continue || terminal.StopReason != "complete" || mode.FinalText() != "final tool-less response" {
		t.Fatalf("terminal mode state = directive %+v text %q", terminal, mode.FinalText())
	}

	snapshot := mode.Snapshot(RunModeMetrics{
		TotalTools:      4,
		EstimatedTokens: 1234,
		Sandbox:         []SandboxExecutionRecord{{ToolID: "one"}, {ToolID: "two"}},
	})
	if err := validateRunModeSnapshot(mode, snapshot); err != nil {
		t.Fatalf("validateRunModeSnapshot() error = %v", err)
	}
	state, ok := snapshot.State.(subAgentRunModeState)
	if !ok {
		t.Fatalf("snapshot state type = %T", snapshot.State)
	}
	if state.TaskID != task.ID || !state.Started || !state.Finished || state.FinalIteration != 3 ||
		state.TotalTools != 4 || state.EstimatedTokens != 1234 || state.SandboxReports != 2 {
		t.Fatalf("snapshot state = %+v", state)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{task.Instruction, task.Files[0], "final tool-less response"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("snapshot leaked task content %q: %s", secret, encoded)
		}
	}
}

func TestSubAgentEventReducerRequiresSuccessfulDone(t *testing.T) {
	typedSuccess := map[string]any{"terminalState": EvidenceTerminalUnverified}
	typedIncomplete := map[string]any{
		"terminalState":    EvidenceTerminalBlocked,
		"completionStatus": CompletionStatusIncomplete,
	}
	tests := []struct {
		name      string
		events    []Event
		runErr    error
		wantOK    bool
		wantText  string
		wantFinal bool
	}{
		{name: "legacy done", events: []Event{{Type: "done"}}, wantOK: true, wantFinal: true},
		{name: "typed successful done", events: []Event{{Type: "done", Data: typedSuccess}}, wantOK: true, wantFinal: true},
		{name: "typed incomplete done", events: []Event{{Type: "done", Data: typedIncomplete}}, wantText: "incomplete"},
		{name: "error before done remains failure", events: []Event{{Type: "error", Data: "provider failed"}, {Type: "done"}}, wantText: "provider failed"},
		{name: "context blocked", events: []Event{{Type: "context_blocked", Data: ContextBlockedEvent{Message: "request does not fit"}}}, wantText: "request does not fit"},
		{name: "closed without done", events: []Event{{Type: "status", Data: "working"}}, wantText: "without a done event"},
		{name: "late cancel after done", events: []Event{{Type: "done"}}, runErr: context.Canceled, wantOK: true, wantFinal: true},
		{name: "late deadline after done", events: []Event{{Type: "done"}}, runErr: context.DeadlineExceeded, wantOK: true, wantFinal: true},
		{name: "cancel without done", events: []Event{{Type: "status", Data: "working"}}, runErr: context.Canceled, wantText: "canceled"},
		{name: "deadline without done", events: []Event{{Type: "status", Data: "working"}}, runErr: context.DeadlineExceeded, wantText: "deadline"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode := newSubAgentRunMode(&SubAgentTask{ID: "sub-reducer", Instruction: "work"})
			if _, err := mode.Start(time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := mode.Advance(RunModeTurn{Text: "preserved final response", Iteration: 1}); err != nil {
				t.Fatal(err)
			}
			reducer := newSubAgentEventReducer("sub-reducer", mode)
			for _, event := range test.events {
				reducer.Observe(event)
			}
			result := reducer.Result(test.runErr)
			if result.Success != test.wantOK {
				t.Fatalf("Result().Success = %v, want %v; result=%+v", result.Success, test.wantOK, result)
			}
			if test.wantFinal && result.Text != "preserved final response" {
				t.Fatalf("Result().Text = %q, want preserved response", result.Text)
			}
			if test.wantText != "" && !strings.Contains(strings.ToLower(result.Text), strings.ToLower(test.wantText)) {
				t.Fatalf("Result().Text = %q, want substring %q", result.Text, test.wantText)
			}
			if len(test.events) > 0 && test.events[len(test.events)-1].Type == "done" && !reducer.observer.Completed() {
				t.Fatal("done event was not fed to DurableRunObserver")
			}
		})
	}
}

func TestSubAgentEventReducerPreservesSandboxWithoutRecording(t *testing.T) {
	mode := newSubAgentRunMode(&SubAgentTask{ID: "sub-sandbox", Instruction: "run check"})
	if _, err := mode.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := mode.Advance(RunModeTurn{Text: "sandbox result", Iteration: 2}); err != nil {
		t.Fatal(err)
	}
	reducer := newSubAgentEventReducer("sub-sandbox", mode)
	report := sandbox.Report{Runner: "fixture", Started: true}
	reducer.Observe(Event{Type: "tool_result", Data: map[string]any{
		"id": "tool-1", "name": "Bash", "result": "ok", "executed": true, "sandbox": report,
	}})
	reducer.Observe(Event{Type: "done"})
	result := reducer.Result(nil)
	if !result.Success || result.ToolCalls != 1 || len(result.Sandbox) != 1 {
		t.Fatalf("Result() = %+v", result)
	}
	if result.Sandbox[0].ToolID != "tool-1" || result.Sandbox[0].ToolName != "Bash" ||
		result.Sandbox[0].Report.Runner != "fixture" {
		t.Fatalf("sandbox records = %+v", result.Sandbox)
	}
}

func TestSubAgentEventReducerPreservesBoundedPostToolPartialTextOnFailure(t *testing.T) {
	mode := newSubAgentRunMode(&SubAgentTask{ID: "sub-partial", Instruction: "work"})
	if _, err := mode.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	reducer := newSubAgentEventReducer("sub-partial", mode)
	reducer.Observe(Event{Type: "text", Data: "discard-before-result"})
	reducer.Observe(Event{Type: "tool_result", Data: map[string]any{
		"id": "tool-1", "name": "Read", "result": "ok", "executed": true,
	}})
	reducer.Observe(Event{Type: "text", Data: "preserve-after-result " + strings.Repeat("x", subAgentPartialTextLimit)})
	reducer.Observe(Event{Type: "error", Data: "provider stream failed"})

	result := reducer.Result(nil)
	if result.Success || !strings.Contains(result.Text, "provider stream failed") ||
		!strings.Contains(result.Text, "preserve-after-result") || strings.Contains(result.Text, "discard-before-result") {
		t.Fatalf("failure result = %q", result.Text)
	}
	if len(result.Text) > subAgentPartialTextLimit+256 {
		t.Fatalf("failure result exceeded bound: %d bytes", len(result.Text))
	}
}

func TestSubAgentOwnershipCheckerBindsWorkerAndOwnedPatterns(t *testing.T) {
	workDir := t.TempDir()
	manager := NewSubAgentManager(nil, "", workDir)
	checker := manager.ownershipChecker(&SubAgentTask{
		ID:    "sub-owner",
		Files: []string{"exact.go", "single/*", "recursive/**"},
	})

	for _, path := range []string{"exact.go", "single/child.go", "recursive/nested/child.go"} {
		if allowed, reason := checker("sub-owner", path); !allowed {
			t.Errorf("owned path %q rejected: %s", path, reason)
		}
	}
	for _, path := range []string{"exact.go.bak", "single/nested/child.go", "recursive-sibling/child.go"} {
		if allowed, _ := checker("sub-owner", path); allowed {
			t.Errorf("unowned path %q accepted", path)
		}
	}
	if allowed, reason := checker("different-worker", "exact.go"); allowed || !strings.Contains(strings.ToLower(reason), "identity") {
		t.Fatalf("mismatched worker decision = %v, %q", allowed, reason)
	}

	if err := os.MkdirAll(filepath.Join(workDir, "recursive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "sibling"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(workDir, "sibling"),
		filepath.Join(workDir, "recursive", "link"),
	); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if allowed, _ := checker("sub-owner", "recursive/link/escape.go"); allowed {
		t.Fatal("recursive ownership followed a symlink into a workspace sibling")
	}
}

func TestSubAgentManagerUsesCommonKernelAndDisablesSlashCommands(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	content, err := os.ReadFile("subagent.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	if strings.Count(source, "RunLoopWithOptions(") != 1 {
		t.Fatalf("RunLoopWithOptions call count = %d", strings.Count(source, "RunLoopWithOptions("))
	}
	for _, forbidden := range []string{".StreamMessage(", "dispatchToolCalls(", "applyToolResponsePolicy("} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("subagent.go contains duplicate loop primitive %q", forbidden)
		}
	}

	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps:  []alternateLoopStep{{text: "slash command stayed task data"}},
	}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", t.TempDir(), SubAgentManagerOptions{
		HarnessProfile: directAlternateLoopHarness(t),
	})
	task := &SubAgentTask{ID: "sub-slash", Name: "slash", Instruction: "/help", Status: "pending"}
	manager.run(task)

	if requests := provider.requestsSnapshot(); len(requests) != 1 {
		t.Fatalf("provider requests = %d, slash command was handled ambiently", len(requests))
	}
	if task.Status != "completed" || task.Result != "slash command stayed task data" {
		t.Fatalf("task = %#v", task)
	}
}

func TestSubAgentSpawnCopiesOwnedFiles(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps:  []alternateLoopStep{{text: "done"}},
	}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", t.TempDir(), SubAgentManagerOptions{
		HarnessProfile: directAlternateLoopHarness(t),
	})
	files := []string{"owned.go"}
	task := manager.Spawn("copy", "finish", files)
	taskID := task.ID
	files[0] = "caller-mutated.go"
	if len(task.Files) != 1 || task.Files[0] != "owned.go" {
		t.Fatalf("Spawn() retained caller-owned Files slice: %#v", task.Files)
	}
	task.Name = "caller-mutated"
	task.Files[0] = "caller-mutated.go"
	current := manager.GetTask(taskID)
	if current == nil || current.Name != "copy" || current.Files[0] != "owned.go" {
		t.Fatalf("Spawn() returned manager-owned state: %#v", current)
	}
	manager.Wait(3 * time.Second)
	completed := manager.GetTask(taskID)
	if completed == nil || completed.Status != "completed" {
		t.Fatalf("spawned task = %#v", completed)
	}
}

func TestSubAgentTaskQueriesReturnDetachedSnapshots(t *testing.T) {
	manager := &SubAgentManager{
		tasks: map[string]*SubAgentTask{
			"sub-1": {
				ID:      "sub-1",
				Status:  "running",
				Files:   []string{"owned.go"},
				Sandbox: []SandboxExecutionRecord{{ToolID: "tool-1"}},
			},
		},
	}

	task := manager.GetTask("sub-1")
	tasks := manager.GetTasks()
	if task == nil || len(tasks) != 1 {
		t.Fatalf("task snapshots = %#v / %#v", task, tasks)
	}
	task.Status = "caller-mutated"
	task.Files[0] = "caller-mutated.go"
	tasks[0].Sandbox[0].ToolID = "caller-mutated"

	current := manager.GetTask("sub-1")
	if current.Status != "running" || current.Files[0] != "owned.go" || current.Sandbox[0].ToolID != "tool-1" {
		t.Fatalf("caller mutation reached manager-owned task: %#v", current)
	}
}

func TestSubAgentEventMessageHandlesErrors(t *testing.T) {
	if got := subAgentEventMessage(errors.New("sentinel failure"), "fallback"); got != "sentinel failure" {
		t.Fatalf("subAgentEventMessage(error) = %q", got)
	}
}
