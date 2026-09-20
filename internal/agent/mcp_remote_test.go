package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type remoteMCPFixture struct {
	mu          sync.Mutex
	initialized int
	listCalls   int
	callCount   int
	authorized  bool
	session     string
}

func (f *remoteMCPFixture) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	if request.Header.Get("Authorization") == "Bearer fixture-token" && request.Header.Get("MCP-Protocol-Version") == mcpRemoteProtocolVersion {
		f.authorized = true
	}
	f.mu.Unlock()
	var message jsonRPCRequest
	if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch message.Method {
	case "initialize":
		f.initialized++
		f.session = fmt.Sprintf("fixture-session-%d", f.initialized)
		w.Header().Set("Mcp-Session-Id", f.session)
		writeRemoteJSONRPC(w, message.ID, map[string]any{"protocolVersion": mcpRemoteProtocolVersion})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.listCalls++
		writeRemoteJSONRPC(w, message.ID, map[string]any{"tools": []map[string]any{{
			"name": "remote_echo", "description": "remote fixture", "inputSchema": map[string]any{"type": "object"},
		}}})
	case "tools/call":
		f.callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", mustRemoteJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustRemoteJSON(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": "remote-ok"}}}}))
	default:
		writeRemoteJSONRPC(w, message.ID, map[string]any{})
	}
}

