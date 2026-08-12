package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	defaultMCPCallTimeout   = 30 * time.Second
	defaultMCPFrameBytes    = 1024 * 1024
	defaultMCPJSONDepth     = 64
	maxMCPTools             = 1024
	maxMCPPendingCalls      = 256
	maxMCPUnsolicitedFrames = 256
	maxMCPMethodBytes       = 256
	maxMCPProtocolTextBytes = 4096
	maxMCPDiagnosticBytes   = 4 * 1024 * 1024
)

// MCPExecutionOptions binds process start and all initial RPCs to one context,
// immutable runner/policy pair, and bounded framing contract. The zero value
// is invalid. Disabled execution is accepted only by the named legacy host
// adapter.
type MCPExecutionOptions struct {
	Context       context.Context
	Runner        processsupervisor.Runner
	Policy        sandbox.Policy
	ObserveStart  func(processsupervisor.Report)
	CallTimeout   time.Duration
	MaxFrameBytes int
	MaxJSONDepth  int
}

// DefaultMCPExecutionOptions selects the truthful streaming adapter for the
// current platform. A workspace argument is required to enable adapters that
// provide filesystem isolation. The variadic shape preserves source
// compatibility with the former fail-closed no-workspace constructor; omitting
// the workspace still returns an unavailable runner and never host execution.
func DefaultMCPExecutionOptions(ctx context.Context, workDirs ...string) MCPExecutionOptions {
	if len(workDirs) != 1 || strings.TrimSpace(workDirs[0]) == "" {
		return MCPExecutionOptions{
			Context: ctx,
			Runner:  processsupervisor.NewUnavailableRunner("secure MCP execution requires one workspace path"),
			Policy:  sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
		}
	}
	runner := processsupervisor.NewAutoRunner()
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
		policy.Workspace = workDirs[0]
		policy.WorkspaceAccess = sandbox.WorkspaceReadWrite
	}
	return MCPExecutionOptions{Context: ctx, Runner: runner, Policy: policy}
}

// LegacyDisabledMCPExecutionOptions is the only compatibility adapter that
// selects unconfined host execution. New production call sites should inject
// an isolating Runner and a Required or Preferred policy instead.
func LegacyDisabledMCPExecutionOptions(ctx context.Context) MCPExecutionOptions {
	return MCPExecutionOptions{
		Context: ctx,
		Runner:  processsupervisor.NewHostRunner(),
		Policy: sandbox.Policy{
			Enforcement: sandbox.EnforcementDisabled,
			Required: sandbox.Capabilities{
				EnvironmentFiltering: true,
				Timeouts:             true,
			},
		},
	}
}

func normalizeMCPExecutionOptions(opts MCPExecutionOptions) MCPExecutionOptions {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultMCPCallTimeout
	}
	if opts.MaxFrameBytes <= 0 {
		opts.MaxFrameBytes = defaultMCPFrameBytes
	}
	if opts.MaxJSONDepth <= 0 {
		opts.MaxJSONDepth = defaultMCPJSONDepth
	}
	return opts
}

