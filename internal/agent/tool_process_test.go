package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type fakeToolProcessRunner struct {
	mu           sync.Mutex
	name         string
	capabilities sandbox.Capabilities
	commands     []sandbox.CommandSpec
	run          func(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report)
}

func (r *fakeToolProcessRunner) Name() string { return r.name }

func (r *fakeToolProcessRunner) Capabilities() sandbox.Capabilities { return r.capabilities }

func (r *fakeToolProcessRunner) Run(
	ctx context.Context,
	policy sandbox.Policy,
	command sandbox.CommandSpec,
) (sandbox.Result, sandbox.Report) {
	command.Args = append([]string(nil), command.Args...)
	command.Environment.Inherit = append([]string(nil), command.Environment.Inherit...)
	r.mu.Lock()
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	if r.run != nil {
		return r.run(ctx, policy, command)
	}
	return sandbox.Result{Started: true, ExitCode: 0}, sandbox.Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		EffectiveEnforcement: policy.Enforcement,
		Capabilities:         r.Capabilities(),
		Started:              true,
	}
}

func (r *fakeToolProcessRunner) commandSnapshot() []sandbox.CommandSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sandbox.CommandSpec(nil), r.commands...)
}

func secureToolProcessOptions(runner sandbox.Runner) ToolExecutionOptions {
	return ToolExecutionOptions{
		Context:       context.Background(),
		SandboxRunner: runner,
		SandboxPolicy: sandbox.Policy{
			Enforcement: sandbox.EnforcementRequired,
			Required:    fakeBashCapabilities(),
		},
	}
}

func TestGrepOneShotProcessUsesRunnerArgvFilteredEnvironmentAndReport(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeToolProcessRunner{
		name:         "fake-process",
		capabilities: fakeBashCapabilities(),
	}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		return sandbox.Result{
				Started:  true,
				Stdout:   []byte(workDir + string(os.PathSeparator) + "notes.txt:1:needle\n"),
				ExitCode: 0,
			}, sandbox.Report{
				Runner:               runner.Name(),
				RequestedEnforcement: policy.Enforcement,
				EffectiveEnforcement: policy.Enforcement,
				Capabilities:         runner.Capabilities(),
				Started:              true,
			}
	}
	var reports []sandbox.Report
	opts := secureToolProcessOptions(runner)
	opts.ObserveSandbox = func(report sandbox.Report) { reports = append(reports, report) }

	output, isError := executeGrepV2WithOptions(
		json.RawMessage(`{"pattern":"needle","glob":"*.txt","ignore_case":true}`),
		workDir,
		opts,
	)
	if isError || !strings.Contains(output, "Found 1 matches") || !strings.Contains(output, "notes.txt:1:needle") {
		t.Fatalf("Grep output=%q isError=%v", output, isError)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 1 {
		t.Fatalf("runner calls=%d, want 1", len(commands))
	}
	wantArgs := []string{"--no-heading", "--line-number", "--color=never", "-i", "--glob", "*.txt", "needle", workDir}
	if commands[0].Path != "rg" || !reflect.DeepEqual(commands[0].Args, wantArgs) {
		t.Fatalf("command=%q %#v, want rg %#v", commands[0].Path, commands[0].Args, wantArgs)
	}
	if len(commands[0].Environment.Set) != 0 || !containsString(commands[0].Environment.Inherit, "PATH") {
		t.Fatalf("child environment=%#v", commands[0].Environment)
	}
	for _, name := range commands[0].Environment.Inherit {
		if isSensitiveEnvironmentName(strings.ToUpper(name)) {
			t.Fatalf("sensitive ambient variable inherited: %q", name)
		}
	}
	if len(reports) != 1 || reports[0].Runner != runner.Name() || !reports[0].Started {
		t.Fatalf("reports=%#v", reports)
	}
}

func TestExecuteToolWithOptionsRoutesOneShotProcessAndReport(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		return sandbox.Result{Started: true, Stdout: []byte("file.go:1:needle"), ExitCode: 0}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
	}
	var observed sandbox.Report
	opts := secureToolProcessOptions(runner)
	opts.ObserveSandbox = func(report sandbox.Report) { observed = report }
	output, isError := ExecuteToolWithOptions(
		"Grep",
		json.RawMessage(`{"pattern":"needle"}`),
		workDir,
		opts,
	)
	if isError || !strings.Contains(output, "file.go:1:needle") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	if len(runner.commandSnapshot()) != 1 || observed.Runner != runner.Name() || !observed.Started {
		t.Fatalf("commands=%#v report=%#v", runner.commandSnapshot(), observed)
	}
}

