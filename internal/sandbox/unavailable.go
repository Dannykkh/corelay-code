package sandbox

import "context"

// UnavailableRunner is the fail-closed runner used when no applicable sandbox
// adapter can be prepared. It never starts a process under any enforcement.
type UnavailableRunner struct {
	reason string
}

func NewUnavailableRunner(reason string) *UnavailableRunner {
	return &UnavailableRunner{reason: reason}
}

func (r *UnavailableRunner) Name() string {
	return "unavailable"
}

func (r *UnavailableRunner) Capabilities() Capabilities {
	return Capabilities{}
}

func (r *UnavailableRunner) Run(_ context.Context, policy Policy, _ CommandSpec) (Result, Report) {
	result := Result{ExitCode: ExitNotStarted}
	report := Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		Capabilities:         r.Capabilities(),
		Failure:              FailureRunnerUnavailable,
		Detail:               r.reason,
	}
	if report.Detail == "" {
		report.Detail = "sandbox runner is unavailable"
	}
	// An unavailable adapter is a setup failure, not a degraded capability
	// match. Preserve sandbox_unavailable for every otherwise valid policy.
	if err := validatePolicyShape(policy); err != nil {
		report.Failure, report.Detail = policyFailure(err)
	}
	result.Err = &RunnerError{Code: report.Failure, Detail: report.Detail}
	return result, report
}

// RunnerError is returned when execution cannot start or cannot complete under
// the selected runner contract.
type RunnerError struct {
	Code   FailureCode
	Detail string
}

func (e *RunnerError) Error() string {
	return e.Detail
}
