package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/server"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type recoveryIntegrationProvider struct {
	calls     atomic.Int32
	tool      bool
	splitTool bool
	release   <-chan struct{}
}

func (*recoveryIntegrationProvider) Name() string              { return "recovery-fixture" }
func (*recoveryIntegrationProvider) DisplayName() string       { return "Recovery Fixture" }
func (*recoveryIntegrationProvider) Models() []types.ModelInfo { return nil }
func (*recoveryIntegrationProvider) Validate() error           { return nil }

func (p *recoveryIntegrationProvider) StreamMessage(ctx context.Context, _ *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	call := p.calls.Add(1)
	events := make(chan types.SSEEvent, 8)
	go func() {
		defer close(events)
		if (p.tool || p.splitTool) && call == 1 {
			if p.splitTool {
				events <- types.SSEEvent{Type: "content_block_start", ContentBlock: recoveryJSON(map[string]string{"type": "text"})}
				events <- types.SSEEvent{Type: "content_block_delta", Delta: recoveryJSON(map[string]string{"type": "text_delta", "text": "HELLO"})}
				events <- types.SSEEvent{Type: "content_block_stop"}
			}
			toolName := "Write"
			toolInput := `{"file_path":"once.txt","content":"written once"}`
			if p.splitTool {
				toolName = "Read"
				toolInput = `{"file_path":"source.txt"}`
			}
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: recoveryJSON(map[string]string{
				"type": "tool_use", "id": "tool-once", "name": toolName,
			})}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: recoveryJSON(map[string]string{
				"type": "input_json_delta", "partial_json": toolInput,
			})}
			events <- types.SSEEvent{Type: "content_block_stop"}
			events <- types.SSEEvent{Type: "message_delta", Delta: recoveryJSON(map[string]string{"stop_reason": "tool_use"})}
			events <- types.SSEEvent{Type: "message_stop"}
			return
		}
		if p.release != nil {
			select {
			case <-p.release:
			case <-ctx.Done():
				return
			}
		}
		parts := []string{"HELLO", " WORLD"}
		if p.splitTool {
			parts = []string{" WORLD"}
		}
		for _, part := range parts {
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: recoveryJSON(map[string]string{"type": "text"})}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: recoveryJSON(map[string]string{"type": "text_delta", "text": part})}
			events <- types.SSEEvent{Type: "content_block_stop"}
		}
		events <- types.SSEEvent{Type: "message_delta", Delta: recoveryJSON(map[string]string{"stop_reason": "end_turn"})}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}

func recoveryJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

type recoveryCutTransport struct {
	base       http.RoundTripper
	cutType    string
	afterDrain func() error
	cut        atomic.Bool
	cutSeen    chan struct{}
	drainDone  chan error
}

func (t *recoveryCutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || request.Method != http.MethodPost || request.URL.Path != "/api/agent" ||
		response.StatusCode != http.StatusOK || !t.cut.CompareAndSwap(false, true) {
		return response, err
	}
	reader := bufio.NewReader(response.Body)
	var prefix bytes.Buffer
	for {
		line, readErr := reader.ReadBytes('\n')
		prefix.Write(line)
		if readErr != nil {
			_ = response.Body.Close()
			return nil, fmt.Errorf("read SSE before fault injection: %w", readErr)
		}
		if bytes.Contains(line, []byte(`"type":"`+t.cutType+`"`)) {
			blank, readErr := reader.ReadBytes('\n')
			prefix.Write(blank)
			if readErr != nil {
				_ = response.Body.Close()
				return nil, fmt.Errorf("read SSE frame end: %w", readErr)
			}
			break
		}
	}
	upstream := response.Body
	go func() {
		_, drainErr := io.Copy(io.Discard, reader)
		_ = upstream.Close()
		if drainErr == nil && t.afterDrain != nil {
			drainErr = t.afterDrain()
		}
		t.drainDone <- drainErr
	}()
	close(t.cutSeen)
	response.Body = &recoveryTruncatedBody{Reader: bytes.NewReader(prefix.Bytes())}
	response.ContentLength = -1
	response.Header.Del("Content-Length")
	return response, nil
}

type recoveryTruncatedBody struct{ *bytes.Reader }

func (*recoveryTruncatedBody) Close() error { return nil }

func recoveryIntegrationPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func startRecoveryIntegrationServer(port int, workDir, storeDir string, provider types.Provider) (*server.Server, <-chan error, error) {
	instance := server.New(provider, "fixture-model", port)
	instance.SetWorkDir(workDir)
	instance.SetSessionStore(agent.NewSessionStore(storeDir))
	stopped := make(chan error, 1)
	go func() { stopped <- instance.Start() }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second}
	for attempt := 0; attempt < 100; attempt++ {
		response, err := client.Get(base + "/health")
		if err == nil {
			_ = response.Body.Close()
			return instance, stopped, nil
		}
		select {
		case startErr := <-stopped:
			return nil, nil, fmt.Errorf("server exited before ready: %v", startErr)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, nil, fmt.Errorf("server did not become ready")
}

func TestAgentRecoveryAgainstRunningServer(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	t.Setenv("CORELAY_ACCESS_TOKEN", "recovery-integration-token")
	t.Setenv("ANICLEW_ACCESS_TOKEN", "")
	t.Setenv("CORELAY_BIND", "127.0.0.1")
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")

	for _, test := range []struct {
		name    string
		cutType string
		tool    bool
		restart bool
	}{
		{name: "before output while running", cutType: "session"},
		{name: "after partial text", cutType: "text"},
		{name: "during tool execution", cutType: "tool_execution_start", tool: true},
		{name: "after tool result", cutType: "tool_result", tool: true},
		{name: "after partial text and server restart", cutType: "text", restart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(workDir, "source.txt"), []byte("read fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			storeDir := t.TempDir()
			store := agent.NewSessionStore(storeDir)
			session := &agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "hello"}}}
			if err := store.Save(session); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			provider := &recoveryIntegrationProvider{tool: test.tool, splitTool: test.cutType == "text", release: release}
			port := recoveryIntegrationPort(t)
			instance, stopped, err := startRecoveryIntegrationServer(port, workDir, storeDir, provider)
			if err != nil {
				t.Fatal(err)
			}
			var serverMu sync.Mutex
			current := instance
			t.Cleanup(func() {
				serverMu.Lock()
				defer serverMu.Unlock()
				if current != nil {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = current.Shutdown(shutdownCtx)
				}
			})
			t.Cleanup(unblock)
			fault := &recoveryCutTransport{
				base: &http.Transport{DisableKeepAlives: true}, cutType: test.cutType,
				cutSeen: make(chan struct{}), drainDone: make(chan error, 1),
			}
			if test.restart {
				fault.afterDrain = func() error {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := instance.Shutdown(shutdownCtx); err != nil {
						return err
					}
					if err := <-stopped; err != nil {
						return err
					}
					restarted, _, err := startRecoveryIntegrationServer(port, workDir, storeDir, provider)
					if err != nil {
						return err
					}
					serverMu.Lock()
					current = restarted
					serverMu.Unlock()
					return nil
				}
			}
			client := &http.Client{Transport: fault, Timeout: 15 * time.Second}
			transport := newAgentStreamTransport(fmt.Sprintf("http://127.0.0.1:%d", port), "recovery-integration-token", client)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			stream := transport.StartTurn(ctx, agentTurnRequest{
				Messages: []chatMsg{{Role: "user", Content: "hello"}}, WorkDir: workDir,
				DurableSessionID: session.ID, ExpectedRevision: &session.Revision, RequestID: "integration-turn",
			})
			completed := make(chan []agentStreamItem, 1)
			go func() {
				var gathered []agentStreamItem
				for item := range stream {
					gathered = append(gathered, item)
				}
				completed <- gathered
			}()
			select {
			case <-fault.cutSeen:
			case <-ctx.Done():
				t.Fatal("fault injection did not reach the selected SSE frame")
			}
			if !test.restart {
				time.Sleep(450 * time.Millisecond)
			}
			unblock()
			var items []agentStreamItem
			select {
			case items = <-completed:
			case <-ctx.Done():
				t.Fatal("recovered stream did not finish")
			}
			var text strings.Builder
			var done int
			for _, item := range items {
				if item.Err != nil {
					t.Fatalf("stream ended with error: %v", item.Err)
				}
				switch item.Event.Type {
				case "text":
					var part string
					if err := json.Unmarshal(item.Event.Data, &part); err != nil {
						t.Fatal(err)
					}
					text.WriteString(part)
				case "done":
					done++
				}
			}
			if strings.Count(text.String(), "HELLO WORLD") != 1 || done != 1 {
				t.Fatalf("recovered stream text=%q done=%d", text.String(), done)
			}
			wantCalls := int32(1)
			if test.tool || test.cutType == "text" {
				wantCalls++
			}
			if calls := provider.calls.Load(); calls != wantCalls {
				t.Fatalf("provider calls=%d, want one logical run", calls)
			}
			persisted, err := agent.NewSessionStore(storeDir).Get(session.ID)
			wantRevision := uint64(2)
			if test.tool || test.cutType == "text" {
				wantRevision++
			}
			if err != nil || persisted == nil {
				t.Fatalf("read persisted session: %v", err)
			}
			if persisted.LastAgentRequest == nil || persisted.Revision != wantRevision {
				t.Fatalf("persisted revision/receipt = %d/%v, want revision %d",
					persisted.Revision, persisted.LastAgentRequest != nil, wantRevision)
			}
			if test.tool {
				written, err := os.ReadFile(filepath.Join(workDir, "once.txt"))
				if err != nil || string(written) != "written once" {
					t.Fatalf("tool result=%q err=%v", written, err)
				}
				var results int
				for _, message := range persisted.Messages {
					if message.Role == "tool" {
						results++
					}
				}
				if results != 1 {
					t.Fatalf("tool result count=%d, want 1", results)
				}
			}
			select {
			case drainErr := <-fault.drainDone:
				if drainErr != nil {
					t.Fatalf("upstream drain or restart: %v", drainErr)
				}
			case <-ctx.Done():
				t.Fatal("upstream stream did not finish")
			}
		})
	}
}
