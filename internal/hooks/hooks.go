// Package hooks loads project hook declarations and executes them through the
// common sandbox boundary. Project hook commands are untrusted input: the
// default registry never falls back to host execution when isolation is
// unavailable.
package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

// HookType defines when a hook fires.
type HookType string

const (
	HookPreToolUse   HookType = "pre_tool_use"
	HookPostToolUse  HookType = "post_tool_use"
	HookSessionStart HookType = "session_start"
	HookSessionEnd   HookType = "session_end"
	HookPreCompact   HookType = "pre_compact"
	HookPostCompact  HookType = "post_compact"
)

// Hook represents one validated project hook declaration. Command is exposed
// only by the configuration inspection API; execution results contain a digest
// instead so logs and durable recorders cannot accidentally retain it.
type Hook struct {
	Type    HookType `json:"type"`
	Matcher string   `json:"matcher,omitempty"`
	Command string   `json:"command"`
	// Args distinguishes Claude's exec form from shell form: nil means shell
	// form, while a non-nil (including empty) slice means direct exec form.
	Args    []string `json:"args"`
	Timeout int      `json:"timeout,omitempty"`
	Source  string   `json:"source"`
}

// HookMetadata is the non-sensitive identity retained in an execution result.
type HookMetadata struct {
	Type          HookType `json:"type"`
	Source        string   `json:"source"`
	CommandDigest string   `json:"commandDigest,omitempty"`
}

// HookFailureCode is a stable, non-sensitive execution outcome.
type HookFailureCode string

const (
	HookFailureNone                  HookFailureCode = ""
	HookFailureConfigInvalid         HookFailureCode = "hook_config_invalid"
	HookFailureEnvironmentInvalid    HookFailureCode = "hook_environment_invalid"
	HookFailurePolicyInvalid         HookFailureCode = "hook_sandbox_policy_invalid"
	HookFailureCapabilityUnavailable HookFailureCode = "hook_sandbox_capability_unavailable"
	HookFailureRunnerUnavailable     HookFailureCode = "hook_sandbox_unavailable"
	HookFailureCommandInvalid        HookFailureCode = "hook_command_invalid"
	HookFailureStartFailed           HookFailureCode = "hook_start_failed"
	HookFailureTimedOut              HookFailureCode = "hook_timeout"
	HookFailureCanceled              HookFailureCode = "hook_canceled"
	HookFailureOutputLimit           HookFailureCode = "hook_output_limit"
	HookFailureNonZeroExit           HookFailureCode = "hook_nonzero_exit"
	HookFailureExecution             HookFailureCode = "hook_execution_failed"
	HookFailureReportInvalid         HookFailureCode = "hook_sandbox_report_invalid"
)

// HookResult contains bounded, redacted output and typed sandbox evidence. It
// never includes the raw command, environment values, or runner error.
type HookResult struct {
	Hook            HookMetadata    `json:"hook"`
	Output          string          `json:"output,omitempty"`
	Error           string          `json:"error,omitempty"`
	Failure         HookFailureCode `json:"failure,omitempty"`
	ExitCode        int             `json:"exitCode"`
	Duration        time.Duration   `json:"duration"`
	Blocked         bool            `json:"blocked"`
	OutputTruncated bool            `json:"outputTruncated,omitempty"`
	StdoutDigest    string          `json:"stdoutDigest,omitempty"`
	StderrDigest    string          `json:"stderrDigest,omitempty"`
	Sandbox         sandbox.Report  `json:"sandbox"`
}

// RegistryOptions configures the process boundary. A zero policy selects the
// secure default. Legacy host execution is accepted only when the caller
// explicitly supplies both an UnconfinedRunner and EnforcementDisabled.
type RegistryOptions struct {
	Runner    sandbox.Runner
	Policy    sandbox.Policy
	ShellPath string
}

type registrySnapshot struct {
	hooks       []Hook
	workDir     string
	loadFailure HookFailureCode
}

// Registry manages one immutable-at-execution hook snapshot. Load swaps the
// complete snapshot atomically, and ExecuteContext works from a defensive copy.
type Registry struct {
	mu       sync.RWMutex
	snapshot registrySnapshot

	runtimeOnce sync.Once
	runner      sandbox.Runner
	policy      sandbox.Policy
	shellPath   string
	runtimeErr  HookFailureCode
}

// NewRegistry returns a fail-closed registry backed by sandbox.AutoRunner.
func NewRegistry() *Registry {
	return &Registry{}
}

