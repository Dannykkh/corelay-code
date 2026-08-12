package acpbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestACPMCPHelperProcess(t *testing.T) {
	if os.Getenv("CORELAY_ACP_MCP_HELPER") != "1" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "acp-fixture", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name": "mcp_acp_echo", "description": "Return the session fixture",
				"inputSchema": map[string]any{
					"type": "object", "properties": map[string]any{
						"value": map[string]any{"type": "string"},
					}, "required": []string{"value"}, "additionalProperties": false,
				},
			}}}
		case "tools/call":
			if os.Getenv("CORELAY_ACP_MCP_BLOCK") == "1" {
				// A bare select triggers the Go runtime's deadlock detector in this
				// single-test helper process. Sleeping keeps the subprocess genuinely
				// alive until the session-owned supervisor closes it.
				time.Sleep(24 * time.Hour)
			}
			value := os.Getenv("CORELAY_ACP_MCP_RESULT")
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": value}}}
		default:
			result = map[string]any{}
		}
		if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
			return
		}
		if request.Method == "tools/call" && os.Getenv("CORELAY_ACP_MCP_EXIT_AFTER_CALL") == "1" {
			// Let the current prompt consume the successful response and finish its
			// provider turn. The next prompt must then observe the dead session
			// runtime and replace its generation before advertising the catalog.
			time.Sleep(500 * time.Millisecond)
			return
		}
	}
}

type acpSecureMCPRunner struct{}

func (*acpSecureMCPRunner) Name() string { return "acp-test-secure-mcp" }
func (*acpSecureMCPRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		ProcessIsolation: true, ProcessTreeKill: true,
		EnvironmentFiltering: true, Timeouts: true,
	}
}
func (r *acpSecureMCPRunner) Start(
	ctx context.Context,
	policy sandbox.Policy,
	spec processsupervisor.Spec,
) (*processsupervisor.Process, processsupervisor.Report) {
	host := processsupervisor.NewHostRunner()
	hostCaps := host.Capabilities()
	process, report := host.Start(ctx, sandbox.Policy{
		Enforcement: sandbox.EnforcementDisabled,
		Required: sandbox.Capabilities{
			ProcessTreeKill: hostCaps.ProcessTreeKill, EnvironmentFiltering: true, Timeouts: true,
		},
	}, spec)
	report.Runner = r.Name()
	report.Policy = policy
	report.Capabilities = r.Capabilities()
	if report.Started {
		report.Applied = r.Capabilities()
	}
	return process, report
}

func acpSecureMCPExecution() *agent.MCPExecutionOptions {
	runner := &acpSecureMCPRunner{}
	return &agent.MCPExecutionOptions{
		Runner: runner,
		Policy: sandbox.Policy{
			Enforcement: sandbox.EnforcementPreferred,
			Required:    runner.Capabilities(),
		},
		CallTimeout: 5 * time.Second,
	}
}

type countedMCPRuntime struct {
	agent.MCPRuntime
	closes atomic.Int32
}

func (r *countedMCPRuntime) Close() {
	r.closes.Add(1)
	r.MCPRuntime.Close()
}

type recordingMCPRuntimeFactory struct {
	mu       sync.Mutex
	runtimes map[string][]*countedMCPRuntime
}

func newRecordingMCPRuntimeFactory() *recordingMCPRuntimeFactory {
	return &recordingMCPRuntimeFactory{runtimes: make(map[string][]*countedMCPRuntime)}
}

func (f *recordingMCPRuntimeFactory) create(
	ctx context.Context,
	workspace string,
	servers []agent.MCPServerSpec,
	execution agent.MCPExecutionOptions,
) (agent.MCPRuntime, error) {
	runtime, err := agent.NewMCPRuntime(ctx, workspace, servers, execution)
	if err != nil {
		return nil, err
	}
	wrapper := &countedMCPRuntime{MCPRuntime: runtime}
	name := ""
	if len(servers) > 0 {
		name = servers[0].Name
	}
	f.mu.Lock()
	f.runtimes[name] = append(f.runtimes[name], wrapper)
	f.mu.Unlock()
	return wrapper, nil
}

func (f *recordingMCPRuntimeFactory) snapshot(name string) []*countedMCPRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*countedMCPRuntime(nil), f.runtimes[name]...)
}

type acpMCPProvider struct {
	mu            sync.Mutex
	firstCatalogs [][]types.ToolDef
}