func startMCPProcess(
	opts MCPExecutionOptions,
	command string,
	args []string,
	workDir string,
	environment map[string]string,
) (*processsupervisor.Process, processsupervisor.Report, error) {
	opts = normalizeMCPExecutionOptions(opts)
	if opts.Runner == nil {
		return nil, processsupervisor.Report{}, fmt.Errorf("MCP process runner is not configured")
	}
	if opts.Policy.Enforcement == "" {
		return nil, processsupervisor.Report{}, fmt.Errorf("MCP process policy is not configured")
	}
	if opts.Policy.Enforcement == sandbox.EnforcementDisabled {
		host, ok := opts.Runner.(*processsupervisor.HostRunner)
		if !ok || host == nil {
			return nil, processsupervisor.Report{}, fmt.Errorf("disabled MCP execution requires the explicit legacy host adapter")
		}
	}
	environmentSpec, err := mcpEnvironmentSpec(environment)
	if err != nil {
		return nil, processsupervisor.Report{}, fmt.Errorf("invalid MCP environment: %s", sanitizeMCPProtocolText(err.Error()))
	}
	process, report := opts.Runner.Start(opts.Context, opts.Policy, processsupervisor.Spec{
		Executable:  command,
		Args:        append([]string(nil), args...),
		Dir:         workDir,
		Environment: environmentSpec,
	})
	if opts.ObserveStart != nil {
		opts.ObserveStart(report)
	}
	if process == nil || !report.Started {
		detail := strings.TrimSpace(report.Detail)
		if detail == "" {
			detail = "process did not start"
		}
		return nil, report, fmt.Errorf("MCP process unavailable: %s", sanitizeMCPProtocolText(detail))
	}
	return process, report, nil
}

// JSON-RPC 2.0 types.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP tool types.
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

type mcpRPCResult struct {
	result json.RawMessage
	err    error
}

// MCPClient is a concurrent stdio JSON-RPC client. One bounded reader owns
// stdout and dispatches responses to ID-keyed pending calls.
type MCPClient struct {
	mu         sync.RWMutex
	writeMu    sync.Mutex
	name       string
	executorID string
	executable string    // canonical identity only; argv and environment remain process-owned
	cmd        *exec.Cmd // compatibility view only; spawning lives in processsupervisor
	process    *processsupervisor.Process
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	nextID     atomic.Int64
	tools      []MCPTool
	running    bool

	pendingMu   sync.Mutex
	pending     map[int64]chan mcpRPCResult
	unsolicited atomic.Uint32
	done        chan struct{}
	closeOnce   sync.Once

	callTimeout   time.Duration
	maxFrameBytes int
	maxJSONDepth  int
}

// effectiveCallTimeout and the framing accessors keep manually assembled
// clients bounded as well as constructor-created clients. Positive explicit
// values are immutable policy inputs and are never relaxed here.
func (c *MCPClient) effectiveCallTimeout() time.Duration {
	if c == nil || c.callTimeout <= 0 {
		return defaultMCPCallTimeout
	}
	return c.callTimeout
}

func (c *MCPClient) effectiveMaxFrameBytes() int {
	if c == nil || c.maxFrameBytes <= 0 {
		return defaultMCPFrameBytes
	}
	return c.maxFrameBytes
}

func (c *MCPClient) effectiveMaxJSONDepth() int {
	if c == nil || c.maxJSONDepth <= 0 {
		return defaultMCPJSONDepth
	}
	return c.maxJSONDepth
}

// NewMCPClient uses the platform's secure streaming default. Explicit legacy
// host execution remains available only through LegacyDisabledMCPExecutionOptions.
func NewMCPClient(name, command string, args []string, workDir string, env map[string]string) (*MCPClient, error) {
	return NewMCPClientWithOptions(name, command, args, workDir, env, DefaultMCPExecutionOptions(context.Background(), workDir))
}

func NewMCPClientWithOptions(
	name, command string,
	args []string,
	workDir string,
	environment map[string]string,
	opts MCPExecutionOptions,
) (*MCPClient, error) {
	return newMCPClientWithOptions(name, command, args, workDir, environment, opts, false)
}

func newMCPClientWithOptions(
	name, command string,
	args []string,
	workDir string,
	environment map[string]string,
	opts MCPExecutionOptions,
	requireToolDiscovery bool,
) (*MCPClient, error) {
	return newMCPClientWithInitializationContext(
		name, command, args, workDir, environment, opts, opts.Context, requireToolDiscovery,
	)
}