// NewRegistryWithOptions creates a registry with an explicit runner seam.
// Invalid combinations are retained as a typed fail-closed runtime error so a
// project hook can never be silently skipped or downgraded.
func NewRegistryWithOptions(options RegistryOptions) *Registry {
	return &Registry{
		runner:    options.Runner,
		policy:    options.Policy,
		shellPath: options.ShellPath,
	}
}

// Load validates all selected project files and atomically replaces the active
// snapshot. A failed load publishes an empty, quarantined snapshot; a later
// successful load clears that failure.
func (r *Registry) Load(workDir string, skillSource string) error {
	snapshot, err := loadRegistrySnapshot(workDir, skillSource)
	if err != nil {
		snapshot.hooks = nil
		snapshot.loadFailure = HookFailureConfigInvalid
	}
	r.mu.Lock()
	r.snapshot = snapshot
	r.mu.Unlock()
	return err
}

// Execute is the compatibility wrapper. It retains the secure default and does
// not detach the command from a caller-provided run context.
func (r *Registry) Execute(hookType HookType, env map[string]string) []HookResult {
	return r.ExecuteContext(context.Background(), hookType, env)
}

// ExecuteAsync is retained for source compatibility. Hook subprocesses are no
// longer detached in an untracked goroutine; callers needing cancellation must
// use ExecuteContext.
func (r *Registry) ExecuteAsync(hookType HookType, env map[string]string) {
	_ = r.ExecuteContext(context.Background(), hookType, env)
}

// ExecuteContext runs matching hooks serially through sandbox.Runner. A
// PreToolUse failure stops the chain and blocks the tool. Other hook failures
// remain observable but never change a completed tool decision retroactively.
func (r *Registry) ExecuteContext(ctx context.Context, hookType HookType, env map[string]string) []HookResult {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := r.snapshotCopy()
	if snapshot.loadFailure != HookFailureNone {
		return []HookResult{syntheticHookResult(hookType, snapshot.loadFailure)}
	}
	if !validHookType(hookType) {
		return []HookResult{syntheticHookResult(hookType, HookFailureConfigInvalid)}
	}

	candidates := make([]Hook, 0, len(snapshot.hooks))
	for _, hook := range snapshot.hooks {
		if hook.Type == hookType {
			candidates = append(candidates, hook)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		failure := HookFailureCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			failure = HookFailureTimedOut
		}
		result := syntheticHookResult(hookType, failure)
		result.Hook = metadataForHook(candidates[0])
		return []HookResult{result}
	}

	environment, envFailure := buildHookEnvironment(env, snapshot.workDir)
	if envFailure != HookFailureNone {
		result := syntheticHookResult(hookType, envFailure)
		result.Hook = metadataForHook(candidates[0])
		return []HookResult{result}
	}
	matching, matchFailure := matchingHooks(candidates, environment.Set)
	if matchFailure != HookFailureNone {
		result := syntheticHookResult(hookType, matchFailure)
		result.Hook = metadataForHook(candidates[0])
		return []HookResult{result}
	}
	if len(matching) == 0 {
		return nil
	}

	r.ensureRuntime()
	if r.runtimeErr != HookFailureNone {
		result := syntheticHookResult(hookType, r.runtimeErr)
		result.Hook = metadataForHook(matching[0])
		return []HookResult{result}
	}
	policy, policyFailure := r.policyForWorkspace(snapshot.workDir)
	if policyFailure != HookFailureNone {
		result := syntheticHookResult(hookType, policyFailure)
		result.Hook = metadataForHook(matching[0])
		return []HookResult{result}
	}
	if err := sandbox.ValidatePolicy(policy, r.runner.Capabilities()); err != nil {
		failure := hookFailureFromPolicyValidation(err)
		result := syntheticHookResult(hookType, failure)
		result.Hook = metadataForHook(matching[0])
		result.Sandbox = safeSandboxReport(sandbox.Report{
			Runner:               r.runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         r.runner.Capabilities(),
			Failure:              sandboxFailureForHookFailure(failure),
		})
		return []HookResult{result}
	}

	results := make([]HookResult, 0, len(matching))
	for _, hook := range matching {
		result := r.runHook(ctx, hook, snapshot.workDir, environment, policy)
		results = append(results, result)
		if hookType == HookPreToolUse && result.Blocked {
			break
		}
	}
	return results
}

// IsBlocked is the compatibility wrapper for IsBlockedContext.
func (r *Registry) IsBlocked(hookType HookType, env map[string]string) (bool, string) {
	return r.IsBlockedContext(context.Background(), hookType, env)
}

