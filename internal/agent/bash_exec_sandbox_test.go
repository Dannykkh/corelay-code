package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type fakeBashRunner struct {
	name         string
	capabilities sandbox.Capabilities
	run          func(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report)
	calls        int
	policy       sandbox.Policy
	command      sandbox.CommandSpec
}

func (r *fakeBashRunner) Name() string { return r.name }

func (r *fakeBashRunner) Capabilities() sandbox.Capabilities { return r.capabilities }

func (r *fakeBashRunner) Run(
	ctx context.Context,
	policy sandbox.Policy,
	command sandbox.CommandSpec,
) (sandbox.Result, sandbox.Report) {
	r.calls++
	r.policy = policy
	r.command = command
	if r.run != nil {
		return r.run(ctx, policy, command)
	}
	report := fakeBashReport(r, policy)
	report.Started = true
	report.EffectiveEnforcement = policy.Enforcement
	return sandbox.Result{Started: true, ExitCode: 0}, report
}

func fakeBashCapabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		ProcessIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func fakeBashPolicy(enforcement sandbox.Enforcement) sandbox.Policy {
	return sandbox.Policy{
		Enforcement: enforcement,
		Required: sandbox.Capabilities{
			ProcessIsolation:     true,
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
}

func fakeBashReport(r *fakeBashRunner, policy sandbox.Policy) sandbox.Report {
	return sandbox.Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		Capabilities:         r.Capabilities(),
	}
}

func bashInput(t *testing.T, value map[string]interface{}) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestExecuteBashDeepWithOptionsDelegatesToRunnerAndReports(t *testing.T) {
	runner := &fakeBashRunner{name: "fake-secure", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := fakeBashReport(runner, policy)
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		report.AppliedIsolation.ProcessIsolation = true
		return sandbox.Result{
			Started:  true,
			Stdout:   []byte("stdout\n"),
			Stderr:   []byte("stderr\n"),
			ExitCode: 0,
			Duration: 25 * time.Millisecond,
		}, report
	}
	var observed []sandbox.Report
	var progress string
	result := ExecuteBashDeepWithOptions(
		bashInput(t, map[string]interface{}{
			"command": "printf test",
			"env":     map[string]string{"CORELAY_MODE": "test"},
		}),
		t.TempDir(),
		BashExecOptions{
			Context: context.Background(),
			Runner:  runner,
			Policy:  fakeBashPolicy(sandbox.EnforcementRequired),
			Progress: func(output string, _ int) {
				progress = output
			},
			ObserveReport: func(report sandbox.Report) {
				observed = append(observed, report)
			},
		},
	)

	if result.IsError || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if runner.calls != 1 || runner.command.Path != "bash" || len(runner.command.Args) != 2 || runner.command.Args[0] != "-c" {
		t.Fatalf("runner command = %+v, calls=%d", runner.command, runner.calls)
	}
	if runner.command.Environment.Set["CORELAY_MODE"] != "test" {
		t.Fatalf("environment = %+v", runner.command.Environment)
	}
	if progress != "stdout\nstderr\n" {
		t.Fatalf("progress = %q", progress)
	}
	if len(observed) != 1 || observed[0].Runner != "fake-secure" || !observed[0].Started {
		t.Fatalf("observed = %+v", observed)
	}
	if !strings.Contains(result.Output, "stdout\nstderr") || !strings.Contains(result.Output, "sandbox: fake-secure") {
		t.Fatalf("output = %q", result.Output)
	}
}

func TestExecuteBashDeepWithOptionsFiltersAmbientSecrets(t *testing.T) {
	t.Setenv("CORELAY_TEST_SECRET", "must-not-cross")
	runner := &fakeBashRunner{name: "fake-secure", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		environment, err := sandbox.BuildEnvironment(command.Environment)
		if err != nil {
			t.Fatalf("BuildEnvironment() error = %v", err)
		}
		for _, item := range environment {
			if strings.HasPrefix(strings.ToUpper(item), "CORELAY_TEST_SECRET=") || strings.Contains(item, "must-not-cross") {
				t.Fatalf("secret crossed process boundary: %q", item)
			}
		}
		report := fakeBashReport(runner, policy)
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		return sandbox.Result{Started: true, ExitCode: 0}, report
	}
	result := ExecuteBashDeepWithOptions(
		bashInput(t, map[string]interface{}{"command": "true"}),
		t.TempDir(),
		BashExecOptions{Runner: runner, Policy: fakeBashPolicy(sandbox.EnforcementRequired)},
	)
	if result.IsError || runner.calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, runner.calls)
	}
}