func newMCPClientWithInitializationContext(
	name, command string,
	args []string,
	workDir string,
	environment map[string]string,
	opts MCPExecutionOptions,
	initializationContext context.Context,
	requireToolDiscovery bool,
) (*MCPClient, error) {
	opts = normalizeMCPExecutionOptions(opts)
	if initializationContext == nil {
		initializationContext = opts.Context
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("MCP server name is empty")
	}
	executable, err := resolveMCPExecutable(command)
	if err != nil {
		return nil, fmt.Errorf("start MCP server %q: executable identity is unavailable", name)
	}
	process, _, err := startMCPProcess(opts, executable, args, workDir, environment)
	if err != nil {
		return nil, fmt.Errorf("start MCP server %q: %w", name, err)
	}

	client := &MCPClient{
		name:          name,
		executorID:    newMCPExecutorID(),
		executable:    executable,
		cmd:           process.Command(),
		process:       process,
		stdin:         process.Stdin(),
		stdout:        bufio.NewReaderSize(process.Stdout(), 64*1024),
		running:       true,
		pending:       make(map[int64]chan mcpRPCResult),
		done:          make(chan struct{}),
		callTimeout:   opts.CallTimeout,
		maxFrameBytes: opts.MaxFrameBytes,
		maxJSONDepth:  opts.MaxJSONDepth,
	}
	go client.drainStderr(process.Stderr())
	go client.readLoop(client.stdout)

	if err := client.initialize(initializationContext); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize MCP %q: %w", name, err)
	}
	if err := client.discoverTools(initializationContext); err != nil {
		if requireToolDiscovery {
			client.Close()
			return nil, fmt.Errorf("discover MCP %q tools: %w", name, err)
		}
		log.Printf("[MCP] Warning: failed to list tools for %q: %v", name, err)
	}
	log.Printf("[MCP] Connected to %q — %d tools available", name, len(client.Tools()))
	return client, nil
}

func (c *MCPClient) initialize(ctx context.Context) error {
	if _, err := c.callContext(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name": "corelaycode", "version": "1.0.0",
		},
	}); err != nil {
		return err
	}
	return c.notifyContext(ctx, "notifications/initialized", nil)
}

func (c *MCPClient) discoverTools(ctx context.Context) error {
	response, err := c.callContext(ctx, "tools/list", nil)
	if err != nil {
		return err
	}
	var result struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return fmt.Errorf("invalid tools/list result")
	}
	if len(result.Tools) > maxMCPTools {
		return fmt.Errorf("tools/list exceeds %d tools", maxMCPTools)
	}
	maxFrameBytes := c.effectiveMaxFrameBytes()
	maxJSONDepth := c.effectiveMaxJSONDepth()
	for i := range result.Tools {
		tool := &result.Tools[i]
		if strings.TrimSpace(tool.Name) == "" || len(tool.Name) > maxMCPMethodBytes || strings.IndexByte(tool.Name, 0) >= 0 {
			return fmt.Errorf("tools/list contains invalid tool name")
		}
		if len(tool.InputSchema) == 0 || len(tool.InputSchema) > maxFrameBytes || !json.Valid(tool.InputSchema) {
			return fmt.Errorf("tools/list contains invalid input schema")
		}
		if err := validateMCPJSONDepth(tool.InputSchema, maxJSONDepth); err != nil {
			return fmt.Errorf("tools/list input schema exceeds nesting limit")
		}
		tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
	}
	c.mu.Lock()
	c.tools = append([]MCPTool(nil), result.Tools...)
	c.mu.Unlock()
	return nil
}

func (c *MCPClient) Tools() []MCPTool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tools := make([]MCPTool, len(c.tools))
	copy(tools, c.tools)
	for i := range tools {
		tools[i].InputSchema = append(json.RawMessage(nil), tools[i].InputSchema...)
	}
	return tools
}

func (c *MCPClient) isRunning() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	running := c.running
	c.mu.RUnlock()
	if !running || c.done == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *MCPClient) CallTool(name string, args json.RawMessage) (string, bool) {
	return c.CallToolContext(context.Background(), name, args)
}

