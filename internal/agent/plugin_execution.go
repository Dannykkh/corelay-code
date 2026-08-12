package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	pluginDefaultTimeout         = 30 * time.Second
	pluginMaximumTimeout         = 5 * time.Minute
	pluginDefaultOutputBytes     = 1 << 20
	pluginMaximumOutputBytes     = 8 << 20
	pluginMaximumInputBytes      = 1 << 20
	pluginMaximumBinaryBytes     = 512 << 20
	pluginExecutorProtocol       = "corelay.plugin-executor.v1"
	legacyPluginExecutorProtocol = "aniclew.plugin-executor.v1"
)

// PluginExecutionOptions is the explicit secure process composition for
// executable plugin tools. The zero Policy is hardened into a required,
// read-only, no-network policy. A Runner is always mandatory.
type PluginExecutionOptions struct {
	Runner           sandbox.Runner
	Policy           sandbox.Policy
	Workspace        string
	Timeout          time.Duration
	OutputLimitBytes int64
	ObserveReport    func(PluginExecutionReport)
}

// PluginExecutionReport contains bounded, redacted execution metadata. It
// intentionally excludes executable paths, arguments, input, environment and
// captured process output.
type PluginExecutionReport struct {
	Plugin           string               `json:"plugin"`
	Tool             string               `json:"tool"`
	ExecutorID       string               `json:"executorId"`
	Runner           string               `json:"runner"`
	Started          bool                 `json:"started"`
	ExitCode         int                  `json:"exitCode"`
	TimedOut         bool                 `json:"timedOut,omitempty"`
	OutputTruncated  bool                 `json:"outputTruncated,omitempty"`
	Failure          sandbox.FailureCode  `json:"failure,omitempty"`
	AppliedIsolation sandbox.Capabilities `json:"appliedIsolation"`
	DurationMillis   int64                `json:"durationMillis"`
	Detail           string               `json:"detail,omitempty"`
}

type pluginToolRuntimeMetadata struct {
	binding pluginExecutorBinding
}

type pluginExecutorBinding struct {
	pluginName       string
	pluginVersion    string
	toolName         string
	schema           json.RawMessage
	rootDir          string
	manifestPath     string
	manifestDigest   string
	executable       string
	executableRel    string
	executableDigest string
	executableSize   int64
	args             []string
	executorID       string
	runner           sandbox.Runner
	runnerName       string
	runnerCaps       sandbox.Capabilities
	policy           sandbox.Policy
	timeout          time.Duration
	outputLimit      int64
	observe          func(PluginExecutionReport)
}

type pluginExecutorContentDescriptor struct {
	Protocol         string               `json:"protocol"`
	PluginName       string               `json:"plugin_name"`
	PluginVersion    string               `json:"plugin_version"`
	ToolName         string               `json:"tool_name"`
	SchemaDigest     string               `json:"schema_digest"`
	ManifestDigest   string               `json:"manifest_digest"`
	Executable       string               `json:"executable"`
	ExecutableDigest string               `json:"executable_digest"`
	ExecutableSize   int64                `json:"executable_size"`
	Args             []string             `json:"args"`
	RunnerName       string               `json:"runner_name"`
	RunnerCaps       sandbox.Capabilities `json:"runner_capabilities"`
	Policy           sandbox.Policy       `json:"policy"`
	TimeoutNanos     int64                `json:"timeout_nanos"`
	OutputLimit      int64                `json:"output_limit"`
}

// LoadExecutablePluginTools is the narrow composition helper for callers that
// want a strict plugin catalog without wiring discovery into RunLoop.
func LoadExecutablePluginTools(dirs []string, options PluginExecutionOptions) ([]types.ToolDef, error) {
	manager := NewPluginManager(dirs...)
	if err := manager.LoadAllStrict(); err != nil {
		return nil, err
	}
	return manager.ExecutableToolDefs(options)
}

func resolveRunPluginToolDefs(
	opts RunOptions,
	workDir string,
	runRunner sandbox.Runner,
) ([]types.ToolDef, error) {
	if opts.DisablePlugins || opts.PluginDirs != nil && len(opts.PluginDirs) == 0 {
		return nil, nil
	}
	directories := opts.PluginDirs
	if directories == nil {
		directories = DefaultPluginDirs(workDir)
	}
	execution := PluginExecutionOptions{Runner: runRunner, Workspace: workDir}
	if opts.PluginExecution != nil {
		execution = *opts.PluginExecution
		if execution.Runner == nil {
			execution.Runner = runRunner
		}
		if strings.TrimSpace(execution.Workspace) == "" {
			execution.Workspace = workDir
		}
	}
	return LoadExecutablePluginTools(append([]string(nil), directories...), execution)
}

