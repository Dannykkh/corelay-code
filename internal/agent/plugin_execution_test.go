package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type recordingPluginRunner struct {
	name   string
	caps   sandbox.Capabilities
	calls  []sandbox.CommandSpec
	result sandbox.Result
	report sandbox.Report
}

func (runner *recordingPluginRunner) Name() string {
	if runner.name == "" {
		return "plugin-fixture-sandbox"
	}
	return runner.name
}

func (runner *recordingPluginRunner) Capabilities() sandbox.Capabilities {
	return runner.caps
}

func (runner *recordingPluginRunner) Run(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	copyCommand := command
	copyCommand.Args = append([]string(nil), command.Args...)
	copyCommand.Stdin = append([]byte(nil), command.Stdin...)
	runner.calls = append(runner.calls, copyCommand)
	report := runner.report
	if report.Runner == "" {
		report.Runner = runner.Name()
	}
	if report.Capabilities.IsZero() {
		report.Capabilities = runner.caps
	}
	if runner.result.Started {
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		if report.AppliedIsolation.IsZero() {
			report.AppliedIsolation = sandbox.Capabilities{
				FilesystemIsolation: true,
				NetworkIsolation:    true,
			}
		}
	}
	return runner.result, report
}

func securePluginCapabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		FilesystemIsolation:  true,
		NetworkIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func executablePluginFixture(t *testing.T, tool PluginTool) (*PluginManager, string, string) {
	t.Helper()
	workspace := t.TempDir()
	pluginDir := filepath.Join(workspace, "plugins", "fixture")
	if err := os.MkdirAll(filepath.Join(pluginDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(pluginDir, "bin", "fixture-tool")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if err := os.WriteFile(executable, []byte("immutable executable fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool.Executable = filepath.ToSlash(filepath.Join("bin", filepath.Base(executable)))
	manifest := Plugin{
		Name:        "fixture-plugin",
		Version:     "1.2.3",
		Description: "fixture",
		Tools: []PluginTool{
			tool,
			{Name: "legacy-only", Description: "listed only", Command: "echo must-not-run"},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewPluginManager(filepath.Join(workspace, "plugins"))
	if err := manager.LoadAllStrict(); err != nil {
		t.Fatalf("LoadAllStrict: %v", err)
	}
	return manager, workspace, executable
}

func pluginFixtureTool(name string) PluginTool {
	return PluginTool{
		Name:        name,
		Description: "bounded JSON fixture",
		Args:        []string{"--mode", "json"},
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
	}
}

func pluginDefinitionAndIdentity(
	t *testing.T,
	manager *PluginManager,
	workspace string,
	runner *recordingPluginRunner,
	observe func(PluginExecutionReport),
) (types.ToolDef, toolExecutorIdentity) {
	t.Helper()
	definitions, err := manager.ExecutableToolDefs(PluginExecutionOptions{
		Runner:        runner,
		Workspace:     workspace,
		ObserveReport: observe,
	})
	if err != nil {
		t.Fatalf("ExecutableToolDefs: %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("executable definitions = %d, want 1", len(definitions))
	}
	allowed := toolCatalogNames(definitions)
	identity, err := executorIdentityForAllowedTool(allowed, definitions[0].Name)
	if err != nil {
		t.Fatalf("executor identity: %v", err)
	}
	return definitions[0], identity
}

func approvedPluginExecutionInput(
	t *testing.T,
	input json.RawMessage,
	identity toolExecutorIdentity,
	sessionID string,
	runID string,
) json.RawMessage {
	t.Helper()
	bound, err := bindToolExecutionInput(input, identity)
	if err != nil {
		t.Fatalf("bindToolExecutionInput: %v", err)
	}
	proof, err := mintPluginApproval(
		"approval-"+strings.ReplaceAll(t.Name(), "/", "-"),
		sessionID,
		runID,
		identity.ToolName,
		identity.ExecutorID,
		input,
		time.Now().Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("mintPluginApproval: %v", err)
	}
	approved, err := bindPluginApprovalExecutionInput(bound, proof)
	if err != nil {
		t.Fatalf("bindPluginApprovalExecutionInput: %v", err)
	}
	return approved
}

func TestExecutablePluginHappyPathUsesBoundExecFormAndJSONStdin(t *testing.T) {
	manager, workspace, executable := executablePluginFixture(t, pluginFixtureTool("plugin_echo"))
	runner := &recordingPluginRunner{
		caps: securePluginCapabilities(),
		result: sandbox.Result{
			Started:  true,
			ExitCode: 0,
			Stdout:   []byte(`{"answer":"ok","token":"sk-12345678901234567890"}`),
		},
		report: sandbox.Report{Detail: "password=must-not-leak"},
	}
	var observed PluginExecutionReport
	definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, func(report PluginExecutionReport) {
		observed = report
	})
	if identity.Kind != toolExecutorPlugin || !strings.HasPrefix(identity.ExecutorID, "plugin:sha256:") {
		t.Fatalf("plugin identity = %#v", identity)
	}
	encodedDefinition, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContainAny(encodedDefinition, executable, "RuntimeBinding", "executable") {
		t.Fatalf("provider-visible ToolDef leaked runtime binding: %s", encodedDefinition)
	}

	input := json.RawMessage(`{"value":"hello"}`)
	bound := approvedPluginExecutionInput(t, input, identity, "session-happy", "run-happy")
	result, isError := ExecuteToolWithOptions(definition.Name, bound, workspace, ToolExecutionOptions{
		Context: context.Background(), ExpectedSessionID: "session-happy", ExpectedRunID: "run-happy",
	})
	if isError || !strings.Contains(result, `"status":"ok"`) || !strings.Contains(result, "answer") {
		t.Fatalf("plugin result = %s, error=%v", result, isError)
	}
	if strings.Contains(result, "sk-12345678901234567890") || strings.Contains(result, "must-not-leak") {
		t.Fatalf("plugin result leaked a secret: %s", result)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d", len(runner.calls))
	}
	call := runner.calls[0]
	callInfo, callErr := os.Stat(call.Path)
	executableInfo, executableErr := os.Stat(executable)
	if callErr != nil || executableErr != nil || !os.SameFile(callInfo, executableInfo) || strings.Join(call.Args, "|") != "--mode|json" {
		t.Fatalf("exec-form call = %#v executable=%q callErr=%v executableErr=%v args=%q", call, executable, callErr, executableErr, strings.Join(call.Args, "|"))
	}
	if string(call.Stdin) != string(input)+"\n" {
		t.Fatalf("stdin = %q", call.Stdin)
	}
	if len(call.Environment.Inherit) != 0 || len(call.Environment.Set) != 0 {
		t.Fatalf("plugin inherited environment: %#v", call.Environment)
	}
	if observed.ExecutorID != identity.ExecutorID || observed.Plugin != "fixture-plugin" || strings.Contains(observed.Detail, "must-not-leak") {
		t.Fatalf("observed report = %#v", observed)
	}

	legacyResult, legacyError := ExecuteTool("legacy-only", json.RawMessage(`{}`), workspace)
	if !legacyError || !strings.Contains(legacyResult, "Unknown tool") || len(runner.calls) != 1 {
		t.Fatalf("legacy command became executable: result=%q error=%v calls=%d", legacyResult, legacyError, len(runner.calls))
	}
}

func TestExecutablePluginRejectsEscapeSymlinkAndMCPCollision(t *testing.T) {
	t.Run("escape", func(t *testing.T) {
		manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_escape"))
		manager.plugins[0].Tools[0].Executable = "../outside-tool"
		outside := filepath.Join(manager.plugins[0].rootDir, "..", "outside-tool")
		if err := os.WriteFile(outside, []byte("outside"), 0o755); err != nil {
			t.Fatal(err)
		}
		runner := &recordingPluginRunner{caps: securePluginCapabilities()}
		if _, err := manager.ExecutableToolDefs(PluginExecutionOptions{Runner: runner, Workspace: workspace}); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("escape error = %v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatal("runner was called for an escaping executable")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		manager, workspace, executable := executablePluginFixture(t, pluginFixtureTool("plugin_symlink"))
		link := filepath.Join(filepath.Dir(executable), "linked-tool")
		if runtime.GOOS == "windows" {
			link += ".exe"
		}
		if err := os.Symlink(executable, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		manager.plugins[0].Tools[0].Executable = filepath.ToSlash(filepath.Join("bin", filepath.Base(link)))
		runner := &recordingPluginRunner{caps: securePluginCapabilities()}
		if _, err := manager.ExecutableToolDefs(PluginExecutionOptions{Runner: runner, Workspace: workspace}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink error = %v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatal("runner was called for a symlink executable")
		}
	})

	t.Run("MCP collision", func(t *testing.T) {
		tool := pluginFixtureTool("plugin_mcp_collision")
		manager, workspace, _ := executablePluginFixture(t, tool)
		installIdentityTestMCPClient(t, "plugin-mcp-collision", &MCPClient{
			executorID: newMCPExecutorID(),
			tools:      []MCPTool{{Name: tool.Name, InputSchema: tool.InputSchema}},
		})
		runner := &recordingPluginRunner{caps: securePluginCapabilities()}
		if _, err := manager.ExecutableToolDefs(PluginExecutionOptions{Runner: runner, Workspace: workspace}); err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("MCP collision error = %v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatal("runner was called for an MCP collision")
		}
	})
}

func TestExecutablePluginRequiresSecureSandboxBeforeRunnerCall(t *testing.T) {
	manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_secure"))
	tests := []struct {
		name   string
		runner *recordingPluginRunner
		policy sandbox.Policy
		want   string
	}{
		{
			name: "network isolation unavailable",
			runner: &recordingPluginRunner{caps: sandbox.Capabilities{
				FilesystemIsolation: true, EnvironmentFiltering: true, Timeouts: true, ProcessTreeKill: true,
			}},
			want: "network_isolation",
		},
		{
			name:   "network allow policy",
			runner: &recordingPluginRunner{caps: securePluginCapabilities()},
			policy: sandbox.Policy{Network: sandbox.NetworkAllowed},
			want:   "network access must be denied",
		},
		{
			name:   "preferred enforcement",
			runner: &recordingPluginRunner{caps: securePluginCapabilities()},
			policy: sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
			want:   "enforcement must be required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.ExecutableToolDefs(PluginExecutionOptions{
				Runner: test.runner, Policy: test.policy, Workspace: workspace,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("secure composition error = %v, want %q", err, test.want)
			}
			if len(test.runner.calls) != 0 {
				t.Fatal("invalid secure composition called the runner")
			}
		})
	}
}

func TestExecutablePluginMapsTimeoutNonzeroAndOutputLimit(t *testing.T) {
	tests := []struct {
		name   string
		result sandbox.Result
		report sandbox.Report
		want   string
	}{
		{
			name:   "nonzero",
			result: sandbox.Result{Started: true, ExitCode: 7, Err: errors.New("exit status 7")},
			report: sandbox.Report{Failure: sandbox.FailureExecutionFailed, Detail: "process failed"},
			want:   string(sandbox.FailureExecutionFailed),
		},
		{
			name:   "timeout",
			result: sandbox.Result{Started: true, ExitCode: -1, TimedOut: true, Err: context.DeadlineExceeded},
			report: sandbox.Report{Failure: sandbox.FailureTimedOut, Detail: "deadline"},
			want:   string(sandbox.FailureTimedOut),
		},
		{
			name: "output cap",
			result: sandbox.Result{
				Started: true, ExitCode: -1, OutputTruncated: true,
				Err: errors.New("output exceeded limit"), Stdout: []byte("partial"),
			},
			report: sandbox.Report{Failure: sandbox.FailureOutputLimit, Detail: "capture limit"},
			want:   string(sandbox.FailureOutputLimit),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_outcome"))
			runner := &recordingPluginRunner{caps: securePluginCapabilities(), result: test.result, report: test.report}
			definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, nil)
			bound := approvedPluginExecutionInput(t, json.RawMessage(`{"value":"x"}`), identity, "session-outcome", "run-outcome")
			result, isError := ExecuteToolWithOptions(definition.Name, bound, workspace, ToolExecutionOptions{
				Context: context.Background(), ExpectedSessionID: "session-outcome", ExpectedRunID: "run-outcome",
			})
			if !isError || !strings.Contains(result, test.want) || !strings.Contains(result, `"status":"failed"`) {
				t.Fatalf("mapped result = %s error=%v", result, isError)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("runner calls = %d", len(runner.calls))
			}
		})
	}
}

func TestExecutablePluginIdentitySwapAndForgeryFailBeforeRunner(t *testing.T) {
	t.Run("executable swap", func(t *testing.T) {
		manager, workspace, executable := executablePluginFixture(t, pluginFixtureTool("plugin_swap"))
		runner := &recordingPluginRunner{caps: securePluginCapabilities(), result: sandbox.Result{Started: true, ExitCode: 0}}
		definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, nil)
		bound := approvedPluginExecutionInput(t, json.RawMessage(`{"value":"x"}`), identity, "session-swap", "run-swap")
		if err := os.WriteFile(executable, []byte("replacement executable"), 0o755); err != nil {
			t.Fatal(err)
		}
		result, isError := ExecuteToolWithOptions(definition.Name, bound, workspace, ToolExecutionOptions{
			Context: context.Background(), ExpectedSessionID: "session-swap", ExpectedRunID: "run-swap",
		})
		if !isError || !strings.Contains(result, "PLUGIN BLOCKED") || !strings.Contains(result, "changed") {
			t.Fatalf("swap result = %q error=%v", result, isError)
		}
		if len(runner.calls) != 0 {
			t.Fatal("identity swap reached the runner")
		}
	})

	t.Run("forged bound identity", func(t *testing.T) {
		manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_forgery"))
		runner := &recordingPluginRunner{caps: securePluginCapabilities(), result: sandbox.Result{Started: true, ExitCode: 0}}
		definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, nil)
		identity.ExecutorID = "plugin:sha256:forged"
		forged, err := json.Marshal(boundToolExecutionEnvelope{
			Protocol: boundToolExecutionProtocol,
			Identity: identity,
			Input:    json.RawMessage(`{"value":"x"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		result, isError := ExecuteToolWithOptions(definition.Name, forged, workspace, ToolExecutionOptions{
			Context: context.Background(), ExpectedSessionID: "session-forged", ExpectedRunID: "run-forged",
		})
		if !isError || !strings.Contains(result, "IDENTITY BLOCKED") {
			t.Fatalf("forgery result = %q error=%v", result, isError)
		}
		if len(runner.calls) != 0 {
			t.Fatal("identity forgery reached the runner")
		}
	})

	t.Run("invalid input and runtime metadata", func(t *testing.T) {
		manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_invalid"))
		runner := &recordingPluginRunner{caps: securePluginCapabilities(), result: sandbox.Result{Started: true, ExitCode: 0}}
		definition, identity := pluginDefinitionAndIdentity(t, manager, workspace, runner, nil)
		bound := approvedPluginExecutionInput(t, json.RawMessage(`{"value":7}`), identity, "session-invalid", "run-invalid")
		result, isError := ExecuteToolWithOptions(definition.Name, bound, workspace, ToolExecutionOptions{
			Context: context.Background(), ExpectedSessionID: "session-invalid", ExpectedRunID: "run-invalid",
		})
		if !isError || !strings.Contains(result, "schema mismatch") {
			t.Fatalf("schema result = %q error=%v", result, isError)
		}
		forged := definition
		forged.RuntimeBinding = struct{}{}
		allowed := toolCatalogNames([]types.ToolDef{forged})
		if _, err := executorIdentityForAllowedTool(allowed, forged.Name); err == nil || !strings.Contains(err.Error(), "unsupported runtime binding") {
			t.Fatalf("forged runtime metadata error = %v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatal("invalid input or metadata reached the runner")
		}
	})
}

func bytesContainAny(value []byte, needles ...string) bool {
	text := string(value)
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func TestExecutablePluginConfiguredBoundsAreEnforced(t *testing.T) {
	manager, workspace, _ := executablePluginFixture(t, pluginFixtureTool("plugin_bounds"))
	runner := &recordingPluginRunner{caps: securePluginCapabilities()}
	for name, options := range map[string]PluginExecutionOptions{
		"timeout": {Runner: runner, Workspace: workspace, Timeout: pluginMaximumTimeout + time.Nanosecond},
		"output":  {Runner: runner, Workspace: workspace, OutputLimitBytes: pluginMaximumOutputBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := manager.ExecutableToolDefs(options); err == nil {
				t.Fatal("out-of-range plugin execution option was accepted")
			}
		})
	}
	if len(runner.calls) != 0 {
		t.Fatal("invalid bounds reached the runner")
	}
}
