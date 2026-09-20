package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	maxMCPConfigPaths     = 64
	maxMCPConfigFileBytes = 256 * 1024
)

var errMCPConfigMissingServers = errors.New("missing mcpServers")

// MCPServerConfig represents an MCP server from .mcp.json
type MCPServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPConfig represents the .mcp.json file structure
type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// MCPConnection holds a running MCP server process.
type MCPConnection struct {
	Name       string
	Config     MCPServerConfig
	Process    *exec.Cmd // compatibility view only; spawning lives in processsupervisor
	Running    bool
	supervised *processsupervisor.Process
}

var (
	mcpConnections   = make(map[string]*MCPConnection)
	mcpConnectionsMu sync.RWMutex
)

// ParseMCPConfig reads and parses .mcp.json
func ParseMCPConfig(configJSON string) (*MCPConfig, error) {
	if configJSON == "" {
		return nil, fmt.Errorf("no MCP config")
	}
	decoder := json.NewDecoder(strings.NewReader(configJSON))
	decoder.DisallowUnknownFields()
	var cfg MCPConfig
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse .mcp.json: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("parse .mcp.json: trailing JSON value")
	}
	for name, server := range cfg.MCPServers {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("parse .mcp.json: server name is empty")
		}
		kind := strings.ToLower(strings.TrimSpace(server.Type))
		if kind == "http" {
			if strings.TrimSpace(server.URL) == "" {
				return nil, fmt.Errorf("parse .mcp.json: server %q URL is empty", name)
			}
		} else if strings.TrimSpace(server.Command) == "" {
			return nil, fmt.Errorf("parse .mcp.json: server %q command is empty", name)
		}
		server.Type = kind
		server.Args = append([]string(nil), server.Args...)
		server.Env = cloneMCPEnvironment(server.Env)
		cfg.MCPServers[name] = server
	}
	return &cfg, nil
}

// configuredMCPConfigPaths returns only the operator-configured supplemental
// paths. A malformed application config produces no paths here; the caller's
// normal configuration validation reports that failure separately.
func configuredMCPConfigPaths() []string {
	cfg, _, err := config.LoadChecked()
	if err != nil {
		return nil
	}
	return append([]string(nil), cfg.MCPConfigPaths...)
}

// LoadMCPConfigWithPaths merges the workspace and supplemental MCP sources in
// increasing precedence order. The later source replaces a server with the
// same name, while unrelated servers are retained:
// user settings < configured paths (listed order) < workspace settings <
// workspace mcp.json < workspace .mcp.json.
//
// Supplemental paths are resolved relative to workDir unless absolute. They
// are explicit operator configuration, so a missing or malformed file fails
// closed. The boolean reports whether at least one source was present.
func LoadMCPConfigWithPaths(workDir string, supplementalPaths []string) (string, bool, error) {
	workspace := strings.TrimSpace(workDir)
	if workspace == "" {
		return "", false, errors.New("MCP workspace is empty")
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", false, errors.New("MCP workspace is invalid")
	}

	merged := MCPConfig{MCPServers: make(map[string]MCPServerConfig)}
	present := false
	seenPaths := make(map[string]struct{})
	mergePath := func(path string, required bool) error {
		canonical, resolveErr := resolveMCPConfigPath(workspace, path)
		if resolveErr != nil {
			if required {
				return resolveErr
			}
			return nil
		}
		key := canonical
		if _, duplicate := seenPaths[key]; duplicate {
			return nil
		}
		seenPaths[key] = struct{}{}
		file, readErr := os.Open(canonical)
		if errors.Is(readErr, os.ErrNotExist) {
			if required {
				return fmt.Errorf("MCP config %q was not found", path)
			}
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read MCP config %q: %w", path, readErr)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxMCPConfigFileBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return fmt.Errorf("read MCP config %q: %w", path, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("read MCP config %q: %w", path, closeErr)
		}
		if len(data) > maxMCPConfigFileBytes {
			return fmt.Errorf("MCP config %q exceeds %d bytes", path, maxMCPConfigFileBytes)
		}
		cfg, parseErr := parseMCPConfigSource(data)
		if parseErr != nil {
			if errors.Is(parseErr, errMCPConfigMissingServers) && !required {
				return nil
			}
			return fmt.Errorf("parse MCP config %q: %w", path, parseErr)
		}
		present = true
		for name, server := range cfg.MCPServers {
			merged.MCPServers[name] = cloneMCPServerConfig(server)
		}
		return nil
	}

	// The user-level settings file remains the lowest-precedence default.
	if home, homeErr := os.UserHomeDir(); homeErr == nil && strings.TrimSpace(home) != "" {
		if err := mergePath(filepath.Join(home, ".claude", "settings.json"), false); err != nil {
			return "", false, err
		}
	}
	if len(supplementalPaths) > maxMCPConfigPaths {
		return "", false, fmt.Errorf("MCP config path count exceeds %d", maxMCPConfigPaths)
	}
	for _, path := range supplementalPaths {
		if strings.TrimSpace(path) == "" {
			return "", false, errors.New("MCP config path is empty")
		}
		if err := mergePath(path, true); err != nil {
			return "", false, err
		}
	}
	if err := mergePath(filepath.Join(workspace, ".claude", "settings.json"), false); err != nil {
		return "", false, err
	}
	if err := mergePath(filepath.Join(workspace, "mcp.json"), false); err != nil {
		return "", false, err
	}
	if err := mergePath(filepath.Join(workspace, ".mcp.json"), false); err != nil {
		return "", false, err
	}
	if !present {
		return "", false, nil
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "", false, fmt.Errorf("encode merged MCP config: %w", err)
	}
	return string(encoded), true, nil
}

