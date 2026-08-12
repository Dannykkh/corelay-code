package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const defaultToolProcessTimeout = 120 * time.Second

// toolProcessResult is the process-level boundary used by one-shot tools. It
// deliberately keeps process outcome separate from the tool's semantic exit
// interpretation (for example, grep exit 1 means no matches).
type toolProcessResult struct {
	sandbox.Result
	Report sandbox.Report
}

func (r toolProcessResult) combinedOutput() string {
	return strings.TrimSpace(combineBashOutput(string(r.Stdout), string(r.Stderr)))
}

func (r toolProcessResult) setupOrExecutionError(fallback string) string {
	if output := r.combinedOutput(); output != "" {
		return output
	}
	if strings.TrimSpace(r.Report.Detail) != "" {
		return r.Report.Detail
	}
	if r.Err != nil {
		return r.Err.Error()
	}
	switch {
	case r.TimedOut:
		return "process timed out"
	case r.Canceled:
		return "process was canceled"
	case !r.Started:
		return "process did not start"
	case r.ExitCode != 0:
		return fmt.Sprintf("process exited with code %d", r.ExitCode)
	default:
		return fallback
	}
}

func (r toolProcessResult) policyOrContextFailure() bool {
	switch r.Report.Failure {
	case sandbox.FailurePolicyInvalid,
		sandbox.FailureCapabilityUnavailable,
		sandbox.FailureRunnerUnavailable,
		sandbox.FailureCommandInvalid,
		sandbox.FailureTimedOut,
		sandbox.FailureCanceled:
		return true
	default:
		return r.TimedOut || r.Canceled
	}
}

// runToolProcess is the only process-spawn boundary used by model-facing
// one-shot tools. Missing or Disabled production configuration fails before
// Runner.Run, and the child receives a deny-by-default environment.
func runToolProcess(
	opts ToolExecutionOptions,
	component string,
	workDir string,
	path string,
	args []string,
	timeout time.Duration,
) toolProcessResult {
	policy := opts.SandboxPolicy
	runner := opts.SandboxRunner
	if timeout <= 0 {
		timeout = defaultToolProcessTimeout
	}

	setupFailure := func(code sandbox.FailureCode, detail string) toolProcessResult {
		report := sandbox.Report{
			Runner:               "unconfigured",
			RequestedEnforcement: policy.Enforcement,
			Failure:              code,
			Detail:               strings.TrimSpace(component) + ": " + detail,
		}
		if runner != nil {
			report.Runner = runner.Name()
			report.Capabilities = runner.Capabilities()
		}
		if opts.ObserveSandbox != nil {
			opts.ObserveSandbox(report)
		}
		return toolProcessResult{
			Result: sandbox.Result{
				ExitCode: sandbox.ExitNotStarted,
				Err:      &sandbox.RunnerError{Code: code, Detail: report.Detail},
			},
			Report: report,
		}
	}

	if runner == nil {
		return setupFailure(sandbox.FailureRunnerUnavailable, "sandbox runner is not configured")
	}
	if policy.Enforcement == "" {
		return setupFailure(sandbox.FailurePolicyInvalid, "sandbox policy is not configured")
	}
	if policy.Enforcement == sandbox.EnforcementDisabled {
		return setupFailure(sandbox.FailurePolicyInvalid, "unconfined execution is disabled for production tool routes")
	}
	if err := sandbox.ValidatePolicy(policy, runner.Capabilities()); err != nil {
		failure := sandbox.FailurePolicyInvalid
		var policyError *sandbox.PolicyError
		if errors.As(err, &policyError) {
			failure = policyError.Code
		}
		return setupFailure(failure, err.Error())
	}
	environment, err := bashEnvironmentSpec(nil)
	if err != nil {
		return setupFailure(sandbox.FailureCommandInvalid, err.Error())
	}
	if strings.TrimSpace(path) == "" {
		return setupFailure(sandbox.FailureCommandInvalid, "command path is empty")
	}

	parent := opts.Context
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		failure := sandbox.FailureCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			failure = sandbox.FailureTimedOut
		}
		return setupFailure(failure, err.Error())
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	result, report := runner.Run(ctx, policy, sandbox.CommandSpec{
		Path:        path,
		Args:        append([]string(nil), args...),
		Dir:         workDir,
		Environment: environment,
		Timeout:     timeout,
	})
	report = normalizeToolProcessReport(runner, policy, result, report)
	if opts.ObserveSandbox != nil {
		opts.ObserveSandbox(report)
	}
	return toolProcessResult{Result: result, Report: report}
}

func normalizeToolProcessReport(
	runner sandbox.Runner,
	policy sandbox.Policy,
	result sandbox.Result,
	report sandbox.Report,
) sandbox.Report {
	if strings.TrimSpace(report.Runner) == "" {
		report.Runner = runner.Name()
	}
	if report.RequestedEnforcement == "" {
		report.RequestedEnforcement = policy.Enforcement
	}
	if report.Capabilities.IsZero() {
		report.Capabilities = runner.Capabilities()
	}
	report.Started = result.Started
	if report.Failure == sandbox.FailureNone {
		switch {
		case result.TimedOut:
			report.Failure = sandbox.FailureTimedOut
			report.Detail = "command exceeded its execution deadline"
		case result.Canceled:
			report.Failure = sandbox.FailureCanceled
			report.Detail = "command execution was canceled"
		case !result.Started:
			report.Failure = sandbox.FailureStartFailed
			report.Detail = "command failed to start"
		case result.Err != nil || result.ExitCode != 0:
			report.Failure = sandbox.FailureExecutionFailed
			report.Detail = fmt.Sprintf("command exited with code %d", result.ExitCode)
		}
	}
	return report
}
