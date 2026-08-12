package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const maxRunMCPServers = 64

// MCPServerSpec is the in-memory, run-owned form of one stdio MCP server.
// Command, arguments, and environment values must never be copied into a
// durable Session, receipt, trace, or log record.
type MCPServerSpec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// MCPRuntime is the narrow lifetime boundary consumed by RunLoop. A runtime
// owns every client process represented by ToolDefs and must be closed by its
// run or protocol-session owner at that owner's lifetime boundary. Tool
// definitions carry opaque in-process executor bindings and do not serialize
// process configuration onto the provider wire.
type MCPRuntime interface {
	ToolDefs() []types.ToolDef
	ServerCount() int
	Generation() string
	Healthy() bool
	Reports() []processsupervisor.Report
	Close()
}

// MCPRuntimeFactory permits protocol adapters to inject a testable factory
// while keeping process creation inside the existing RunLoop composition.
type MCPRuntimeFactory func(
	context.Context,
	string,
	[]MCPServerSpec,
	MCPExecutionOptions,
) (MCPRuntime, error)

type runMCPRuntime struct {
	id        string
	workspace string

	mu      sync.RWMutex
	clients map[string]*MCPClient
	tools   []types.ToolDef
	reports []processsupervisor.Report
	closed  atomic.Bool
	once    sync.Once
}

type mcpToolRuntimeMetadata struct {
	binding mcpRuntimeExecutorBinding
}

type mcpRuntimeExecutorBinding struct {
	runtime    *runMCPRuntime
	runtimeID  string
	serverName string
	executorID string
	executable string
	tool       MCPTool
}

// NewMCPRuntime starts and initializes every declared stdio server, including
// tools/list, before returning. A partial generation is closed on any error so
// callers can never advertise a catalog backed by only part of the request.
func NewMCPRuntime(
	ctx context.Context,
	workDir string,
	servers []MCPServerSpec,
	execution MCPExecutionOptions,
) (MCPRuntime, error) {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, errors.New("MCP workspace is unavailable")
	}
	specs, err := normalizeMCPServerSpecs(servers)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return &runMCPRuntime{
			id:        newMCPExecutorID(),
			workspace: workspace,
			clients:   make(map[string]*MCPClient),
		}, nil
	}
	execution, err = bindRunMCPExecution(ctx, workspace, execution)
	if err != nil {
		return nil, err
	}
	runtimeState := &runMCPRuntime{
		id:        newMCPExecutorID(),
		workspace: workspace,
		clients:   make(map[string]*MCPClient, len(specs)),
	}
	configuredObserver := execution.ObserveStart
	execution.ObserveStart = func(report processsupervisor.Report) {
		runtimeState.mu.Lock()
		runtimeState.reports = append(runtimeState.reports, report)
		runtimeState.mu.Unlock()
		if configuredObserver != nil {
			configuredObserver(report)
		}
	}
	fail := func(serverName string) (MCPRuntime, error) {
		runtimeState.Close()
		return nil, fmt.Errorf("MCP server %q could not establish a secure stdio runtime", serverName)
	}
	for _, server := range specs {
		executable, resolveErr := resolveMCPExecutable(server.Command)
		if resolveErr != nil {
			return fail(server.Name)
		}
		client, connectErr := newMCPClientWithInitializationContext(
			server.Name,
			executable,
			server.Args,
			workspace,
			server.Env,
			execution,
			ctx,
			true,
		)
		if connectErr != nil {
			return fail(server.Name)
		}
		runtimeState.clients[server.Name] = client
	}
	if err := runtimeState.buildCatalog(); err != nil {
		runtimeState.Close()
		return nil, err
	}
	return runtimeState, nil
}

