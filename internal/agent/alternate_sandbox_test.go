package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type concurrentSandboxRunner struct {
	mu           sync.Mutex
	calls        int
	active       int
	maxActive    int
	policies     []sandbox.Policy
	commands     []sandbox.CommandSpec
	capabilities sandbox.Capabilities
	behavior     func(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report)
}

func newConcurrentSandboxRunner() *concurrentSandboxRunner {
	return &concurrentSandboxRunner{capabilities: fakeBashCapabilities()}
}

func (r *concurrentSandboxRunner) Name() string { return "concurrent-fake" }

func (r *concurrentSandboxRunner) Capabilities() sandbox.Capabilities { return r.capabilities }

func (r *concurrentSandboxRunner) Run(
	ctx context.Context,
	policy sandbox.Policy,
	command sandbox.CommandSpec,
) (sandbox.Result, sandbox.Report) {
	r.mu.Lock()
	r.calls++
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.policies = append(r.policies, policy)
	r.commands = append(r.commands, command)
	behavior := r.behavior
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()
	if behavior != nil {
		return behavior(ctx, policy, command)
	}
	report := sandbox.Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		EffectiveEnforcement: policy.Enforcement,
		Capabilities:         r.capabilities,
		AppliedIsolation:     sandbox.Capabilities{ProcessIsolation: true},
		Started:              true,
	}
	return sandbox.Result{Started: true, Stdout: []byte("ok\n"), ExitCode: 0}, report
}

func (r *concurrentSandboxRunner) snapshot() (int, int, []sandbox.Policy, []sandbox.CommandSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls,
		r.maxActive,
		append([]sandbox.Policy(nil), r.policies...),
		append([]sandbox.CommandSpec(nil), r.commands...)
}

func TestChronosPropagatesSandboxAndEmitsTypedReport(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	workDir := t.TempDir()
	runner := newConcurrentSandboxRunner()
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "Bash", toolID: "chronos-bash", inputChunks: []string{`{"command":"echo ok"}`}},
			{text: "[COMPLETE]"},
		},
	}
	recorder := &sandboxLoopRecorder{}
	cfg := DefaultChronosConfig()
	cfg.MaxCycles = 1
	cfg.TotalTimeout = 5 * time.Second
	cfg.SandboxRunner = runner
	cfg.SandboxPolicy = fakeBashPolicy(sandbox.EnforcementRequired)
	cfg.Recorder = recorder
	eventCh := make(chan Event, 64)

	RunChronos(context.Background(), provider, "test-model", "run a check", workDir, cfg, eventCh)

	var gotReport sandbox.Report
	for event := range eventCh {
		if event.Type != "tool_result" {
			continue
		}
		data, _ := event.Data.(map[string]interface{})
		gotReport, _ = data["sandbox"].(sandbox.Report)
	}
	calls, _, policies, _ := runner.snapshot()
	if calls != 1 || len(policies) != 1 || policies[0].Enforcement != sandbox.EnforcementRequired {
		t.Fatalf("calls=%d policies=%+v", calls, policies)
	}
	if gotReport.Runner != runner.Name() || !gotReport.Started {
		t.Fatalf("event report = %+v", gotReport)
	}
	if len(recorder.reports) != 1 || recorder.reports[0].ToolID != "chronos-bash" {
		t.Fatalf("recorder reports = %+v", recorder.reports)
	}
}

func TestChronosHalfConfiguredSandboxFailsClosedWithoutCallingRunner(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	runner := newConcurrentSandboxRunner()
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "Bash", toolID: "chronos-blocked", inputChunks: []string{`{"command":"echo must-not-run"}`}},
			{text: "[COMPLETE]"},
		},
	}
	cfg := DefaultChronosConfig()
	cfg.MaxCycles = 1
	cfg.TotalTimeout = 5 * time.Second
	cfg.SandboxRunner = runner
	eventCh := make(chan Event, 64)

	RunChronos(context.Background(), provider, "test-model", "do not degrade", t.TempDir(), cfg, eventCh)

	var errorText string
	for event := range eventCh {
		if event.Type == "error" {
			errorText = toEventString(event.Data)
		}
	}
	calls, _, _, _ := runner.snapshot()
	if calls != 0 {
		t.Fatalf("half-configured runner was called %d time(s)", calls)
	}
	if !strings.Contains(errorText, "sandbox runner is configured without an enforcement policy") {
		t.Fatalf("preflight error = %q", errorText)
	}
}

