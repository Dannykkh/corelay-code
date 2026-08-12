package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type fakeProvider struct {
	models []types.ModelInfo
}

func (p *fakeProvider) Name() string        { return "fake" }
func (p *fakeProvider) DisplayName() string { return "Fake Provider" }
func (p *fakeProvider) Models() []types.ModelInfo {
	return append([]types.ModelInfo(nil), p.models...)
}
func (*fakeProvider) Validate() error { return nil }
func (*fakeProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	return nil, errors.New("unexpected provider call")
}

type recordingClient struct {
	mu                 sync.Mutex
	updateStartedOnce  sync.Once
	updates            []acp.SessionNotification
	permissions        []acp.RequestPermissionRequest
	permissionResponse acp.RequestPermissionResponse
	updateErr          error
	updateStarted      chan struct{}
	updateRelease      <-chan struct{}
}

func (c *recordingClient) SessionUpdate(ctx context.Context, update acp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, update)
	err := c.updateErr
	started := c.updateStarted
	release := c.updateRelease
	c.mu.Unlock()
	if started != nil {
		c.updateStartedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (c *recordingClient) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, request)
	response := c.permissionResponse
	c.mu.Unlock()
	return response, nil
}

func (*recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}
func (*recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}
func (*recordingClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, nil
}
func (*recordingClient) TerminalOutput(context.Context, acp.TerminalRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}
func (*recordingClient) WaitForTerminalExit(context.Context, acp.TerminalRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}
func (*recordingClient) KillTerminal(context.Context, acp.TerminalRequest) (acp.EmptyResponse, error) {
	return acp.EmptyResponse{}, nil
}
func (*recordingClient) ReleaseTerminal(context.Context, acp.TerminalRequest) (acp.EmptyResponse, error) {
	return acp.EmptyResponse{}, nil
}
func (*recordingClient) CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, nil
}
func (*recordingClient) CompleteElicitation(context.Context, acp.CompleteElicitationNotification) error {
	return nil
}

func (c *recordingClient) snapshot() ([]acp.SessionNotification, []acp.RequestPermissionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acp.SessionNotification(nil), c.updates...), append([]acp.RequestPermissionRequest(nil), c.permissions...)
}

type backendFixture struct {
	backend   *Backend
	store     *agent.SessionStore
	workspace string
	client    *recordingClient
}

type failingInterruptStore struct {
	*agent.SessionStore
	err error
}

func (s *failingInterruptStore) MarkInterrupted(
	string,
	uint64,
	agent.SessionInterruption,
) (*agent.Session, error) {
	return nil, s.err
}

func newBackendFixture(t *testing.T, runner Runner) backendFixture {
	t.Helper()
	store := agent.NewSessionStore(t.TempDir())
	backend, err := New(Options{
		Provider: &fakeProvider{models: []types.ModelInfo{
			{ID: "model-a", DisplayName: "Model A"},
			{ID: "model-b", DisplayName: "Model B"},
		}},
		DefaultModel: "model-a",
		Store:        store,
		Runner:       runner,
		Version:      "test",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return backendFixture{backend: backend, store: store, workspace: t.TempDir(), client: &recordingClient{}}
}

func (f backendFixture) newSession(t *testing.T) string {
	t.Helper()
	response, err := f.backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD:        f.workspace,
		MCPServers: []acp.MCPServer{},
	}, f.client)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if response.SessionID == "" || len(response.ConfigOptions) != 2 {
		t.Fatalf("NewSession() = %#v", response)
	}
	return response.SessionID
}

func scriptedRunner(events ...agent.Event) Runner {
	return RunnerFunc(func(
		context.Context,
		types.Provider,
		string,
		[]types.Message,
		string,
		agent.RunOptions,
	) (<-chan agent.Event, error) {
		stream := make(chan agent.Event, len(events))
		for _, event := range events {
			stream <- event
		}
		close(stream)
		return stream, nil
	})
}

