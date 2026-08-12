package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type hookRunnerCall struct {
	ctx     context.Context
	policy  sandbox.Policy
	command sandbox.CommandSpec
}

type fakeHookRunner struct {
	mu       sync.Mutex
	name     string
	caps     sandbox.Capabilities
	calls    []hookRunnerCall
	behavior func(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report)
}

func (r *fakeHookRunner) Name() string {
	if r.name == "" {
		return "fake-hook-sandbox"
	}
	return r.name
}

func (r *fakeHookRunner) Capabilities() sandbox.Capabilities { return r.caps }

func (r *fakeHookRunner) Run(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	copyCommand := command
	copyCommand.Args = append([]string(nil), command.Args...)
	if command.Args != nil && copyCommand.Args == nil {
		copyCommand.Args = []string{}
	}
	copyCommand.Environment = cloneEnvironmentSpec(command.Environment)
	r.mu.Lock()
	r.calls = append(r.calls, hookRunnerCall{ctx: ctx, policy: policy, command: copyCommand})
	behavior := r.behavior
	r.mu.Unlock()
	if behavior != nil {
		return behavior(ctx, policy, command)
	}
	return successfulHookRun(r.Name(), r.caps, policy, []byte("hook ok"))
}

func (r *fakeHookRunner) callSnapshot() []hookRunnerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hookRunnerCall(nil), r.calls...)
}

func secureFakeHookRunner() *fakeHookRunner {
	return &fakeHookRunner{caps: sandbox.Capabilities{
		FilesystemIsolation:  true,
		NetworkIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}}
}

func successfulHookRun(name string, capabilities sandbox.Capabilities, policy sandbox.Policy, stdout []byte) (sandbox.Result, sandbox.Report) {
	return sandbox.Result{
			Started:  true,
			Stdout:   append([]byte(nil), stdout...),
			ExitCode: 0,
			Duration: 7 * time.Millisecond,
		}, sandbox.Report{
			Runner:               name,
			RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement,
			Capabilities:         capabilities,
			AppliedIsolation: sandbox.Capabilities{
				FilesystemIsolation: true,
				NetworkIsolation:    true,
			},
			Started: true,
		}
}

func newFakeRegistry(t *testing.T, runner *fakeHookRunner, hookType HookType, command string, timeout int) (*Registry, string) {
	t.Helper()
	workDir := t.TempDir()
	writeClaudeHooks(t, workDir, map[string][]map[string]any{
		string(hookType): {{"command": command, "timeout": timeout}},
	})
	registry := NewRegistryWithOptions(RegistryOptions{Runner: runner, ShellPath: "hook-shell"})
	if err := registry.Load(workDir, "claude"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		t.Fatal(err)
	}
	return registry, canonical
}

