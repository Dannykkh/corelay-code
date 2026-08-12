package agent

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	maxBashEnvironmentVariables = 64
	maxBashEnvironmentValueSize = 32 * 1024
)

var bashEnvironmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// BashExecOptions binds one Bash execution to an explicit context, runner,
// and immutable policy. The zero value is intentionally invalid; callers that
// want host execution must select EnforcementDisabled and an UnconfinedRunner.
type BashExecOptions struct {
	Context       context.Context
	Runner        sandbox.Runner
	Policy        sandbox.Policy
	Progress      BashProgressCallback
	ObserveReport func(sandbox.Report)
}

// DefaultSandboxExecution is the application-level default for model-issued
// processes. It selects the platform adapter once and requests only controls
// that adapter truthfully advertises. Preferred remains fail-closed when no
// isolating adapter is available; it never means unconfined fallback.
func DefaultSandboxExecution(workDir string) (sandbox.Runner, sandbox.Policy) {
	runner := sandbox.NewAutoRunner()
	capabilities := runner.Capabilities()
	policy := sandbox.Policy{
		Enforcement: sandbox.EnforcementPreferred,
		Required: sandbox.Capabilities{
			ProcessIsolation:     capabilities.ProcessIsolation,
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
	if capabilities.FilesystemIsolation {
		policy.Workspace = workDir
		policy.WorkspaceAccess = sandbox.WorkspaceReadWrite
	}
	return runner, policy
}

func resolveRunSandboxExecution(opts RunOptions, workDir string) (sandbox.Runner, sandbox.Policy, error) {
	if opts.SandboxRunner == nil && opts.SandboxPolicy.Enforcement == "" {
		runner, policy := DefaultSandboxExecution(workDir)
		return runner, policy, nil
	}
	if opts.SandboxRunner == nil {
		return nil, sandbox.Policy{}, fmt.Errorf("sandbox policy is configured without a runner")
	}
	if opts.SandboxPolicy.Enforcement == "" {
		return nil, sandbox.Policy{}, fmt.Errorf("sandbox runner is configured without an enforcement policy")
	}
	return opts.SandboxRunner, opts.SandboxPolicy, nil
}

// configuredSandboxExecution preserves an explicitly supplied isolating pair.
// Alternate production loops use an unavailable runner when either half is
// missing or enforcement is Disabled, so only the named legacy wrappers can
// opt into unconfined host execution.
func configuredSandboxExecution(
	runner sandbox.Runner,
	policy sandbox.Policy,
	component string,
) (sandbox.Runner, sandbox.Policy) {
	if runner != nil && policy.Enforcement != "" && policy.Enforcement != sandbox.EnforcementDisabled {
		return runner, policy
	}
	detail := strings.TrimSpace(component) + " requires an explicitly configured isolating sandbox"
	return sandbox.NewUnavailableRunner(detail), sandbox.Policy{
		Enforcement: sandbox.EnforcementPreferred,
	}
}

func legacyUnconfinedBashOptions(progress BashProgressCallback) BashExecOptions {
	return BashExecOptions{
		Context: context.Background(),
		Runner:  sandbox.NewUnconfinedRunner(),
		Policy: sandbox.Policy{
			Enforcement: sandbox.EnforcementDisabled,
		},
		Progress: progress,
	}
}

// bashEnvironmentSpec constructs the only environment allowed to cross from
// the agent process into a model-issued shell. Ambient variables are denied by
// default; only PATH and minimum host essentials are inherited.
func bashEnvironmentSpec(modelEnvironment map[string]string) (sandbox.EnvironmentSpec, error) {
	if len(modelEnvironment) > maxBashEnvironmentVariables {
		return sandbox.EnvironmentSpec{}, fmt.Errorf("environment contains %d variables; maximum is %d", len(modelEnvironment), maxBashEnvironmentVariables)
	}

	set := make(map[string]string, len(modelEnvironment))
	for name, value := range modelEnvironment {
		if !bashEnvironmentNamePattern.MatchString(name) {
			return sandbox.EnvironmentSpec{}, fmt.Errorf("invalid environment name %q", name)
		}
		identity := strings.ToUpper(name)
		if _, reserved := reservedBashEnvironmentNames[identity]; reserved {
			return sandbox.EnvironmentSpec{}, fmt.Errorf("environment name %q is reserved", name)
		}
		if isSensitiveEnvironmentName(identity) {
			return sandbox.EnvironmentSpec{}, fmt.Errorf("sensitive environment name %q is not allowed", name)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return sandbox.EnvironmentSpec{}, fmt.Errorf("environment value for %q contains NUL", name)
		}
		if len(value) > maxBashEnvironmentValueSize {
			return sandbox.EnvironmentSpec{}, fmt.Errorf("environment value for %q exceeds %d bytes", name, maxBashEnvironmentValueSize)
		}
		set[name] = value
	}

	return sandbox.EnvironmentSpec{
		Inherit: minimalBashInheritedEnvironment(),
		Set:     set,
	}, nil
}

var reservedBashEnvironmentNames = map[string]struct{}{
	"BASH_ENV":              {},
	"CDPATH":                {},
	"COMSPEC":               {},
	"DYLD_INSERT_LIBRARIES": {},
	"DYLD_LIBRARY_PATH":     {},
	"ENV":                   {},
	"GLOBIGNORE":            {},
	"HOME":                  {},
	"IFS":                   {},
	"LD_LIBRARY_PATH":       {},
	"LD_PRELOAD":            {},
	"PATH":                  {},
	"PATHEXT":               {},
	"PS4":                   {},
	"SHELL":                 {},
	"SYSTEMROOT":            {},
	"TEMP":                  {},
	"TMP":                   {},
	"TMPDIR":                {},
	"USERPROFILE":           {},
	"WINDIR":                {},
}

func minimalBashInheritedEnvironment() []string {
	if runtime.GOOS == "windows" {
		return []string{"PATH", "PATHEXT", "SystemRoot", "WINDIR", "COMSPEC", "TEMP", "TMP"}
	}
	return []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ"}
}

func isSensitiveEnvironmentName(identity string) bool {
	for _, marker := range []string{
		"API_KEY",
		"AUTH",
		"COOKIE",
		"CREDENTIAL",
		"PASSWORD",
		"PASSWD",
		"PRIVATE_KEY",
		"SECRET",
		"TOKEN",
	} {
		if strings.Contains(identity, marker) {
			return true
		}
	}
	return false
}