func TestPromptStreamsSafeProgressToolStateAndPersistsSanitizedTranscript(t *testing.T) {
	const inputSecret = "super-secret-credential"
	const outputSecret = "sk-proj-abcdefghijklmnop"
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "status", Data: "Thinking... (iteration 1/10)"},
		agent.Event{Type: "tool_start", Data: map[string]string{"id": "call-1", "name": "Bash"}},
		agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-1", "name": "Bash",
			"inputDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
		agent.Event{Type: "tool_result", Data: map[string]interface{}{
			"id": "call-1", "name": "Bash", "result": "Authorization: Bearer " + outputSecret, "isError": false, "executed": true,
		}},
		agent.Event{Type: "text", Data: "complete Authorization: Bearer " + outputSecret},
		agent.Event{Type: "done"},
	))
	sessionID := fixture.newSession(t)
	_, err := fixture.backend.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		SessionID: sessionID,
		ConfigID:  "reasoning",
		Value:     json.RawMessage(`"progress"`),
	}, fixture.client)
	if err != nil {
		t.Fatalf("SetSessionConfigOption(reasoning) error = %v", err)
	}

	response, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "api_key=" + inputSecret}},
	}, fixture.client)
	if err != nil || response.StopReason != acp.StopEndTurn {
		t.Fatalf("Prompt() = (%#v, %v)", response, err)
	}
	updates, _ := fixture.client.snapshot()
	encoded, err := json.Marshal(updates)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(encoded)
	for _, secret := range []string{inputSecret, outputSecret} {
		if strings.Contains(wire, secret) {
			t.Fatalf("ACP update leaked secret %q: %s", secret, wire)
		}
	}
	for _, required := range []string{"agent_thought_chunk", "Reasoning", "tool_call", "tool_call_update", "[REDACTED]"} {
		if !strings.Contains(wire, required) {
			t.Fatalf("ACP update missing %q: %s", required, wire)
		}
	}
	if strings.Contains(wire, "result") {
		t.Fatalf("tool result payload crossed ACP boundary: %s", wire)
	}
	var toolStatuses []acp.ToolCallStatus
	for _, update := range updates {
		if update.Update.ToolCallID == "call-1" {
			toolStatuses = append(toolStatuses, update.Update.Status)
		}
	}
	wantStatuses := []acp.ToolCallStatus{acp.ToolPending, acp.ToolInProgress, acp.ToolCompleted}
	if len(toolStatuses) != len(wantStatuses) {
		t.Fatalf("tool status sequence = %#v, want %#v", toolStatuses, wantStatuses)
	}
	for index := range wantStatuses {
		if toolStatuses[index] != wantStatuses[index] {
			t.Fatalf("tool status sequence = %#v, want %#v", toolStatuses, wantStatuses)
		}
	}

	persisted, err := fixture.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, _ := json.Marshal(persisted.Messages)
	for _, secret := range []string{inputSecret, outputSecret} {
		if strings.Contains(string(transcript), secret) {
			t.Fatalf("persisted transcript leaked secret %q: %s", secret, transcript)
		}
	}
	if len(persisted.Messages) != 3 || persisted.Messages[0].Role != "user" ||
		persisted.Messages[1].Role != "tool" || persisted.Messages[2].Role != "assistant" {
		t.Fatalf("persisted messages = %#v", persisted.Messages)
	}
	toolReference, ok := persisted.Messages[1].ToolInput.(map[string]interface{})
	if !ok || toolReference["toolCallId"] != "call-1" ||
		toolReference["inputDigest"] != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("persisted tool reference = %#v", persisted.Messages[1].ToolInput)
	}
}

func TestRepeatedLoadReplayFailureRestoresExistingRuntimeSession(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "text", Data: "answer"},
		agent.Event{Type: "done"},
	))
	sessionID := fixture.newSession(t)
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "question"}},
	}, fixture.client); err != nil {
		t.Fatal(err)
	}

	loaded, err := New(Options{
		Provider:     fixture.backend.provider,
		DefaultModel: "model-a",
		Store:        fixture.store,
		Runner:       scriptedRunner(agent.Event{Type: "done"}),
		Version:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := acp.LoadSessionRequest{SessionID: sessionID, CWD: fixture.workspace, MCPServers: []acp.MCPServer{}}
	if _, err := loaded.LoadSession(context.Background(), request, &recordingClient{}); err != nil {
		t.Fatalf("initial LoadSession() error = %v", err)
	}
	replayErr := errors.New("client replay failed")
	if _, err := loaded.LoadSession(context.Background(), request, &recordingClient{updateErr: replayErr}); !errors.Is(err, replayErr) {
		t.Fatalf("repeated LoadSession() error = %v, want replay failure", err)
	}
	if _, err := loaded.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "still bound"}},
	}, &recordingClient{}); err != nil {
		t.Fatalf("Prompt() after replay rollback error = %v", err)
	}
}

