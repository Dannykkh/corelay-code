package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func pluginDiscoveryFixture(t *testing.T, name string) (*PluginManager, string, string, string) {
	t.Helper()
	manager, workspace, executable := executablePluginFixture(t, pluginFixtureTool(name))
	pluginRoot := manager.plugins[0].rootDir
	discoveryDir := strings.TrimSuffix(pluginRoot, string(os.PathSeparator)+"fixture")
	return manager, workspace, discoveryDir, executable
}

func hasToolNamed(tools []types.ToolDef, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func TestChronosPluginCatalogApprovalAndIdentityParity(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	t.Setenv("CORELAY_MAX_TOOLS", "")
	_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_chronos")
	runner := &recordingPluginRunner{
		caps: securePluginCapabilities(),
		result: sandbox.Result{
			Started:  true,
			ExitCode: 0,
			Stdout:   []byte("chronos-plugin-ok"),
		},
	}
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "plugin_chronos", toolID: "chronos-plugin-call", inputChunks: []string{`{"value":"chronos"}`}},
			{text: "[COMPLETE]"},
		},
	}
	requester := &pluginDecisionRequester{outcome: approval.OutcomeAllowOnce}
	cfg := DefaultChronosConfig()
	cfg.MaxCycles = 1
	cfg.TotalTimeout = 5 * time.Second
	cfg.SessionID = "chronos-plugin-session"
	cfg.ApprovalRequester = requester
	cfg.SandboxRunner = runner
	cfg.SandboxPolicy = securePluginPolicy(workspace)
	cfg.PluginDirs = []string{discoveryDir}
	events := make(chan Event, 64)

	RunChronos(context.Background(), provider, "test-model", "run plugin_chronos", workspace, cfg, events)

	var sawApproval, sawResult bool
	for event := range events {
		switch event.Type {
		case "approval_required":
			sawApproval = true
		case "tool_result":
			sawResult = strings.Contains(toEventString(event.Data), "chronos-plugin-ok")
		}
	}
	requests := provider.requestsSnapshot()
	var firstTools []types.ToolDef
	if len(requests) > 0 {
		firstTools = requests[0].Tools
	}
	if len(requests) != 2 || !hasToolNamed(firstTools, "plugin_chronos") || hasToolNamed(firstTools, "legacy-only") {
		t.Fatalf("Chronos catalog requests=%d tools=%v", len(requests), firstTools)
	}
	if !sawApproval || !sawResult || len(runner.calls) != 1 || len(requester.snapshot()) != 1 {
		t.Fatalf("approval=%v result=%v runner=%d drafts=%d", sawApproval, sawResult, len(runner.calls), len(requester.snapshot()))
	}
	draft := requester.snapshot()[0]
	if draft.SessionID != cfg.SessionID || draft.ToolName != "plugin_chronos" || draft.RunID == "" || draft.ToolCallID != "chronos-plugin-call" {
		t.Fatalf("approval identity = %#v", draft)
	}
}

func TestChronosPluginApprovalFailsClosedWithoutBrokerOrOnDeny(t *testing.T) {
	for _, test := range []struct {
		name      string
		requester *pluginDecisionRequester
		want      string
	}{
		{name: "no broker", want: "no approval requester"},
		{name: "deny", requester: &pluginDecisionRequester{outcome: approval.OutcomeDeny}, want: "denied or became unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateAlternateLoopEnvironment(t)
			_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_chronos_blocked")
			runner := &recordingPluginRunner{
				caps:   securePluginCapabilities(),
				result: sandbox.Result{Started: true, ExitCode: 0},
			}
			provider := &alternateLoopTestProvider{
				name:   "remote",
				models: []types.ModelInfo{{ID: "test-model"}},
				steps: []alternateLoopStep{
					{toolName: "plugin_chronos_blocked", toolID: "blocked-call", inputChunks: []string{`{"value":"x"}`}},
					{text: "[COMPLETE]"},
				},
			}
			cfg := DefaultChronosConfig()
			cfg.MaxCycles = 1
			cfg.TotalTimeout = 5 * time.Second
			cfg.SessionID = "chronos-blocked-session"
			if test.requester != nil {
				cfg.ApprovalRequester = test.requester
			}
			cfg.SandboxRunner = runner
			cfg.SandboxPolicy = securePluginPolicy(workspace)
			cfg.PluginDirs = []string{discoveryDir}
			events := make(chan Event, 64)

			RunChronos(context.Background(), provider, "test-model", "try plugin", workspace, cfg, events)
			for range events {
			}

			requests := provider.requestsSnapshot()
			if len(requests) != 2 || len(runner.calls) != 0 {
				t.Fatalf("requests=%d runner=%d", len(requests), len(runner.calls))
			}
			history := marshalMessagesForTest(t, requests[1].Messages)
			if !strings.Contains(history, test.want) {
				t.Fatalf("missing fail-closed result %q in %s", test.want, history)
			}
		})
	}
}