func (c *MCPClient) CallToolContext(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if len(args) > c.effectiveMaxFrameBytes() || !json.Valid(args) || validateMCPJSONDepth(args, c.effectiveMaxJSONDepth()) != nil {
		return "MCP tool error: invalid arguments", true
	}
	response, err := c.callContext(ctx, "tools/call", map[string]interface{}{
		"name": name, "arguments": json.RawMessage(args),
	})
	if err != nil {
		return fmt.Sprintf("MCP tool error: %s", sanitizeMCPProtocolText(err.Error())), true
	}
	var result MCPToolResult
	if err := json.Unmarshal(response, &result); err != nil {
		return string(response), false
	}
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if content.Type == "text" {
			texts = append(texts, content.Text)
		}
	}
	if len(texts) == 0 {
		return string(response), result.IsError
	}
	return strings.Join(texts, "\n"), result.IsError
}

func (c *MCPClient) Close() {
	c.shutdown(fmt.Errorf("MCP client closed"), true)
}

func (c *MCPClient) shutdown(reason error, stop bool) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.running = false
		stdin := c.stdin
		process := c.process
		c.stdin = nil
		c.stdout = nil
		c.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}

		c.pendingMu.Lock()
		for id, waiter := range c.pending {
			delete(c.pending, id)
			select {
			case waiter <- mcpRPCResult{err: reason}:
			default:
			}
		}
		c.pendingMu.Unlock()
		close(c.done)
		if stop && process != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = process.Stop(ctx)
			cancel()
		}
		if process != nil {
			process.CloseIO()
		}
		log.Printf("[MCP] Disconnected from %q", c.name)
	})
}

func (c *MCPClient) call(method string, params interface{}) (json.RawMessage, error) {
	return c.callContext(context.Background(), method, params)
}

func (c *MCPClient) callContext(parent context.Context, method string, params interface{}) (json.RawMessage, error) {
	if strings.TrimSpace(method) == "" || len(method) > maxMCPMethodBytes || strings.IndexByte(method, 0) >= 0 {
		return nil, fmt.Errorf("invalid MCP method")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, c.effectiveCallTimeout())
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id := c.nextID.Add(1)
	if id <= 0 {
		return nil, fmt.Errorf("MCP request ID exhausted")
	}
	frame, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil || len(frame) > c.effectiveMaxFrameBytes() || validateMCPJSONDepth(frame, c.effectiveMaxJSONDepth()) != nil {
		return nil, fmt.Errorf("invalid MCP request")
	}
	waiter := make(chan mcpRPCResult, 1)
	if err := c.addPending(id, waiter); err != nil {
		return nil, err
	}

	if err := c.writeFrameContext(ctx, frame); err != nil {
		c.removePending(id)
		return nil, err
	}
	select {
	case result := <-waiter:
		return result.result, result.err
	case <-c.done:
		// A server may write a valid response and then immediately exit. The
		// reader publishes the buffered response before it closes done, so when
		// both cases are ready prefer the response instead of spuriously
		// reporting the runtime as unavailable.
		select {
		case result := <-waiter:
			return result.result, result.err
		default:
		}
		c.removePending(id)
		return nil, fmt.Errorf("MCP server %q is not running", c.name)
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	}
}

func (c *MCPClient) notify(method string, params interface{}) {
	_ = c.notifyContext(context.Background(), method, params)
}

func (c *MCPClient) notifyContext(parent context.Context, method string, params interface{}) error {
	if strings.TrimSpace(method) == "" || len(method) > maxMCPMethodBytes {
		return fmt.Errorf("invalid MCP notification method")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, c.effectiveCallTimeout())
	defer cancel()
	frame, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil || len(frame) > c.effectiveMaxFrameBytes() || validateMCPJSONDepth(frame, c.effectiveMaxJSONDepth()) != nil {
		return fmt.Errorf("invalid MCP notification")
	}
	return c.writeFrameContext(ctx, frame)
}