func TestReplayHistoryPreservesDigestBoundToolOrderingWithoutPayloads(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	session := agent.Session{
		ID: "sess_history",
		Messages: []agent.SessionMessage{
			{Role: "user", Content: "before"},
			{
				Role: "tool", ToolName: "Bash", ToolResult: "must-not-cross-replay",
				ToolInput: map[string]interface{}{"toolCallId": "call-history", "inputDigest": digest},
			},
			{Role: "assistant", Content: "after"},
			{
				Role: "tool", ToolName: "Legacy", ToolResult: "legacy-raw",
				ToolInput: map[string]interface{}{"command": "raw input"},
			},
		},
	}
	client := &recordingClient{}
	if err := replayHistory(context.Background(), client, session); err != nil {
		t.Fatal(err)
	}
	updates, _ := client.snapshot()
	if len(updates) != 3 || updates[0].Update.SessionUpdate != "user_message_chunk" ||
		updates[1].Update.SessionUpdate != "tool_call" ||
		updates[1].Update.ToolCallID != "call-history" ||
		updates[1].Update.Status != acp.ToolCompleted ||
		updates[2].Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("replayed history order = %#v", updates)
	}
	wire, _ := json.Marshal(updates)
	if strings.Contains(string(wire), "must-not-cross-replay") ||
		strings.Contains(string(wire), "legacy-raw") || strings.Contains(string(wire), "raw input") {
		t.Fatalf("replayed history exposed a tool payload: %s", wire)
	}
}

func TestLoadGuardsHalfPublishedRuntimeState(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "text", Data: "history"},
		agent.Event{Type: "done"},
	))
	sessionID := fixture.newSession(t)
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "seed"}},
	}, fixture.client); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(Options{
		Provider: fixture.backend.provider, DefaultModel: "model-a", Store: fixture.store,
		Runner: scriptedRunner(agent.Event{Type: "done"}), Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	client := &recordingClient{updateStarted: started, updateRelease: release}
	loadDone := make(chan error, 1)
	go func() {
		_, loadErr := loaded.LoadSession(context.Background(), acp.LoadSessionRequest{
			SessionID: sessionID, CWD: fixture.workspace, MCPServers: []acp.MCPServer{},
		}, client)
		loadDone <- loadErr
	}()
	<-started
	_, promptErr := loaded.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "race"}},
	}, &recordingClient{})
	_, closeErr := loaded.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: sessionID}, &recordingClient{})
	for name, got := range map[string]error{"prompt": promptErr, "close": closeErr} {
		var rpcErr *acp.RPCError
		if !errors.As(got, &rpcErr) || rpcErr.Code != acp.CodeInvalidRequest {
			t.Fatalf("%s during load error = %#v", name, got)
		}
	}
	close(release)
	if err := <-loadDone; err != nil {
		t.Fatalf("LoadSession() error = %v", err)
	}
	if _, err := loaded.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "after load"}},
	}, &recordingClient{}); err != nil {
		t.Fatalf("Prompt() after load error = %v", err)
	}
}