func TestSubAgentPropagatesSandboxReport(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	runner := newConcurrentSandboxRunner()
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "Bash", toolID: "sub-bash", inputChunks: []string{`{"command":"echo sub"}`}},
			{text: "done"},
		},
	}
	recorder := &sandboxLoopRecorder{}
	manager := NewSubAgentManagerWithOptions(provider, "test-model", t.TempDir(), SubAgentManagerOptions{
		SandboxRunner: runner,
		SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired),
		Recorder:      recorder,
	})
	task := &SubAgentTask{ID: "sub-sandbox", Name: "sandbox", Instruction: "run check", Status: "pending"}

	manager.run(task)

	calls, _, _, _ := runner.snapshot()
	if calls != 1 || len(task.Sandbox) != 1 || task.Sandbox[0].Report.Runner != runner.Name() {
		t.Fatalf("calls=%d task sandbox=%+v", calls, task.Sandbox)
	}
	if len(recorder.reports) != 1 || recorder.reports[0].ToolID != "sub-bash" {
		t.Fatalf("recorder reports = %+v", recorder.reports)
	}
}

func TestSubAgentMissingSandboxFailsClosed(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "Bash", toolID: "sub-blocked", inputChunks: []string{`{"command":"echo must-not-run"}`}},
			{text: "done"},
		},
	}
	manager := NewSubAgentManager(provider, "test-model", t.TempDir())
	task := &SubAgentTask{ID: "sub-blocked", Name: "blocked", Instruction: "do not degrade", Status: "pending"}

	manager.run(task)

	if len(task.Sandbox) != 1 || task.Sandbox[0].Report.Runner != "unavailable" || task.Sandbox[0].Report.Started {
		t.Fatalf("task sandbox = %+v", task.Sandbox)
	}
}

func TestSubAgentCancellationReachesRunner(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	runner := newConcurrentSandboxRunner()
	started := make(chan struct{})
	runner.behavior = func(ctx context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		close(started)
		<-ctx.Done()
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
			Failure:              sandbox.FailureCanceled,
		}
		return sandbox.Result{Started: true, ExitCode: 1, Canceled: true, Err: ctx.Err()}, report
	}
	provider := &alternateLoopTestProvider{
		name:   "remote",
		models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{
			{toolName: "Bash", toolID: "sub-cancel", inputChunks: []string{`{"command":"echo wait"}`}},
			{text: "done"},
		},
	}
	parent, cancel := context.WithCancel(context.Background())
	manager := NewSubAgentManagerWithOptions(provider, "test-model", t.TempDir(), SubAgentManagerOptions{
		Context:       parent,
		SandboxRunner: runner,
		SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired),
	})
	task := &SubAgentTask{ID: "sub-cancel", Name: "cancel", Instruction: "wait", Status: "pending"}
	done := make(chan struct{})
	go func() {
		manager.run(task)
		close(done)
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sub-agent did not observe cancellation")
	}
	if len(task.Sandbox) != 1 || task.Sandbox[0].Report.Failure != sandbox.FailureCanceled {
		t.Fatalf("task sandbox = %+v", task.Sandbox)
	}
}

func TestTeamSharesConcurrentRunnerWithImmutablePolicySnapshots(t *testing.T) {
	runner := newConcurrentSandboxRunner()
	runner.behavior = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		time.Sleep(25 * time.Millisecond)
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			AppliedIsolation:     sandbox.Capabilities{ProcessIsolation: true},
			Started:              true,
		}
		return sandbox.Result{Started: true, Stdout: []byte("ok\n"), ExitCode: 0}, report
	}
	policy := fakeBashPolicy(sandbox.EnforcementRequired)
	team := NewTeam(nil, "", t.TempDir(), t.TempDir(), TeamConfig{
		Name:          "sandbox-concurrency",
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
	input := json.RawMessage(`{"command":"echo team"}`)
	const workers = 6
	var wg sync.WaitGroup
	errorsByWorker := make(chan bool, workers)
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result := team.executeVerificationBash(context.Background(), input, "verify-"+string(rune('a'+index)))
			errorsByWorker <- result.IsError
		}(index)
	}
	wg.Wait()
	close(errorsByWorker)
	for isError := range errorsByWorker {
		if isError {
			t.Fatal("concurrent team sandbox call failed")
		}
	}
	calls, maxActive, policies, _ := runner.snapshot()
	if calls != workers || maxActive < 2 {
		t.Fatalf("calls=%d maxActive=%d", calls, maxActive)
	}
	for _, snapshot := range policies {
		if snapshot != policy {
			t.Fatalf("policy snapshot changed: got %+v want %+v", snapshot, policy)
		}
	}
}