func (c *MCPClient) addPending(id int64, waiter chan mcpRPCResult) error {
	c.mu.RLock()
	running := c.running
	c.mu.RUnlock()
	if !running {
		return fmt.Errorf("MCP server %q is not running", c.name)
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	select {
	case <-c.done:
		return fmt.Errorf("MCP server %q is not running", c.name)
	default:
	}
	if len(c.pending) >= maxMCPPendingCalls {
		return fmt.Errorf("MCP pending call limit reached")
	}
	c.pending[id] = waiter
	return nil
}

func (c *MCPClient) removePending(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *MCPClient) writeFrameContext(ctx context.Context, frame []byte) error {
	result := make(chan error, 1)
	go func() { result <- c.writeFrame(frame) }()
	select {
	case err := <-result:
		return err
	case <-c.done:
		return fmt.Errorf("MCP server %q is not running", c.name)
	case <-ctx.Done():
		// A blocked or partially completed write has ambiguous delivery. Closing
		// the generation prevents a late ghost request from executing.
		c.shutdown(fmt.Errorf("MCP write canceled"), true)
		return ctx.Err()
	}
}

func (c *MCPClient) writeFrame(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.RLock()
	stdin := c.stdin
	running := c.running
	c.mu.RUnlock()
	if !running || stdin == nil {
		return fmt.Errorf("MCP server %q is not running", c.name)
	}
	encoded := make([]byte, len(frame)+1)
	copy(encoded, frame)
	encoded[len(frame)] = '\n'
	if _, err := stdin.Write(encoded); err != nil {
		c.shutdown(fmt.Errorf("MCP transport write failed"), true)
		return fmt.Errorf("MCP transport write failed")
	}
	return nil
}

func (c *MCPClient) readLoop(reader *bufio.Reader) {
	for {
		frame, err := readMCPFrame(reader, c.effectiveMaxFrameBytes())
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.shutdown(err, true)
			} else {
				// EOF is not proof that the process (or its descendants) exited: a
				// server can close stdout and remain alive. Stop the supervised tree
				// so a broken transport cannot orphan a long-lived payload.
				c.shutdown(fmt.Errorf("MCP transport closed"), true)
			}
			return
		}
		if len(strings.TrimSpace(string(frame))) == 0 {
			continue
		}
		if err := c.dispatchFrame(frame); err != nil {
			c.shutdown(err, true)
			return
		}
	}
}

func (c *MCPClient) drainStderr(reader io.Reader) {
	written, _ := io.CopyN(io.Discard, reader, maxMCPDiagnosticBytes+1)
	if written > maxMCPDiagnosticBytes {
		c.shutdown(fmt.Errorf("MCP stderr limit exceeded"), true)
	}
}

func (c *MCPClient) dispatchFrame(frame []byte) error {
	if !json.Valid(frame) {
		return fmt.Errorf("invalid MCP JSON frame")
	}
	if err := validateMCPJSONDepth(frame, c.effectiveMaxJSONDepth()); err != nil {
		return err
	}
	if _, err := decodeUniqueJSONObject(frame); err != nil {
		return fmt.Errorf("invalid MCP response envelope")
	}
	var response jsonRPCResponse
	if err := json.Unmarshal(frame, &response); err != nil {
		return fmt.Errorf("invalid MCP response envelope")
	}
	if response.JSONRPC != "2.0" {
		return fmt.Errorf("invalid MCP JSON-RPC version")
	}
	if response.Method != "" {
		if len(response.Method) > maxMCPMethodBytes || strings.IndexByte(response.Method, 0) >= 0 {
			return fmt.Errorf("invalid MCP notification method")
		}
		// Server notifications are intentionally ignored. Server-initiated
		// requests are unsupported and cannot be mistaken for responses.
		if len(response.ID) == 0 || string(response.ID) == "null" {
			return c.observeUnsolicitedMCPFrame()
		}
		return fmt.Errorf("unsupported MCP server request")
	}
	id, err := parseMCPResponseID(response.ID)
	if err != nil {
		return err
	}
	if response.Error != nil && len(response.Result) != 0 {
		return fmt.Errorf("invalid MCP response with result and error")
	}
	if response.Error == nil && len(response.Result) == 0 {
		return fmt.Errorf("invalid MCP response without result or error")
	}
	result := mcpRPCResult{result: append(json.RawMessage(nil), response.Result...)}
	if response.Error != nil {
		result.err = fmt.Errorf("MCP RPC error %d: %s", response.Error.Code, sanitizeMCPProtocolText(response.Error.Message))
	}
	c.pendingMu.Lock()
	waiter := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if waiter != nil {
		waiter <- result
		return nil
	}
	// Unknown valid IDs can be late responses for timed-out calls; ignore them
	// without redirecting them to a newer generation.
	return c.observeUnsolicitedMCPFrame()
}