func TestInterruptionPersistenceFailureQuarantinesRuntimeSession(t *testing.T) {
	store := &failingInterruptStore{SessionStore: agent.NewSessionStore(t.TempDir()), err: errors.New("disk unavailable")}
	var starts int
	backend, err := New(Options{
		Provider: &fakeProvider{models: []types.ModelInfo{{ID: "model-a"}}}, DefaultModel: "model-a", Store: store,
		Runner: RunnerFunc(func(context.Context, types.Provider, string, []types.Message, string, agent.RunOptions) (<-chan agent.Event, error) {
			starts++
			return scriptedRunner(
				agent.Event{Type: "tool_execution_start", Data: map[string]string{
					"id": "call-1", "name": "Bash", "runId": "kernel-run-1",
					"inputDigest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				}},
				agent.Event{Type: "error", Data: "provider failed"},
			).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingClient{}
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}, client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "execute"}},
	}, client)
	if err == nil {
		t.Fatal("Prompt() unexpectedly succeeded when interruption persistence failed")
	}
	_, retryErr := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "retry"}},
	}, client)
	_, closeErr := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: created.SessionID}, client)
	for name, got := range map[string]error{"retry": retryErr, "close": closeErr} {
		var rpcErr *acp.RPCError
		if !errors.As(got, &rpcErr) || rpcErr.Code != acp.CodeInvalidRequest {
			t.Fatalf("%s quarantined session error = %#v", name, got)
		}
	}
	if starts != 1 {
		t.Fatalf("runner starts = %d, want 1", starts)
	}
}

func TestModelConfigRoundTripsIntoRunnerAndDurableSession(t *testing.T) {
	var capturedModel string
	fixture := newBackendFixture(t, RunnerFunc(func(
		_ context.Context,
		_ types.Provider,
		model string,
		_ []types.Message,
		_ string,
		_ agent.RunOptions,
	) (<-chan agent.Event, error) {
		capturedModel = model
		stream := make(chan agent.Event, 2)
		stream <- agent.Event{Type: "text", Data: "ok"}
		stream <- agent.Event{Type: "done"}
		close(stream)
		return stream, nil
	}))
	sessionID := fixture.newSession(t)
	response, err := fixture.backend.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		SessionID: sessionID,
		ConfigID:  "model",
		Value:     json.RawMessage(`"model-b"`),
	}, fixture.client)
	if err != nil {
		t.Fatalf("SetSessionConfigOption(model) error = %v", err)
	}
	if len(response.ConfigOptions) != 2 || response.ConfigOptions[0].CurrentValue != "model-b" {
		t.Fatalf("config response = %#v", response)
	}
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "hello"}},
	}, fixture.client); err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if capturedModel != "model-b" {
		t.Fatalf("runner model = %q, want model-b", capturedModel)
	}
	persisted, err := fixture.store.Get(sessionID)
	if err != nil || persisted.Model != "model-b" {
		t.Fatalf("persisted model = %q, error = %v", persisted.Model, err)
	}
	_, err = fixture.backend.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		SessionID: sessionID,
		ConfigID:  "model",
		Value:     json.RawMessage(`"unknown"`),
	}, fixture.client)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeInvalidParams {
		t.Fatalf("unknown model error = %#v", err)
	}
}

func TestLoadReplaysSafeHistoryListAndCloseLifecycle(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "text", Data: "answer"},
		agent.Event{Type: "done"},
	))
	sessionID := fixture.newSession(t)
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "question"}},
	}, fixture.client); err != nil {
		t.Fatal(err)
	}

	loaded, err := New(Options{
		Provider:     fixture.backend.provider,
		DefaultModel: "model-a",
		Store:        fixture.store,
		Runner:       scriptedRunner(agent.Event{Type: "done"}),
		Version:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	loadClient := &recordingClient{}
	loadResponse, err := loaded.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID:  sessionID,
		CWD:        fixture.workspace,
		MCPServers: []acp.MCPServer{},
	}, loadClient)
	if err != nil || len(loadResponse.ConfigOptions) != 2 {
		t.Fatalf("LoadSession() = (%#v, %v)", loadResponse, err)
	}
	updates, _ := loadClient.snapshot()
	if len(updates) != 2 || updates[0].Update.SessionUpdate != "user_message_chunk" || updates[1].Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("history updates = %#v", updates)
	}
	cwd := fixture.workspace
	listed, err := loaded.ListSessions(context.Background(), acp.ListSessionsRequest{CWD: &cwd}, loadClient)
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != sessionID {
		t.Fatalf("ListSessions() = (%#v, %v)", listed, err)
	}
	if _, err := loaded.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: sessionID}, loadClient); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	persisted, err := fixture.store.Get(sessionID)
	if err != nil || persisted.LifecycleStatus == agent.SessionLifecycleClosed {
		t.Fatalf("ACP close mutated durable lifecycle = (%#v, %v)", persisted, err)
	}
	listed, err = loaded.ListSessions(context.Background(), acp.ListSessionsRequest{CWD: &cwd}, loadClient)
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != sessionID {
		t.Fatalf("ListSessions() after close = (%#v, %v)", listed, err)
	}
	_, err = loaded.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID:  sessionID,
		CWD:        fixture.workspace,
		MCPServers: []acp.MCPServer{},
	}, loadClient)
	if err != nil {
		t.Fatalf("LoadSession() after ACP close error = %v", err)
	}
}