// IsBlockedContext reports the first fail-closed PreToolUse result.
func (r *Registry) IsBlockedContext(ctx context.Context, hookType HookType, env map[string]string) (bool, string) {
	for _, result := range r.ExecuteContext(ctx, hookType, env) {
		if result.Blocked {
			reason := result.Output
			if reason == "" {
				reason = result.Error
			}
			return true, reason
		}
	}
	return false, ""
}

// GetHooks returns a defensive copy of the current validated declarations.
func (r *Registry) GetHooks() []Hook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneHooks(r.snapshot.hooks)
}

func (r *Registry) snapshotCopy() registrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := r.snapshot
	snapshot.hooks = cloneHooks(snapshot.hooks)
	return snapshot
}

func (r *Registry) ensureRuntime() {
	r.runtimeOnce.Do(func() {
		if r.runner == nil {
			r.runner = sandbox.NewAutoRunner()
		}
		if r.policy.Enforcement == "" {
			r.policy = secureHookPolicy("")
		}
		if strings.TrimSpace(r.shellPath) == "" {
			r.shellPath = defaultHookShell()
		}
		if !validCommandToken(r.shellPath) {
			r.runtimeErr = HookFailureCommandInvalid
			return
		}
		if r.policy.Enforcement == sandbox.EnforcementDisabled {
			if _, ok := r.runner.(*sandbox.UnconfinedRunner); !ok {
				r.runtimeErr = HookFailurePolicyInvalid
			}
			return
		}
		if r.policy.Enforcement != sandbox.EnforcementRequired &&
			r.policy.Enforcement != sandbox.EnforcementPreferred {
			r.runtimeErr = HookFailurePolicyInvalid
		}
	})
}

func (r *Registry) policyForWorkspace(workDir string) (sandbox.Policy, HookFailureCode) {
	policy := r.policy
	if policy.Enforcement == sandbox.EnforcementDisabled {
		if policy.Workspace != "" ||
			policy.WorkspaceAccess != sandbox.WorkspaceAccessUnspecified ||
			policy.Network != sandbox.NetworkAccessUnspecified ||
			policy.Required.HasIsolation() {
			return sandbox.Policy{}, HookFailurePolicyInvalid
		}
		policy.Workspace = ""
		policy.Network = sandbox.NetworkAccessUnspecified
		policy.Required.ProcessTreeKill = true
		policy.Required.EnvironmentFiltering = true
		policy.Required.Timeouts = true
		return policy, HookFailureNone
	}
	if workDir == "" {
		return sandbox.Policy{}, HookFailurePolicyInvalid
	}
	if policy.Network == sandbox.NetworkAllowed {
		return sandbox.Policy{}, HookFailurePolicyInvalid
	}
	if policy.Workspace != "" {
		configured, err := canonicalWorkspace(policy.Workspace)
		if err != nil || !samePath(configured, workDir) {
			return sandbox.Policy{}, HookFailurePolicyInvalid
		}
	}
	policy.Workspace = workDir
	policy.WorkspaceAccess = sandbox.WorkspaceReadWrite
	policy.Network = sandbox.NetworkDenied
	policy.Required.FilesystemIsolation = true
	policy.Required.NetworkIsolation = true
	policy.Required.ProcessTreeKill = true
	policy.Required.EnvironmentFiltering = true
	policy.Required.Timeouts = true
	return policy, HookFailureNone
}

