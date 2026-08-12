package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type loopHookCall struct {
	ctx     context.Context
	policy  sandbox.Policy
	command sandbox.CommandSpec
}

type loopHookRunner struct {
	mu    sync.Mutex
	calls []loopHookCall
}

func (*loopHookRunner) Name() string { return "loop-hook-sandbox" }

func (*loopHookRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		FilesystemIsolation:  true,
		NetworkIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func (r *loopHookRunner) Run(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	copyCommand := command
	copyCommand.Args = append([]string(nil), command.Args...)
	copyCommand.Environment.Inherit = append([]string(nil), command.Environment.Inherit...)
	copyCommand.Environment.Set = make(map[string]string, len(command.Environment.Set))
	for key, value := range command.Environment.Set {
		copyCommand.Environment.Set[key] = value
	}
	r.mu.Lock()
	r.calls = append(r.calls, loopHookCall{ctx: ctx, policy: policy, command: copyCommand})
	r.mu.Unlock()
	report := sandbox.Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		EffectiveEnforcement: policy.Enforcement,
		Capabilities:         r.Capabilities(),
		AppliedIsolation: sandbox.Capabilities{
			FilesystemIsolation: true,
			NetworkIsolation:    true,
		},
		Started: true,
	}
	commandText := ""
	if len(command.Args) == 2 {
		commandText = command.Args[1]
	}
	if commandText == "pre-block" || commandText == "post-fail" {
		report.Failure = sandbox.FailureExecutionFailed
		return sandbox.Result{Started: true, ExitCode: 9, Err: errors.New("raw hook runner error")}, report
	}
	return sandbox.Result{Started: true, ExitCode: 0}, report
}

func (r *loopHookRunner) snapshot() []loopHookCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]loopHookCall(nil), r.calls...)
}

type loopHookRecorder struct {
	mu      sync.Mutex
	hooks   []hooks.HookResult
	summary RunSummary
}

func (*loopHookRecorder) RunStarted()                         {}
func (*loopHookRecorder) ReceiptWritten(string, AgentReceipt) {}

func (r *loopHookRecorder) HookRecorded(result hooks.HookResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, result)
}

func (r *loopHookRecorder) RunCompleted(summary RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summary = summary
}

func (r *loopHookRecorder) snapshot() ([]hooks.HookResult, RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hooks.HookResult(nil), r.hooks...), r.summary
}

func writeLoopHookConfig(t *testing.T, workDir string, values map[string][]map[string]any) {
	t.Helper()
	directory := filepath.Join(workDir, ".claude")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"hooks": values})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunLoopPreHookFailureBlocksToolAndUsesRunContext(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	writeLoopHookConfig(t, workDir, map[string][]map[string]any{
		"session_start": {{"command": "session-start"}},
		"pre_tool_use":  {{"command": "pre-block"}},
		"post_tool_use": {{"command": "post-must-not-run"}},
		"session_end":   {{"command": "session-end"}},
	})
	hookRunner := &loopHookRunner{}
	hookRegistry := hooks.NewRegistryWithOptions(hooks.RegistryOptions{Runner: hookRunner, ShellPath: "hook-shell"})
	toolRunner := &fakeBashRunner{name: "tool-must-not-run", capabilities: fakeBashCapabilities()}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_hook_block", "Bash", map[string]string{"command": "echo should-not-run"}),
		textStep("blocked safely"),
	}}
	recorder := &loopHookRecorder{}
	userContent, _ := json.Marshal("Execute the command once.")
	type hookContextKey struct{}
	ctx := context.WithValue(context.Background(), hookContextKey{}, "same-run")
	eventCh := make(chan Event, 64)
	go RunLoopWithOptions(ctx, provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workDir, RunOptions{
		HookRegistry:   hookRegistry,
		Recorder:       recorder,
		SandboxRunner:  toolRunner,
		SandboxPolicy:  fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy: EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, eventCh)
	for range eventCh {
	}
	if toolRunner.calls != 0 {
		t.Fatalf("blocked tool runner calls = %d", toolRunner.calls)
	}
	calls := hookRunner.snapshot()
	if len(calls) != 3 {
		t.Fatalf("hook runner calls = %d, want session/pre/session-end", len(calls))
	}
	for _, call := range calls {
		if got := call.ctx.Value(hookContextKey{}); got != "same-run" {
			t.Fatalf("hook lost run context: %v", got)
		}
	}
	commands := []string{calls[0].command.Args[1], calls[1].command.Args[1], calls[2].command.Args[1]}
	if commands[0] != "session-start" || commands[1] != "pre-block" || commands[2] != "session-end" {
		t.Fatalf("hook command order = %#v", commands)
	}
	recorded, summary := recorder.snapshot()
	if len(recorded) != 3 || recorded[1].Failure != hooks.HookFailureNonZeroExit || !recorded[1].Blocked {
		t.Fatalf("recorded hooks = %#v", recorded)
	}
	if len(summary.Hooks) != 3 || summary.Hooks[1].Failure != hooks.HookFailureNonZeroExit {
		t.Fatalf("summary hooks = %#v", summary.Hooks)
	}
}

