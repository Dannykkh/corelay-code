package agent

import (
	"github.com/Dannykkh/corelay-code/internal/executionpolicy"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

// Agent aliases preserve the existing package-level call sites while the
// shared contract itself lives in internal/executionpolicy for reuse by other
// execution boundaries.
type ExecutionMode = executionpolicy.Mode
type ExecutionPolicyRequest = executionpolicy.Request
type ExecutionPolicySnapshot = executionpolicy.Snapshot

const (
	ExecutionModeReadOnly  = executionpolicy.ModeReadOnly
	ExecutionModeWorkspace = executionpolicy.ModeWorkspace
	ExecutionModeFull      = executionpolicy.ModeFull
)

func ResolveExecutionPolicy(
	request ExecutionPolicyRequest,
	legacyAutoApprove string,
	capabilities sandbox.Capabilities,
) (ExecutionPolicySnapshot, error) {
	return executionpolicy.Resolve(request, legacyAutoApprove, capabilities)
}

func ValidateExecutionPolicySnapshot(snapshot ExecutionPolicySnapshot) error {
	return executionpolicy.ValidateSnapshot(snapshot)
}

func deriveChildExecutionPolicy(
	parent *ExecutionPolicySnapshot,
	requestedMode ExecutionMode,
	runner sandbox.Runner,
) *ExecutionPolicySnapshot {
	if parent == nil {
		if requestedMode == "" {
			return nil
		}
		var capabilities sandbox.Capabilities
		if runner != nil {
			capabilities = runner.Capabilities()
		}
		child, err := ResolveExecutionPolicy(ExecutionPolicyRequest{Mode: requestedMode}, "", capabilities)
		if err != nil {
			return &ExecutionPolicySnapshot{}
		}
		return &child
	}
	var capabilities sandbox.Capabilities
	if runner != nil {
		capabilities = runner.Capabilities()
	}
	child, err := ResolveChildExecutionPolicy(*parent, requestedMode, capabilities)
	if err != nil {
		return &ExecutionPolicySnapshot{}
	}
	return &child
}

func ResolveChildExecutionPolicy(
	parent ExecutionPolicySnapshot,
	requestedMode ExecutionMode,
	capabilities sandbox.Capabilities,
) (ExecutionPolicySnapshot, error) {
	return executionpolicy.ResolveChild(parent, requestedMode, capabilities)
}