func resolveMCPConfigPath(workspace, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || len(path) > 4096 || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("MCP config path is invalid")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", errors.New("MCP config home path is unavailable")
		}
		path = filepath.Join(home, strings.TrimLeft(path[1:], "/\\"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("MCP config path is invalid")
	}
	return filepath.Clean(absolute), nil
}

func parseMCPConfigSource(data []byte) (*MCPConfig, error) {
	if cfg, err := ParseMCPConfig(string(data)); err == nil {
		return cfg, nil
	}
	var settings struct {
		MCPServers map[string]MCPServerConfig `json:"mcpServers"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&settings); err != nil {
		return nil, err
	}
	if settings.MCPServers == nil {
		return nil, errMCPConfigMissingServers
	}
	encoded, err := json.Marshal(MCPConfig{MCPServers: settings.MCPServers})
	if err != nil {
		return nil, err
	}
	return ParseMCPConfig(string(encoded))
}

// ListMCPServers returns available MCP server names and their commands.
func ListMCPServers(workDir string) []map[string]string {
	configJSON, _, err := LoadMCPConfigWithPaths(workDir, configuredMCPConfigPaths())
	if err != nil {
		return nil
	}
	if configJSON == "" {
		return nil
	}
	cfg, err := ParseMCPConfig(configJSON)
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	servers := make([]map[string]string, 0, len(names))
	mcpConnectionsMu.RLock()
	defer mcpConnectionsMu.RUnlock()
	for _, name := range names {
		srv := cfg.MCPServers[name]
		status := "stopped"
		if conn, ok := mcpConnections[name]; ok && conn.Running {
			status = "running"
		} else {
			mcpClientsMu.RLock()
			if client, connected := mcpClients[name]; connected && client.isRunning() {
				status = "running"
			}
			mcpClientsMu.RUnlock()
		}
		servers = append(servers, map[string]string{
			"name":    name,
			"command": srv.Command + " " + strings.Join(srv.Args, " "),
			"status":  status,
		})
	}
	return servers
}

// StartMCPServer starts an MCP server process.
func StartMCPServer(name string, workDir string) (string, error) {
	return StartMCPServerWithOptions(name, workDir, DefaultMCPExecutionOptions(context.Background(), workDir))
}

// StartMCPServerWithOptions starts one configured server through the explicit
// long-lived process contract. A zero options value fails before process start.
func StartMCPServerWithOptions(name string, workDir string, opts MCPExecutionOptions) (string, error) {
	configJSON, _, err := LoadMCPConfigWithPaths(workDir, configuredMCPConfigPaths())
	if err != nil {
		return "", err
	}
	cfg, err := ParseMCPConfig(configJSON)
	if err != nil {
		return "", err
	}

	srv, ok := cfg.MCPServers[name]
	if !ok {
		return "", fmt.Errorf("MCP server '%s' not found in config", name)
	}
	if strings.EqualFold(strings.TrimSpace(srv.Type), "http") {
		return "", errors.New("HTTP MCP servers are connected by a run-owned runtime; start is not applicable")
	}

	process, _, err := startMCPProcess(opts, srv.Command, srv.Args, workDir, srv.Env)
	if err != nil {
		return "", fmt.Errorf("start MCP server '%s': %w", name, err)
	}
	_ = process.Stdin().Close()

	connection := &MCPConnection{
		Name: name, Config: cloneMCPServerConfig(srv), Process: process.Command(), Running: true, supervised: process,
	}
	go drainStandaloneMCPOutput(connection, process.Stdout())
	go drainStandaloneMCPOutput(connection, process.Stderr())
	mcpConnectionsMu.Lock()
	old := mcpConnections[name]
	mcpConnections[name] = connection
	mcpConnectionsMu.Unlock()
	if old != nil && old.supervised != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = old.supervised.Stop(stopCtx)
		cancel()
	}
	go func(expected *MCPConnection) {
		<-process.Done()
		mcpConnectionsMu.Lock()
		if mcpConnections[name] == expected {
			expected.Running = false
		}
		mcpConnectionsMu.Unlock()
	}(connection)

	return fmt.Sprintf("MCP server '%s' started (PID %d)", name, process.PID()), nil
}

// StopMCPServer stops a running MCP server.
func StopMCPServer(name string) string {
	mcpConnectionsMu.Lock()
	conn, ok := mcpConnections[name]
	if !ok || !conn.Running {
		mcpConnectionsMu.Unlock()
		return fmt.Sprintf("MCP server '%s' is not running", name)
	}
	conn.Running = false
	process := conn.supervised
	mcpConnectionsMu.Unlock()
	if process != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = process.Stop(ctx)
		cancel()
	}
	return fmt.Sprintf("MCP server '%s' stopped", name)
}

func cloneMCPServerConfig(config MCPServerConfig) MCPServerConfig {
	return MCPServerConfig{
		Type: config.Type, Command: config.Command,
		Args: append([]string(nil), config.Args...),
		Env:  cloneMCPEnvironment(config.Env),
		URL:  config.URL, Headers: cloneMCPEnvironment(config.Headers),
	}
}

func cloneMCPEnvironment(environment map[string]string) map[string]string {
	cloned := make(map[string]string, len(environment))
	for name, value := range environment {
		cloned[name] = value
	}
	return cloned
}

func mcpEnvironmentSpec(environment map[string]string) (sandbox.EnvironmentSpec, error) {
	return bashEnvironmentSpec(cloneMCPEnvironment(environment))
}

func drainStandaloneMCPOutput(connection *MCPConnection, reader io.Reader) {
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	written, _ := io.CopyN(io.Discard, reader, maxMCPDiagnosticBytes+1)
	if written <= maxMCPDiagnosticBytes || connection == nil || connection.supervised == nil {
		return
	}
	mcpConnectionsMu.Lock()
	if current := mcpConnections[connection.Name]; current == connection {
		connection.Running = false
	}
	mcpConnectionsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = connection.supervised.Stop(ctx)
	cancel()
}

// ── Sub-Agent System ──

// SubAgent represents a spawned sub-agent for parallel work.
type SubAgent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Task   string `json:"task"`
	Status string `json:"status"` // "running", "completed", "failed"
	Result string `json:"result"`
}

var subAgents = make(map[string]*SubAgent)
var subAgentCounter = 0

// SpawnSubAgent creates a conceptual sub-agent (executed sequentially in current impl).
func SpawnSubAgent(name, task string) *SubAgent {
	subAgentCounter++
	id := fmt.Sprintf("agent-%d", subAgentCounter)
	agent := &SubAgent{
		ID: id, Name: name, Task: task, Status: "running",
	}
	subAgents[id] = agent
	return agent
}

// CompleteSubAgent marks a sub-agent as done.
func CompleteSubAgent(id, result string) {
	if a, ok := subAgents[id]; ok {
		a.Status = "completed"
		a.Result = result
	}
}

// ListSubAgents returns all sub-agents.
func ListSubAgents() []*SubAgent {
	var result []*SubAgent
	for _, a := range subAgents {
		result = append(result, a)
	}
	return result
}