func explicitRunPluginConfiguration(opts RunOptions) bool {
	return opts.PluginDirs != nil || opts.PluginExecution != nil
}

// ExecutableToolDefs returns only exec-form plugin tools. Legacy command
// descriptors remain listable through GetAllTools but never acquire a runtime
// binding. The returned metadata is opaque and provider-invisible.
func (pm *PluginManager) ExecutableToolDefs(options PluginExecutionOptions) ([]types.ToolDef, error) {
	if pm.loadErr != nil {
		return nil, fmt.Errorf("plugin manifests are unavailable: %w", pm.loadErr)
	}
	executableCount := 0
	for _, plugin := range pm.plugins {
		for _, tool := range plugin.Tools {
			if strings.TrimSpace(tool.Executable) != "" {
				executableCount++
			}
		}
	}
	if executableCount == 0 {
		return nil, nil
	}
	normalized, err := normalizePluginExecutionOptions(options)
	if err != nil {
		return nil, err
	}
	definitions := make([]types.ToolDef, 0, executableCount)
	seen := make(map[string]struct{})
	for _, plugin := range pm.plugins {
		for _, tool := range plugin.Tools {
			if strings.TrimSpace(tool.Executable) == "" {
				continue
			}
			if _, duplicate := seen[tool.Name]; duplicate {
				return nil, fmt.Errorf("duplicate executable plugin tool %q", tool.Name)
			}
			seen[tool.Name] = struct{}{}
			if _, reserved := currentBuiltInToolDefinitions()[tool.Name]; reserved {
				return nil, fmt.Errorf("plugin tool %q collides with a reserved built-in name", tool.Name)
			}
			if mcpToolNameClaimed(tool.Name) {
				return nil, fmt.Errorf("plugin tool %q collides with an MCP executor", tool.Name)
			}
			binding, err := buildPluginExecutorBinding(plugin, tool, normalized)
			if err != nil {
				return nil, fmt.Errorf("prepare plugin %q tool %q: %w", plugin.Name, tool.Name, err)
			}
			definitions = append(definitions, types.ToolDef{
				Name:           tool.Name,
				Description:    tool.Description,
				InputSchema:    append(json.RawMessage(nil), tool.InputSchema...),
				RuntimeBinding: pluginToolRuntimeMetadata{binding: binding},
			})
		}
	}
	return definitions, nil
}