func TestChronosPluginTamperAfterApprovalNeverReachesRunner(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	_, workspace, discoveryDir, executable := pluginDiscoveryFixture(t, "plugin_chronos_tamper")
	runner := &recordingPluginRunner{
		caps:   securePluginCapabilities(),
		result: sandbox.Result{Started: true, ExitCode: 0},
	}
	requester := &pluginDecisionRequester{
		outcome: approval.OutcomeAllowOnce,
		mutate: func() {
			if err := os.WriteFile(executable, []byte("tampered after approval"), 0o755); err != nil {
				t.Errorf("tamper executable: %v", err)
			}
		},
	}
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "plugin_chronos_tamper", toolID: "tamper-call", inputChunks: []string{`{"value":"x"}`}},
			{text: "[COMPLETE]"},
		},
	}
	cfg := DefaultChronosConfig()
	cfg.MaxCycles = 1
	cfg.TotalTimeout = 5 * time.Second
	cfg.SessionID = "chronos-tamper-session"
	cfg.ApprovalRequester = requester
	cfg.SandboxRunner = runner
	cfg.SandboxPolicy = securePluginPolicy(workspace)
	cfg.PluginDirs = []string{discoveryDir}
	events := make(chan Event, 64)

	RunChronos(context.Background(), provider, "test-model", "try plugin", workspace, cfg, events)
	for range events {
	}

	requests := provider.requestsSnapshot()
	if len(requests) != 2 || len(runner.calls) != 0 || len(requester.snapshot()) != 1 {
		t.Fatalf("requests=%d runner=%d approvals=%d", len(requests), len(runner.calls), len(requester.snapshot()))
	}
	history := marshalMessagesForTest(t, requests[1].Messages)
	if !strings.Contains(history, "changed after catalog binding") {
		t.Fatalf("tamper result missing from history: %s", history)
	}
}

func TestChronosExplicitPluginSandboxFailureStopsBeforeProvider(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_chronos_unavailable")
	runner := &recordingPluginRunner{caps: sandbox.Capabilities{
		FilesystemIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
	}}
	provider := &alternateLoopTestProvider{name: "remote", models: []types.ModelInfo{{ID: "test-model"}}, steps: []alternateLoopStep{{text: "must not run"}}}
	cfg := DefaultChronosConfig()
	cfg.SandboxRunner = runner
	cfg.SandboxPolicy = sandbox.Policy{
		Enforcement:     sandbox.EnforcementRequired,
		Required:        runner.caps,
		Workspace:       workspace,
		WorkspaceAccess: sandbox.WorkspaceReadWrite,
	}
	cfg.PluginDirs = []string{discoveryDir}
	events := make(chan Event, 32)

	RunChronos(context.Background(), provider, "test-model", "must fail", workspace, cfg, events)

	var errorText string
	for event := range events {
		if event.Type == "error" {
			errorText = toEventString(event.Data)
		}
	}
	if len(provider.requestsSnapshot()) != 0 || len(runner.calls) != 0 || !strings.Contains(errorText, "plugin configuration failed") {
		t.Fatalf("provider=%d runner=%d error=%q", len(provider.requestsSnapshot()), len(runner.calls), errorText)
	}
}

