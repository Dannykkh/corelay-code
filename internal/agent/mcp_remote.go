package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

const mcpRemoteProtocolVersion = "2025-06-18"

type mcpRemoteTransport struct {
	endpoint *url.URL
	headers  map[string]string
	client   *http.Client

	mu        sync.Mutex
	reconnect chan struct{}
	sessionID string
	running   atomic.Bool
}

type mcpRemoteError struct {
	retryable bool
	status    int
	err       error
}

func (e *mcpRemoteError) Error() string {
	if e == nil || e.err == nil {
		return "MCP remote transport failed"
	}
	return e.err.Error()
}

func newMCPRemoteClientWithInitializationContext(
	name, endpoint string,
	headers map[string]string,
	opts MCPExecutionOptions,
	initializationContext context.Context,
	requireToolDiscovery bool,
) (*MCPClient, error) {
	opts = normalizeMCPExecutionOptions(opts)
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("MCP server name is empty")
	}
	parsed, err := validateMCPRemoteEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	validatedHeaders, err := validateMCPRemoteHeaders(headers)
	if err != nil {
		return nil, err
	}
	if initializationContext == nil {
		initializationContext = opts.Context
	}
	remote := &mcpRemoteTransport{
		endpoint:  parsed,
		headers:   validatedHeaders,
		reconnect: make(chan struct{}, 1),
		client: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	remote.running.Store(true)
	client := &MCPClient{
		name: name, executorID: newMCPExecutorID(), executable: parsed.String(),
		remote: remote, running: true, pending: make(map[int64]chan mcpRPCResult),
		done: make(chan struct{}), callTimeout: opts.CallTimeout,
		maxFrameBytes: opts.MaxFrameBytes, maxJSONDepth: opts.MaxJSONDepth,
	}
	if err := client.initialize(initializationContext); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize MCP %q: %w", name, err)
	}
	if err := client.discoverTools(initializationContext); err != nil {
		if requireToolDiscovery {
			client.Close()
			return nil, fmt.Errorf("discover MCP %q tools: %w", name, err)
		}
	}
	return client, nil
}

func NewMCPRemoteClientWithOptions(name, endpoint string, headers map[string]string, opts MCPExecutionOptions) (*MCPClient, error) {
	return newMCPRemoteClientWithInitializationContext(name, endpoint, headers, opts, opts.Context, false)
}

func validateMCPRemoteEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("MCP HTTP endpoint is invalid")
	}
	return parsed, nil
}

func validateMCPRemoteHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) > 128 {
		return nil, errors.New("MCP HTTP header count exceeds 128")
	}
	validated := make(map[string]string, len(headers))
	for name, value := range headers {
		if http.CanonicalHeaderKey(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") || len(name) > 256 || len(value) > maxMCPProtocolTextBytes {
			return nil, errors.New("MCP HTTP header is invalid")
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "transfer-encoding", "connection", "upgrade":
			return nil, errors.New("MCP HTTP header is reserved")
		}
		validated[name] = value
	}
	return validated, nil
}

func (r *mcpRemoteTransport) close() {
	if r == nil {
		return
	}
	r.running.Store(false)
	r.mu.Lock()
	r.sessionID = ""
	r.mu.Unlock()
}

func (r *mcpRemoteTransport) invalidateSession() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sessionID = ""
	r.mu.Unlock()
}

func (r *mcpRemoteTransport) post(ctx context.Context, frame []byte) ([][]byte, error) {
	if r == nil || !r.running.Load() {
		return nil, errors.New("MCP remote server is not running")
	}
	r.mu.Lock()
	sessionID := r.sessionID
	r.mu.Unlock()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint.String(), bytes.NewReader(frame))
	if err != nil {
		return nil, &mcpRemoteError{err: errors.New("MCP remote request could not be created")}
	}
	for name, value := range r.headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", mcpRemoteProtocolVersion)
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	response, err := r.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &mcpRemoteError{err: errors.New("MCP remote request failed")}
	}
	defer response.Body.Close()
	if response.Header.Get("Mcp-Session-Id") != "" {
		r.mu.Lock()
		r.sessionID = response.Header.Get("Mcp-Session-Id")
		r.mu.Unlock()
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, &mcpRemoteError{status: response.StatusCode, err: fmt.Errorf("MCP remote authorization failed (%d)", response.StatusCode)}
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		return nil, &mcpRemoteError{retryable: true, status: response.StatusCode, err: fmt.Errorf("MCP remote HTTP status %d", response.StatusCode)}
	}
	if response.StatusCode >= 500 {
		return nil, &mcpRemoteError{status: response.StatusCode, err: fmt.Errorf("MCP remote HTTP status %d", response.StatusCode)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &mcpRemoteError{status: response.StatusCode, err: fmt.Errorf("MCP remote HTTP status %d", response.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(defaultMCPFrameBytes)+1))
	if err != nil {
		return nil, &mcpRemoteError{err: errors.New("MCP remote response could not be read")}
	}
	if len(body) > defaultMCPFrameBytes {
		return nil, errors.New("MCP remote response exceeds frame limit")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return parseMCPRemoteSSE(body, defaultMCPFrameBytes)
	}
	if !json.Valid(body) {
		return nil, errors.New("MCP remote response is not JSON")
	}
	return [][]byte{body}, nil
}

func parseMCPRemoteSSE(body []byte, maximum int) ([][]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), maximum)
	var data []string
	frames := make([][]byte, 0, 1)
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		frame := []byte(strings.Join(data, "\n"))
		if len(frame) > maximum || !json.Valid(frame) {
			return errors.New("MCP remote SSE data is invalid")
		}
		frames = append(frames, frame)
		data = nil
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, errors.New("MCP remote SSE response had no data")
	}
	return frames, nil
}

// A reconnect's discovery request must propagate session loss instead of
// recursively starting another handshake while owning the reconnect gate.
type mcpReconnectContextKey struct{}

func (c *MCPClient) callRemoteContext(ctx context.Context, method string, id int64, frame []byte) (json.RawMessage, error) {
	call := func(waiter chan mcpRPCResult) (json.RawMessage, error) {
		frames, err := c.remote.post(ctx, frame)
		if err != nil {
			return nil, err
		}
		for _, response := range frames {
			if err := c.dispatchFrame(response); err != nil {
				return nil, err
			}
		}
		select {
		case result := <-waiter:
			return result.result, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	waiter := make(chan mcpRPCResult, 1)
	if err := c.addPending(id, waiter); err != nil {
		return nil, err
	}
	result, err := call(waiter)
	if err == nil || method == "initialize" || ctx.Value(mcpReconnectContextKey{}) == c.remote || !isRetryableMCPRemoteError(err) || ctx.Err() != nil {
		c.removePending(id)
		return result, err
	}
	c.removePending(id)
	select {
	case c.remote.reconnect <- struct{}{}:
		defer func() { <-c.remote.reconnect }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("MCP remote client closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reconnectCtx := context.WithValue(ctx, mcpReconnectContextKey{}, c.remote)
	c.remote.invalidateSession()
	if initErr := c.initialize(reconnectCtx); initErr != nil {
		return nil, initErr
	}
	if refreshErr := c.discoverTools(reconnectCtx); refreshErr != nil {
		return nil, refreshErr
	}
	waiter = make(chan mcpRPCResult, 1)
	if err := c.addPending(id, waiter); err != nil {
		return nil, err
	}
	result, err = call(waiter)
	c.removePending(id)
	return result, err
}

func isRetryableMCPRemoteError(err error) bool {
	var remoteErr *mcpRemoteError
	return errors.As(err, &remoteErr) && remoteErr.retryable
}