func bindRunMCPExecution(
	ctx context.Context,
	workspace string,
	execution MCPExecutionOptions,
) (MCPExecutionOptions, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if execution.Context == nil {
		execution.Context = ctx
	}
	execution = normalizeMCPExecutionOptions(execution)
	if execution.Runner == nil || execution.Policy.Enforcement == "" {
		return MCPExecutionOptions{}, errors.New("secure MCP execution is not configured")
	}
	if execution.Policy.Enforcement == sandbox.EnforcementDisabled {
		return MCPExecutionOptions{}, errors.New("run-owned MCP forbids host execution")
	}
	if _, host := execution.Runner.(*processsupervisor.HostRunner); host {
		return MCPExecutionOptions{}, errors.New("run-owned MCP forbids host execution")
	}
	capabilities := execution.Runner.Capabilities()
	if capabilities.FilesystemIsolation {
		if strings.TrimSpace(execution.Policy.Workspace) != "" {
			policyWorkspace, err := canonicalWorkspace(execution.Policy.Workspace)
			if err != nil || !sameMCPWorkspace(policyWorkspace, workspace) {
				return MCPExecutionOptions{}, errors.New("MCP sandbox workspace does not match the run workspace")
			}
		}
		execution.Policy.Workspace = workspace
		execution.Policy.WorkspaceAccess = sandbox.WorkspaceReadWrite
	}
	return execution, nil
}

func resolveMCPExecutable(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" || strings.IndexByte(command, 0) >= 0 {
		return "", errors.New("MCP executable is invalid")
	}
	resolved := command
	if !filepath.IsAbs(resolved) {
		if strings.ContainsAny(resolved, `/\\`) {
			return "", errors.New("relative MCP executable paths are not allowed")
		}
		located, err := exec.LookPath(resolved)
		if err != nil {
			return "", errors.New("MCP executable was not found")
		}
		resolved = located
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("MCP executable could not be resolved")
	}
	if canonical, canonicalErr := filepath.EvalSymlinks(absolute); canonicalErr == nil {
		absolute = canonical
	} else {
		return "", errors.New("MCP executable could not be canonicalized")
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return "", errors.New("MCP executable is not a regular file")
	}
	return filepath.Clean(absolute), nil
}

func sameMCPWorkspace(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func normalizeMCPServerSpecs(servers []MCPServerSpec) ([]MCPServerSpec, error) {
	if len(servers) > maxRunMCPServers {
		return nil, fmt.Errorf("MCP server count exceeds %d", maxRunMCPServers)
	}
	normalized := make([]MCPServerSpec, 0, len(servers))
	seen := make(map[string]struct{}, len(servers))
	for _, source := range servers {
		name := strings.TrimSpace(source.Name)
		if name == "" || len(name) > 256 || strings.IndexByte(name, 0) >= 0 {
			return nil, errors.New("MCP server name is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate MCP server name %q", name)
		}
		seen[name] = struct{}{}
		command := strings.TrimSpace(source.Command)
		if command == "" || len(command) > 4096 || strings.IndexByte(command, 0) >= 0 {
			return nil, fmt.Errorf("MCP server %q command is invalid", name)
		}
		if len(source.Args) > 256 || len(source.Env) > 256 {
			return nil, fmt.Errorf("MCP server %q configuration exceeds limits", name)
		}
		args := append([]string(nil), source.Args...)
		for _, argument := range args {
			if len(argument) > 4096 || strings.IndexByte(argument, 0) >= 0 {
				return nil, fmt.Errorf("MCP server %q argument is invalid", name)
			}
		}
		environment := cloneMCPEnvironment(source.Env)
		if _, err := mcpEnvironmentSpec(environment); err != nil {
			return nil, fmt.Errorf("MCP server %q environment is invalid", name)
		}
		normalized = append(normalized, MCPServerSpec{
			Name: name, Command: command, Args: args, Env: environment,
		})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Name < normalized[j].Name })
	return normalized, nil
}

// ValidateMCPServerSpecs validates an in-memory stdio declaration without
// starting a process. It intentionally returns no expanded command detail.
func ValidateMCPServerSpecs(servers []MCPServerSpec) error {
	_, err := normalizeMCPServerSpecs(cloneMCPServerSpecs(servers))
	return err
}

func cloneMCPServerSpecs(servers []MCPServerSpec) []MCPServerSpec {
	if servers == nil {
		return nil
	}
	cloned := make([]MCPServerSpec, len(servers))
	for index, server := range servers {
		cloned[index] = MCPServerSpec{
			Name: server.Name, Command: server.Command,
			Args: append([]string(nil), server.Args...), Env: cloneMCPEnvironment(server.Env),
		}
	}
	return cloned
}

