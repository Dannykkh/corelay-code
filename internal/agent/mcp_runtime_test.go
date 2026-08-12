package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// secureTestMCPRunner is an in-process test adapter. It advertises an isolated
// boundary to exercise MCP composition, while delegating process mechanics to
// HostRunner only inside tests. Production never receives this adapter.
type secureTestMCPRunner struct {
	starts atomic.Int32
	mu     sync.Mutex
	specs  []processsupervisor.Spec
}

func (*secureTestMCPRunner) Name() string { return "test-secure-mcp" }
func (*secureTestMCPRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		ProcessIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}
func (r *secureTestMCPRunner) Start(
	ctx context.Context,
	policy sandbox.Policy,
	spec processsupervisor.Spec,
) (*processsupervisor.Process, processsupervisor.Report) {
	r.starts.Add(1)
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	host := processsupervisor.NewHostRunner()
	hostCaps := host.Capabilities()
	hostPolicy := sandbox.Policy{
		Enforcement: sandbox.EnforcementDisabled,
		Required: sandbox.Capabilities{
			ProcessTreeKill:      hostCaps.ProcessTreeKill,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
	process, report := host.Start(ctx, hostPolicy, spec)
	report.Runner = r.Name()
	report.Policy = policy
	report.Capabilities = r.Capabilities()
	if report.Started {
		report.Applied = r.Capabilities()
	}
	return process, report
}

func (r *secureTestMCPRunner) snapshotSpecs() []processsupervisor.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]processsupervisor.Spec(nil), r.specs...)
}

func secureTestMCPExecution(ctx context.Context) MCPExecutionOptions {
	runner := &secureTestMCPRunner{}
	return MCPExecutionOptions{
		Context: ctx,
		Runner:  runner,
		Policy: sandbox.Policy{
			Enforcement: sandbox.EnforcementPreferred,
			Required:    runner.Capabilities(),
		},
		CallTimeout: 5 * time.Second,
	}
}