func writeRemoteJSONRPC(w http.ResponseWriter, id any, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func mustRemoteJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestMCPRemoteHTTPAuthenticatesRefreshesSchemaAndReconnects(t *testing.T) {
	fixture := &remoteMCPFixture{}
	server := httptest.NewServer(fixture)
	defer server.Close()
	client, err := NewMCPRemoteClientWithOptions("remote", server.URL, map[string]string{"Authorization": "Bearer fixture-token"}, MCPExecutionOptions{Context: context.Background(), CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, failed := client.CallTool("remote_echo", json.RawMessage(`{"value":"hello"}`))
	if failed || result != "remote-ok" {
		t.Fatalf("remote call = (%q, %v)", result, failed)
	}
	result, failed = client.CallTool("remote_echo", json.RawMessage(`{"value":"again"}`))
	if failed || result != "remote-ok" {
		t.Fatalf("refreshed remote call = (%q, %v)", result, failed)
	}
	fixture.mu.Lock()
	authorized, initialized, listCalls, calls := fixture.authorized, fixture.initialized, fixture.listCalls, fixture.callCount
	fixture.mu.Unlock()
	if !authorized || initialized != 1 || listCalls < 2 || calls != 2 {
		t.Fatalf("remote lifecycle authorized=%v initialized=%d list=%d calls=%d", authorized, initialized, listCalls, calls)
	}
}

func TestMCPRuntimeOwnsRemoteHTTPWithoutFilesystemRunner(t *testing.T) {
	fixture := &remoteMCPFixture{}
	server := httptest.NewServer(fixture)
	defer server.Close()
	runtime, err := NewMCPRuntime(context.Background(), t.TempDir(), []MCPServerSpec{{
		Name: "remote", Type: "http", URL: server.URL,
	}}, MCPExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtime.ServerCount() != 1 || len(runtime.ToolDefs()) != 1 {
		t.Fatalf("remote runtime catalog = servers=%d tools=%d", runtime.ServerCount(), len(runtime.ToolDefs()))
	}
	if scope, ok := runtime.(MCPRuntimeFilesystemIsolationProvider); !ok || scope.RequiresFilesystemIsolation() {
		t.Fatal("remote runtime incorrectly requires a filesystem-isolated child runner")
	}
}

func TestMCPRemoteHTTPReconnectsAfterSessionLoss(t *testing.T) {
	var mu sync.Mutex
	initialized := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var message jsonRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch message.Method {
		case "initialize":
			initialized++
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", initialized))
			writeRemoteJSONRPC(w, message.ID, map[string]any{"protocolVersion": mcpRemoteProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRemoteJSONRPC(w, message.ID, map[string]any{"tools": []map[string]any{{"name": "remote_echo", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			if request.Header.Get("Mcp-Session-Id") == "session-1" {
				http.Error(w, "expired", http.StatusNotFound)
				return
			}
			writeRemoteJSONRPC(w, message.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": "reconnected"}}})
		}
	}))
	defer server.Close()
	client, err := NewMCPRemoteClientWithOptions("remote", server.URL, nil, MCPExecutionOptions{Context: context.Background(), CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, failed := client.CallTool("remote_echo", nil)
	if failed || result != "reconnected" {
		t.Fatalf("reconnected call = (%q, %v)", result, failed)
	}
	mu.Lock()
	count := initialized
	mu.Unlock()
	if count != 2 {
		t.Fatalf("initialize count=%d, want reconnect handshake", count)
	}
}

func TestMCPRemoteHTTPCancellationAbortsRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var message jsonRPCRequest
		_ = json.NewDecoder(request.Body).Decode(&message)
		if message.Method == "initialize" {
			writeRemoteJSONRPC(w, message.ID, map[string]any{"protocolVersion": mcpRemoteProtocolVersion})
			return
		}
		if message.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if message.Method == "tools/list" {
			writeRemoteJSONRPC(w, message.ID, map[string]any{"tools": []map[string]any{{"name": "remote_echo", "inputSchema": map[string]any{"type": "object"}}}})
			return
		}
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := NewMCPRemoteClientWithOptions("remote", server.URL, nil, MCPExecutionOptions{Context: context.Background(), CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, failed := client.CallToolContext(ctx, "remote_echo", nil)
	if !failed {
		t.Fatal("canceled remote call was accepted")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("remote call did not reach fixture")
	}
}

func TestMCPRemoteHTTPAuthorizationFailureDoesNotRetry(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := NewMCPRemoteClientWithOptions("remote", server.URL, map[string]string{"Authorization": "Bearer wrong"}, MCPExecutionOptions{Context: context.Background(), CallTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "authorization failed") {
		t.Fatalf("authorization error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("unauthorized initialization requests=%d, want one", requests)
	}
}

func TestMCPRemoteReconnectDiscoveryLossHonorsCancellation(t *testing.T) {
	initCount := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("decode request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case "initialize":
			initCount++
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", initCount))
			writeRemoteJSONRPC(w, req.ID, map[string]any{"protocolVersion": mcpRemoteProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			if initCount > 1 {
				w.WriteHeader(404)
				return
			}
			writeRemoteJSONRPC(w, req.ID, map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := NewMCPRemoteClientWithOptions("audit", server.URL, nil, MCPExecutionOptions{Context: context.Background(), CallTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() { _, failed := client.CallToolContext(ctx, "echo", json.RawMessage(`{}`)); done <- failed }()
	select {
	case failed := <-done:
		if !failed {
			t.Fatal("session loss during discovery was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("call remained blocked after 100ms context deadline: reconnect tools/list 404 recursively locks reconnect mutex")
	}
}

func TestMCPRemoteConcurrentReconnectWaitHonorsCancellation(t *testing.T) {
	reconnectStarted := make(chan struct{})
	releaseReconnect := make(chan struct{})
	secondCall := make(chan struct{})
	var mu sync.Mutex
	initializes, calls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request jsonRPCRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			return
		}
		switch request.Method {
		case "initialize":
			mu.Lock()
			initializes++
			current := initializes
			mu.Unlock()
			if current == 2 {
				close(reconnectStarted)
				select {
				case <-releaseReconnect:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", current))
			writeRemoteJSONRPC(w, request.ID, map[string]any{"protocolVersion": mcpRemoteProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			writeRemoteJSONRPC(w, request.ID, map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			mu.Lock()
			calls++
			current := calls
			mu.Unlock()
			if current == 2 {
				close(secondCall)
			}
			if current <= 2 {
				w.WriteHeader(404)
				return
			}
			writeRemoteJSONRPC(w, request.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": "reconnected"}}})
		}
	}))
	defer server.Close()
	defer close(releaseReconnect)
	client, err := NewMCPRemoteClientWithOptions("concurrent", server.URL, nil, MCPExecutionOptions{Context: context.Background(), CallTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	firstDone := make(chan bool, 1)
	go func() { text, failed := client.CallTool("echo", nil); firstDone <- (!failed && text == "reconnected") }()
	select {
	case <-reconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	secondDone := make(chan bool, 1)
	go func() { _, failed := client.CallToolContext(ctx, "echo", nil); secondDone <- failed }()
	select {
	case <-secondCall:
	case <-time.After(time.Second):
		t.Fatal("concurrent call did not reach server")
	}
	select {
	case failed := <-secondDone:
		if !failed {
			t.Fatal("canceled call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled caller remained blocked on reconnect ownership")
	}
	// Releasing the owner must still allow its normal reconnect and retry.
	releaseReconnect <- struct{}{}
	select {
	case ok := <-firstDone:
		if !ok {
			t.Fatal("owner reconnect failed")
		}
	case <-time.After(time.Second):
		t.Fatal("owner reconnect did not finish")
	}
}