func writeClaudeHooks(t *testing.T, workDir string, definitions map[string][]map[string]any) {
	t.Helper()
	directory := filepath.Join(workDir, ".claude")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"hooks": definitions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteContextUsesSandboxPolicyAndFilteredEnvironment(t *testing.T) {
	runner := secureFakeHookRunner()
	registry, workDir := newFakeRegistry(t, runner, HookPreToolUse, "project-hook --check", 9)
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("trace"), "hook-context")
	rawResult := "raw result must never cross"
	results := registry.ExecuteContext(ctx, HookPreToolUse, map[string]string{
		"TOOL_NAME":   "Write",
		"TOOL_RESULT": rawResult,
		"TOOL_ERROR":  "false",
		"WORK_DIR":    "caller value is ignored",
	})
	if len(results) != 1 || results[0].Blocked || results[0].Failure != HookFailureNone {
		t.Fatalf("results = %#v", results)
	}
	calls := runner.callSnapshot()
	if len(calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if got := call.ctx.Value(contextKey("trace")); got != "hook-context" {
		t.Fatalf("runner context value = %v", got)
	}
	if call.command.Path != "hook-shell" || !reflect.DeepEqual(call.command.Args, []string{"-c", "project-hook --check"}) {
		t.Fatalf("command = %#v", call.command)
	}
	if call.command.Dir != workDir || call.command.Timeout != 9*time.Second {
		t.Fatalf("command dir/timeout = %q / %s", call.command.Dir, call.command.Timeout)
	}
	if call.command.OutputLimitBytes != maxHookOutputScanBytes {
		t.Fatalf("command output limit = %d", call.command.OutputLimitBytes)
	}
	if call.policy.Enforcement != sandbox.EnforcementRequired ||
		call.policy.Workspace != workDir || call.policy.WorkspaceAccess != sandbox.WorkspaceReadWrite ||
		call.policy.Network != sandbox.NetworkDenied {
		t.Fatalf("policy = %#v", call.policy)
	}
	required := call.policy.Required
	if !required.FilesystemIsolation || !required.NetworkIsolation || !required.ProcessTreeKill ||
		!required.EnvironmentFiltering || !required.Timeouts {
		t.Fatalf("required capabilities = %#v", required)
	}
	if _, exists := call.command.Environment.Set["TOOL_RESULT"]; exists {
		t.Fatal("raw TOOL_RESULT crossed sandbox boundary")
	}
	digest := sha256.Sum256([]byte(rawResult))
	if got := call.command.Environment.Set["TOOL_RESULT_SHA256"]; got != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("TOOL_RESULT_SHA256 = %q", got)
	}
	if got := call.command.Environment.Set["TOOL_RESULT_BYTES"]; got != "27" {
		t.Fatalf("TOOL_RESULT_BYTES = %q", got)
	}
	if got := call.command.Environment.Set["WORK_DIR"]; got != workDir {
		t.Fatalf("WORK_DIR = %q", got)
	}
	if !reflect.DeepEqual(call.command.Environment.Inherit, expectedAmbientNames()) {
		t.Fatalf("inherited env names = %#v", call.command.Environment.Inherit)
	}
	if results[0].Hook.CommandDigest != hookIdentityDigest(Hook{Type: HookPreToolUse, Command: "project-hook --check", Source: "claude"}) || results[0].Hook.Source != "claude" {
		t.Fatalf("hook metadata = %#v", results[0].Hook)
	}
}

func expectedAmbientNames() []string {
	names := []string{"PATH"}
	if runtime.GOOS == "windows" {
		names = append(names, "SYSTEMROOT", "TEMP", "TMP", "WINDIR")
	}
	return names
}

func TestUnavailableCapabilitiesNeverStartAndPreHookBlocks(t *testing.T) {
	runner := &fakeHookRunner{caps: sandbox.Capabilities{
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}}
	registry, _ := newFakeRegistry(t, runner, HookPreToolUse, "must-not-start", 1)
	results := registry.ExecuteContext(context.Background(), HookPreToolUse, map[string]string{"TOOL_NAME": "Write"})
	if len(runner.callSnapshot()) != 0 {
		t.Fatal("runner was called without filesystem/network isolation")
	}
	if len(results) != 1 || !results[0].Blocked || results[0].Failure != HookFailureCapabilityUnavailable {
		t.Fatalf("results = %#v", results)
	}
}