func secureHookPolicy(workDir string) sandbox.Policy {
	return sandbox.Policy{
		Enforcement:     sandbox.EnforcementRequired,
		Workspace:       workDir,
		WorkspaceAccess: sandbox.WorkspaceReadWrite,
		Network:         sandbox.NetworkDenied,
		Required: sandbox.Capabilities{
			FilesystemIsolation:  true,
			NetworkIsolation:     true,
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
}

func (r *Registry) runHook(
	ctx context.Context,
	hook Hook,
	workDir string,
	environment sandbox.EnvironmentSpec,
	policy sandbox.Policy,
) HookResult {
	timeout := time.Duration(hook.Timeout) * time.Second
	if hook.Timeout == 0 {
		timeout = defaultHookTimeout
	}
	path, arguments := hookInvocation(r.shellPath, hook, workDir)
	command := sandbox.CommandSpec{
		Path:             path,
		Args:             arguments,
		Dir:              workDir,
		Environment:      cloneEnvironmentSpec(environment),
		Timeout:          timeout,
		OutputLimitBytes: maxHookOutputScanBytes,
	}
	result, report := r.runner.Run(ctx, policy, command)
	failure := classifyHookFailure(result, report, policy)
	output, truncated := safeHookOutput(result.Stdout, result.Stderr)
	hookResult := HookResult{
		Hook:            metadataForHook(hook),
		Output:          output,
		Failure:         failure,
		ExitCode:        result.ExitCode,
		Duration:        result.Duration,
		Blocked:         hook.Type == HookPreToolUse && failure != HookFailureNone,
		OutputTruncated: truncated || result.OutputTruncated,
		StdoutDigest:    digestBytes(result.Stdout),
		StderrDigest:    digestBytes(result.Stderr),
		Sandbox:         safeSandboxReport(report),
	}
	if failure != HookFailureNone {
		hookResult.Error = safeHookFailureMessage(failure)
	}
	return hookResult
}

func syntheticHookResult(hookType HookType, failure HookFailureCode) HookResult {
	if !validHookType(hookType) {
		hookType = ""
	}
	return HookResult{
		Hook:     HookMetadata{Type: hookType, Source: "registry"},
		Error:    safeHookFailureMessage(failure),
		Failure:  failure,
		ExitCode: sandbox.ExitNotStarted,
		Blocked:  hookType == HookPreToolUse && failure != HookFailureNone,
		Sandbox: safeSandboxReport(sandbox.Report{
			Failure: sandboxFailureForHookFailure(failure),
		}),
	}
}

func metadataForHook(hook Hook) HookMetadata {
	return HookMetadata{
		Type:          hook.Type,
		Source:        sanitizeMetadata(hook.Source, maxHookSourceBytes),
		CommandDigest: hookIdentityDigest(hook),
	}
}

func classifyHookFailure(result sandbox.Result, report sandbox.Report, policy sandbox.Policy) HookFailureCode {
	switch {
	case result.TimedOut || report.Failure == sandbox.FailureTimedOut:
		return HookFailureTimedOut
	case result.Canceled || report.Failure == sandbox.FailureCanceled:
		return HookFailureCanceled
	case result.OutputTruncated || report.Failure == sandbox.FailureOutputLimit:
		return HookFailureOutputLimit
	case !result.Started:
		return hookFailureFromSandbox(report.Failure)
	case !report.Started || strings.TrimSpace(report.Runner) == "" ||
		report.RequestedEnforcement != policy.Enforcement:
		return HookFailureReportInvalid
	case sandbox.ValidatePolicy(policy, report.Capabilities) != nil:
		return HookFailureReportInvalid
	case policy.Enforcement != sandbox.EnforcementDisabled &&
		(report.EffectiveEnforcement != policy.Enforcement ||
			!report.AppliedIsolation.FilesystemIsolation ||
			!report.AppliedIsolation.NetworkIsolation):
		return HookFailureReportInvalid
	case result.ExitCode != 0:
		return HookFailureNonZeroExit
	case result.Err != nil || report.Failure != sandbox.FailureNone:
		return HookFailureExecution
	default:
		return HookFailureNone
	}
}

func hookFailureFromSandbox(failure sandbox.FailureCode) HookFailureCode {
	switch failure {
	case sandbox.FailurePolicyInvalid:
		return HookFailurePolicyInvalid
	case sandbox.FailureCapabilityUnavailable:
		return HookFailureCapabilityUnavailable
	case sandbox.FailureRunnerUnavailable:
		return HookFailureRunnerUnavailable
	case sandbox.FailureCommandInvalid:
		return HookFailureCommandInvalid
	case sandbox.FailureStartFailed:
		return HookFailureStartFailed
	case sandbox.FailureTimedOut:
		return HookFailureTimedOut
	case sandbox.FailureCanceled:
		return HookFailureCanceled
	case sandbox.FailureOutputLimit:
		return HookFailureOutputLimit
	case sandbox.FailureExecutionFailed:
		return HookFailureExecution
	case sandbox.FailureNone:
		return HookFailureStartFailed
	default:
		return HookFailureExecution
	}
}

func hookFailureFromPolicyValidation(err error) HookFailureCode {
	var policyErr *sandbox.PolicyError
	if !errors.As(err, &policyErr) {
		return HookFailurePolicyInvalid
	}
	return hookFailureFromSandbox(policyErr.Code)
}

func sandboxFailureForHookFailure(failure HookFailureCode) sandbox.FailureCode {
	switch failure {
	case HookFailurePolicyInvalid, HookFailureConfigInvalid, HookFailureEnvironmentInvalid:
		return sandbox.FailurePolicyInvalid
	case HookFailureCapabilityUnavailable:
		return sandbox.FailureCapabilityUnavailable
	case HookFailureRunnerUnavailable:
		return sandbox.FailureRunnerUnavailable
	case HookFailureCommandInvalid:
		return sandbox.FailureCommandInvalid
	case HookFailureStartFailed, HookFailureReportInvalid:
		return sandbox.FailureStartFailed
	case HookFailureTimedOut:
		return sandbox.FailureTimedOut
	case HookFailureCanceled:
		return sandbox.FailureCanceled
	case HookFailureOutputLimit:
		return sandbox.FailureOutputLimit
	case HookFailureNonZeroExit, HookFailureExecution:
		return sandbox.FailureExecutionFailed
	default:
		return sandbox.FailureNone
	}
}

func safeHookFailureMessage(failure HookFailureCode) string {
	switch failure {
	case HookFailureConfigInvalid:
		return "hook configuration rejected"
	case HookFailureEnvironmentInvalid:
		return "hook environment rejected"
	case HookFailurePolicyInvalid:
		return "hook sandbox policy rejected"
	case HookFailureCapabilityUnavailable:
		return "required hook sandbox capability unavailable"
	case HookFailureRunnerUnavailable:
		return "hook sandbox unavailable"
	case HookFailureCommandInvalid:
		return "hook command rejected"
	case HookFailureStartFailed:
		return "hook process did not start"
	case HookFailureTimedOut:
		return "hook execution timed out"
	case HookFailureCanceled:
		return "hook execution canceled"
	case HookFailureOutputLimit:
		return "hook output exceeded its capture limit"
	case HookFailureNonZeroExit:
		return "hook exited unsuccessfully"
	case HookFailureExecution:
		return "hook execution failed"
	case HookFailureReportInvalid:
		return "hook sandbox evidence rejected"
	default:
		return ""
	}
}

func digestString(value string) string {
	return digestBytes([]byte(value))
}

func digestBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneEnvironmentSpec(input sandbox.EnvironmentSpec) sandbox.EnvironmentSpec {
	return sandbox.EnvironmentSpec{
		Inherit: append([]string(nil), input.Inherit...),
		Set:     cloneStringMap(input.Set),
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func cloneHooks(input []Hook) []Hook {
	if input == nil {
		return nil
	}
	result := make([]Hook, len(input))
	for index, hook := range input {
		result[index] = hook
		result[index].Args = append([]string(nil), hook.Args...)
		if hook.Args != nil && result[index].Args == nil {
			result[index].Args = []string{}
		}
	}
	return result
}

func hookInvocation(shellPath string, hook Hook, workDir string) (string, []string) {
	resolve := func(value string) string {
		return strings.ReplaceAll(value, "${CLAUDE_PROJECT_DIR}", workDir)
	}
	if hook.Args == nil {
		return shellPath, []string{"-c", resolve(hook.Command)}
	}
	command := resolve(hook.Command)
	if filepath.IsAbs(command) {
		command = filepath.Clean(command)
	}
	arguments := make([]string, len(hook.Args))
	for index, argument := range hook.Args {
		arguments[index] = resolve(argument)
	}
	return command, arguments
}

func hookIdentityDigest(hook Hook) string {
	var identity strings.Builder
	identity.WriteString("hook-v1\x00")
	identity.WriteString(string(hook.Type))
	identity.WriteByte(0)
	identity.WriteString(hook.Matcher)
	identity.WriteByte(0)
	identity.WriteString(hook.Command)
	if hook.Args == nil {
		identity.WriteString("\x00shell")
	} else {
		identity.WriteString("\x00exec")
		for _, argument := range hook.Args {
			identity.WriteByte(0)
			identity.WriteString(argument)
		}
	}
	return digestString(identity.String())
}

func defaultHookShell() string {
	if runtime.GOOS == "windows" {
		if gitBash := findGitBash(); gitBash != "" {
			return gitBash
		}
	}
	return "bash"
}

func findGitBash() string {
	for _, directory := range filepath.SplitList(ambientValue("PATH")) {
		candidate := filepath.Join(directory, "bash.exe")
		if pathIsRegular(candidate) && strings.Contains(strings.ToLower(filepath.Clean(candidate)), `\git\`) {
			return candidate
		}
	}
	for _, candidate := range []string{
		filepath.Join(ambientValue("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(ambientValue("ProgramFiles(x86)"), "Git", "bin", "bash.exe"),
		filepath.Join(ambientValue("LocalAppData"), "Programs", "Git", "bin", "bash.exe"),
	} {
		if pathIsRegular(candidate) {
			return candidate
		}
	}
	return ""
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func validCommandToken(value string) bool {
	return value != "" && len(value) <= maxHookCommandBytes &&
		!strings.ContainsRune(value, '\x00')
}