func TestCancelSessionWaitsForRunnerTerminationAndBoundsShutdown(t *testing.T) {
	t.Run("cooperative", func(t *testing.T) {
		started := make(chan struct{})
		fixture := newBackendFixture(t, RunnerFunc(func(
			ctx context.Context,
			_ types.Provider,
			_ string,
			_ []types.Message,
			_ string,
			_ agent.RunOptions,
		) (<-chan agent.Event, error) {
			stream := make(chan agent.Event)
			close(started)
			go func() {
				<-ctx.Done()
				close(stream)
			}()
			return stream, nil
		}))
		sessionID := fixture.newSession(t)
		result := make(chan error, 1)
		go func() {
			_, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
				SessionID: sessionID,
				Prompt:    []acp.ContentBlock{{Type: "text", Text: "wait"}},
			}, fixture.client)
			result <- err
		}()
		<-started
		_ = fixture.backend.CancelSession(context.Background(), acp.CancelNotification{SessionID: sessionID})
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Prompt() error = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cooperative cancellation did not return")
		}
	})

	t.Run("uncooperative", func(t *testing.T) {
		started := make(chan struct{})
		stream := make(chan agent.Event, 2)
		fixture := newBackendFixture(t, RunnerFunc(func(
			context.Context,
			types.Provider,
			string,
			[]types.Message,
			string,
			agent.RunOptions,
		) (<-chan agent.Event, error) {
			close(started)
			return stream, nil
		}))
		sessionID := fixture.newSession(t)
		result := make(chan error, 1)
		go func() {
			_, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
				SessionID: sessionID,
				Prompt:    []acp.ContentBlock{{Type: "text", Text: "wait"}},
			}, fixture.client)
			result <- err
		}()
		<-started
		_ = fixture.backend.CancelSession(context.Background(), acp.CancelNotification{SessionID: sessionID})
		select {
		case err := <-result:
			t.Fatalf("Prompt() returned before runner termination: %v", err)
		case <-time.After(50 * time.Millisecond):
			// ACP requires prompt completion to remain pending until the run
			// actually terminates, even though shutdown itself is bounded.
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if err := fixture.backend.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown() before stream close error = %v, want deadline", err)
		}
		shutdownCancel()
		stream <- agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id":          "late-call",
			"name":        "Bash",
			"inputDigest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}}
		close(stream)
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Prompt() after runner close error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Prompt() did not finish after runner stream closed")
		}
		shutdownCtx, shutdownCancel = context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		if err := fixture.backend.Shutdown(shutdownCtx); err != nil {
			t.Fatalf("Shutdown() after stream close error = %v", err)
		}
		persisted, err := fixture.store.Get(sessionID)
		if err != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
			persisted.Interruption.ToolCallID != "late-call" {
			t.Fatalf("late execution interruption = (%#v, %v)", persisted, err)
		}
	})
}

