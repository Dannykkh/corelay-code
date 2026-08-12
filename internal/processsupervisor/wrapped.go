package processsupervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type preparedSpec struct {
	Spec    Spec
	Applied sandbox.Capabilities
	Cleanup func()
}

type specBuilder func(sandbox.Policy, Spec) (preparedSpec, error)

type wrappedRunner struct {
	name         string
	capabilities sandbox.Capabilities
	host         Runner
	build        specBuilder
}

func (r *wrappedRunner) Name() string { return r.name }

func (r *wrappedRunner) Capabilities() sandbox.Capabilities { return r.capabilities }

func (r *wrappedRunner) Start(ctx context.Context, policy sandbox.Policy, input Spec) (*Process, Report) {
	report := Report{Runner: r.name, Policy: policy, Capabilities: r.capabilities}
	fail := func(code sandbox.FailureCode, detail string) (*Process, Report) {
		report.Failure = code
		report.Detail = detail
		return nil, report
	}
	if policy.Enforcement == sandbox.EnforcementDisabled {
		return fail(sandbox.FailurePolicyInvalid, "secure streaming adapter does not accept disabled enforcement")
	}
	if err := sandbox.ValidatePolicy(policy, r.capabilities); err != nil {
		return fail(sandboxFailure(err), err.Error())
	}
	spec := cloneSpec(input)
	if err := validateSpec(spec); err != nil {
		return fail(sandbox.FailureCommandInvalid, err.Error())
	}
	if r.host == nil || r.build == nil {
		return fail(sandbox.FailureRunnerUnavailable, "secure streaming adapter is incomplete")
	}
	prepared, err := r.build(policy, spec)
	if err != nil {
		var typed *adapterError
		if errors.As(err, &typed) {
			return fail(typed.code, typed.detail)
		}
		return fail(sandbox.FailureRunnerUnavailable, err.Error())
	}
	hostPolicy := sandbox.Policy{
		Enforcement: sandbox.EnforcementDisabled,
		Required: sandbox.Capabilities{
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
	process, hostReport := r.host.Start(ctx, hostPolicy, prepared.Spec)
	if process == nil || !hostReport.Started {
		if prepared.Cleanup != nil {
			prepared.Cleanup()
		}
		code := hostReport.Failure
		if code == sandbox.FailureNone {
			code = sandbox.FailureStartFailed
		}
		return fail(code, hostReport.Detail)
	}
	if prepared.Cleanup != nil {
		go func() {
			<-process.Done()
			prepared.Cleanup()
		}()
	}
	report.Started = true
	report.Applied = prepared.Applied
	report.Applied.ProcessTreeKill = true
	report.Applied.EnvironmentFiltering = true
	report.Applied.Timeouts = true
	return process, report
}

type adapterError struct {
	code   sandbox.FailureCode
	detail string
}

func (e *adapterError) Error() string { return fmt.Sprintf("%s: %s", e.code, e.detail) }

func failAdapter(code sandbox.FailureCode, detail string) error {
	return &adapterError{code: code, detail: detail}
}