// WorkspaceMCPServerSpecs resolves the existing .mcp.json/settings lookup into
// immutable run-owned specs. The boolean reports whether any config source was
// present, including a valid empty configuration.
func WorkspaceMCPServerSpecs(workDir string) ([]MCPServerSpec, bool, error) {
	raw := LoadMCPConfig(workDir)
	if strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}
	config, err := ParseMCPConfig(raw)
	if err != nil {
		return nil, true, err
	}
	servers := make([]MCPServerSpec, 0, len(config.MCPServers))
	for name, server := range config.MCPServers {
		servers = append(servers, MCPServerSpec{
			Name: name, Command: server.Command,
			Args: append([]string(nil), server.Args...), Env: cloneMCPEnvironment(server.Env),
		})
	}
	normalized, err := normalizeMCPServerSpecs(servers)
	return normalized, true, err
}

func resolveRunMCPServerSpecs(opts RunOptions, workDir string) ([]MCPServerSpec, bool, error) {
	if opts.MCPServers != nil {
		if !opts.DisableWorkspaceMCP {
			return nil, true, errors.New("explicit MCP servers require workspace MCP to be disabled")
		}
		normalized, err := normalizeMCPServerSpecs(cloneMCPServerSpecs(opts.MCPServers))
		return normalized, true, err
	}
	if opts.DisableWorkspaceMCP {
		return nil, false, nil
	}
	return WorkspaceMCPServerSpecs(workDir)
}

func (r *runMCPRuntime) buildCatalog() error {
	if r == nil {
		return errors.New("MCP runtime is nil")
	}
	names := make([]string, 0, len(r.clients))
	for name := range r.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	seenTools := make(map[string]string)
	definitions := make([]types.ToolDef, 0)
	for _, serverName := range names {
		client := r.clients[serverName]
		for _, tool := range client.Tools() {
			if previous, duplicate := seenTools[tool.Name]; duplicate {
				return fmt.Errorf("MCP tool %q is claimed by both %q and %q", tool.Name, previous, serverName)
			}
			seenTools[tool.Name] = serverName
			binding := mcpRuntimeExecutorBinding{
				runtime: r, runtimeID: r.id, serverName: serverName,
				executorID: client.executorID,
				executable: client.executable,
				tool:       cloneMCPTool(tool),
			}
			definitions = append(definitions, types.ToolDef{
				Name: tool.Name, Description: tool.Description,
				InputSchema:    append(json.RawMessage(nil), tool.InputSchema...),
				RuntimeBinding: mcpToolRuntimeMetadata{binding: binding},
			})
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	r.tools = definitions
	return nil
}

func cloneMCPTool(tool MCPTool) MCPTool {
	tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
	return tool
}

func (r *runMCPRuntime) ToolDefs() []types.ToolDef {
	if r == nil || r.closed.Load() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	definitions := make([]types.ToolDef, len(r.tools))
	for index, definition := range r.tools {
		definitions[index] = definition
		definitions[index].InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	}
	return definitions
}

func (r *runMCPRuntime) ServerCount() int {
	if r == nil || r.closed.Load() {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

func (r *runMCPRuntime) Generation() string {
	if r == nil {
		return ""
	}
	return r.id
}

func (r *runMCPRuntime) Healthy() bool {
	if r == nil || r.closed.Load() {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, client := range r.clients {
		if client == nil || !client.isRunning() {
			return false
		}
	}
	return true
}

func (r *runMCPRuntime) Reports() []processsupervisor.Report {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]processsupervisor.Report(nil), r.reports...)
}

func (r *runMCPRuntime) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.closed.Store(true)
		r.mu.Lock()
		clients := make([]*MCPClient, 0, len(r.clients))
		for name, client := range r.clients {
			delete(r.clients, name)
			clients = append(clients, client)
		}
		r.mu.Unlock()
		for _, client := range clients {
			client.Close()
		}
	})
}

