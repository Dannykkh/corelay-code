package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// MCPResource represents an MCP resource.
type MCPResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// MCPPrompt represents an MCP prompt template.
type MCPPrompt struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Arguments   []MCPArgDef `json:"arguments,omitempty"`
}

// MCPArgDef is a prompt argument definition.
type MCPArgDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// MCPServerStatus holds the health status of an MCP server.
type MCPServerStatus struct {
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Tools     int       `json:"tools"`
	Resources int       `json:"resources"`
	Uptime    string    `json:"uptime,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

// ── Enhanced MCP Client Methods ──

// ListResources discovers available resources from the MCP server.
func (c *MCPClient) ListResources() ([]MCPResource, error) {
	resp, err := c.call("resources/list", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Resources []MCPResource `json:"resources"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return result.Resources, nil
}

// ReadResource reads the content of a resource by URI.
func (c *MCPClient) ReadResource(uri string) (string, error) {
	resp, err := c.call("resources/read", map[string]string{"uri": uri})
	if err != nil {
		return "", err
	}
	var result struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return string(resp), nil
	}
	if len(result.Contents) > 0 {
		return result.Contents[0].Text, nil
	}
	return "", nil
}

// ListPrompts discovers available prompt templates.
func (c *MCPClient) ListPrompts() ([]MCPPrompt, error) {
	resp, err := c.call("prompts/list", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Prompts []MCPPrompt `json:"prompts"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return result.Prompts, nil
}

// GetPrompt retrieves a specific prompt with arguments.
func (c *MCPClient) GetPrompt(name string, args map[string]string) (string, error) {
	resp, err := c.call("prompts/get", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var result struct {
		Messages []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return string(resp), nil
	}
	var texts []string
	for _, m := range result.Messages {
		texts = append(texts, m.Content.Text)
	}
	if len(texts) > 0 {
		return texts[0], nil
	}
	return "", nil
}

// IsAlive checks if the MCP server process is still running.
func (c *MCPClient) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return false
	}
	// Check if process exited
	if c.cmd.ProcessState != nil {
		c.running = false
		return false
	}
	return true
}

// ── Enhanced MCP Manager ──

// MCPManager manages MCP server lifecycle with health checks and reconnection.
type MCPManager struct {
	mu       sync.RWMutex
	clients  map[string]*MCPClient
	configs  map[string]MCPServerDef
	workDir  string
	statuses map[string]*MCPServerStatus
}

// MCPServerDef from config.
type MCPServerDef struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// NewMCPManager creates a manager.
func NewMCPManager() *MCPManager {
	return &MCPManager{
		clients:  make(map[string]*MCPClient),
		configs:  make(map[string]MCPServerDef),
		statuses: make(map[string]*MCPServerStatus),
	}
}

// ConnectAll connects to all servers from config.
func (m *MCPManager) ConnectAll(workDir string) int {
	m.mu.Lock()
	m.workDir = workDir
	m.mu.Unlock()

	configJSON, _, loadErr := LoadMCPConfigWithPaths(workDir, configuredMCPConfigPaths())
	if loadErr != nil {
		log.Printf("[MCP Manager] Config error: %v", loadErr)
		return 0
	}
	if configJSON == "" {
		return 0
	}

	cfg, err := ParseMCPConfig(configJSON)
	if err != nil {
		log.Printf("[MCP Manager] Config error: %v", err)
		return 0
	}

	connected := 0
	for name, srv := range cfg.MCPServers {
		m.mu.Lock()
		m.configs[name] = MCPServerDef{
			Type: srv.Type, Command: srv.Command,
			Args: srv.Args,
			Env:  srv.Env,
			URL:  srv.URL, Headers: srv.Headers,
		}
		m.mu.Unlock()

		if err := m.connect(name); err != nil {
			log.Printf("[MCP Manager] Failed '%s': %v", name, err)
			m.mu.Lock()
			m.statuses[name] = &MCPServerStatus{
				Name: name, Connected: false, LastError: err.Error(),
			}
			m.mu.Unlock()
		} else {
			connected++
		}
	}
	return connected
}

func (m *MCPManager) connect(name string) error {
	m.mu.RLock()
	def, ok := m.configs[name]
	workDir := m.workDir
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown server: %s", name)
	}

	var client *MCPClient
	var err error
	if strings.EqualFold(strings.TrimSpace(def.Type), "http") {
		client, err = NewMCPRemoteClientWithOptions(name, def.URL, def.Headers, DefaultMCPExecutionOptions(context.Background(), workDir))
	} else {
		client, err = NewMCPClient(name, def.Command, def.Args, workDir, def.Env)
	}
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.clients[name] = client
	m.statuses[name] = &MCPServerStatus{
		Name:      name,
		Connected: true,
		Tools:     len(client.Tools()),
		StartedAt: time.Now(),
	}
	m.mu.Unlock()
	return nil
}

// Reconnect restarts a specific server.
func (m *MCPManager) Reconnect(name string) error {
	m.mu.RLock()
	old, hasOld := m.clients[name]
	m.mu.RUnlock()

	if hasOld {
		old.Close()
	}
	return m.connect(name)
}

// HealthCheck checks all servers and reconnects dead ones.
func (m *MCPManager) HealthCheck() {
	m.mu.RLock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	m.mu.RUnlock()

	for _, name := range names {
		m.mu.RLock()
		client := m.clients[name]
		m.mu.RUnlock()

		if client != nil && !client.IsAlive() {
			log.Printf("[MCP Manager] '%s' died, reconnecting...", name)
			if err := m.Reconnect(name); err != nil {
				log.Printf("[MCP Manager] Reconnect '%s' failed: %v", name, err)
				m.mu.Lock()
				m.statuses[name].Connected = false
				m.statuses[name].LastError = err.Error()
				m.mu.Unlock()
			}
		}
	}
}

// GetAllTools returns tools from all connected servers.
func (m *MCPManager) GetAllTools() []MCPTool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []MCPTool
	for _, client := range m.clients {
		if client.IsAlive() {
			all = append(all, client.Tools()...)
		}
	}
	return all
}

// CallTool routes a tool call to the correct server.
func (m *MCPManager) CallTool(toolName string, args json.RawMessage) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, client := range m.clients {
		for _, t := range client.Tools() {
			if t.Name == toolName {
				return client.CallTool(toolName, args)
			}
		}
	}
	return fmt.Sprintf("MCP tool '%s' not found", toolName), true
}

// Statuses returns health info for all servers.
func (m *MCPManager) Statuses() []MCPServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []MCPServerStatus
	for _, s := range m.statuses {
		status := *s
		if s.Connected && !s.StartedAt.IsZero() {
			status.Uptime = time.Since(s.StartedAt).Truncate(time.Second).String()
		}
		result = append(result, status)
	}
	return result
}

// DisconnectAll closes all MCP servers.
func (m *MCPManager) DisconnectAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, client := range m.clients {
		client.Close()
		delete(m.clients, name)
	}
}