func (*acpMCPProvider) Name() string        { return "acp-mcp-test" }
func (*acpMCPProvider) DisplayName() string { return "ACP MCP Test" }
func (*acpMCPProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{{ID: "mcp-model", DisplayName: "MCP Model", ContextWindow: 32768, MaxOutput: 4096}}
}
func (*acpMCPProvider) Validate() error { return nil }
func (p *acpMCPProvider) StreamMessage(
	_ context.Context,
	request *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	hasToolResult := false
	for _, message := range request.Messages {
		if strings.Contains(string(message.Content), `"tool_result"`) {
			hasToolResult = true
			break
		}
	}
	if !hasToolResult {
		p.mu.Lock()
		p.firstCatalogs = append(p.firstCatalogs, append([]types.ToolDef(nil), request.Tools...))
		p.mu.Unlock()
	}
	events := make(chan types.SSEEvent, 6)
	go func() {
		defer close(events)
		if !hasToolResult {
			block, _ := json.Marshal(map[string]string{
				"type": "tool_use", "id": "mcp-call", "name": "mcp_acp_echo",
			})
			delta, _ := json.Marshal(map[string]string{
				"type": "input_json_delta", "partial_json": `{"value":"hello"}`,
			})
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: block}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
			events <- types.SSEEvent{Type: "content_block_stop"}
		} else {
			block, _ := json.Marshal(map[string]string{"type": "text"})
			delta, _ := json.Marshal(map[string]string{"type": "text_delta", "text": "done"})
			events <- types.SSEEvent{Type: "content_block_start", ContentBlock: block}
			events <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
			events <- types.SSEEvent{Type: "content_block_stop"}
		}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}

func (p *acpMCPProvider) catalogs() [][]types.ToolDef {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([][]types.ToolDef, len(p.firstCatalogs))
	for index, catalog := range p.firstCatalogs {
		result[index] = append([]types.ToolDef(nil), catalog...)
	}
	return result
}

func acpMCPServer(t *testing.T, name, result string) acp.MCPServer {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return acp.MCPServer{
		Name: name, Command: executable,
		Args: []string{"-test.run=^TestACPMCPHelperProcess$"},
		Env: []acp.EnvVariable{
			{Name: "CORELAY_ACP_MCP_HELPER", Value: "1"},
			{Name: "CORELAY_ACP_MCP_RESULT", Value: result},
		},
	}
}