func TestSubAgentPluginCatalogApprovalAndIdentityParity(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_subagent")
	runner := &recordingPluginRunner{
		caps:   securePluginCapabilities(),
		result: sandbox.Result{Started: true, ExitCode: 0, Stdout: []byte("subagent-plugin-ok")},
	}
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "plugin_subagent", toolID: "subagent-plugin-call", inputChunks: []string{`{"value":"subagent"}`}},
			{text: "done"},
		},
	}
	requester := &pluginDecisionRequester{outcome: approval.OutcomeAllowOnce}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", workspace, SubAgentManagerOptions{
		SessionID:         "subagent-plugin-session",
		ApprovalRequester: requester,
		SandboxRunner:     runner,
		SandboxPolicy:     securePluginPolicy(workspace),
		PluginDirs:        []string{discoveryDir},
	})
	task := &SubAgentTask{ID: "sub-plugin", Name: "plugin", Instruction: "run plugin_subagent", Status: "pending"}

	manager.run(task)

	requests := provider.requestsSnapshot()
	var firstTools []types.ToolDef
	if len(requests) > 0 {
		firstTools = requests[0].Tools
	}
	if len(requests) != 2 || !hasToolNamed(firstTools, "plugin_subagent") || hasToolNamed(firstTools, "legacy-only") {
		t.Fatalf("SubAgent catalog requests=%d tools=%v", len(requests), firstTools)
	}
	if task.Status != "completed" || len(runner.calls) != 1 || len(requester.snapshot()) != 1 {
		t.Fatalf("task=%s runner=%d approvals=%d", task.Status, len(runner.calls), len(requester.snapshot()))
	}
	draft := requester.snapshot()[0]
	if draft.SessionID != manager.SessionID() || draft.ToolName != "plugin_subagent" || draft.RunID == "" || draft.ToolCallID != "subagent-plugin-call" {
		t.Fatalf("approval identity = %#v", draft)
	}
}

func TestSubAgentPluginApprovalFailsClosedWithoutBrokerOrOnDeny(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	for _, test := range []struct {
		name      string
		requester *pluginDecisionRequester
		want      string
	}{
		{name: "no broker", want: "no approval requester"},
		{name: "deny", requester: &pluginDecisionRequester{outcome: approval.OutcomeDeny}, want: "denied or became unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_subagent_blocked")
			runner := &recordingPluginRunner{
				caps:   securePluginCapabilities(),
				result: sandbox.Result{Started: true, ExitCode: 0},
			}
			provider := &alternateLoopTestProvider{
				name:   "remote",
				models: []types.ModelInfo{{ID: "test-model"}},
				steps: []alternateLoopStep{
					{toolName: "plugin_subagent_blocked", toolID: "blocked-call", inputChunks: []string{`{"value":"x"}`}},
					{text: "done"},
				},
			}
			manager := NewSubAgentManagerWithOptions(provider, "test-model", workspace, SubAgentManagerOptions{
				SessionID:     "subagent-blocked-session",
				SandboxRunner: runner,
				SandboxPolicy: securePluginPolicy(workspace),
				PluginDirs:    []string{discoveryDir},
			})
			if test.requester != nil {
				manager.approvalRequester = test.requester
			}
			task := &SubAgentTask{ID: "sub-blocked", Name: "blocked", Instruction: "try plugin", Status: "pending"}

			manager.run(task)

			requests := provider.requestsSnapshot()
			if len(requests) != 2 || len(runner.calls) != 0 {
				t.Fatalf("requests=%d runner=%d", len(requests), len(runner.calls))
			}
			history := marshalMessagesForTest(t, requests[1].Messages)
			if !strings.Contains(history, test.want) {
				t.Fatalf("missing fail-closed result %q in %s", test.want, history)
			}
		})
	}
}

func TestSubAgentExplicitPluginSandboxFailureStopsBeforeProvider(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_subagent_unavailable")
	runner := &recordingPluginRunner{caps: sandbox.Capabilities{
		FilesystemIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
	}}
	provider := &alternateLoopTestProvider{name: "remote", models: []types.ModelInfo{{ID: "test-model"}}, steps: []alternateLoopStep{{text: "must not run"}}}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", workspace, SubAgentManagerOptions{
		SandboxRunner: runner,
		SandboxPolicy: sandbox.Policy{
			Enforcement:     sandbox.EnforcementRequired,
			Required:        runner.caps,
			Workspace:       workspace,
			WorkspaceAccess: sandbox.WorkspaceReadWrite,
		},
		PluginDirs: []string{discoveryDir},
	})
	task := &SubAgentTask{ID: "sub-unavailable", Name: "unavailable", Instruction: "must fail", Status: "pending"}

	manager.run(task)

	if len(provider.requestsSnapshot()) != 0 || len(runner.calls) != 0 || task.Status != "failed" || !strings.Contains(task.Result, "plugin configuration failed") {
		t.Fatalf("provider=%d runner=%d task=%#v", len(provider.requestsSnapshot()), len(runner.calls), task)
	}
}