func (c *MCPClient) observeUnsolicitedMCPFrame() error {
	if c.unsolicited.Add(1) > maxMCPUnsolicitedFrames {
		return fmt.Errorf("MCP unsolicited frame limit exceeded")
	}
	return nil
}

func parseMCPResponseID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" || raw[0] == '"' {
		return 0, fmt.Errorf("invalid MCP response ID")
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid MCP response ID")
	}
	return id, nil
}

func readMCPFrame(reader *bufio.Reader, maximum int) ([]byte, error) {
	frame := make([]byte, 0, 4096)
	for {
		fragment, more, err := reader.ReadLine()
		if len(frame)+len(fragment) > maximum {
			return nil, fmt.Errorf("MCP frame exceeds %d bytes", maximum)
		}
		frame = append(frame, fragment...)
		if err != nil {
			return nil, err
		}
		if !more {
			return frame, nil
		}
	}
}

func validateMCPJSONDepth(data []byte, maximum int) error {
	depth := 0
	inString := false
	escaped := false
	for _, current := range data {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}
		switch current {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximum {
				return fmt.Errorf("MCP JSON exceeds nesting depth %d", maximum)
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("invalid MCP JSON nesting")
			}
		}
	}
	if inString || depth != 0 {
		return fmt.Errorf("invalid MCP JSON nesting")
	}
	return nil
}

func sanitizeMCPProtocolText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) > maxMCPProtocolTextBytes {
		value = value[:maxMCPProtocolTextBytes] + "..."
	}
	return value
}

// MCP manager and executor provenance.
var (
	mcpClients          = make(map[string]*MCPClient)
	mcpClientsMu        sync.RWMutex
	mcpLifecycleMu      sync.Mutex
	mcpExecutorSequence atomic.Uint64
)

type mcpToolBinding struct {
	ServerName string
	ExecutorID string
	Tool       MCPTool
}

func newMCPExecutorID() string {
	return fmt.Sprintf("mcp-executor-%x-%x", time.Now().UnixNano(), mcpExecutorSequence.Add(1))
}

func ConnectMCPServers(workDir string) (int, error) {
	return ConnectMCPServersWithOptions(workDir, DefaultMCPExecutionOptions(context.Background(), workDir))
}

