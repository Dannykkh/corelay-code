package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestProbeApprovalRequesterAllowsExactFixtureMutationOnce(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "probe.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"file_path":"probe.txt","content":"after"}`)
	requester, err := newProbeApprovalRequester(workspace, "profile-session", []probeApprovedMutation{{
		tool: "Write", path: "probe.txt", input: input,
	}})
	if err != nil {
		t.Fatal(err)
	}
	draft := approval.Draft{
		SessionID: "profile-session", RunID: "profile-run", ToolCallID: "tool-1",
		ToolName: "Write", RedactedInput: `{"file_path":"probe.txt"}`,
		InputDigest: probeToolInputDigest("Write", input), DangerLevel: "moderate",
		Scope: "probe.txt", RememberAllowed: false,
	}
	pending, err := requester.Open(draft)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := requester.Await(context.Background(), draft.SessionID, pending.ID)
	if err != nil || !resolution.Allowed() {
		t.Fatalf("resolution=%+v error=%v", resolution, err)
	}
	if _, err := requester.Open(draft); !errors.Is(err, errProbeApprovalDenied) {
		t.Fatalf("second Open error=%v, want one-shot denial", err)
	}
}

func TestProbeApprovalRequesterRejectsOutsideBashAndWrongDigest(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "probe.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"file_path":"probe.txt","content":"after"}`)
	base := approval.Draft{
		SessionID: "profile-session", RunID: "profile-run", ToolCallID: "tool-1",
		ToolName: "Write", RedactedInput: `{"file_path":"probe.txt"}`,
		InputDigest: probeToolInputDigest("Write", input), DangerLevel: "moderate",
		Scope: "probe.txt",
	}
	tests := []struct {
		name   string
		mutate func(*approval.Draft)
	}{
		{name: "outside path", mutate: func(draft *approval.Draft) {
			draft.RedactedInput = `{"file_path":"../outside.txt"}`
			draft.Scope = "../outside.txt"
		}},
		{name: "bash", mutate: func(draft *approval.Draft) {
			draft.ToolName = "Bash"
			draft.InputDigest = probeToolInputDigest("Bash", json.RawMessage(`{"command":"echo unsafe"}`))
			draft.RedactedInput = `{"input":"omitted"}`
			draft.Scope = "workspace"
		}},
		{name: "wrong digest", mutate: func(draft *approval.Draft) {
			draft.InputDigest = probeToolInputDigest("Write", json.RawMessage(`{"file_path":"probe.txt","content":"different"}`))
		}},
		{name: "wrong session", mutate: func(draft *approval.Draft) {
			draft.SessionID = "different-session"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requester, err := newProbeApprovalRequester(workspace, "profile-session", []probeApprovedMutation{{
				tool: "Write", path: "probe.txt", input: input,
			}})
			if err != nil {
				t.Fatal(err)
			}
			draft := base
			test.mutate(&draft)
			if _, err := requester.Open(draft); !errors.Is(err, errProbeApprovalDenied) {
				t.Fatalf("Open error=%v, want denial", err)
			}
		})
	}
}

func TestProbeRunOptionsDisableAmbientExecutableInputs(t *testing.T) {
	workspace := t.TempDir()
	requester, err := newProbeApprovalRequester(workspace, "profile-fixture-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &probeProcessDenyRunner{delegate: sandbox.NewAutoRunner()}
	policy := sandbox.Policy{Enforcement: sandbox.EnforcementRequired}
	opts := probeRunOptions(capabilityprofile.ProbeExecution{
		Case: capabilityprofile.ProbeCase{ID: "fixture"}, Attempt: 1,
	}, harness.HarnessProfile{}, nil, runner, policy, requester)
	if !opts.DisablePlugins || !opts.DisableWorkspaceMCP || opts.PluginDirs == nil || len(opts.PluginDirs) != 0 {
		t.Fatalf("ambient executable sources are not disabled: %+v", opts)
	}
	if opts.HookRegistry == nil || opts.ApprovalRequester != requester || opts.SandboxRunner != runner {
		t.Fatal("probe-owned hook, approval, or process boundary is missing")
	}
}

func TestProbeProcessRunnerDeniesWithoutCallingDelegate(t *testing.T) {
	delegate := &countingProbeSandboxRunner{}
	runner := &probeProcessDenyRunner{delegate: delegate}
	result, report := runner.Run(context.Background(), sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired,
	}, sandbox.CommandSpec{Path: "bash", Args: []string{"-lc", "curl https://outside.invalid"}})
	if result.Started || report.Started || report.Failure != sandbox.FailureCommandInvalid {
		t.Fatalf("result=%+v report=%+v", result, report)
	}
	if delegate.calls != 0 {
		t.Fatalf("delegate calls=%d, want zero", delegate.calls)
	}
}

func TestScopeProbeEnvironmentHidesGlobalContextAndDisablesSideEffects(t *testing.T) {
	fakeHome := t.TempDir()
	globalClaude := filepath.Join(fakeHome, ".claude", "CLAUDE.md")
	globalSkill := filepath.Join(fakeHome, ".claude", "skills", "ambient", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(globalClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalClaude, []byte("AMBIENT_GLOBAL_INSTRUCTION"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(globalSkill), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalSkill, []byte("AMBIENT_GLOBAL_SKILL"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	workspace := t.TempDir()
	restore, err := scopeProbeEnvironment(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	for name, want := range map[string]string{
		"CORELAY_MEMORY": "off", "CORELAY_AUTOSKILL": "off",
		"CORELAY_AUTOVERIFY": "off", "CORELAY_OFFLINE": "1",
	} {
		if got := os.Getenv(name); got != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
	if got := agent.LoadProjectContext(workspace); got != "" {
		t.Fatalf("ambient project context was visible: %q", got)
	}
	if got := agent.LoadSkills(workspace); len(got) != 0 {
		t.Fatalf("ambient skills were visible: %+v", got)
	}
}

func TestScopeProbeEnvironmentRemovesEveryBuiltInEgressTool(t *testing.T) {
	restore, err := scopeProbeEnvironment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if !agent.OfflineMode() {
		t.Fatal("probe environment did not enable the offline boundary")
	}
	for _, definition := range agent.StaticToolDefs(t.TempDir()) {
		if agent.IsEgressTool(definition.Name) {
			t.Fatalf("egress tool %q remains in the probe catalog", definition.Name)
		}
	}
}

func TestPrepareAgentProbeFixtureRejectsPreloadedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".claude", "settings.json"), []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := prepareAgentProbeFixture(capabilityprofile.ProbeExecution{WorkspaceRoot: workspace})
	if !errors.Is(err, capabilityprofile.ErrInvalidRuntime) {
		t.Fatalf("prepareAgentProbeFixture error=%v, want ErrInvalidRuntime", err)
	}
}

type countingProbeSandboxRunner struct {
	calls int
}

func (r *countingProbeSandboxRunner) Name() string { return "counting" }

func (r *countingProbeSandboxRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{FilesystemIsolation: true, NetworkIsolation: true, ProcessIsolation: true}
}

func (r *countingProbeSandboxRunner) Run(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	r.calls++
	return sandbox.Result{Started: true}, sandbox.Report{Started: true}
}