func normalizePluginExecutionOptions(options PluginExecutionOptions) (PluginExecutionOptions, error) {
	if options.Runner == nil {
		return PluginExecutionOptions{}, errors.New("plugin sandbox runner is required")
	}
	workspace := strings.TrimSpace(options.Workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(options.Policy.Workspace)
	}
	if workspace == "" {
		return PluginExecutionOptions{}, errors.New("plugin workspace is required")
	}
	canonical, err := canonicalWorkspace(workspace)
	if err != nil {
		return PluginExecutionOptions{}, fmt.Errorf("plugin workspace: %w", err)
	}

	policy := options.Policy
	if policy.Enforcement == "" {
		policy.Enforcement = sandbox.EnforcementRequired
	}
	if policy.Enforcement != sandbox.EnforcementRequired {
		return PluginExecutionOptions{}, errors.New("plugin sandbox enforcement must be required")
	}
	if policy.Workspace == "" {
		policy.Workspace = canonical
	} else {
		policyWorkspace, err := canonicalWorkspace(policy.Workspace)
		if err != nil {
			return PluginExecutionOptions{}, fmt.Errorf("plugin policy workspace: %w", err)
		}
		if !sameCanonicalPath(policyWorkspace, canonical) {
			return PluginExecutionOptions{}, errors.New("plugin workspace does not match sandbox policy workspace")
		}
		policy.Workspace = policyWorkspace
	}
	if policy.WorkspaceAccess == sandbox.WorkspaceAccessUnspecified {
		policy.WorkspaceAccess = sandbox.WorkspaceReadOnly
	}
	if policy.WorkspaceAccess != sandbox.WorkspaceReadOnly && policy.WorkspaceAccess != sandbox.WorkspaceReadWrite {
		return PluginExecutionOptions{}, errors.New("plugin sandbox requires explicit read_only or read_write workspace access")
	}
	if policy.Network == sandbox.NetworkAccessUnspecified {
		policy.Network = sandbox.NetworkDenied
	}
	if policy.Network != sandbox.NetworkDenied {
		return PluginExecutionOptions{}, errors.New("plugin sandbox network access must be denied")
	}
	policy.Required.FilesystemIsolation = true
	policy.Required.NetworkIsolation = true
	policy.Required.EnvironmentFiltering = true
	policy.Required.Timeouts = true
	policy.Required.ProcessTreeKill = true
	capabilities := options.Runner.Capabilities()
	if err := sandbox.ValidatePolicy(policy, capabilities); err != nil {
		return PluginExecutionOptions{}, fmt.Errorf("plugin sandbox cannot satisfy secure policy: %w", err)
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = pluginDefaultTimeout
	}
	if timeout < 0 || timeout > pluginMaximumTimeout {
		return PluginExecutionOptions{}, fmt.Errorf("plugin timeout must be between 1ns and %s", pluginMaximumTimeout)
	}
	outputLimit := options.OutputLimitBytes
	if outputLimit == 0 {
		outputLimit = pluginDefaultOutputBytes
	}
	if outputLimit < 1 || outputLimit > pluginMaximumOutputBytes || outputLimit > sandbox.MaximumOutputLimitBytes {
		return PluginExecutionOptions{}, fmt.Errorf("plugin output limit must be between 1 and %d bytes", pluginMaximumOutputBytes)
	}
	options.Policy = policy
	options.Workspace = canonical
	options.Timeout = timeout
	options.OutputLimitBytes = outputLimit
	return options, nil
}

func buildPluginExecutorBinding(plugin Plugin, tool PluginTool, options PluginExecutionOptions) (pluginExecutorBinding, error) {
	executable, executableRel, digest, size, err := inspectPluginExecutable(plugin.rootDir, tool.Executable)
	if err != nil {
		return pluginExecutorBinding{}, err
	}
	binding := pluginExecutorBinding{
		pluginName:       plugin.Name,
		pluginVersion:    plugin.Version,
		toolName:         tool.Name,
		schema:           append(json.RawMessage(nil), tool.InputSchema...),
		rootDir:          plugin.rootDir,
		manifestPath:     filepath.Join(plugin.rootDir, "plugin.json"),
		manifestDigest:   plugin.manifestDigest,
		executable:       executable,
		executableRel:    executableRel,
		executableDigest: digest,
		executableSize:   size,
		args:             append([]string(nil), tool.Args...),
		runner:           options.Runner,
		runnerName:       options.Runner.Name(),
		runnerCaps:       options.Runner.Capabilities(),
		policy:           options.Policy,
		timeout:          options.Timeout,
		outputLimit:      options.OutputLimitBytes,
		observe:          options.ObserveReport,
	}
	binding.executorID = pluginExecutorID(binding)
	return binding, nil
}

func pluginExecutorID(binding pluginExecutorBinding) string {
	return pluginExecutorIDForProtocol(binding, pluginExecutorProtocol)
}

func legacyPluginExecutorID(binding pluginExecutorBinding) string {
	return pluginExecutorIDForProtocol(binding, legacyPluginExecutorProtocol)
}

func pluginExecutorIDForProtocol(binding pluginExecutorBinding, protocol string) string {
	descriptor := pluginExecutorContentDescriptor{
		Protocol:         protocol,
		PluginName:       binding.pluginName,
		PluginVersion:    binding.pluginVersion,
		ToolName:         binding.toolName,
		SchemaDigest:     toolSchemaDigest(binding.schema),
		ManifestDigest:   binding.manifestDigest,
		Executable:       filepath.ToSlash(binding.executableRel),
		ExecutableDigest: binding.executableDigest,
		ExecutableSize:   binding.executableSize,
		Args:             append([]string(nil), binding.args...),
		RunnerName:       binding.runnerName,
		RunnerCaps:       binding.runnerCaps,
		Policy:           binding.policy,
		TimeoutNanos:     int64(binding.timeout),
		OutputLimit:      binding.outputLimit,
	}
	encoded, _ := json.Marshal(descriptor)
	return "plugin:" + sha256Bytes(encoded)
}

