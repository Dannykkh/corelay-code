package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const ExitNotStarted = -1

// Enforcement states whether isolation is mandatory, desired, or explicitly
// disabled. The zero value is invalid so missing policy fails closed.
type Enforcement string

const (
	EnforcementRequired  Enforcement = "required"
	EnforcementPreferred Enforcement = "preferred"
	EnforcementDisabled  Enforcement = "disabled"
)

// Capabilities describes facts about a Runner. Isolation capabilities are
// separate from execution controls: filtering a child environment or timing
// out a process does not make host execution a sandbox.
type Capabilities struct {
	FilesystemIsolation  bool `json:"filesystemIsolation"`
	NetworkIsolation     bool `json:"networkIsolation"`
	ProcessIsolation     bool `json:"processIsolation"`
	EnvironmentIsolation bool `json:"environmentIsolation"`
	ProcessLimits        bool `json:"processLimits"`
	MemoryLimits         bool `json:"memoryLimits"`
	ProcessTreeKill      bool `json:"processTreeKill"`
	EnvironmentFiltering bool `json:"environmentFiltering"`
	Timeouts             bool `json:"timeouts"`
}

// HasIsolation reports only actual isolation capability. Process cleanup,
// environment construction, and timeouts are useful controls but do not count.
func (c Capabilities) HasIsolation() bool {
	return c.FilesystemIsolation || c.NetworkIsolation || c.ProcessIsolation || c.EnvironmentIsolation
}

// IsZero reports whether no capability is claimed.
func (c Capabilities) IsZero() bool {
	return c == (Capabilities{})
}

type WorkspaceAccess string

const (
	WorkspaceAccessUnspecified WorkspaceAccess = ""
	WorkspaceReadOnly          WorkspaceAccess = "read_only"
	WorkspaceReadWrite         WorkspaceAccess = "read_write"
)

type NetworkAccess string

const (
	NetworkAccessUnspecified NetworkAccess = ""
	NetworkAllowed           NetworkAccess = "allow"
	NetworkDenied            NetworkAccess = "deny"
)

// Policy is the immutable isolation requirement for one prepared command.
// Required lists individual capabilities the selected Runner must provide.
// WorkspaceAccess and NetworkDenied imply their matching capability. Limits
// likewise imply process-limit or memory-limit capability.
type Policy struct {
	Enforcement      Enforcement     `json:"enforcement"`
	Required         Capabilities    `json:"required"`
	Workspace        string          `json:"workspace,omitempty"`
	WorkspaceAccess  WorkspaceAccess `json:"workspaceAccess,omitempty"`
	Network          NetworkAccess   `json:"network,omitempty"`
	MaxProcesses     uint32          `json:"maxProcesses,omitempty"`
	MemoryLimitBytes uint64          `json:"memoryLimitBytes,omitempty"`
}

// EnvironmentSpec is deny-by-default. Inherit names ambient variables that may
// cross the process boundary; Set provides explicit values and wins on name
// collisions. A zero value produces an empty child environment.
type EnvironmentSpec struct {
	Inherit []string          `json:"inherit,omitempty"`
	Set     map[string]string `json:"set,omitempty"`
}

// CommandSpec contains process inputs only. Permission, command validation,
// workspace scope, and ownership must already have been checked by the common
// tool safety pipeline before a Runner is called.
type CommandSpec struct {
	Path        string          `json:"path"`
	Args        []string        `json:"args,omitempty"`
	Dir         string          `json:"dir,omitempty"`
	Environment EnvironmentSpec `json:"environment"`
	Stdin       []byte          `json:"-"`
	Timeout     time.Duration   `json:"timeout"`
	// OutputLimitBytes is one combined stdout+stderr capture budget. Zero uses
	// DefaultOutputLimitBytes; negative values and values above
	// MaximumOutputLimitBytes are invalid. There is no unlimited sentinel.
	OutputLimitBytes int64 `json:"outputLimitBytes,omitempty"`
}

type FailureCode string

const (
	FailureNone                  FailureCode = ""
	FailurePolicyInvalid         FailureCode = "sandbox_policy_invalid"
	FailureCapabilityUnavailable FailureCode = "sandbox_capability_unavailable"
	FailureRunnerUnavailable     FailureCode = "sandbox_unavailable"
	FailureCommandInvalid        FailureCode = "sandbox_command_invalid"
	FailureStartFailed           FailureCode = "sandbox_start_failed"
	FailureTimedOut              FailureCode = "sandbox_timeout"
	FailureCanceled              FailureCode = "sandbox_canceled"
	FailureOutputLimit           FailureCode = "sandbox_output_limit"
	FailureExecutionFailed       FailureCode = "sandbox_execution_failed"
)

// Result preserves the process outcome. Started distinguishes setup failures
// from commands that ran and returned a non-zero exit status.
type Result struct {
	Started         bool
	Stdout          []byte
	Stderr          []byte
	ExitCode        int
	TimedOut        bool
	Canceled        bool
	OutputTruncated bool
	Duration        time.Duration
	Err             error
}

// Report is safe execution metadata for evidence and receipts. It intentionally
// contains no command arguments, environment values, or captured output.
type Report struct {
	Runner               string       `json:"runner"`
	RequestedEnforcement Enforcement  `json:"requestedEnforcement"`
	EffectiveEnforcement Enforcement  `json:"effectiveEnforcement,omitempty"`
	Capabilities         Capabilities `json:"capabilities"`
	AppliedIsolation     Capabilities `json:"appliedIsolation"`
	Started              bool         `json:"started"`
	Failure              FailureCode  `json:"failure,omitempty"`
	Detail               string       `json:"detail,omitempty"`
}