func TestPromptTerminalReasonsAndUIOnlyCommands(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []agent.Event
		want   acp.StopReason
	}{
		{
			name: "provider max tokens",
			events: []agent.Event{{Type: "done", Data: map[string]interface{}{
				"stopReason": "max_tokens",
			}}},
			want: acp.StopMaxTokens,
		},
		{
			name: "provider refusal",
			events: []agent.Event{{Type: "done", Data: map[string]interface{}{
				"stopReason": "refusal",
			}}},
			want: acp.StopRefusal,
		},
		{
			name:   "loop max turns",
			events: []agent.Event{{Type: "error", Data: "Max iterations reached"}},
			want:   acp.StopMaxTurnRequests,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBackendFixture(t, scriptedRunner(test.events...))
			response, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
				SessionID: fixture.newSession(t),
				Prompt:    []acp.ContentBlock{{Type: "text", Text: "continue"}},
			}, fixture.client)
			if err != nil || response.StopReason != test.want {
				t.Fatalf("Prompt() = (%#v, %v), want %q", response, err, test.want)
			}
		})
	}

	var starts int
	fixture := newBackendFixture(t, RunnerFunc(func(
		context.Context,
		types.Provider,
		string,
		[]types.Message,
		string,
		agent.RunOptions,
	) (<-chan agent.Event, error) {
		starts++
		return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
	}))
	sessionID := fixture.newSession(t)
	for _, command := range []string{"/clear", "/model", "/model model-b"} {
		_, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
			SessionID: sessionID,
			Prompt:    []acp.ContentBlock{{Type: "text", Text: command}},
		}, fixture.client)
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeInvalidParams {
			t.Fatalf("Prompt(%q) error = %#v", command, err)
		}
	}
	persisted, err := fixture.store.Get(sessionID)
	if err != nil || len(persisted.Messages) != 0 || starts != 0 {
		t.Fatalf("UI command mutated session: starts=%d persisted=(%#v, %v)", starts, persisted, err)
	}
}

func TestMaxIterationsCommitsObservedToolTranscript(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-max", "name": "Bash", "runId": "run-max", "inputDigest": digest,
		}},
		agent.Event{Type: "tool_result", Data: map[string]any{
			"id": "call-max", "name": "Bash", "result": "ok", "executed": true,
		}},
		agent.Event{Type: "error", Data: "Max iterations reached"},
	))
	sessionID := fixture.newSession(t)
	response, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "run until bounded stop"}},
	}, fixture.client)
	if err != nil || response.StopReason != acp.StopMaxTurnRequests {
		t.Fatalf("Prompt() = (%#v, %v)", response, err)
	}
	persisted, err := fixture.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ReconcileRequired || len(persisted.Messages) != 2 ||
		persisted.Messages[1].Role != "tool" || persisted.Messages[1].ToolName != "Bash" {
		t.Fatalf("max-iteration transcript was not committed: %#v", persisted)
	}
}

func TestClientFailureFollowedByDoneStillMarksToolForReconciliation(t *testing.T) {
	digest := "sha256:" + strings.Repeat("f", 64)
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "text", Data: "client-visible chunk"},
		agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-drain", "name": "Write", "runId": "run-drain", "inputDigest": digest,
		}},
		agent.Event{Type: "tool_result", Data: map[string]any{
			"id": "call-drain", "name": "Write", "result": "ok", "executed": true,
		}},
		agent.Event{Type: "done"},
	))
	sessionID := fixture.newSession(t)
	_, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "disconnect"}},
	}, &recordingClient{updateErr: errors.New("client write failed")})
	if err == nil {
		t.Fatal("Prompt() unexpectedly succeeded after client failure")
	}
	persisted, loadErr := fixture.store.Get(sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.ToolCallID != "call-drain" ||
		persisted.Interruption.SideEffectState != agent.SessionSideEffectApplied {
		t.Fatalf("drained completed tool was not marked for reconciliation: %#v", persisted)
	}
}

func TestPromptAmbiguousReusedToolCallIDFailsClosedIntoReconciliation(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(
		agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-reused", "name": "Write", "runId": "run-reused",
			"inputDigest": "sha256:" + strings.Repeat("1", 64),
		}},
		agent.Event{Type: "tool_execution_start", Data: map[string]string{
			"id": "call-reused", "name": "Edit", "runId": "run-reused",
			"inputDigest": "sha256:" + strings.Repeat("2", 64),
		}},
		agent.Event{Type: "tool_result", Data: map[string]any{
			"id": "call-reused", "name": "Edit", "result": "second", "executed": true,
		}},
		agent.Event{Type: "tool_result", Data: map[string]any{
			"id": "call-reused", "name": "Write", "result": "first", "executed": true,
		}},
		agent.Event{Type: "done"},
	))
	sessionID := fixture.newSession(t)
	_, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "overlap"}},
	}, fixture.client)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeInvalidRequest {
		t.Fatalf("Prompt() error = %#v, want invalid request", err)
	}
	persisted, loadErr := fixture.store.Get(sessionID)
	if loadErr != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.ToolName != "multiple_tools" {
		t.Fatalf("ambiguous ACP run checkpoint = (%#v, %v)", persisted, loadErr)
	}
}

