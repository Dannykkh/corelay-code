package sandbox

import (
	"context"
	"errors"
)

type preparedAdapterCommand struct {
	Command          CommandSpec
	AppliedIsolation Capabilities
	Cleanup          func() error
}

type adapterCommandBuilder func(policy Policy, command CommandSpec) (preparedAdapterCommand, error)

type wrappedSandboxRunner struct {
	name         string
	capabilities Capabilities
	host         Runner
	build        adapterCommandBuilder
}

func (r *wrappedSandboxRunner) Name() string {
	return r.name
}

func (r *wrappedSandboxRunner) Capabilities() Capabilities {
	return r.capabilities
}

func (r *wrappedSandboxRunner) Run(ctx context.Context, policy Policy, command CommandSpec) (Result, Report) {
	report := Report{
		Runner:               r.name,
		RequestedEnforcement: policy.Enforcement,
		Capabilities:         r.capabilities,
	}
	if policy.Enforcement == EnforcementDisabled {
		return adapterSetupFailure(report, FailurePolicyInvalid, "secure sandbox adapter does not accept disabled enforcement")
	}
	if err := ValidatePolicy(policy, r.capabilities); err != nil {
		code, detail := policyFailure(err)
		return adapterSetupFailure(report, code, detail)
	}
	if err := validateCommand(command); err != nil {
		return adapterSetupFailure(report, FailureCommandInvalid, err.Error())
	}
	prepared, err := r.build(policy, command)
	if err != nil {
		var runnerErr *RunnerError
		if errors.As(err, &runnerErr) {
			return adapterSetupFailure(report, runnerErr.Code, runnerErr.Detail)
		}
		return adapterSetupFailure(report, FailureRunnerUnavailable, err.Error())
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}

	hostPolicy := Policy{
		Enforcement: EnforcementDisabled,
		Required: Capabilities{
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
	result, hostReport := r.host.Run(ctx, hostPolicy, prepared.Command)
	report.Started = result.Started
	report.Failure = hostReport.Failure
	report.Detail = hostReport.Detail
	if result.Started {
		report.EffectiveEnforcement = policy.Enforcement
		report.AppliedIsolation = prepared.AppliedIsolation
	}
	return result, report
}

func adapterSetupFailure(report Report, code FailureCode, detail string) (Result, Report) {
	report.Failure = code
	report.Detail = detail
	result := Result{
		ExitCode: ExitNotStarted,
		Err:      &RunnerError{Code: code, Detail: detail},
	}
	return result, report
}