func TestToolProcessConfigurationFailsClosedBeforeRunner(t *testing.T) {
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	tests := []struct {
		name string
		opts ToolExecutionOptions
		code sandbox.FailureCode
	}{
		{name: "missing runner", opts: ToolExecutionOptions{SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired)}, code: sandbox.FailureRunnerUnavailable},
		{name: "missing policy", opts: ToolExecutionOptions{SandboxRunner: runner}, code: sandbox.FailurePolicyInvalid},
		{name: "disabled policy", opts: ToolExecutionOptions{SandboxRunner: runner, SandboxPolicy: sandbox.Policy{Enforcement: sandbox.EnforcementDisabled}}, code: sandbox.FailurePolicyInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(runner.commandSnapshot())
			result := runToolProcess(test.opts, "test", t.TempDir(), "tool-that-must-not-run", nil, time.Second)
			if result.Started || result.Report.Failure != test.code || result.ExitCode != sandbox.ExitNotStarted {
				t.Fatalf("result=%#v", result)
			}
			if after := len(runner.commandSnapshot()); after != before {
				t.Fatalf("runner calls changed from %d to %d", before, after)
			}
		})
	}
}

func TestToolProcessPropagatesCancellationToRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	runner.run = func(ctx context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		close(started)
		<-ctx.Done()
		return sandbox.Result{
				Started:  true,
				ExitCode: -1,
				Canceled: true,
				Err:      ctx.Err(),
			}, sandbox.Report{
				Runner:               runner.Name(),
				RequestedEnforcement: policy.Enforcement,
				Capabilities:         runner.Capabilities(),
				Started:              true,
				Failure:              sandbox.FailureCanceled,
			}
	}
	opts := secureToolProcessOptions(runner)
	opts.Context = ctx
	done := make(chan toolProcessResult, 1)
	go func() {
		done <- runToolProcess(opts, "cancel", t.TempDir(), "long-running", nil, time.Minute)
	}()
	<-started
	cancel()
	select {
	case result := <-done:
		if !result.Canceled || result.Report.Failure != sandbox.FailureCanceled {
			t.Fatalf("result=%#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not receive cancellation")
	}
}

func TestToolProcessPropagatesTimeoutToRunner(t *testing.T) {
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	runner.run = func(ctx context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		<-ctx.Done()
		return sandbox.Result{
				Started:  true,
				ExitCode: -1,
				TimedOut: true,
				Err:      ctx.Err(),
			}, sandbox.Report{
				Runner:               runner.Name(),
				RequestedEnforcement: policy.Enforcement,
				Capabilities:         runner.Capabilities(),
				Started:              true,
				Failure:              sandbox.FailureTimedOut,
			}
	}
	result := runToolProcess(secureToolProcessOptions(runner), "timeout", t.TempDir(), "slow", nil, 20*time.Millisecond)
	if !result.TimedOut || result.Report.Failure != sandbox.FailureTimedOut {
		t.Fatalf("result=%#v", result)
	}
}

func TestGrepFallsBackAfterExecutableStartFailure(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
		}
		if command.Path == "rg" {
			report.Failure = sandbox.FailureStartFailed
			return sandbox.Result{ExitCode: sandbox.ExitNotStarted, Err: errors.New("rg missing")}, report
		}
		report.Started = true
		return sandbox.Result{Started: true, Stdout: []byte("fallback.txt:1:needle\n"), ExitCode: 0}, report
	}
	output, isError := executeGrepV2WithOptions(
		json.RawMessage(`{"pattern":"needle"}`),
		workDir,
		secureToolProcessOptions(runner),
	)
	if isError || !strings.Contains(output, "fallback.txt:1:needle") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 2 || commands[0].Path != "rg" || commands[1].Path != "grep" {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestPDFReadFallbackUsesExecutableArgsWithoutShell(t *testing.T) {
	workDir := t.TempDir()
	path := workDir + string(os.PathSeparator) + "quote's.pdf"
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
		}
		switch {
		case command.Path == "pdftotext":
			report.Failure = sandbox.FailureStartFailed
			return sandbox.Result{ExitCode: sandbox.ExitNotStarted, Err: errors.New("missing")}, report
		case len(command.Args) == 1 && command.Args[0] == "--version":
			report.Started = true
			return sandbox.Result{Started: true, Stdout: []byte("Python 3.12.1"), ExitCode: 0}, report
		default:
			report.Started = true
			return sandbox.Result{Started: true, Stdout: []byte("page text"), ExitCode: 0}, report
		}
	}
	input, _ := json.Marshal(map[string]string{"file_path": path})
	output, isError := executePDFRead(input, workDir, secureToolProcessOptions(runner))
	if isError || !strings.Contains(output, "page text") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 3 {
		t.Fatalf("commands=%#v", commands)
	}
	for _, command := range commands {
		if command.Path == "bash" || command.Path == "sh" {
			t.Fatalf("shell execution escaped process route: %#v", command)
		}
	}
	python := commands[2]
	canonicalWorkDir, canonicalErr := canonicalWorkspace(workDir)
	if canonicalErr != nil {
		t.Fatalf("canonical PDF workspace: %v", canonicalErr)
	}
	canonicalPath, canonicalErr := canonicalPathWithinWorkspace(path, workDir, canonicalWorkDir, PermissionConfig{})
	if canonicalErr != nil {
		t.Fatalf("canonical PDF path: %v", canonicalErr)
	}
	if python.Path != "python3" || len(python.Args) != 3 || python.Args[0] != "-c" || python.Args[2] != canonicalPath {
		t.Fatalf("Python command=%q %#v", python.Path, python.Args)
	}
}