func inspectPluginExecutable(root, requested string) (string, string, string, int64, error) {
	if strings.TrimSpace(requested) == "" {
		return "", "", "", 0, errors.New("executable is empty")
	}
	if strings.IndexByte(requested, 0) >= 0 || filepath.IsAbs(requested) || filepath.VolumeName(requested) != "" {
		return "", "", "", 0, errors.New("executable must be a relative path inside the plugin directory")
	}
	clean := filepath.Clean(requested)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", "", 0, errors.New("executable escapes the plugin directory")
	}
	target := filepath.Join(root, clean)
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", "", "", 0, err
	}
	if !pathWithin(absolute, root) {
		return "", "", "", 0, errors.New("executable escapes the plugin directory")
	}
	current := root
	for _, component := range strings.FieldsFunc(clean, func(character rune) bool {
		return character == '/' || character == '\\'
	}) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", "", "", 0, fmt.Errorf("inspect executable component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", "", 0, errors.New("executable path contains a symlink")
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", "", "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", "", "", 0, errors.New("executable must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", "", "", 0, errors.New("plugin executable has no execute permission")
	}
	if info.Size() > pluginMaximumBinaryBytes {
		return "", "", "", 0, fmt.Errorf("plugin executable exceeds %d-byte limit", pluginMaximumBinaryBytes)
	}
	digest, size, err := hashStablePluginFile(absolute, pluginMaximumBinaryBytes)
	if err != nil {
		return "", "", "", 0, err
	}
	return filepath.Clean(absolute), clean, digest, size, nil
}