func TestAlreadyCanceledContextNeverStartsHook(t *testing.T) {
	for _, hookType := range []HookType{HookPreToolUse, HookPostToolUse} {
		runner := secureFakeHookRunner()
		registry, _ := newFakeRegistry(t, runner, hookType, "must-not-start", 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		results := registry.ExecuteContext(ctx, hookType, nil)
		if len(runner.callSnapshot()) != 0 {
			t.Fatalf("%s: canceled context reached runner", hookType)
		}
		if len(results) != 1 || results[0].Failure != HookFailureCanceled {
			t.Fatalf("%s: results = %#v", hookType, results)
		}
		if got, want := results[0].Blocked, hookType == HookPreToolUse; got != want {
			t.Fatalf("%s: blocked = %v, want %v", hookType, got, want)
		}
	}
}

func TestZeroRegistrySelectsAutoRunnerAndRequiredPolicy(t *testing.T) {
	var registry Registry
	registry.ensureRuntime()
	if _, ok := registry.runner.(*sandbox.AutoRunner); !ok {
		t.Fatalf("zero registry runner = %T, want *sandbox.AutoRunner", registry.runner)
	}
	if registry.policy.Enforcement != sandbox.EnforcementRequired ||
		registry.policy.WorkspaceAccess != sandbox.WorkspaceReadWrite ||
		registry.policy.Network != sandbox.NetworkDenied {
		t.Fatalf("zero registry policy = %#v", registry.policy)
	}
	if !registry.policy.Required.FilesystemIsolation || !registry.policy.Required.NetworkIsolation ||
		!registry.policy.Required.ProcessTreeKill || !registry.policy.Required.EnvironmentFiltering ||
		!registry.policy.Required.Timeouts {
		t.Fatalf("zero registry required capabilities = %#v", registry.policy.Required)
	}
}

func TestDisabledPolicyRequiresExplicitUnconfinedRunner(t *testing.T) {
	runner := secureFakeHookRunner()
	registry, _ := newFakeRegistry(t, runner, HookPreToolUse, "must-not-start", 1)
	registry.policy = sandbox.Policy{Enforcement: sandbox.EnforcementDisabled}
	results := registry.ExecuteContext(context.Background(), HookPreToolUse, nil)
	if len(runner.callSnapshot()) != 0 {
		t.Fatal("disabled policy reached a non-Unconfined runner")
	}
	if len(results) != 1 || !results[0].Blocked || results[0].Failure != HookFailurePolicyInvalid {
		t.Fatalf("results = %#v", results)
	}

	legacy := NewRegistryWithOptions(RegistryOptions{
		Runner: sandbox.NewUnconfinedRunner(),
		Policy: sandbox.Policy{Enforcement: sandbox.EnforcementDisabled},
	})
	legacy.ensureRuntime()
	if legacy.runtimeErr != HookFailureNone {
		t.Fatalf("explicit legacy boundary rejected: %s", legacy.runtimeErr)
	}
}

func TestPreFailuresBlockWhilePostFailuresRemainObservational(t *testing.T) {
	tests := []struct {
		name    string
		result  sandbox.Result
		report  sandbox.Report
		failure HookFailureCode
	}{
		{
			name:    "timeout",
			result:  sandbox.Result{Started: true, ExitCode: -1, TimedOut: true},
			report:  sandbox.Report{Started: true, Failure: sandbox.FailureTimedOut},
			failure: HookFailureTimedOut,
		},
		{
			name:    "canceled",
			result:  sandbox.Result{Started: true, ExitCode: -1, Canceled: true},
			report:  sandbox.Report{Started: true, Failure: sandbox.FailureCanceled},
			failure: HookFailureCanceled,
		},
		{
			name:    "output limit",
			result:  sandbox.Result{Started: true, ExitCode: -1, OutputTruncated: true},
			report:  sandbox.Report{Started: true, Failure: sandbox.FailureOutputLimit},
			failure: HookFailureOutputLimit,
		},
		{
			name:    "nonzero",
			result:  sandbox.Result{Started: true, ExitCode: 7, Err: errors.New("raw process error")},
			report:  sandbox.Report{Started: true, Failure: sandbox.FailureExecutionFailed},
			failure: HookFailureNonZeroExit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := secureFakeHookRunner()
			runner.behavior = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
				report := test.report
				report.Runner = runner.Name()
				report.RequestedEnforcement = policy.Enforcement
				report.EffectiveEnforcement = policy.Enforcement
				report.Capabilities = runner.caps
				report.AppliedIsolation.FilesystemIsolation = true
				report.AppliedIsolation.NetworkIsolation = true
				return test.result, report
			}

			pre, _ := newFakeRegistry(t, runner, HookPreToolUse, "fails", 1)
			preResult := pre.ExecuteContext(context.Background(), HookPreToolUse, nil)
			if len(preResult) != 1 || !preResult[0].Blocked || preResult[0].Failure != test.failure {
				t.Fatalf("pre result = %#v", preResult)
			}

			post, _ := newFakeRegistry(t, runner, HookPostToolUse, "fails", 1)
			postResult := post.ExecuteContext(context.Background(), HookPostToolUse, nil)
			if len(postResult) != 1 || postResult[0].Blocked || postResult[0].Failure != test.failure {
				t.Fatalf("post result = %#v", postResult)
			}
		})
	}
}