func newACPMCPBackend(
	t *testing.T,
	provider types.Provider,
	factory *recordingMCPRuntimeFactory,
) (*Backend, *agent.SessionStore) {
	t.Helper()
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	store := agent.NewSessionStore(t.TempDir())
	backend, err := New(Options{
		Provider: provider, DefaultModel: "mcp-model", Store: store,
		MCPRuntimeFactory: factory.create, MCPExecution: acpSecureMCPExecution(),
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend, store
}

func TestACPStdioMCPIsAvailableOnFirstRequestAndIsolatedAcrossConcurrentSessions(t *testing.T) {
	provider := &acpMCPProvider{}
	factory := newRecordingMCPRuntimeFactory()
	backend, store := newACPMCPBackend(t, provider, factory)
	clientOne := &recordingClient{}
	clientTwo := &recordingClient{}
	createdOne, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: t.TempDir(), MCPServers: []acp.MCPServer{acpMCPServer(t, "session-one", "one")},
	}, clientOne)
	if err != nil {
		t.Fatal(err)
	}
	createdTwo, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: t.TempDir(), MCPServers: []acp.MCPServer{acpMCPServer(t, "session-two", "two")},
	}, clientTwo)
	if err != nil {
		t.Fatal(err)
	}

	type promptResult struct{ err error }
	results := make(chan promptResult, 2)
	for _, item := range []struct {
		id     string
		client *recordingClient
		text   string
	}{{createdOne.SessionID, clientOne, "one"}, {createdTwo.SessionID, clientTwo, "two"}} {
		item := item
		go func() {
			_, promptErr := backend.Prompt(context.Background(), acp.PromptRequest{
				SessionID: item.id, Prompt: []acp.ContentBlock{{Type: "text", Text: item.text}},
			}, item.client)
			results <- promptResult{err: promptErr}
		}()
	}
	for range 2 {
		if result := <-results; result.err != nil {
			t.Fatal(result.err)
		}
	}

	for _, expected := range []struct {
		id, server, result string
	}{{createdOne.SessionID, "session-one", "one"}, {createdTwo.SessionID, "session-two", "two"}} {
		persisted, loadErr := store.Get(expected.id)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		found := false
		for _, message := range persisted.Messages {
			if message.Role == "tool" && message.ToolName == "mcp_acp_echo" && message.Content == expected.result {
				found = true
			}
		}
		if !found {
			t.Fatalf("session %s did not persist isolated result %q: %#v", expected.id, expected.result, persisted.Messages)
		}
		runtimes := factory.snapshot(expected.server)
		if len(runtimes) != 1 || runtimes[0].closes.Load() != 0 || !runtimes[0].Healthy() {
			t.Fatalf("session runtime %s = %#v closes=%d", expected.server, runtimes, runtimes[0].closes.Load())
		}
		reports := runtimes[0].Reports()
		if len(reports) != 1 || !reports[0].Started || reports[0].Runner == "host-disabled" {
			t.Fatalf("session runtime %s redacted start evidence = %#v", expected.server, reports)
		}
	}
	catalogs := provider.catalogs()
	if len(catalogs) != 2 {
		t.Fatalf("first provider catalogs = %d, want 2", len(catalogs))
	}
	for _, catalog := range catalogs {
		found := false
		for _, tool := range catalog {
			if tool.Name == "mcp_acp_echo" {
				found = true
			}
		}
		if !found {
			t.Fatalf("first provider request omitted MCP tool: %#v", catalog)
		}
	}

	firstGeneration := factory.snapshot("session-one")[0].Generation()
	if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: createdOne.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "again"}},
	}, clientOne); err != nil {
		t.Fatal(err)
	}
	if runtimes := factory.snapshot("session-one"); len(runtimes) != 1 ||
		runtimes[0].Generation() != firstGeneration || runtimes[0].closes.Load() != 0 {
		t.Fatalf("second prompt restarted session runtime: %#v", runtimes)
	}

	for _, item := range []struct {
		id, server string
		client     *recordingClient
	}{{createdOne.SessionID, "session-one", clientOne}, {createdTwo.SessionID, "session-two", clientTwo}} {
		if _, err := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: item.id}, item.client); err != nil {
			t.Fatal(err)
		}
		if closes := factory.snapshot(item.server)[0].closes.Load(); closes != 1 {
			t.Fatalf("session %s close calls = %d, want 1", item.server, closes)
		}
	}
}

func TestACPPromptCancelKeepsSessionMCPUntilClose(t *testing.T) {
	provider := &acpMCPProvider{}
	factory := newRecordingMCPRuntimeFactory()
	backend, store := newACPMCPBackend(t, provider, factory)
	client := &recordingClient{}
	server := acpMCPServer(t, "cancel-session", "unused")
	server.Env = append(server.Env, acp.EnvVariable{Name: "CORELAY_ACP_MCP_BLOCK", Value: "1"})
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: t.TempDir(), MCPServers: []acp.MCPServer{server},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := backend.Prompt(context.Background(), acp.PromptRequest{
			SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "block"}},
		}, client)
		promptDone <- promptErr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		updates, _ := client.snapshot()
		started := false
		for _, update := range updates {
			if update.Update.Status == acp.ToolInProgress {
				started = true
				break
			}
		}
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MCP tool did not enter in_progress")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := backend.CancelSession(context.Background(), acp.CancelNotification{SessionID: created.SessionID}); err != nil {
		t.Fatal(err)
	}
	select {
	case promptErr := <-promptDone:
		if !errors.Is(promptErr, context.Canceled) {
			persisted, loadErr := store.Get(created.SessionID)
			backend.mu.Lock()
			quarantined := backend.sessions[created.SessionID].quarantined
			backend.mu.Unlock()
			t.Fatalf("Prompt cancellation error = %v; load=%v lifecycle=%q reconcile=%v quarantined=%v",
				promptErr, loadErr, persisted.LifecycleStatus, persisted.ReconcileRequired, quarantined)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not drain after cancellation")
	}
	runtimes := factory.snapshot("cancel-session")
	if len(runtimes) != 1 || runtimes[0].closes.Load() != 0 || !runtimes[0].Healthy() {
		t.Fatalf("prompt cancellation closed session MCP runtime: %#v", runtimes)
	}
	if _, err := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: created.SessionID}, client); err != nil {
		t.Fatal(err)
	}
	if closes := runtimes[0].closes.Load(); closes != 1 {
		t.Fatalf("session close calls = %d, want 1", closes)
	}
}

