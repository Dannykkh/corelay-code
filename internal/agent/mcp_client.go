package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// ── JSON-RPC 2.0 Types ──

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── MCP Tool from server ──

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type MCPToolResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ── MCP Client (stdio transport) ──

type MCPClient struct {
	mu      sync.Mutex
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	nextID  atomic.Int64
	tools   []MCPTool
	running bool
}

// NewMCPClient creates and starts an MCP server via stdio.
func NewMCPClient(name, command string, args []string, workDir string, env map[string]string) (*MCPClient, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = workDir
	cmd.Stderr = os.Stderr

	// Set environment
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server '%s': %w", name, err)
	}

	client := &MCPClient{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 256*1024),
		running: true,
	}

	// Initialize the MCP session
	if err := client.initialize(); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize MCP '%s': %w", name, err)
	}

	// Discover tools
	if err := client.discoverTools(); err != nil {
		log.Printf("[MCP] Warning: failed to list tools for '%s': %v", name, err)
	}

	log.Printf("[MCP] Connected to '%s' — %d tools available", name, len(client.tools))
	return client, nil
}

func (c *MCPClient) initialize() error {
	resp, err := c.call("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name":    "aniclew",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return err
	}

	// Send initialized notification
	c.notify("notifications/initialized", nil)

	_ = resp
	return nil
}

func (c *MCPClient) discoverTools() error {
	resp, err := c.call("tools/list", nil)
	if err != nil {
		return err
	}

	var result struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return err
	}

	c.tools = result.Tools
	return nil
}

// Tools returns the available tools from this MCP server.
func (c *MCPClient) Tools() []MCPTool {
	return c.tools
}

// CallTool invokes a tool on the MCP server.
func (c *MCPClient) CallTool(name string, args json.RawMessage) (string, bool) {
	resp, err := c.call("tools/call", map[string]interface{}{
		"name":      name,
		"arguments": json.RawMessage(args),
	})
	if err != nil {
		return fmt.Sprintf("MCP tool error: %v", err), true
	}

	var result MCPToolResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return string(resp), false
	}

	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}

	output := ""
	if len(texts) > 0 {
		output = texts[0]
		for _, t := range texts[1:] {
			output += "\n" + t
		}
	} else {
		output = string(resp)
	}

	return output, result.IsError
}

// Close shuts down the MCP server.
func (c *MCPClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *MCPClient) closeLocked() {
	if !c.running && c.cmd == nil && c.stdin == nil && c.stdout == nil {
		return
	}
	c.running = false
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if c.cmd != nil {
		_ = c.cmd.Wait()
		c.cmd = nil
	}
	c.stdout = nil
	log.Printf("[MCP] Disconnected from '%s'", c.name)
}

// ── Internal JSON-RPC communication ──

const mcpCallTimeout = 30 * time.Second

func (c *MCPClient) call(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running || c.stdin == nil || c.stdout == nil {
		return nil, fmt.Errorf("MCP server '%s' is not running", c.name)
	}

	id := c.nextID.Add(1)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, _ := json.Marshal(req)
	if _, err := fmt.Fprintf(c.stdin, "%s\n", data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	// Read response with timeout
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := c.stdout.ReadBytes('\n')
		ch <- readResult{line, err}
	}()

	select {
	case result := <-ch:
		if result.err != nil {
			return nil, fmt.Errorf("read: %w", result.err)
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(result.line, &resp); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("RPC error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil

	case <-time.After(mcpCallTimeout):
		log.Printf("[MCP] Call '%s' timed out after %v — closing server", method, mcpCallTimeout)
		c.closeLocked()
		return nil, fmt.Errorf("MCP call '%s' timed out after %v", method, mcpCallTimeout)
	}
}

func (c *MCPClient) notify(method string, params interface{}) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(req)
	fmt.Fprintf(c.stdin, "%s\n", data)
}

// ── MCP Manager (manages multiple MCP servers) ──

var (
	mcpClients   = make(map[string]*MCPClient)
	mcpClientsMu sync.RWMutex
)

// ConnectMCPServers reads .mcp.json and connects to all configured servers.
func ConnectMCPServers(workDir string) (int, error) {
	configJSON := LoadMCPConfig(workDir)
	if configJSON == "" {
		return 0, nil
	}

	cfg, err := ParseMCPConfig(configJSON)
	if err != nil {
		return 0, err
	}

	connected := 0
	for name, srv := range cfg.MCPServers {
		client, err := NewMCPClient(name, srv.Command, srv.Args, workDir, srv.Env)
		if err != nil {
			log.Printf("[MCP] Failed to connect '%s': %v", name, err)
			continue
		}
		mcpClientsMu.Lock()
		mcpClients[name] = client
		mcpClientsMu.Unlock()
		connected++
	}

	return connected, nil
}

// GetMCPTools returns all tools from all connected MCP servers.
func GetMCPTools() []MCPTool {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()

	var all []MCPTool
	for _, client := range mcpClients {
		all = append(all, client.Tools()...)
	}
	return all
}

// CallMCPTool finds which server owns a tool and calls it.
func CallMCPTool(toolName string, args json.RawMessage) (string, bool) {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()

	for _, client := range mcpClients {
		for _, t := range client.Tools() {
			if t.Name == toolName {
				return client.CallTool(toolName, args)
			}
		}
	}
	return fmt.Sprintf("MCP tool '%s' not found", toolName), true
}

// DisconnectAllMCP closes all MCP server connections.
func DisconnectAllMCP() {
	mcpClientsMu.Lock()
	defer mcpClientsMu.Unlock()
	for name, client := range mcpClients {
		client.Close()
		delete(mcpClients, name)
		_ = name
	}
}