func TestSuccessfulProcessWithoutCompleteSandboxEvidenceFailsClosed(t *testing.T) {
	runner := secureFakeHookRunner()
	runner.behavior = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		return sandbox.Result{Started: true, ExitCode: 0}, sandbox.Report{
			RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement,
			Started:              true,
			AppliedIsolation: sandbox.Capabilities{
				FilesystemIsolation: true,
				NetworkIsolation:    true,
			},
			// Runner identity and capabilities are deliberately missing.
		}
	}
	registry, _ := newFakeRegistry(t, runner, HookPreToolUse, "missing-evidence", 1)
	results := registry.ExecuteContext(context.Background(), HookPreToolUse, nil)
	if len(results) != 1 || !results[0].Blocked || results[0].Failure != HookFailureReportInvalid {
		t.Fatalf("results = %#v", results)
	}
}

func TestEnvironmentRejectsUnknownCredentialsAndBoundsWithoutStarting(t *testing.T) {
	if isCredentialLikeName("MAX_TOKENS") || isCredentialLikeName("CONTEXT_TOKENS") {
		t.Fatal("ordinary token-count names were classified as credentials")
	}
	if !isCredentialLikeName("API_KEY") || !isCredentialLikeName("ACCESS_TOKEN") {
		t.Fatal("credential names were not detected")
	}
	largeEnvironment := map[string]string{}
	for _, name := range []string{"AFTER_MESSAGES", "BLOCK_CODE", "ESTIMATED_INPUT", "MESSAGE_COUNT", "MODEL", "RESULT", "SNAPSHOT_DIGEST", "STRATEGY", "TOOL_ERROR"} {
		largeEnvironment[name] = strings.Repeat("x", maxHookEnvironmentValue)
	}
	tests := []map[string]string{
		{"API_KEY": "sensitive"},
		{"UNKNOWN": "value"},
		{"TOOL_NAME": strings.Repeat("x", maxHookEnvironmentValue+1)},
		{"TOOL_RESULT_SHA256": "spoofed"},
		largeEnvironment,
	}
	for index, env := range tests {
		runner := secureFakeHookRunner()
		registry, _ := newFakeRegistry(t, runner, HookPreToolUse, "never", 1)
		results := registry.ExecuteContext(context.Background(), HookPreToolUse, env)
		if len(runner.callSnapshot()) != 0 || len(results) != 1 || !results[0].Blocked || results[0].Failure != HookFailureEnvironmentInvalid {
			t.Fatalf("case %d: calls/results = %d / %#v", index, len(runner.callSnapshot()), results)
		}
	}
}

func TestCompactionMetadataEnvironmentIsAllowedAndBounded(t *testing.T) {
	workDir := t.TempDir()
	environment, failure := buildHookEnvironment(map[string]string{
		"MODEL":           "small-model",
		"MESSAGE_COUNT":   "20",
		"ESTIMATED_INPUT": "12000",
		"RESULT":          "compacted",
		"STRATEGY":        "deterministic-fallback",
		"SNAPSHOT_DIGEST": "sha256:abc",
		"AFTER_MESSAGES":  "4",
	}, workDir)
	if failure != HookFailureNone {
		t.Fatalf("compaction environment failure = %s", failure)
	}
	for _, name := range []string{"MODEL", "MESSAGE_COUNT", "ESTIMATED_INPUT", "RESULT", "STRATEGY", "SNAPSHOT_DIGEST", "AFTER_MESSAGES"} {
		if environment.Set[name] == "" {
			t.Fatalf("missing compaction environment %q: %#v", name, environment.Set)
		}
	}
}