func TestBridgeRejectsClientMCPAndDisablesWorkspaceMCP(t *testing.T) {
	var captured agent.RunOptions
	fixture := newBackendFixture(t, RunnerFunc(func(
		_ context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		options agent.RunOptions,
	) (<-chan agent.Event, error) {
		captured = options
		return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
	}))
	_, err := fixture.backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: fixture.workspace,
		MCPServers: []acp.MCPServer{{
			Name: "client-server", Command: "mcp-server",
		}},
	}, fixture.client)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeInvalidParams {
		t.Fatalf("NewSession(MCP) error = %#v", err)
	}
	sessionID := fixture.newSession(t)
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "safe"}},
	}, fixture.client); err != nil {
		t.Fatal(err)
	}
	if !captured.DisableWorkspaceMCP {
		t.Fatal("ACP run did not disable workspace MCP authority")
	}
	if captured.SandboxRunner == nil || captured.SandboxPolicy.Enforcement == "" {
		t.Fatalf("ACP run did not bind an explicit sandbox: %#v", captured.SandboxPolicy)
	}
	if captured.SandboxRunner.Capabilities().FilesystemIsolation && captured.SandboxPolicy.Workspace != fixture.workspace {
		t.Fatalf("sandbox workspace = %q, want %q", captured.SandboxPolicy.Workspace, fixture.workspace)
	}
}

func TestApprovalRequesterMapsAllowDenyWithoutRawInput(t *testing.T) {
	for _, test := range []struct {
		name      string
		optionID  string
		wantAllow bool
	}{
		{name: "allow once", optionID: "allow_once", wantAllow: true},
		{name: "deny", optionID: "deny", wantAllow: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingClient{permissionResponse: acp.RequestPermissionResponse{
				Outcome: acp.PermissionOutcome{Outcome: "selected", OptionID: test.optionID},
			}}
			requester := newApprovalRequester(client, "runtime-1", "session-1", 7, time.Minute)
			pending, err := requester.Open(approval.Draft{
				SessionID:     "runtime-1",
				RunID:         "run-1",
				ToolCallID:    "call-1",
				ToolName:      "Bash",
				RedactedInput: `{"password":"raw-secret-value"}`,
				InputDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			})
			if err != nil {
				t.Fatal(err)
			}
			resolution, err := requester.Await(context.Background(), "runtime-1", pending.ID)
			if err != nil || resolution.Allowed() != test.wantAllow {
				t.Fatalf("Await() = (%#v, %v)", resolution, err)
			}
			_, permissions := client.snapshot()
			wire, _ := json.Marshal(permissions)
			if strings.Contains(string(wire), "raw-secret-value") || strings.Contains(string(wire), "password") {
				t.Fatalf("permission payload leaked input: %s", wire)
			}
			if len(permissions) != 1 || len(permissions[0].Options) != 2 || permissions[0].SessionID != "session-1" ||
				permissions[0].ToolCall.ToolCallID != "call-1" || permissions[0].ToolCall.Title != "Permission required: Bash" {
				t.Fatalf("permission request = %#v", permissions)
			}
		})
	}
}

func TestSanitizeTextSecretCorpusAndPEM(t *testing.T) {
	raw := strings.Join([]string{
		"Authorization: Bearer bearer-secret",
		"api_key=api-secret",
		"https://user:pass@example.test/path",
		"ghp_abcdefghijklmnop",
		"-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
	}, "\n")
	got := sanitizeText(raw, 4096)
	for _, secret := range []string{"bearer-secret", "api-secret", "user:pass", "ghp_abcdefghijklmnop", "private-material"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitizeText leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitizeText() = %q", got)
	}
}