func TestExecuteBashDeepWithOptionsRejectsReservedAndSensitiveEnvironmentBeforeRunner(t *testing.T) {
	tests := []map[string]string{
		{"PATH": "C:/attacker"},
		{"api_token": "secret"},
		{"BASH_ENV": "bootstrap.sh"},
	}
	for _, environment := range tests {
		runner := &fakeBashRunner{name: "fake-secure", capabilities: fakeBashCapabilities()}
		result := ExecuteBashDeepWithOptions(
			bashInput(t, map[string]interface{}{"command": "true", "env": environment}),
			t.TempDir(),
			BashExecOptions{Runner: runner, Policy: fakeBashPolicy(sandbox.EnforcementRequired)},
		)
		if !result.IsError || result.SandboxReport.Failure != sandbox.FailureCommandInvalid || runner.calls != 0 {
			t.Fatalf("environment=%+v result=%+v calls=%d", environment, result, runner.calls)
		}
	}
}

func TestExecuteBashDeepWithOptionsFailsClosedForMissingOrUnavailableSandbox(t *testing.T) {
	missing := ExecuteBashDeepWithOptions(
		bashInput(t, map[string]interface{}{"command": "true"}),
		t.TempDir(),
		BashExecOptions{},
	)
	if !missing.IsError || missing.ExitCode != sandbox.ExitNotStarted || missing.SandboxReport.Failure != sandbox.FailureRunnerUnavailable {
		t.Fatalf("missing result = %+v", missing)
	}

	for _, enforcement := range []sandbox.Enforcement{sandbox.EnforcementRequired, sandbox.EnforcementPreferred} {
		runner := &fakeBashRunner{name: "not-isolating"}
		result := ExecuteBashDeepWithOptions(
			bashInput(t, map[string]interface{}{"command": "true"}),
			t.TempDir(),
			BashExecOptions{Runner: runner, Policy: sandbox.Policy{Enforcement: enforcement}},
		)
		if !result.IsError || result.SandboxReport.Failure != sandbox.FailureCapabilityUnavailable || runner.calls != 0 {
			t.Fatalf("enforcement=%s result=%+v calls=%d", enforcement, result, runner.calls)
		}
	}
}