func TestACPCancelDuringMCPInitializationIsTypedAndCancelsLifetime(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	store := agent.NewSessionStore(t.TempDir())
	initializationStarted := make(chan context.Context, 1)
	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a"}}},
		DefaultModel: "model-a",
		Store:        store,
		MCPRuntimeFactory: func(
			ctx context.Context,
			_ string,
			_ []agent.MCPServerSpec,
			execution agent.MCPExecutionOptions,
		) (agent.MCPRuntime, error) {
			initializationStarted <- execution.Context
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingClient{}
	server := acpMCPServer(t, "initializing-session", "unused")
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: t.TempDir(), MCPServers: []acp.MCPServer{server},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := backend.Prompt(context.Background(), acp.PromptRequest{
			SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "cancel init"}},
		}, client)
		promptDone <- promptErr
	}()
	var lifetimeCtx context.Context
	select {
	case lifetimeCtx = <-initializationStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("MCP initialization did not start")
	}
	if lifetimeCtx == nil || lifetimeCtx.Err() != nil {
		t.Fatalf("session lifetime context was not independent at initialization: %v", lifetimeCtx)
	}
	if err := backend.CancelSession(context.Background(), acp.CancelNotification{SessionID: created.SessionID}); err != nil {
		t.Fatal(err)
	}
	select {
	case promptErr := <-promptDone:
		if !errors.Is(promptErr, context.Canceled) {
			t.Fatalf("initialization cancellation error = %v", promptErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MCP initialization did not drain after cancellation")
	}
	select {
	case <-lifetimeCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("failed MCP initialization did not cancel its session lifetime")
	}
	if _, err := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: created.SessionID}, client); err != nil {
		t.Fatal(err)
	}
}

func TestACPReplacesUnhealthyMCPGenerationBeforeNextProviderRequest(t *testing.T) {
	provider := &acpMCPProvider{}
	factory := newRecordingMCPRuntimeFactory()
	backend, _ := newACPMCPBackend(t, provider, factory)
	client := &recordingClient{}
	server := acpMCPServer(t, "restart-session", "restart")
	server.Env = append(server.Env, acp.EnvVariable{Name: "CORELAY_ACP_MCP_EXIT_AFTER_CALL", Value: "1"})
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: t.TempDir(), MCPServers: []acp.MCPServer{server},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	prompt := func(text string) {
		t.Helper()
		if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
			SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: text}},
		}, client); err != nil {
			t.Fatal(err)
		}
	}
	prompt("first")
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtimes := factory.snapshot("restart-session")
		if len(runtimes) == 1 && !runtimes[0].Healthy() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first runtime did not become unhealthy: %#v", runtimes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	firstGeneration := factory.snapshot("restart-session")[0].Generation()
	prompt("second")
	runtimes := factory.snapshot("restart-session")
	if len(runtimes) != 2 || runtimes[0].closes.Load() != 1 ||
		runtimes[1].Generation() == firstGeneration {
		t.Fatalf("stale runtime replacement = %#v", runtimes)
	}
	if _, err := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: created.SessionID}, client); err != nil {
		t.Fatal(err)
	}
	if runtimes[1].closes.Load() != 1 {
		t.Fatalf("replacement close calls = %d", runtimes[1].closes.Load())
	}
}

func TestACPLoadAcceptsAndReplacesSessionMCPConfiguration(t *testing.T) {
	provider := &acpMCPProvider{}
	factory := newRecordingMCPRuntimeFactory()
	backend, store := newACPMCPBackend(t, provider, factory)
	client := &recordingClient{}
	workspace := t.TempDir()
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: workspace, MCPServers: []acp.MCPServer{},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID: created.SessionID, CWD: workspace,
		MCPServers: []acp.MCPServer{acpMCPServer(t, "load-one", "one")},
	}, client); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "one"}},
	}, client); err != nil {
		t.Fatal(err)
	}
	first := factory.snapshot("load-one")
	if len(first) != 1 || first[0].closes.Load() != 0 {
		t.Fatalf("first loaded runtime = %#v", first)
	}
	if _, err := backend.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID: created.SessionID, CWD: workspace,
		MCPServers: []acp.MCPServer{acpMCPServer(t, "load-two", "two")},
	}, client); err != nil {
		t.Fatal(err)
	}
	if first[0].closes.Load() != 1 {
		t.Fatalf("replaced Load runtime close calls = %d", first[0].closes.Load())
	}
	if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "two"}},
	}, client); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	foundTwo := false
	for _, message := range persisted.Messages {
		if message.Role == "tool" && message.Content == "two" {
			foundTwo = true
		}
	}
	if !foundTwo {
		t.Fatalf("loaded replacement did not execute: %#v", persisted.Messages)
	}
	second := factory.snapshot("load-two")
	if len(second) != 1 || second[0].closes.Load() != 0 {
		t.Fatalf("second loaded runtime = %#v", second)
	}
	if _, err := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: created.SessionID}, client); err != nil {
		t.Fatal(err)
	}
	if second[0].closes.Load() != 1 {
		t.Fatalf("second loaded runtime close calls = %d", second[0].closes.Load())
	}
}