func TestResultIsBoundedRedactedAndContainsNoRawErrorOrCommand(t *testing.T) {
	runner := secureFakeHookRunner()
	rawCommand := "curl https://private.example.test/path?api_key=command-secret"
	rawError := "runner failed: api_key=error-secret https://internal.example.test"
	runner.behavior = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		output := strings.Repeat("x", maxHookOutputBytes+100) + " api_key=output-secret Bearer abcdefghijkl https://endpoint.example.test"
		return sandbox.Result{
				Started:  true,
				Stdout:   []byte(output),
				Stderr:   []byte("password=stderr-secret"),
				ExitCode: 9,
				Err:      errors.New(rawError),
			}, sandbox.Report{
				Runner:               "fake https://runner.example.test?token=runner-secret",
				RequestedEnforcement: policy.Enforcement,
				EffectiveEnforcement: policy.Enforcement,
				Capabilities:         runner.caps,
				AppliedIsolation: sandbox.Capabilities{
					FilesystemIsolation: true,
					NetworkIsolation:    true,
				},
				Started: true,
				Failure: sandbox.FailureExecutionFailed,
				Detail:  rawError,
			}
	}
	registry, _ := newFakeRegistry(t, runner, HookPreToolUse, rawCommand, 1)
	results := registry.ExecuteContext(context.Background(), HookPreToolUse, nil)
	if len(results) != 1 || !results[0].OutputTruncated || len(results[0].Output) > maxHookOutputBytes {
		t.Fatalf("result bounds = %#v", results)
	}
	encoded, err := json.Marshal(results[0])
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{rawCommand, "command-secret", "error-secret", "output-secret", "stderr-secret", "endpoint.example", "runner.example", rawError} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("result leaked %q: %s", forbidden, serialized)
		}
	}
	if results[0].Hook.CommandDigest != hookIdentityDigest(Hook{Type: HookPreToolUse, Command: rawCommand, Source: "claude"}) || results[0].StdoutDigest == "" || results[0].StderrDigest == "" {
		t.Fatalf("result digests = %#v", results[0])
	}
}

func TestLoadStrictValidationAndQuarantine(t *testing.T) {
	deep := `{"hooks":{},"extra":`
	for index := 0; index < maxHookJSONDepth+2; index++ {
		deep += "["
	}
	deep += "0"
	for index := 0; index < maxHookJSONDepth+2; index++ {
		deep += "]"
	}
	deep += "}"
	many := make([]map[string]any, maxHookCount+1)
	for index := range many {
		many[index] = map[string]any{"command": "echo ok"}
	}
	manyData, _ := json.Marshal(map[string]any{"hooks": map[string]any{"pre_tool_use": many}})
	tests := []struct {
		name   string
		data   string
		source string
	}{
		{name: "unknown hook type", data: `{"hooks":{"before_everything":[{"command":"echo ok"}]}}`, source: "claude"},
		{name: "unknown definition field", data: `{"hooks":{"pre_tool_use":[{"command":"echo ok","extra":true}]}}`, source: "claude"},
		{name: "duplicate key", data: `{"hooks":{"pre_tool_use":[{"command":"first","command":"second"}]}}`, source: "claude"},
		{name: "excessive depth", data: deep, source: "claude"},
		{name: "timeout too large", data: `{"hooks":{"pre_tool_use":[{"command":"echo ok","timeout":301}]}}`, source: "claude"},
		{name: "command too large", data: `{"hooks":{"pre_tool_use":[{"command":"` + strings.Repeat("x", maxHookCommandBytes+1) + `"}]}}`, source: "claude"},
		{name: "too many", data: string(manyData), source: "claude"},
		{name: "unknown source", data: `{"hooks":{}}`, source: "other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workDir, ".claude", "settings.json"), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := secureFakeHookRunner()
			registry := NewRegistryWithOptions(RegistryOptions{Runner: runner, ShellPath: "hook-shell"})
			if err := registry.Load(workDir, test.source); err == nil {
				t.Fatal("Load() unexpectedly succeeded")
			}
			if got := registry.GetHooks(); len(got) != 0 {
				t.Fatalf("quarantined hooks = %#v", got)
			}
			result := registry.ExecuteContext(context.Background(), HookPreToolUse, nil)
			if len(result) != 1 || !result[0].Blocked || result[0].Failure != HookFailureConfigInvalid || len(runner.callSnapshot()) != 0 {
				t.Fatalf("quarantine result/calls = %#v / %d", result, len(runner.callSnapshot()))
			}
		})
	}
}

