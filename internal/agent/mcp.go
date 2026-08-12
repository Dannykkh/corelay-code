package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

// MCPServerConfig represents an MCP server from .mcp.json
type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
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
		if strings.TrimSpace(server.Command) == "" {
			return nil, fmt.Errorf("parse .mcp.json: server %q command is empty", name)
		}
		server.Args = append([]string(nil), server.Args...)
		server.Env = cloneMCPEnvironment(server.Env)
		cfg.MCPServers[name] = server
	}
	return &cfg, nil
}

// ListMCPServers returns available MCP server names and their commands.
func ListMCPServers(workDir string) []map[string]string {
	configJSON := LoadMCPConfig(workDir)
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
	configJSON := LoadMCPConfig(workDir)
	cfg, err := ParseMCPConfig(configJSON)
	if err != nil {
		return "", err
	}

	srv, ok := cfg.MCPServers[name]
	if !ok {
		return "", fmt.Errorf("MCP server '%s' not found in config", name)
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
		Command: config.Command,
		Args:    append([]string(nil), config.Args...),
		Env:     cloneMCPEnvironment(config.Env),
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