type inertMCPRuntime struct{ closes atomic.Int32 }

func (*inertMCPRuntime) ToolDefs() []types.ToolDef           { return nil }
func (*inertMCPRuntime) ServerCount() int                    { return 1 }
func (*inertMCPRuntime) Generation() string                  { return "inert-generation" }
func (*inertMCPRuntime) Healthy() bool                       { return true }
func (*inertMCPRuntime) Reports() []processsupervisor.Report { return nil }
func (r *inertMCPRuntime) Close()                            { r.closes.Add(1) }

func TestACPClientMCPRawSpecsRemainMemoryOnly(t *testing.T) {
	const commandSecret = "raw-command-secret"
	const argumentSecret = "raw-argument-secret"
	const environmentSecret = "raw-environment-secret"
	var captured agent.RunOptions
	runtime := &inertMCPRuntime{}
	store := agent.NewSessionStore(t.TempDir())
	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a"}}},
		DefaultModel: "model-a", Store: store,
		Runner: RunnerFunc(func(
			_ context.Context, _ types.Provider, _ string, _ []types.Message, _ string, options agent.RunOptions,
		) (<-chan agent.Event, error) {
			captured = options
			return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
		}),
		MCPRuntimeFactory: func(
			context.Context, string, []agent.MCPServerSpec, agent.MCPExecutionOptions,
		) (agent.MCPRuntime, error) {
			return runtime, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingClient{}
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: t.TempDir(), MCPServers: []acp.MCPServer{{
			Name: "private-server", Command: commandSecret,
			Args: []string{argumentSecret},
			Env:  []acp.EnvVariable{{Name: "VISIBLE", Value: environmentSecret}},
		}},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "safe"}},
	}, client); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	updates, permissions := client.snapshot()
	wire, _ := json.Marshal(struct {
		Session     *agent.Session
		Updates     []acp.SessionNotification
		Permissions []acp.RequestPermissionRequest
	}{persisted, updates, permissions})
	for _, secret := range []string{commandSecret, argumentSecret, environmentSecret} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("raw MCP spec leaked to durable/outbound data: %q in %s", secret, wire)
		}
	}
	if captured.MCPRuntime != runtime || captured.MCPServers != nil || !captured.DisableWorkspaceMCP {
		t.Fatalf("runner did not receive only the borrowed runtime: %#v", captured)
	}
	if _, err := backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: created.SessionID}, client); err != nil {
		t.Fatal(err)
	}
	if runtime.closes.Load() != 1 {
		t.Fatalf("inert runtime close calls = %d", runtime.closes.Load())
	}
}

func TestACPRejectsUnsupportedAndDuplicateMCPDeclarations(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(agent.Event{Type: "done"}))
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	valid := acp.MCPServer{Name: "valid", Command: executable, Args: []string{}, Env: []acp.EnvVariable{}}
	tests := []struct {
		name    string
		servers []acp.MCPServer
	}{
		{name: "HTTP is capability gated", servers: []acp.MCPServer{{Type: "http", Name: "remote", URL: "https://example.invalid", Headers: []acp.HTTPHeader{}}}},
		{name: "duplicate server", servers: []acp.MCPServer{valid, valid}},
		{name: "duplicate environment", servers: []acp.MCPServer{{
			Name: "env", Command: executable, Args: []string{},
			Env: []acp.EnvVariable{{Name: "VISIBLE", Value: "a"}, {Name: "VISIBLE", Value: "b"}},
		}}},
		{name: "nil argv", servers: []acp.MCPServer{{Name: "nil", Command: executable, Env: []acp.EnvVariable{}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.backend.NewSession(context.Background(), acp.NewSessionRequest{
				CWD: fixture.workspace, MCPServers: test.servers,
			}, fixture.client)
			var rpcErr *acp.RPCError
			if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeInvalidParams {
				t.Fatalf("NewSession() error = %#v", err)
			}
		})
	}
}