func TestExecuteBashDeepWithOptionsPreservesNonzeroTimeoutAndCancel(t *testing.T) {
	t.Run("nonzero", func(t *testing.T) {
		runner := &fakeBashRunner{name: "fake-secure", capabilities: fakeBashCapabilities()}
		runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
			report := fakeBashReport(runner, policy)
			report.Started = true
			report.EffectiveEnforcement = policy.Enforcement
			report.Failure = sandbox.FailureExecutionFailed
			return sandbox.Result{Started: true, Stderr: []byte("boom"), ExitCode: 7, Err: errors.New("exit 7")}, report
		}
		result := ExecuteBashDeepWithOptions(
			bashInput(t, map[string]interface{}{"command": "custom-command"}),
			t.TempDir(),
			BashExecOptions{Runner: runner, Policy: fakeBashPolicy(sandbox.EnforcementRequired)},
		)
		if !result.IsError || result.ExitCode != 7 || !strings.Contains(result.Output, "boom") || !strings.Contains(result.Output, "[exit 7]") {
			t.Fatalf("result = %+v", result)
		}
	})

	for _, test := range []struct {
		name     string
		context  func() context.Context
		failure  sandbox.FailureCode
		timedOut bool
		canceled bool
	}{
		{
			name: "timeout",
			context: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			failure:  sandbox.FailureTimedOut,
			timedOut: true,
		},
		{
			name: "cancel",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			failure:  sandbox.FailureCanceled,
			canceled: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeBashRunner{name: "fake-secure", capabilities: fakeBashCapabilities()}
			runner.run = func(ctx context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
				err := ctx.Err()
				report := fakeBashReport(runner, policy)
				report.Started = true
				report.EffectiveEnforcement = policy.Enforcement
				report.Failure = test.failure
				return sandbox.Result{
					Started:  true,
					ExitCode: 1,
					TimedOut: test.timedOut,
					Canceled: test.canceled,
					Err:      err,
				}, report
			}
			result := ExecuteBashDeepWithOptions(
				bashInput(t, map[string]interface{}{"command": "wait"}),
				t.TempDir(),
				BashExecOptions{Context: test.context(), Runner: runner, Policy: fakeBashPolicy(sandbox.EnforcementRequired)},
			)
			if !result.IsError || result.TimedOut != test.timedOut || result.Canceled != test.canceled {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestExecuteBashDeepWithOptionsTruncatesOutput(t *testing.T) {
	runner := &fakeBashRunner{name: "fake-secure", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := fakeBashReport(runner, policy)
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		return sandbox.Result{Started: true, Stdout: []byte(strings.Repeat("x", maxOutputBytes+1000)), ExitCode: 0}, report
	}
	result := ExecuteBashDeepWithOptions(
		bashInput(t, map[string]interface{}{"command": "large-output"}),
		t.TempDir(),
		BashExecOptions{Runner: runner, Policy: fakeBashPolicy(sandbox.EnforcementRequired)},
	)
	if !result.Truncated || !strings.Contains(result.Output, "middle truncated") || len(result.Output) >= maxOutputBytes {
		t.Fatalf("truncated=%v len=%d", result.Truncated, len(result.Output))
	}
}

func TestExecuteBashDeepLegacyWrapperIsExplicitlyUnconfinedAndFiltersSecrets(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable on this host")
	}
	t.Setenv("CORELAY_TEST_SECRET", "must-not-cross")
	result := ExecuteBashDeep(
		bashInput(t, map[string]interface{}{
			"command": `if [ -n "$CORELAY_TEST_SECRET" ]; then exit 91; else printf legacy-ok; fi`,
		}),
		t.TempDir(),
		nil,
	)
	if result.IsError || result.ExitCode != 0 || !strings.Contains(result.Output, "legacy-ok") {
		t.Fatalf("result = %+v", result)
	}
	if result.SandboxReport.Runner != "unconfined" || result.SandboxReport.EffectiveEnforcement != sandbox.EnforcementDisabled {
		t.Fatalf("report = %+v", result.SandboxReport)
	}
	if !strings.Contains(result.Output, "sandbox: unconfined") {
		t.Fatalf("legacy execution did not disclose unconfined mode: %q", result.Output)
	}
}

func TestDefaultSandboxExecutionUsesWindowsJobObjectOnCurrentHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Job Object contract is host-specific")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable on this Windows host")
	}
	workDir := t.TempDir()
	runner, policy := DefaultSandboxExecution(workDir)
	if policy.Enforcement != sandbox.EnforcementPreferred || !policy.Required.ProcessIsolation {
		t.Fatalf("default policy = %+v", policy)
	}
	if policy.Required.FilesystemIsolation || policy.Required.NetworkIsolation || policy.WorkspaceAccess != sandbox.WorkspaceAccessUnspecified {
		t.Fatalf("Windows default overclaims filesystem/network isolation: %+v", policy)
	}
	result := ExecuteBashDeepWithOptions(
		bashInput(t, map[string]interface{}{"command": "printf windows-sandbox-ok"}),
		workDir,
		BashExecOptions{Runner: runner, Policy: policy},
	)
	if result.IsError || !strings.Contains(result.Output, "windows-sandbox-ok") {
		t.Fatalf("result = %+v", result)
	}
	if result.SandboxReport.Runner != "windows-job-object" || !result.SandboxReport.AppliedIsolation.ProcessIsolation {
		t.Fatalf("report = %+v", result.SandboxReport)
	}
}