func mcpHelperExecutable(t *testing.T) string {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func mcpHelperSpec(t *testing.T, name, result string) MCPServerSpec {
	t.Helper()
	return MCPServerSpec{
		Name: name, Command: mcpHelperExecutable(t),
		Args: []string{"-test.run=^TestRunLoopMCPHelper$"},
		Env: map[string]string{
			"CORELAY_MCP_HELPER": "1",
			"CORELAY_MCP_RESULT": result,
		},
	}
}

func executeRuntimeTool(
	t *testing.T,
	runtime MCPRuntime,
	ctx context.Context,
) (string, bool, json.RawMessage) {
	t.Helper()
	bound := boundRuntimeToolInput(t, runtime)
	result, failed := ExecuteToolWithOptions(
		"mcp_fixture_echo", bound, t.TempDir(), ToolExecutionOptions{Context: ctx},
	)
	return result, failed, bound
}

func boundRuntimeToolInput(t *testing.T, runtime MCPRuntime) json.RawMessage {
	t.Helper()
	tools := append(StaticToolDefs(t.TempDir()), runtime.ToolDefs()...)
	allowed := toolCatalogNamesForRun(tools)
	if _, err := snapshotForAllowedTools(allowed); err != nil {
		t.Fatalf("runtime catalog: %v", err)
	}
	identity, err := executorIdentityForAllowedTool(allowed, "mcp_fixture_echo")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindToolExecutionInput(json.RawMessage(`{"value":"hello"}`), identity)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestRunOwnedMCPRuntimeBindsCatalogToExactGenerationAndIsolatesGlobalState(t *testing.T) {
	DisconnectAllMCP()
	t.Cleanup(DisconnectAllMCP)
	workspace := t.TempDir()
	executionOne := secureTestMCPExecution(context.Background())
	runtimeOne, err := NewMCPRuntime(
		context.Background(), workspace,
		[]MCPServerSpec{mcpHelperSpec(t, "session-one", "one")}, executionOne,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeOne.Close()

	// A process-global client claiming the same name must not participate in a
	// run-owned catalog or redirect its bound executor.
	mcpClientsMu.Lock()
	mcpClients["global-leak"] = &MCPClient{
		name: "global-leak", executorID: newMCPExecutorID(),
		tools:   []MCPTool{{Name: "mcp_fixture_echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		pending: make(map[int64]chan mcpRPCResult), done: make(chan struct{}),
	}
	mcpClientsMu.Unlock()

	result, failed, boundOne := executeRuntimeTool(t, runtimeOne, context.Background())
	if failed || result != "one" {
		t.Fatalf("session one tool = (%q, %v)", result, failed)
	}

	executionTwo := secureTestMCPExecution(context.Background())
	runtimeTwo, err := NewMCPRuntime(
		context.Background(), workspace,
		[]MCPServerSpec{mcpHelperSpec(t, "session-two", "two")}, executionTwo,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeTwo.Close()
	result, failed, _ = executeRuntimeTool(t, runtimeTwo, context.Background())
	if failed || result != "two" {
		t.Fatalf("session two tool = (%q, %v)", result, failed)
	}
	result, failed = ExecuteToolWithOptions(
		"mcp_fixture_echo", boundOne, workspace, ToolExecutionOptions{Context: context.Background()},
	)
	if failed || result != "one" {
		t.Fatalf("session one binding was redirected = (%q, %v)", result, failed)
	}

	runtimeOne.Close()
	runtimeOne.Close()
	result, failed = ExecuteToolWithOptions(
		"mcp_fixture_echo", boundOne, workspace, ToolExecutionOptions{Context: context.Background()},
	)
	if !failed || !strings.Contains(result, "runtime is closed") {
		t.Fatalf("closed generation result = (%q, %v)", result, failed)
	}
}

func TestRunOwnedMCPRuntimeRejectsDuplicateToolsAndResolvesPATHExecutable(t *testing.T) {
	workspace := t.TempDir()
	runner := &secureTestMCPRunner{}
	execution := MCPExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy: sandbox.Policy{Enforcement: sandbox.EnforcementPreferred, Required: runner.Capabilities()},
	}
	_, err := NewMCPRuntime(context.Background(), workspace, []MCPServerSpec{
		mcpHelperSpec(t, "alpha", "a"), mcpHelperSpec(t, "beta", "b"),
	}, execution)
	if err == nil || !strings.Contains(err.Error(), "claimed by both") || runner.starts.Load() != 2 {
		t.Fatalf("duplicate tool result = (%v, starts=%d)", err, runner.starts.Load())
	}

	commandName := filepath.Base(mcpHelperExecutable(t))
	pathDir := filepath.Dir(mcpHelperExecutable(t))
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner = &secureTestMCPRunner{}
	execution.Runner = runner
	execution.Policy.Required = runner.Capabilities()
	runtime, err := NewMCPRuntime(context.Background(), workspace, []MCPServerSpec{{
		Name: "path-command", Command: commandName,
		Args: []string{"-test.run=^TestRunLoopMCPHelper$"},
		Env:  map[string]string{"CORELAY_MCP_HELPER": "1"},
	}}, execution)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	specs := runner.snapshotSpecs()
	expectedExecutable, resolveErr := resolveMCPExecutable(mcpHelperExecutable(t))
	if len(specs) != 1 || !filepath.IsAbs(specs[0].Executable) ||
		resolveErr != nil || !sameMCPExecutable(specs[0].Executable, expectedExecutable) {
		t.Fatalf("resolved executable = %#v, want %q (%v)", specs, expectedExecutable, resolveErr)
	}
}

func TestRunOwnedMCPRuntimeCancellationAndCloseAreBounded(t *testing.T) {
	workspace := t.TempDir()
	runCtx, cancel := context.WithCancel(context.Background())
	spec := mcpHelperSpec(t, "blocking", "unused")
	spec.Env["CORELAY_MCP_BLOCK"] = "1"
	execution := secureTestMCPExecution(runCtx)
	runtime, err := NewMCPRuntime(runCtx, workspace, []MCPServerSpec{spec}, execution)
	if err != nil {
		t.Fatal(err)
	}
	bound := boundRuntimeToolInput(t, runtime)
	executionDir := t.TempDir()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ExecuteToolWithOptions(
			"mcp_fixture_echo", bound, executionDir, ToolExecutionOptions{Context: runCtx},
		)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("MCP tool call did not stop after cancellation")
	}
	runtime.Close()
	runtime.Close()
}

func TestRunLoopRejectsRunOwnedMCPCatalogCollisionBeforeProvider(t *testing.T) {
	isolateEvidenceLoopTest(t)
	spec := mcpHelperSpec(t, "reserved-collision", "unused")
	spec.Env["CORELAY_MCP_TOOL_NAME"] = "Read"
	execution := secureTestMCPExecution(context.Background())
	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{{visible: "must-not-run"}}}
	events := make(chan Event, 32)
	go RunLoopWithOptions(
		context.Background(), provider, "mcp-collision-model",
		[]types.Message{{Role: "user", Content: mustJSON("hello")}}, t.TempDir(),
		RunOptions{
			DisableWorkspaceMCP: true, MCPServers: []MCPServerSpec{spec}, MCPExecution: &execution,
		},
		events,
	)
	var failure string
	for event := range events {
		if event.Type == "error" {
			failure, _ = event.Data.(string)
		}
	}
	if calls := len(provider.requestsSnapshot()); calls != 0 || !strings.Contains(failure, "reserved built-in") {
		t.Fatalf("provider calls=%d failure=%q", calls, failure)
	}
}

type mcpFailureRecorder struct {
	mu        sync.Mutex
	failures  []string
	receipts  []AgentReceipt
	summaries []RunSummary
}

func (*mcpFailureRecorder) RunStarted() {}
func (r *mcpFailureRecorder) ReceiptWritten(_ string, receipt AgentReceipt) {
	r.mu.Lock()
	r.receipts = append(r.receipts, receipt)
	r.mu.Unlock()
}
func (r *mcpFailureRecorder) RunCompleted(summary RunSummary) {
	r.mu.Lock()
	r.summaries = append(r.summaries, summary)
	r.mu.Unlock()
}
func (r *mcpFailureRecorder) RunFailed(message string) {
	r.mu.Lock()
	r.failures = append(r.failures, message)
	r.mu.Unlock()
}

func TestRunLoopNeverLeaksRawMCPFactorySpecs(t *testing.T) {
	isolateEvidenceLoopTest(t)
	const commandSecret = "raw-command-secret"
	const argumentSecret = "raw-argument-secret"
	const environmentSecret = "raw-environment-secret"
	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{{visible: "must-not-run"}}}
	recorder := &mcpFailureRecorder{}
	var logs bytes.Buffer
	originalLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(originalLogWriter)
	events := make(chan Event, 32)
	go RunLoopWithOptions(
		context.Background(), provider, "mcp-secret-model",
		[]types.Message{{Role: "user", Content: mustJSON("hello")}}, t.TempDir(),
		RunOptions{
			Recorder: recorder, DisableWorkspaceMCP: true,
			MCPServers: []MCPServerSpec{{
				Name: "secret-fixture", Command: commandSecret,
				Args: []string{argumentSecret}, Env: map[string]string{"VISIBLE": environmentSecret},
			}},
			MCPRuntimeFactory: func(
				context.Context, string, []MCPServerSpec, MCPExecutionOptions,
			) (MCPRuntime, error) {
				return nil, errors.New(commandSecret + argumentSecret + environmentSecret)
			},
		},
		events,
	)
	var wire strings.Builder
	for event := range events {
		encoded, _ := json.Marshal(event)
		wire.Write(encoded)
	}
	recorder.mu.Lock()
	for _, failure := range recorder.failures {
		wire.WriteString(failure)
	}
	if durable, marshalErr := json.Marshal(struct {
		Receipts  []AgentReceipt `json:"receipts"`
		Summaries []RunSummary   `json:"summaries"`
	}{recorder.receipts, recorder.summaries}); marshalErr == nil {
		wire.Write(durable)
	}
	recorder.mu.Unlock()
	wire.Write(logs.Bytes())
	for _, secret := range []string{commandSecret, argumentSecret, environmentSecret} {
		if strings.Contains(wire.String(), secret) {
			t.Fatalf("raw MCP spec leaked: %q in %s", secret, wire.String())
		}
	}
	if calls := len(provider.requestsSnapshot()); calls != 0 {
		t.Fatalf("provider called %d times after MCP setup failure", calls)
	}
}