func TestRunLoopPostHookFailureIsRecordedAndRawToolResultIsDigested(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	writeLoopHookConfig(t, workDir, map[string][]map[string]any{
		"post_tool_use": {{"command": "post-fail"}},
		"session_end":   {{"command": "session-end"}},
	})
	hookRunner := &loopHookRunner{}
	hookRegistry := hooks.NewRegistryWithOptions(hooks.RegistryOptions{Runner: hookRunner, ShellPath: "hook-shell"})
	toolRunner := &fakeBashRunner{name: "tool-success", capabilities: fakeBashCapabilities()}
	toolRunner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := fakeBashReport(toolRunner, policy)
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		report.AppliedIsolation.ProcessIsolation = true
		return sandbox.Result{Started: true, Stdout: []byte("raw-tool-result-sentinel\n"), ExitCode: 0}, report
	}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_hook_post", "Bash", map[string]string{"command": "echo result"}),
		textStep("done"),
	}}
	recorder := &loopHookRecorder{}
	userContent, _ := json.Marshal("Execute the command once.")
	eventCh := make(chan Event, 64)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workDir, RunOptions{
		HookRegistry:   hookRegistry,
		Recorder:       recorder,
		SandboxRunner:  toolRunner,
		SandboxPolicy:  fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy: EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, eventCh)
	for range eventCh {
	}
	if toolRunner.calls != 1 {
		t.Fatalf("tool runner calls = %d", toolRunner.calls)
	}
	calls := hookRunner.snapshot()
	if len(calls) != 2 || calls[0].command.Args[1] != "post-fail" || calls[1].command.Args[1] != "session-end" {
		t.Fatalf("hook calls = %#v", calls)
	}
	postEnvironment := calls[0].command.Environment.Set
	if _, exists := postEnvironment["TOOL_RESULT"]; exists {
		t.Fatal("raw tool result reached hook subprocess")
	}
	if postEnvironment["TOOL_RESULT_SHA256"] == "" || postEnvironment["TOOL_RESULT_BYTES"] == "" {
		t.Fatalf("tool result metadata = %#v", postEnvironment)
	}
	for _, value := range postEnvironment {
		if value == "raw-tool-result-sentinel" || value == "raw-tool-result-sentinel\n" {
			t.Fatal("raw tool result leaked through another environment key")
		}
	}
	recorded, summary := recorder.snapshot()
	if len(recorded) != 2 || recorded[0].Failure != hooks.HookFailureNonZeroExit || recorded[0].Blocked {
		t.Fatalf("post hook records = %#v", recorded)
	}
	if len(summary.Hooks) != 2 || summary.Hooks[0].Blocked {
		t.Fatalf("summary hooks = %#v", summary.Hooks)
	}
}