func TestSubAgentDefaultUnavailablePluginIsNotAdvertised(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	_, workspace, _, _ := pluginDiscoveryFixture(t, "plugin_subagent_default_unavailable")
	defaultParent := filepath.Join(workspace, ".claude")
	if err := os.MkdirAll(defaultParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(workspace, "plugins"), filepath.Join(defaultParent, "plugins")); err != nil {
		t.Fatal(err)
	}
	runner := &recordingPluginRunner{caps: sandbox.Capabilities{
		FilesystemIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
	}}
	provider := &alternateLoopTestProvider{name: "remote", models: []types.ModelInfo{{ID: "test-model"}}, steps: []alternateLoopStep{{text: "done"}}}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", workspace, SubAgentManagerOptions{
		SandboxRunner: runner,
		SandboxPolicy: sandbox.Policy{
			Enforcement:     sandbox.EnforcementRequired,
			Required:        runner.caps,
			Workspace:       workspace,
			WorkspaceAccess: sandbox.WorkspaceReadWrite,
		},
	})
	task := &SubAgentTask{ID: "sub-default", Name: "default", Instruction: "finish", Status: "pending"}

	manager.run(task)

	requests := provider.requestsSnapshot()
	if len(requests) != 1 || hasToolNamed(requests[0].Tools, "plugin_subagent_default_unavailable") || len(runner.calls) != 0 || task.Status != "completed" {
		t.Fatalf("requests=%d advertised=%v runner=%d status=%s", len(requests), len(requests) > 0 && hasToolNamed(requests[0].Tools, "plugin_subagent_default_unavailable"), len(runner.calls), task.Status)
	}
}

func TestTeamWorkerPropagatesPluginOptionsAndApprovalSession(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_team_worker")
	runner := &recordingPluginRunner{
		caps:   securePluginCapabilities(),
		result: sandbox.Result{Started: true, ExitCode: 0, Stdout: []byte("team-plugin-ok")},
	}
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "plugin_team_worker", toolID: "team-plugin-call", inputChunks: []string{`{"value":"team"}`}},
			{text: "done"},
		},
	}
	requester := &pluginDecisionRequester{outcome: approval.OutcomeAllowOnce}
	team := NewTeam(provider, "test-model", workspace, t.TempDir(), TeamConfig{
		Name:              "plugin-team",
		SessionID:         "team-plugin-session",
		ApprovalRequester: requester,
		SandboxRunner:     runner,
		SandboxPolicy:     securePluginPolicy(workspace),
		PluginDirs:        []string{discoveryDir},
	})
	task := &TeamTask{ID: "task-plugin", Name: "plugin", Description: "run plugin_team_worker", Status: "pending"}

	team.executeTask(context.Background(), task)

	requests := provider.requestsSnapshot()
	if len(requests) != 2 || !hasToolNamed(requests[0].Tools, "plugin_team_worker") || len(runner.calls) != 1 || len(requester.snapshot()) != 1 {
		t.Fatalf("requests=%d advertised=%v runner=%d approvals=%d", len(requests), len(requests) > 0 && hasToolNamed(requests[0].Tools, "plugin_team_worker"), len(runner.calls), len(requester.snapshot()))
	}
	draft := requester.snapshot()[0]
	if draft.SessionID != "team-plugin-session" || draft.ToolCallID != "team-plugin-call" || draft.RunID == "" {
		t.Fatalf("team approval identity = %#v", draft)
	}

	if task.Status != "completed" {
		t.Fatalf("team task = %#v", task)
	}
}

func TestTeamWorkerExplicitPluginSandboxFailureStopsBeforeProvider(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	_, workspace, discoveryDir, _ := pluginDiscoveryFixture(t, "plugin_team_unavailable")
	runner := &recordingPluginRunner{caps: sandbox.Capabilities{
		FilesystemIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
	}}
	provider := &alternateLoopTestProvider{name: "remote", models: []types.ModelInfo{{ID: "test-model"}}, steps: []alternateLoopStep{{text: "must not run"}}}
	team := NewTeam(provider, "test-model", workspace, t.TempDir(), TeamConfig{
		Name:          "plugin-team-unavailable",
		SandboxRunner: runner,
		SandboxPolicy: sandbox.Policy{
			Enforcement:     sandbox.EnforcementRequired,
			Required:        runner.caps,
			Workspace:       workspace,
			WorkspaceAccess: sandbox.WorkspaceReadWrite,
		},
		PluginDirs: []string{discoveryDir},
	})
	task := &TeamTask{ID: "task-unavailable", Name: "unavailable", Description: "must fail", Status: "pending"}

	team.executeTask(context.Background(), task)

	if len(provider.requestsSnapshot()) != 0 || len(runner.calls) != 0 || task.Status != "failed" || !strings.Contains(task.Result, "plugin configuration failed") {
		t.Fatalf("provider=%d runner=%d task=%#v", len(provider.requestsSnapshot()), len(runner.calls), task)
	}
}