func TestTeamMissingSandboxFailsClosed(t *testing.T) {
	team := NewTeam(nil, "", t.TempDir(), t.TempDir(), TeamConfig{Name: "missing-sandbox"})
	result := team.executeVerificationBash(
		context.Background(),
		json.RawMessage(`{"command":"echo must-not-run"}`),
		"verify-missing",
	)
	if !result.IsError || result.SandboxReport.Started || result.SandboxReport.Runner != "unavailable" {
		t.Fatalf("result = %+v", result)
	}
}

func TestAlternateProductionPathRejectsExplicitDisabledEnforcement(t *testing.T) {
	runner := newConcurrentSandboxRunner()
	team := NewTeam(nil, "", t.TempDir(), t.TempDir(), TeamConfig{
		Name:          "disabled-sandbox",
		SandboxRunner: runner,
		SandboxPolicy: sandbox.Policy{Enforcement: sandbox.EnforcementDisabled},
	})
	result := team.executeVerificationBash(
		context.Background(),
		json.RawMessage(`{"command":"echo must-not-run"}`),
		"verify-disabled",
	)
	calls, _, _, _ := runner.snapshot()
	if calls != 0 || !result.IsError || result.SandboxReport.Runner != "unavailable" {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
}

func TestExecuteToolWithMemoryOptionsFiltersSecretsAndPropagatesCancellation(t *testing.T) {
	t.Setenv("CORELAY_ALTERNATE_SECRET", "must-not-cross")
	runner := newConcurrentSandboxRunner()
	runner.behavior = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		environment, err := sandbox.BuildEnvironment(command.Environment)
		if err != nil {
			t.Fatalf("BuildEnvironment() error = %v", err)
		}
		for _, item := range environment {
			if strings.Contains(item, "must-not-cross") || strings.HasPrefix(strings.ToUpper(item), "CORELAY_ALTERNATE_SECRET=") {
				t.Fatalf("ambient secret crossed boundary: %q", item)
			}
		}
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
		result := sandbox.Result{Started: true, ExitCode: 1, Err: ctx.Err()}
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			result.TimedOut = true
			report.Failure = sandbox.FailureTimedOut
		case errors.Is(ctx.Err(), context.Canceled):
			result.Canceled = true
			report.Failure = sandbox.FailureCanceled
		}
		return result, report
	}

	for _, test := range []struct {
		name    string
		context func() context.Context
		failure sandbox.FailureCode
	}{
		{
			name: "cancel",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			failure: sandbox.FailureCanceled,
		},
		{
			name: "timeout",
			context: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			failure: sandbox.FailureTimedOut,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var observed sandbox.Report
			_, isError := ExecuteToolWithMemoryOptions(
				"Bash",
				json.RawMessage(`{"command":"echo check"}`),
				t.TempDir(),
				nil,
				ToolExecutionOptions{
					Context:       test.context(),
					SandboxRunner: runner,
					SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired),
					ObserveSandbox: func(report sandbox.Report) {
						observed = report
					},
				},
			)
			if !isError || observed.Failure != test.failure {
				t.Fatalf("isError=%v report=%+v", isError, observed)
			}
		})
	}
}

func TestExecuteToolWithMemoryLegacyBashFailsClosed(t *testing.T) {
	result, isError := ExecuteToolWithMemory(
		"Bash",
		json.RawMessage(`{"command":"echo must-not-run"}`),
		t.TempDir(),
		nil,
	)
	if !isError || !strings.Contains(result, "runner cannot satisfy sandbox enforcement") {
		t.Fatalf("isError=%v result=%q", isError, result)
	}
}

func TestAlternateProductionPathsDoNotCallLegacyExecutionWrappers(t *testing.T) {
	for _, path := range []string{"chronos.go", "subagent.go", "team.go", "session_memory.go"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, forbidden := range []string{"ExecuteBashDeep(", "ExecuteTool("} {
			if strings.Contains(string(content), forbidden) {
				t.Fatalf("%s still reaches legacy %s", path, forbidden)
			}
		}
	}
	for _, path := range []string{"../../cmd/proxy/team.go", "../../cmd/proxy/worker.go"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(content)
		for _, required := range []string{"DefaultSandboxExecution", "SandboxRunner:", "SandboxPolicy:"} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s is missing secure CLI composition %q", path, required)
			}
		}
	}
}