func TestProcessToolExitAndTruncationSemantics(t *testing.T) {
	t.Run("diff exit one is content", func(t *testing.T) {
		runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
		runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
			return sandbox.Result{
					Started:  true,
					Stdout:   []byte("--- a\n+++ b\n"),
					ExitCode: 1,
					Err:      errors.New("exit status 1"),
				}, sandbox.Report{
					Runner:               runner.Name(),
					RequestedEnforcement: policy.Enforcement,
					Capabilities:         runner.Capabilities(),
					Started:              true,
					Failure:              sandbox.FailureExecutionFailed,
				}
		}
		output, isError := executeDiff(
			json.RawMessage(`{"file_a":"a","file_b":"b"}`),
			t.TempDir(),
			secureToolProcessOptions(runner),
		)
		if isError || !strings.Contains(output, "+++ b") {
			t.Fatalf("output=%q isError=%v", output, isError)
		}
	})

	t.Run("test nonzero truncates and fails", func(t *testing.T) {
		workDir := t.TempDir()
		if err := os.WriteFile(workDir+string(os.PathSeparator)+"go.mod", []byte("module example.test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
		runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
			return sandbox.Result{
					Started:  true,
					Stderr:   []byte(strings.Repeat("failure", 10000)),
					ExitCode: 2,
					Err:      errors.New("exit status 2"),
				}, sandbox.Report{
					Runner:               runner.Name(),
					RequestedEnforcement: policy.Enforcement,
					Capabilities:         runner.Capabilities(),
					Started:              true,
					Failure:              sandbox.FailureExecutionFailed,
				}
		}
		output, isError := executeTestWithOptions(json.RawMessage(`{}`), workDir, secureToolProcessOptions(runner))
		if !isError || !strings.Contains(output, "... (truncated)") || !strings.HasSuffix(output, "[tests failed]") {
			t.Fatalf("output length=%d isError=%v suffix=%q", len(output), isError, output[testMaxInt(0, len(output)-60):])
		}
	})
}

func TestAdvancedTestAndExtendedGitUseSameProcessRoute(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(workDir+string(os.PathSeparator)+"go.mod", []byte("module example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "fake-process", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		output := "ok"
		if command.Path == "git" {
			output = " M file.go"
		}
		return sandbox.Result{Started: true, Stdout: []byte(output), ExitCode: 0}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
	}
	opts := secureToolProcessOptions(runner)

	if output, isError := executeTestWithOptions(json.RawMessage(`{"filter":"Focused"}`), workDir, opts); isError || output != "ok" {
		t.Fatalf("Test output=%q isError=%v", output, isError)
	}
	if output, isError := executeGit(json.RawMessage(`{"command":"status","args":"--short"}`), workDir, opts); isError || !strings.Contains(output, "file.go") {
		t.Fatalf("Git output=%q isError=%v", output, isError)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 2 {
		t.Fatalf("runner calls=%d, want 2", len(commands))
	}
	if commands[0].Path != "go" || !reflect.DeepEqual(commands[0].Args, []string{"test", "-run", "Focused", "./..."}) {
		t.Fatalf("test command=%q %#v", commands[0].Path, commands[0].Args)
	}
	if commands[1].Path != "git" || !reflect.DeepEqual(commands[1].Args, []string{"status", "--short"}) {
		t.Fatalf("git command=%q %#v", commands[1].Path, commands[1].Args)
	}
}

func TestToolProcessUsesWindowsSandboxAdapterOnCurrentHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows adapter contract")
	}
	workDir := t.TempDir()
	runner, policy := DefaultSandboxExecution(workDir)
	var report sandbox.Report
	result := runToolProcess(ToolExecutionOptions{
		Context:        context.Background(),
		SandboxRunner:  runner,
		SandboxPolicy:  policy,
		ObserveSandbox: func(value sandbox.Report) { report = value },
	}, "Windows actual", workDir, "where.exe", []string{"cmd.exe"}, 10*time.Second)
	if !result.Started || result.ExitCode != 0 || result.Err != nil {
		t.Fatalf("result=%#v", result)
	}
	if report.Runner != "windows-job-object" || !report.AppliedIsolation.ProcessIsolation {
		t.Fatalf("report=%#v", report)
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(value, expected) {
			return true
		}
	}
	return false
}

func testMaxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
