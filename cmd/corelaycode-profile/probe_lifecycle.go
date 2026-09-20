package main

import (
	"context"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type lifecycleProbe struct {
	executor  *agentProbeExecutor
	execution capabilityprofile.ProbeExecution
	fixture   agentProbeFixture
	options   agent.RunOptions
}

func isLifecycleProbe(category capabilityprofile.ProbeCategory) bool {
	return category == capabilityprofile.CategoryDecisionRetention ||
		category == capabilityprofile.CategoryProjectSwitch ||
		category == capabilityprofile.CategoryInterruptRecovery
}

func (p lifecycleProbe) execute(ctx context.Context) observedAgentProbe {
	started := time.Now()
	var result observedAgentProbe
	switch p.execution.Case.Category {
	case capabilityprofile.CategoryDecisionRetention:
		result = p.decision(ctx)
	case capabilityprofile.CategoryProjectSwitch:
		result = p.project(ctx)
	default:
		result = p.recovery(ctx)
	}
	result.value.Latency = time.Since(started)
	return result
}

// Each lifecycle owner adds its state-transition acceptance to this terminal
// and marker check. The original snapshot-only category checks are not used.
func (p lifecycleProbe) run(ctx context.Context, workspace string, messages []types.Message, fixture agentProbeFixture, options agent.RunOptions) observedAgentProbe {
	events := make(chan agent.Event, 64)
	started := time.Now()
	options.SandboxPolicy.Workspace = workspace
	go agent.RunLoopWithOptions(ctx, p.executor.provider, p.executor.model, messages, workspace, options, events)
	execution := p.execution
	execution.WorkspaceRoot = workspace
	execution.Case.Category = capabilityprofile.CategoryPlanAnchor
	return observeAgentProbe(events, fixture, execution, started)
}

func lifecycleFailure(code string) observedAgentProbe {
	return observedAgentProbe{
		transportFailed: true, failureCode: code,
		value: capabilityprofile.ProbeObservation{
			SchemaVersion: capabilityprofile.CurrentObservationSchemaVersion,
			TraceDigest:   digestJSON(code), SafetyPassed: true,
		},
	}
}