func ConnectMCPServersWithOptions(workDir string, opts MCPExecutionOptions) (int, error) {
	mcpLifecycleMu.Lock()
	defer mcpLifecycleMu.Unlock()
	configJSON := LoadMCPConfig(workDir)
	if configJSON == "" {
		disconnectAllMCPUnlocked()
		return 0, nil
	}
	config, err := ParseMCPConfig(configJSON)
	if err != nil {
		disconnectAllMCPUnlocked()
		return 0, err
	}
	connected := 0
	names := make([]string, 0, len(config.MCPServers))
	for name := range config.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	configured := make(map[string]struct{}, len(names))
	for _, name := range names {
		configured[name] = struct{}{}
	}
	mcpClientsMu.Lock()
	obsolete := make([]*MCPClient, 0)
	for name, client := range mcpClients {
		if _, exists := configured[name]; !exists {
			delete(mcpClients, name)
			obsolete = append(obsolete, client)
		}
	}
	mcpClientsMu.Unlock()
	for _, client := range obsolete {
		client.Close()
	}
	failed := make([]string, 0)
	for _, name := range names {
		server := config.MCPServers[name]
		mcpClientsMu.Lock()
		old := mcpClients[name]
		delete(mcpClients, name)
		mcpClientsMu.Unlock()
		if old != nil {
			old.Close()
		}
		client, connectErr := NewMCPClientWithOptions(name, server.Command, server.Args, workDir, server.Env, opts)
		if connectErr != nil {
			log.Printf("[MCP] Failed to connect %q: %v", name, connectErr)
			failed = append(failed, name)
			continue
		}
		mcpClientsMu.Lock()
		mcpClients[name] = client
		mcpClientsMu.Unlock()
		connected++
	}
	if len(failed) > 0 {
		return connected, fmt.Errorf("secure MCP start failed for %d server(s): %s", len(failed), strings.Join(failed, ", "))
	}
	return connected, nil
}

func GetMCPTools() []MCPTool {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()
	var all []MCPTool
	for _, client := range mcpClients {
		all = append(all, client.Tools()...)
	}
	return all
}

func getMCPToolBindings() []mcpToolBinding {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()
	var bindings []mcpToolBinding
	for serverName, client := range mcpClients {
		for _, tool := range client.Tools() {
			bindings = append(bindings, mcpToolBinding{ServerName: serverName, ExecutorID: client.executorID, Tool: tool})
		}
	}
	return bindings
}

func mcpToolNameClaimed(toolName string) bool {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()
	for _, client := range mcpClients {
		for _, tool := range client.Tools() {
			if tool.Name == toolName {
				return true
			}
		}
	}
	return false
}

func CallMCPTool(toolName string, args json.RawMessage) (string, bool) {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()
	for _, client := range mcpClients {
		for _, tool := range client.Tools() {
			if tool.Name == toolName {
				return client.CallTool(toolName, args)
			}
		}
	}
	return fmt.Sprintf("MCP tool '%s' not found", toolName), true
}

func callMCPToolBound(toolName string, args json.RawMessage, expected toolExecutorIdentity) (string, bool) {
	mcpClientsMu.RLock()
	defer mcpClientsMu.RUnlock()
	client, exists := mcpClients[expected.Owner]
	if !exists || client.executorID == "" || client.executorID != expected.ExecutorID {
		return fmt.Sprintf("[IDENTITY BLOCKED] MCP executor for %q was replaced or removed", toolName), true
	}
	matches := 0
	for _, tool := range client.Tools() {
		if tool.Name != toolName {
			continue
		}
		matches++
		if toolSchemaDigest(tool.InputSchema) != expected.SchemaDigest {
			return fmt.Sprintf("[IDENTITY BLOCKED] MCP schema for %q changed", toolName), true
		}
	}
	if matches != 1 {
		return fmt.Sprintf("[IDENTITY BLOCKED] MCP executor for %q is ambiguous", toolName), true
	}
	return client.CallTool(toolName, args)
}

func DisconnectAllMCP() {
	mcpLifecycleMu.Lock()
	defer mcpLifecycleMu.Unlock()
	disconnectAllMCPUnlocked()
}

func disconnectAllMCPUnlocked() {
	mcpClientsMu.Lock()
	clients := make([]*MCPClient, 0, len(mcpClients))
	for name, client := range mcpClients {
		delete(mcpClients, name)
		clients = append(clients, client)
	}
	mcpClientsMu.Unlock()
	for _, client := range clients {
		client.Close()
	}
}