// Runner is the common process execution boundary. Implementations must report
// capabilities truthfully, validate Policy before starting a process, and be
// safe for concurrent Run calls. Callers may share one immutable Runner across
// parallel workers; all mutable execution state belongs to the individual call.
type Runner interface {
	Name() string
	Capabilities() Capabilities
	Run(ctx context.Context, policy Policy, command CommandSpec) (Result, Report)
}

// PolicyError is a typed fail-closed validation result.
type PolicyError struct {
	Code        FailureCode
	Enforcement Enforcement
	Missing     []string
	Detail      string
}

func (e *PolicyError) Error() string {
	if len(e.Missing) == 0 {
		return e.Detail
	}
	return fmt.Sprintf("%s: %s", e.Detail, strings.Join(e.Missing, ", "))
}

// ValidatePolicy checks the requested policy against truthful Runner
// capabilities. Preferred is not permission to silently fall back to host
// execution: selecting an unconfined fallback requires a new Disabled policy.
func ValidatePolicy(policy Policy, available Capabilities) error {
	if err := validatePolicyShape(policy); err != nil {
		return err
	}

	switch policy.Enforcement {
	case EnforcementDisabled:
	case EnforcementRequired, EnforcementPreferred:
		if !available.HasIsolation() {
			return &PolicyError{
				Code:        FailureCapabilityUnavailable,
				Enforcement: policy.Enforcement,
				Missing:     []string{"isolation"},
				Detail:      "runner cannot satisfy sandbox enforcement",
			}
		}
	}

	missing := missingCapabilities(policy.RequiredCapabilities(), available)
	if len(missing) > 0 {
		return &PolicyError{
			Code:        FailureCapabilityUnavailable,
			Enforcement: policy.Enforcement,
			Missing:     missing,
			Detail:      "runner is missing required sandbox capabilities",
		}
	}
	return nil
}

// RequiredCapabilities combines explicit requirements with requirements
// implied by concrete policy rules.
func (p Policy) RequiredCapabilities() Capabilities {
	required := p.Required
	if p.WorkspaceAccess != WorkspaceAccessUnspecified {
		required.FilesystemIsolation = true
	}
	if p.Network == NetworkDenied {
		required.NetworkIsolation = true
	}
	if p.MaxProcesses > 0 {
		required.ProcessLimits = true
	}
	if p.MemoryLimitBytes > 0 {
		required.MemoryLimits = true
	}
	return required
}

func validatePolicyShape(policy Policy) error {
	switch policy.WorkspaceAccess {
	case WorkspaceAccessUnspecified:
	case WorkspaceReadOnly, WorkspaceReadWrite:
		if strings.TrimSpace(policy.Workspace) == "" {
			return &PolicyError{
				Code:        FailurePolicyInvalid,
				Enforcement: policy.Enforcement,
				Detail:      "workspace access requires a workspace path",
			}
		}
	default:
		return &PolicyError{
			Code:        FailurePolicyInvalid,
			Enforcement: policy.Enforcement,
			Detail:      fmt.Sprintf("invalid workspace access %q", policy.WorkspaceAccess),
		}
	}
	switch policy.Network {
	case NetworkAccessUnspecified, NetworkAllowed, NetworkDenied:
	default:
		return &PolicyError{
			Code:        FailurePolicyInvalid,
			Enforcement: policy.Enforcement,
			Detail:      fmt.Sprintf("invalid network access %q", policy.Network),
		}
	}
	if policy.Network == NetworkAllowed && policy.Required.NetworkIsolation {
		return &PolicyError{
			Code:        FailurePolicyInvalid,
			Enforcement: policy.Enforcement,
			Detail:      "network allow contradicts required network isolation",
		}
	}
	switch policy.Enforcement {
	case EnforcementDisabled:
		if policy.RequiredCapabilities().HasIsolation() {
			return &PolicyError{
				Code:        FailurePolicyInvalid,
				Enforcement: policy.Enforcement,
				Detail:      "disabled enforcement cannot require isolation",
			}
		}
		return nil
	case EnforcementRequired, EnforcementPreferred:
		return nil
	default:
		return &PolicyError{
			Code:        FailurePolicyInvalid,
			Enforcement: policy.Enforcement,
			Detail:      fmt.Sprintf("invalid sandbox enforcement %q", policy.Enforcement),
		}
	}
}

func missingCapabilities(required, available Capabilities) []string {
	missing := make([]string, 0, 9)
	checks := []struct {
		name      string
		required  bool
		available bool
	}{
		{name: "environment_filtering", required: required.EnvironmentFiltering, available: available.EnvironmentFiltering},
		{name: "environment_isolation", required: required.EnvironmentIsolation, available: available.EnvironmentIsolation},
		{name: "filesystem_isolation", required: required.FilesystemIsolation, available: available.FilesystemIsolation},
		{name: "memory_limits", required: required.MemoryLimits, available: available.MemoryLimits},
		{name: "network_isolation", required: required.NetworkIsolation, available: available.NetworkIsolation},
		{name: "process_isolation", required: required.ProcessIsolation, available: available.ProcessIsolation},
		{name: "process_limits", required: required.ProcessLimits, available: available.ProcessLimits},
		{name: "process_tree_kill", required: required.ProcessTreeKill, available: available.ProcessTreeKill},
		{name: "timeouts", required: required.Timeouts, available: available.Timeouts},
	}
	for _, check := range checks {
		if check.required && !check.available {
			missing = append(missing, check.name)
		}
	}
	sort.Strings(missing)
	return missing
}

func policyFailure(err error) (FailureCode, string) {
	var policyErr *PolicyError
	if errors.As(err, &policyErr) {
		return policyErr.Code, policyErr.Error()
	}
	return FailurePolicyInvalid, err.Error()
}
