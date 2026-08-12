package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

const processWaitDelay = time.Second

// UnconfinedRunner executes directly on the host. Its explicit name and strict
// EnforcementDisabled gate prevent it from masquerading as an OS sandbox.
type UnconfinedRunner struct{}

func NewUnconfinedRunner() *UnconfinedRunner {
	return &UnconfinedRunner{}
}

func (r *UnconfinedRunner) Name() string {
	return "unconfined"
}

func (r *UnconfinedRunner) Capabilities() Capabilities {
	return Capabilities{
		ProcessTreeKill:      platformProcessTreeKillSupported(),
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func (r *UnconfinedRunner) Run(ctx context.Context, policy Policy, command CommandSpec) (Result, Report) {
	capabilities := r.Capabilities()
	result := Result{ExitCode: ExitNotStarted}
	report := Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		Capabilities:         capabilities,
	}
	if err := ValidatePolicy(policy, capabilities); err != nil {
		report.Failure, report.Detail = policyFailure(err)
		result.Err = &RunnerError{Code: report.Failure, Detail: report.Detail}
		return result, report
	}
	// Defense in depth: this remains explicit even though ValidatePolicy rejects
	// Required and Preferred against a runner with zero isolation capability.
	if policy.Enforcement != EnforcementDisabled {
		report.Failure = FailureCapabilityUnavailable
		report.Detail = "unconfined runner requires disabled enforcement"
		result.Err = &RunnerError{Code: report.Failure, Detail: report.Detail}
		return result, report
	}
	if err := validateCommand(command); err != nil {
		report.Failure = FailureCommandInvalid
		report.Detail = err.Error()
		result.Err = &RunnerError{Code: report.Failure, Detail: report.Detail}
		return result, report
	}
	environment, err := BuildEnvironment(command.Environment)
	if err != nil {
		report.Failure = FailureCommandInvalid
		report.Detail = err.Error()
		result.Err = &RunnerError{Code: report.Failure, Detail: report.Detail}
		return result, report
	}

	if ctx == nil {
		ctx = context.Background()
	}
	runContext := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		runContext, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()
	executionContext, cancelExecution := context.WithCancel(runContext)
	defer cancelExecution()
	terminal := &executionTerminal{}
	capture := newBoundedOutputCapture(command.OutputLimitBytes, func() {
		terminal.claimOutputLimit(runContext)
		cancelExecution()
	})

	process := exec.CommandContext(executionContext, command.Path, command.Args...)
	process.Dir = command.Dir
	process.Env = environment
	if command.Stdin != nil {
		process.Stdin = bytes.NewReader(command.Stdin)
	}
	process.Stdout = capture.stdoutWriter()
	process.Stderr = capture.stderrWriter()
	configureProcessTree(process)
	var terminationRequested atomic.Bool
	process.Cancel = func() error {
		terminationRequested.Store(true)
		return terminateProcessTree(process)
	}
	process.WaitDelay = processWaitDelay

	startedAt := time.Now()
	err = process.Run()
	terminal.claimContext(runContext)
	result.Duration = time.Since(startedAt)
	result.Started = process.Process != nil
	result.Stdout, result.Stderr, result.OutputTruncated = capture.snapshot()
	result.Err = err
	if process.ProcessState != nil {
		result.ExitCode = process.ProcessState.ExitCode()
	}
	contextErr := runContext.Err()
	contextTerminatedProcess := err != nil && (terminationRequested.Load() || !result.Started)
	terminalReason := terminal.load()
	if (terminalReason == executionTerminalDeadline || terminalReason == executionTerminalCanceled) && !contextTerminatedProcess {
		// Preserve the existing root-exit-wins behavior when a process exits
		// cleanly at the same instant its parent context becomes done.
		terminalReason = executionTerminalNone
	}
	if terminalReason == executionTerminalNone && result.OutputTruncated {
		terminalReason = executionTerminalOutputLimit
	}
	result.TimedOut = terminalReason == executionTerminalDeadline && errors.Is(contextErr, context.DeadlineExceeded)
	result.Canceled = terminalReason == executionTerminalCanceled && errors.Is(contextErr, context.Canceled)

	report.Started = result.Started
	report.EffectiveEnforcement = EnforcementDisabled
	// AppliedIsolation intentionally remains the zero value. Process-tree kill,
	// env filtering, and timeout controls are not represented as isolation.
	switch {
	case terminalReason == executionTerminalOutputLimit:
		result.Err = &RunnerError{Code: FailureOutputLimit, Detail: "command output exceeded its capture limit"}
		report.Failure = FailureOutputLimit
		report.Detail = result.Err.Error()
	case result.TimedOut:
		report.Failure = FailureTimedOut
		report.Detail = "command exceeded its execution deadline"
	case result.Canceled:
		report.Failure = FailureCanceled
		report.Detail = "command execution was canceled"
	case !result.Started:
		report.Failure = FailureStartFailed
		report.Detail = errorDetail(err, "command failed to start")
	case err != nil:
		report.Failure = FailureExecutionFailed
		report.Detail = errorDetail(err, "command execution failed")
	default:
		report.Failure = FailureNone
	}
	return result, report
}

func validateCommand(command CommandSpec) error {
	if command.Path == "" {
		return fmt.Errorf("command path is empty")
	}
	if strings.IndexByte(command.Path, 0) >= 0 {
		return fmt.Errorf("command path contains NUL")
	}
	if strings.IndexByte(command.Dir, 0) >= 0 {
		return fmt.Errorf("command directory contains NUL")
	}
	for _, argument := range command.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("command argument contains NUL")
		}
	}
	if command.Timeout < 0 {
		return fmt.Errorf("command timeout is negative")
	}
	if command.OutputLimitBytes < 0 {
		return fmt.Errorf("command output limit is negative")
	}
	if command.OutputLimitBytes > MaximumOutputLimitBytes {
		return fmt.Errorf("command output limit exceeds maximum of %d bytes", MaximumOutputLimitBytes)
	}
	return nil
}

func errorDetail(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return err.Error()
}