func TestLoadSupportsSourcesOrderingAndDefensiveCopy(t *testing.T) {
	workDir := t.TempDir()
	writeClaudeHooks(t, workDir, map[string][]map[string]any{
		"post_tool_use": {{"command": "post"}},
		"pre_tool_use":  {{"command": "pre", "timeout": 5}},
	})
	if err := os.WriteFile(filepath.Join(workDir, "codex.json"), []byte(`{"hooks":{"session_start":"start"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithOptions(RegistryOptions{Runner: secureFakeHookRunner(), ShellPath: "hook-shell"})
	if err := registry.Load(workDir, "all"); err != nil {
		t.Fatal(err)
	}
	loaded := registry.GetHooks()
	if len(loaded) != 3 || loaded[0].Type != HookPostToolUse || loaded[1].Type != HookPreToolUse || loaded[2].Type != HookSessionStart {
		t.Fatalf("deterministic hooks = %#v", loaded)
	}
	loaded[0].Command = "mutated"
	loaded[1].Args = []string{"mutated"}
	if registry.GetHooks()[0].Command == "mutated" {
		t.Fatal("GetHooks returned registry backing storage")
	}
	if registry.GetHooks()[1].Args != nil {
		t.Fatal("GetHooks did not preserve/defend nil exec-form state")
	}
	if err := registry.Load(workDir, "none"); err != nil || len(registry.GetHooks()) != 0 {
		t.Fatalf("none load = %v / %#v", err, registry.GetHooks())
	}
}

func TestLoadAndExecuteOfficialClaudeNestedCommandShape(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{
		"hooks": {
			"PreToolUse": [{
				"matcher": "Bash|Edit",
				"hooks": [
					{"type":"command","command":"shell-handler","timeout":7},
					{"type":"command","command":"${CLAUDE_PROJECT_DIR}/bin/check","args":[]}
				]
			}]
		}
	}`
	if err := os.WriteFile(filepath.Join(workDir, ".claude", "settings.local.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := secureFakeHookRunner()
	registry := NewRegistryWithOptions(RegistryOptions{Runner: runner, ShellPath: "hook-shell"})
	if err := registry.Load(workDir, "claude"); err != nil {
		t.Fatalf("Load() nested official shape error = %v", err)
	}
	loaded := registry.GetHooks()
	if len(loaded) != 2 || loaded[0].Matcher != "Bash|Edit" || loaded[0].Args != nil || loaded[1].Args == nil || len(loaded[1].Args) != 0 {
		t.Fatalf("loaded nested hooks = %#v", loaded)
	}

	if results := registry.ExecuteContext(context.Background(), HookPreToolUse, map[string]string{"TOOL_NAME": "Read"}); len(results) != 0 {
		t.Fatalf("non-matching tool results = %#v", results)
	}
	if len(runner.callSnapshot()) != 0 {
		t.Fatal("non-matching official matcher started a process")
	}
	results := registry.ExecuteContext(context.Background(), HookPreToolUse, map[string]string{"TOOL_NAME": "Bash"})
	if len(results) != 2 {
		t.Fatalf("matching tool results = %#v", results)
	}
	calls := runner.callSnapshot()
	if len(calls) != 2 {
		t.Fatalf("matching runner calls = %d", len(calls))
	}
	canonical, _ := canonicalWorkspace(workDir)
	if calls[0].command.Path != "hook-shell" || !reflect.DeepEqual(calls[0].command.Args, []string{"-c", "shell-handler"}) || calls[0].command.Timeout != 7*time.Second {
		t.Fatalf("shell-form command = %#v", calls[0].command)
	}
	if calls[1].command.Path != filepath.Join(canonical, "bin", "check") || calls[1].command.Args == nil || len(calls[1].command.Args) != 0 {
		t.Fatalf("exec-form command = %#v", calls[1].command)
	}
	if calls[1].command.Environment.Set["CLAUDE_PROJECT_DIR"] != canonical {
		t.Fatalf("CLAUDE_PROJECT_DIR = %q", calls[1].command.Environment.Set["CLAUDE_PROJECT_DIR"])
	}
	if results[0].Hook.CommandDigest == results[1].Hook.CommandDigest {
		t.Fatal("shell and exec handlers shared an identity digest")
	}
}

func TestOfficialClaudeMatcherAndHandlerSubsetFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{
			name: "unsupported http handler",
			data: `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"http","command":"ignored"}]}]}}`,
		},
		{
			name: "unsupported if field",
			data: `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"check","if":"Bash(git *)"}]}]}}`,
		},
		{
			name: "meaningful session matcher without source evidence",
			data: `{"hooks":{"SessionStart":[{"matcher":"resume","hooks":[{"type":"command","command":"check"}]}]}}`,
		},
		{
			name: "javascript only regex",
			data: `{"hooks":{"PreToolUse":[{"matcher":"Bash(?=Tool)","hooks":[{"type":"command","command":"check"}]}]}}`,
		},
		{
			name: "unsupported prompt handler",
			data: `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"prompt","command":"ignored"}]}]}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workDir, ".claude", "settings.json"), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			registry := NewRegistryWithOptions(RegistryOptions{Runner: secureFakeHookRunner(), ShellPath: "hook-shell"})
			if err := registry.Load(workDir, "claude"); err == nil {
				t.Fatal("Load() accepted unsupported official semantics")
			}
		})
	}
}

func TestClaudeToolMatchersUseExactListsAndUnanchoredRegex(t *testing.T) {
	tests := []struct {
		matcher string
		target  string
		want    bool
	}{
		{matcher: "*", target: "Anything", want: true},
		{matcher: "Edit, Write", target: "Write", want: true},
		{matcher: "Bash|Edit", target: "NotebookEdit", want: false},
		{matcher: `^Notebook`, target: "NotebookEdit", want: true},
		{matcher: `mcp__memory__.*`, target: "mcp__memory__write", want: true},
	}
	for _, test := range tests {
		if err := validateHookMatcher(HookPreToolUse, test.matcher); err != nil {
			t.Fatalf("matcher %q validation error = %v", test.matcher, err)
		}
		got, err := hookMatcherMatches(strings.TrimSpace(test.matcher), test.target)
		if err != nil || got != test.want {
			t.Fatalf("matcher %q target %q = (%v, %v), want %v", test.matcher, test.target, got, err, test.want)
		}
	}
}

func TestConcurrentLoadExecuteAndSnapshots(t *testing.T) {
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	writeClaudeHooks(t, firstDir, map[string][]map[string]any{"post_tool_use": {{"command": "first"}}})
	writeClaudeHooks(t, secondDir, map[string][]map[string]any{"post_tool_use": {{"command": "second"}}})
	runner := secureFakeHookRunner()
	registry := NewRegistryWithOptions(RegistryOptions{Runner: runner, ShellPath: "hook-shell"})
	if err := registry.Load(firstDir, "claude"); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := 0; iteration < 50; iteration++ {
				if worker%2 == 0 {
					if iteration%2 == 0 {
						_ = registry.Load(firstDir, "claude")
					} else {
						_ = registry.Load(secondDir, "claude")
					}
				} else {
					_ = registry.GetHooks()
					results := registry.ExecuteContext(context.Background(), HookPostToolUse, map[string]string{"TOOL_NAME": "Read"})
					if len(results) != 1 || results[0].Blocked {
						t.Errorf("concurrent results = %#v", results)
						return
					}
				}
			}
		}(worker)
	}
	wait.Wait()
}

func TestCompatibilityExecuteStillUsesSecureDefaultPolicy(t *testing.T) {
	runner := secureFakeHookRunner()
	registry, _ := newFakeRegistry(t, runner, HookPostToolUse, "compat", 0)
	results := registry.Execute(HookPostToolUse, nil)
	if len(results) != 1 || results[0].Failure != HookFailureNone {
		t.Fatalf("results = %#v", results)
	}
	calls := runner.callSnapshot()
	if len(calls) != 1 || calls[0].policy.Enforcement != sandbox.EnforcementRequired || calls[0].command.Timeout != 30*time.Second {
		t.Fatalf("compat call = %#v", calls)
	}
}