func mcpRuntimeBindingFromTool(tool types.ToolDef) (mcpRuntimeExecutorBinding, bool, error) {
	metadata, ok := tool.RuntimeBinding.(mcpToolRuntimeMetadata)
	if !ok {
		return mcpRuntimeExecutorBinding{}, false, nil
	}
	binding := metadata.binding
	binding.tool = cloneMCPTool(binding.tool)
	if err := validateMCPRuntimeBindingDefinition(tool, binding); err != nil {
		return mcpRuntimeExecutorBinding{}, true, err
	}
	return binding, true, nil
}

func validateMCPRuntimeBindingDefinition(tool types.ToolDef, binding mcpRuntimeExecutorBinding) error {
	if binding.runtime == nil || binding.runtimeID == "" || binding.serverName == "" ||
		binding.executorID == "" || binding.executable == "" || binding.tool.Name != tool.Name {
		return fmt.Errorf("MCP binding for %q is incomplete", tool.Name)
	}
	if binding.runtime.closed.Load() || binding.runtime.id != binding.runtimeID {
		return fmt.Errorf("MCP binding for %q belongs to a closed or replaced runtime", tool.Name)
	}
	if !bytes.Equal(bytes.TrimSpace(binding.tool.InputSchema), bytes.TrimSpace(tool.InputSchema)) {
		return fmt.Errorf("MCP binding for %q does not match its schema", tool.Name)
	}
	binding.runtime.mu.RLock()
	client := binding.runtime.clients[binding.serverName]
	binding.runtime.mu.RUnlock()
	if client == nil || !client.isRunning() || client.executorID != binding.executorID ||
		!sameMCPExecutable(client.executable, binding.executable) {
		return fmt.Errorf("MCP binding for %q no longer owns its executor", tool.Name)
	}
	matches := 0
	for _, candidate := range client.Tools() {
		if candidate.Name == tool.Name {
			matches++
			if toolSchemaDigest(candidate.InputSchema) != toolSchemaDigest(tool.InputSchema) {
				return fmt.Errorf("MCP binding for %q changed schema", tool.Name)
			}
		}
	}
	if matches != 1 {
		return fmt.Errorf("MCP binding for %q is ambiguous", tool.Name)
	}
	return nil
}

func sameMCPExecutable(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func (binding mcpRuntimeExecutorBinding) call(
	ctx context.Context,
	toolName string,
	input json.RawMessage,
) (string, bool) {
	runtimeState := binding.runtime
	if runtimeState == nil || runtimeState.closed.Load() {
		return "[IDENTITY BLOCKED] MCP runtime is closed", true
	}
	runtimeState.mu.RLock()
	defer runtimeState.mu.RUnlock()
	client := runtimeState.clients[binding.serverName]
	if runtimeState.id != binding.runtimeID || client == nil || !client.isRunning() || client.executorID != binding.executorID ||
		!sameMCPExecutable(client.executable, binding.executable) {
		return "[IDENTITY BLOCKED] MCP runtime generation changed", true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return client.CallToolContext(ctx, toolName, input)
}

func callMCPToolBoundWithContext(
	ctx context.Context,
	toolName string,
	input json.RawMessage,
	expected toolExecutorIdentity,
) (string, bool) {
	if err := validateBoundToolExecutorIdentity(expected); err != nil {
		return "[IDENTITY BLOCKED] " + err.Error(), true
	}
	loaded, ok := toolCatalogSchemaSnapshots.Load(expected.CatalogMarker)
	if !ok {
		return "[IDENTITY BLOCKED] MCP executor catalog is unavailable", true
	}
	snapshot, ok := loaded.(toolCatalogSchemaSnapshot)
	if !ok || snapshot.err != nil {
		return "[IDENTITY BLOCKED] MCP executor catalog is invalid", true
	}
	if binding, runOwned := snapshot.mcpExecutors[toolName]; runOwned {
		if binding.serverName != expected.Owner || binding.executorID != expected.ExecutorID ||
			toolSchemaDigest(binding.tool.InputSchema) != expected.SchemaDigest {
			return "[IDENTITY BLOCKED] MCP runtime binding does not match the immutable catalog", true
		}
		return binding.call(ctx, toolName, input)
	}
	if !snapshot.processGlobalMCP {
		return "[IDENTITY BLOCKED] MCP executor is not owned by this run", true
	}
	return callMCPToolBound(toolName, input, expected)
}