func hashStablePluginFile(path string, maximum int64) (string, int64, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", 0, errors.New("plugin artifact must be a non-symlink regular file")
	}
	if before.Size() > maximum {
		return "", 0, fmt.Errorf("plugin artifact exceeds %d-byte limit", maximum)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, io.LimitReader(file, maximum+1))
	inside, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if statErr != nil {
		return "", 0, statErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if written > maximum {
		return "", 0, fmt.Errorf("plugin artifact exceeds %d-byte limit", maximum)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(before, inside) || !os.SameFile(inside, after) || before.Size() != written || after.Size() != written {
		return "", 0, errors.New("plugin artifact changed while being hashed")
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), written, nil
}

func clonePluginExecutorBinding(binding pluginExecutorBinding) pluginExecutorBinding {
	result := binding
	result.schema = append(json.RawMessage(nil), binding.schema...)
	result.args = append([]string(nil), binding.args...)
	return result
}

func pluginRuntimeBindingFromTool(tool types.ToolDef) (pluginExecutorBinding, bool, error) {
	if tool.RuntimeBinding == nil {
		return pluginExecutorBinding{}, false, nil
	}
	metadata, ok := tool.RuntimeBinding.(pluginToolRuntimeMetadata)
	if !ok {
		return pluginExecutorBinding{}, false, fmt.Errorf("tool %q has an unsupported runtime binding", tool.Name)
	}
	binding := clonePluginExecutorBinding(metadata.binding)
	if err := validatePluginBindingDefinition(tool, binding); err != nil {
		return pluginExecutorBinding{}, true, err
	}
	return binding, true, nil
}

func validatePluginBindingDefinition(tool types.ToolDef, binding pluginExecutorBinding) error {
	if binding.runner == nil || binding.executorID == "" || binding.toolName != tool.Name ||
		binding.pluginName == "" || binding.executable == "" {
		return fmt.Errorf("plugin binding for %q is incomplete", tool.Name)
	}
	if !bytes.Equal(bytes.TrimSpace(binding.schema), bytes.TrimSpace(tool.InputSchema)) {
		return fmt.Errorf("plugin binding for %q does not match its schema", tool.Name)
	}
	if pluginExecutorID(binding) != binding.executorID &&
		legacyPluginExecutorID(binding) != binding.executorID {
		return fmt.Errorf("plugin binding for %q has an invalid content identity", tool.Name)
	}
	if binding.runner.Name() != binding.runnerName || binding.runner.Capabilities() != binding.runnerCaps {
		return fmt.Errorf("plugin binding for %q changed runner identity", tool.Name)
	}
	if err := sandbox.ValidatePolicy(binding.policy, binding.runnerCaps); err != nil {
		return fmt.Errorf("plugin binding for %q has an unsatisfied policy: %w", tool.Name, err)
	}
	if binding.policy.Enforcement != sandbox.EnforcementRequired ||
		binding.policy.Network != sandbox.NetworkDenied ||
		!binding.policy.RequiredCapabilities().FilesystemIsolation ||
		!binding.policy.RequiredCapabilities().NetworkIsolation {
		return fmt.Errorf("plugin binding for %q lacks mandatory isolation", tool.Name)
	}
	return nil
}

func validatePluginBindingArtifacts(binding pluginExecutorBinding) error {
	manifestDigest, _, err := hashStablePluginFile(binding.manifestPath, maxPluginManifestBytes)
	if err != nil {
		return fmt.Errorf("plugin manifest identity: %w", err)
	}
	if manifestDigest != binding.manifestDigest {
		return errors.New("plugin manifest changed after catalog binding")
	}
	executable, relative, digest, size, err := inspectPluginExecutable(binding.rootDir, binding.executableRel)
	if err != nil {
		return fmt.Errorf("plugin executable identity: %w", err)
	}
	if !sameCanonicalPath(executable, binding.executable) || relative != binding.executableRel ||
		digest != binding.executableDigest || size != binding.executableSize {
		return errors.New("plugin executable changed after catalog binding")
	}
	return nil
}

func executeBoundPluginTool(
	name string,
	input json.RawMessage,
	workDir string,
	identity toolExecutorIdentity,
	opts ToolExecutionOptions,
) (string, bool) {
	binding, err := pluginBindingForIdentity(identity)
	if err != nil {
		return "[PLUGIN BLOCKED] " + sanitizeReceiptString(err.Error()), true
	}
	if binding.toolName != name {
		return "[PLUGIN BLOCKED] executor identity does not match tool name", true
	}
	workspace, err := canonicalWorkspace(workDir)
	if err != nil || !sameCanonicalPath(workspace, binding.policy.Workspace) {
		return "[PLUGIN BLOCKED] execution workspace does not match the immutable plugin policy", true
	}
	if len(input) > pluginMaximumInputBytes {
		return "[PLUGIN BLOCKED] input exceeds the plugin JSON limit", true
	}
	if _, err := decodeUniqueJSONObject(input); err != nil {
		return "[PLUGIN BLOCKED] invalid JSON input: " + sanitizeReceiptString(err.Error()), true
	}
	if err := validateToolInputSchema(input, binding.schema); err != nil {
		return "[PLUGIN BLOCKED] input schema mismatch: " + sanitizeReceiptString(err.Error()), true
	}
	if err := validatePluginBindingArtifacts(binding); err != nil {
		return "[PLUGIN BLOCKED] " + sanitizeReceiptString(err.Error()), true
	}
	if binding.runner.Name() != binding.runnerName || binding.runner.Capabilities() != binding.runnerCaps {
		return "[PLUGIN BLOCKED] sandbox runner identity changed after catalog binding", true
	}
	if err := sandbox.ValidatePolicy(binding.policy, binding.runner.Capabilities()); err != nil {
		return "[PLUGIN BLOCKED] secure sandbox is unavailable: " + sanitizeReceiptString(err.Error()), true
	}
	if err := validatePluginApproval(
		opts.pluginApproval,
		name,
		identity.ExecutorID,
		input,
		opts.ExpectedSessionID,
		opts.ExpectedRunID,
	); err != nil {
		return "[PLUGIN BLOCKED] explicit per-call approval is required: " + sanitizeReceiptString(err.Error()), true
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	stdin := append([]byte(nil), input...)
	stdin = append(stdin, '\n')
	result, runnerReport := binding.runner.Run(ctx, binding.policy, sandbox.CommandSpec{
		Path:             binding.executable,
		Args:             append([]string(nil), binding.args...),
		Dir:              binding.policy.Workspace,
		Environment:      sandbox.EnvironmentSpec{},
		Stdin:            stdin,
		Timeout:          binding.timeout,
		OutputLimitBytes: binding.outputLimit,
	})
	safeRunnerReport := runnerReport
	safeRunnerReport.Runner = sanitizeReceiptString(safeRunnerReport.Runner)
	safeRunnerReport.Detail = sanitizeReceiptString(safeRunnerReport.Detail)
	report := safePluginExecutionReport(binding, result, safeRunnerReport)
	if result.Started && (!runnerReport.AppliedIsolation.FilesystemIsolation || !runnerReport.AppliedIsolation.NetworkIsolation) {
		report.Failure = sandbox.FailureCapabilityUnavailable
		report.Detail = "sandbox report did not prove mandatory filesystem and network isolation"
	}
	if binding.observe != nil {
		binding.observe(report)
	}
	if opts.ObserveSandbox != nil {
		opts.ObserveSandbox(safeRunnerReport)
	}
	if report.Failure == sandbox.FailureCapabilityUnavailable && result.Started &&
		(!runnerReport.AppliedIsolation.FilesystemIsolation || !runnerReport.AppliedIsolation.NetworkIsolation) {
		return marshalPluginResult("blocked", result, report), true
	}
	if runnerReport.Failure != sandbox.FailureNone || result.Err != nil || !result.Started || result.ExitCode != 0 {
		return marshalPluginResult("failed", result, report), true
	}
	return marshalPluginResult("ok", result, report), false
}

func pluginBindingForIdentity(identity toolExecutorIdentity) (pluginExecutorBinding, error) {
	if err := validateBoundToolExecutorIdentity(identity); err != nil {
		return pluginExecutorBinding{}, err
	}
	loaded, ok := toolCatalogSchemaSnapshots.Load(identity.CatalogMarker)
	if !ok {
		return pluginExecutorBinding{}, errors.New("plugin executor catalog is unavailable")
	}
	snapshot, ok := loaded.(toolCatalogSchemaSnapshot)
	if !ok || snapshot.err != nil {
		return pluginExecutorBinding{}, errors.New("plugin executor catalog is invalid")
	}
	binding, ok := snapshot.pluginExecutors[identity.ToolName]
	if !ok || binding.executorID != identity.ExecutorID {
		return pluginExecutorBinding{}, errors.New("plugin executor is not bound to this catalog")
	}
	return clonePluginExecutorBinding(binding), nil
}

func safePluginExecutionReport(binding pluginExecutorBinding, result sandbox.Result, report sandbox.Report) PluginExecutionReport {
	detail := sanitizeReceiptString(report.Detail)
	if detail == "" && result.Err != nil {
		detail = sanitizeReceiptString(result.Err.Error())
	}
	return PluginExecutionReport{
		Plugin:           sanitizeReceiptString(binding.pluginName),
		Tool:             sanitizeReceiptString(binding.toolName),
		ExecutorID:       binding.executorID,
		Runner:           sanitizeReceiptString(report.Runner),
		Started:          result.Started,
		ExitCode:         result.ExitCode,
		TimedOut:         result.TimedOut,
		OutputTruncated:  result.OutputTruncated,
		Failure:          report.Failure,
		AppliedIsolation: report.AppliedIsolation,
		DurationMillis:   result.Duration.Milliseconds(),
		Detail:           detail,
	}
}

func marshalPluginResult(status string, result sandbox.Result, report PluginExecutionReport) string {
	type safeResult struct {
		Status          string              `json:"status"`
		Stdout          string              `json:"stdout,omitempty"`
		Stderr          string              `json:"stderr,omitempty"`
		ExitCode        int                 `json:"exit_code"`
		TimedOut        bool                `json:"timed_out,omitempty"`
		OutputTruncated bool                `json:"output_truncated,omitempty"`
		Failure         sandbox.FailureCode `json:"failure,omitempty"`
		Detail          string              `json:"detail,omitempty"`
		ExecutorID      string              `json:"executor_id"`
	}
	payload := safeResult{
		Status:          status,
		Stdout:          sanitizeReceiptString(string(result.Stdout)),
		Stderr:          sanitizeReceiptString(string(result.Stderr)),
		ExitCode:        result.ExitCode,
		TimedOut:        result.TimedOut,
		OutputTruncated: result.OutputTruncated,
		Failure:         report.Failure,
		Detail:          report.Detail,
		ExecutorID:      report.ExecutorID,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"status":"failed","detail":"plugin result encoding failed"}`
	}
	return string(encoded)
}

func sameCanonicalPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}
