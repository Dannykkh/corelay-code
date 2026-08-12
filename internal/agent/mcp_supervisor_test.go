package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type countingMCPRunner struct {
	mu    sync.Mutex
	calls int
	specs []processsupervisor.Spec
}

func (r *countingMCPRunner) Name() string { return "fake" }
func (r *countingMCPRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{ProcessIsolation: true, EnvironmentFiltering: true, Timeouts: true}
}
func (r *countingMCPRunner) Start(_ context.Context, _ sandbox.Policy, spec processsupervisor.Spec) (*processsupervisor.Process, processsupervisor.Report) {
	r.mu.Lock()
	r.calls++
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return nil, processsupervisor.Report{}
}
func (r *countingMCPRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *countingMCPRunner) snapshotSpecs() []processsupervisor.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]processsupervisor.Spec(nil), r.specs...)
}

func TestMCPWithOptionsFailsBeforeStartForMissingOrDisabledConfiguration(t *testing.T) {
	runner := &countingMCPRunner{}
	executable := mcpHelperExecutable(t)
	tests := []MCPExecutionOptions{
		{},
		{Runner: runner},
		{Runner: runner, Policy: sandbox.Policy{Enforcement: sandbox.EnforcementDisabled}},
	}
	for index, options := range tests {
		if _, err := NewMCPClientWithOptions("test", executable, nil, t.TempDir(), nil, options); err == nil {
			t.Fatalf("case %d: expected fail-closed error", index)
		}
	}
	if runner.count() != 0 {
		t.Fatalf("runner starts = %d, want 0", runner.count())
	}
}

func TestMCPDefaultSecureAdapterReportsUnavailableWithoutStarting(t *testing.T) {
	var observed processsupervisor.Report
	options := DefaultMCPExecutionOptions(context.Background())
	options.ObserveStart = func(report processsupervisor.Report) { observed = report }
	if _, err := NewMCPClientWithOptions("test", mcpHelperExecutable(t), nil, t.TempDir(), nil, options); err == nil {
		t.Fatal("expected unavailable adapter error")
	}
	if observed.Started || observed.Failure != sandbox.FailureRunnerUnavailable || observed.Policy.Enforcement != sandbox.EnforcementPreferred {
		t.Fatalf("observed report = %+v", observed)
	}
}

func TestMCPClientResolvesExecutableIdentityBeforeRunnerStart(t *testing.T) {
	runner := &countingMCPRunner{}
	options := MCPExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy: sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
	}
	if _, err := NewMCPClientWithOptions(
		"missing", "corelaycode-mcp-command-that-does-not-exist", nil, t.TempDir(), nil, options,
	); err == nil {
		t.Fatal("missing executable identity was accepted")
	}
	if runner.count() != 0 {
		t.Fatalf("runner starts after resolution failure = %d, want 0", runner.count())
	}

	executable := mcpHelperExecutable(t)
	t.Setenv("PATH", filepath.Dir(executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := NewMCPClientWithOptions(
		"path", filepath.Base(executable), nil, t.TempDir(), nil, options,
	); err == nil {
		t.Fatal("recording unavailable runner unexpectedly started an MCP client")
	}
	specs := runner.snapshotSpecs()
	resolved, err := resolveMCPExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || !sameMCPExecutable(specs[0].Executable, resolved) {
		t.Fatalf("runner executable specs = %#v, want canonical %q", specs, resolved)
	}
}

func TestMCPDefaultSecureOptionsSelectPlatformAdapterAndTruthfulPolicy(t *testing.T) {
	workspace := t.TempDir()
	options := DefaultMCPExecutionOptions(context.Background(), workspace)
	if options.Runner == nil {
		t.Fatal("default runner is nil")
	}
	if options.Runner.Name() != "auto" {
		t.Fatalf("runner=%T name=%q", options.Runner, options.Runner.Name())
	}
	if options.Policy.Enforcement != sandbox.EnforcementPreferred ||
		!options.Policy.Required.ProcessTreeKill ||
		!options.Policy.Required.EnvironmentFiltering ||
		!options.Policy.Required.Timeouts {
		t.Fatalf("policy=%#v", options.Policy)
	}
	capabilities := options.Runner.Capabilities()
	if options.Policy.Required.ProcessIsolation != capabilities.ProcessIsolation {
		t.Fatalf("process isolation policy=%v capability=%v", options.Policy.Required.ProcessIsolation, capabilities.ProcessIsolation)
	}
	if capabilities.FilesystemIsolation {
		if options.Policy.Workspace != workspace || options.Policy.WorkspaceAccess != sandbox.WorkspaceReadWrite {
			t.Fatalf("filesystem policy=%#v", options.Policy)
		}
	} else if options.Policy.Workspace != "" || options.Policy.WorkspaceAccess != sandbox.WorkspaceAccessUnspecified {
		t.Fatalf("adapter without filesystem isolation received workspace claim: %#v", options.Policy)
	}
}

func TestMCPLegacyHostExecutionRemainsExplicitlyDisabledOnly(t *testing.T) {
	options := LegacyDisabledMCPExecutionOptions(context.Background())
	if _, ok := options.Runner.(*processsupervisor.HostRunner); !ok || options.Policy.Enforcement != sandbox.EnforcementDisabled {
		t.Fatalf("legacy options=%#v runner=%T", options.Policy, options.Runner)
	}
}

func TestConnectMCPServersReportsSecureStartFailuresDeterministically(t *testing.T) {
	workspace := t.TempDir()
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"zeta":  map[string]string{"command": mcpHelperExecutable(t)},
		"alpha": map[string]string{"command": mcpHelperExecutable(t)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := &MCPClient{
		name: "stale-alpha", running: true, pending: make(map[int64]chan mcpRPCResult), done: make(chan struct{}),
	}
	mcpClientsMu.Lock()
	mcpClients["alpha"] = stale
	mcpClientsMu.Unlock()
	runner := &countingMCPRunner{}
	connected, err := ConnectMCPServersWithOptions(workspace, MCPExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy: sandbox.Policy{Enforcement: sandbox.EnforcementRequired},
	})
	if connected != 0 || err == nil || !strings.Contains(err.Error(), "alpha, zeta") {
		t.Fatalf("connected=%d error=%v", connected, err)
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls=%d", runner.count())
	}
	mcpClientsMu.RLock()
	_, staleStillRegistered := mcpClients["alpha"]
	mcpClientsMu.RUnlock()
	if staleStillRegistered {
		t.Fatal("failed secure replacement left stale MCP client registered")
	}
	select {
	case <-stale.done:
	default:
		t.Fatal("failed secure replacement did not close stale MCP client")
	}
}

func TestMCPRejectsSensitiveConfiguredEnvironmentBeforeStart(t *testing.T) {
	_, _, err := startMCPProcess(
		LegacyDisabledMCPExecutionOptions(context.Background()),
		"must-not-run",
		nil,
		t.TempDir(),
		map[string]string{"SERVICE_API_KEY": "secret"},
	)
	if err == nil || !strings.Contains(err.Error(), "sensitive environment") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPReaderDispatchesConcurrentResponsesByID(t *testing.T) {
	client, serverReader, serverWriter := newPipeMCPClient(t, time.Second)
	results := make(chan string, 2)
	go func() {
		result, err := client.callContext(context.Background(), "first", nil)
		results <- rpcTestResult(result, err)
	}()
	go func() {
		result, err := client.callContext(context.Background(), "second", nil)
		results <- rpcTestResult(result, err)
	}()

	requests := make([]jsonRPCRequest, 0, 2)
	for len(requests) < 2 {
		line, err := serverReader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var request jsonRPCRequest
		if err := json.Unmarshal(line, &request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, request)
	}
	for i := len(requests) - 1; i >= 0; i-- {
		_, _ = io.WriteString(serverWriter, `{"jsonrpc":"2.0","id":`+jsonNumber(requests[i].ID)+`,"result":"`+requests[i].Method+`"}`+"\n")
	}

	got := []string{<-results, <-results}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "first") || !strings.Contains(joined, "second") {
		t.Fatalf("results = %#v", got)
	}
}

func TestMCPCallCancellationRemovesPendingWithoutRedirectingLateResponse(t *testing.T) {
	client, serverReader, serverWriter := newPipeMCPClient(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.callContext(ctx, "slow", nil)
		done <- err
	}()
	line, err := serverReader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var request jsonRPCRequest
	_ = json.Unmarshal(line, &request)
	if err := <-done; err == nil {
		t.Fatal("expected context deadline")
	}
	client.pendingMu.Lock()
	pending := len(client.pending)
	client.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending calls = %d, want 0", pending)
	}
	_, _ = io.WriteString(serverWriter, `{"jsonrpc":"2.0","id":`+jsonNumber(request.ID)+`,"result":"late"}`+"\n")
}

func TestMCPFramingDepthAndIDValidation(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("123456\n"))
	if _, err := readMCPFrame(reader, 5); err == nil {
		t.Fatal("expected frame limit error")
	}
	if err := validateMCPJSONDepth([]byte(`[[[{}]]]`), 3); err == nil {
		t.Fatal("expected JSON depth error")
	}
	for _, invalid := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`"1"`), json.RawMessage(`0`), json.RawMessage(`1.5`)} {
		if _, err := parseMCPResponseID(invalid); err == nil {
			t.Fatalf("ID %s should be rejected", invalid)
		}
	}
}

func TestMCPRejectsDuplicateEnvelopeAndUnsolicitedFlood(t *testing.T) {
	client := &MCPClient{pending: make(map[int64]chan mcpRPCResult)}
	if err := client.dispatchFrame([]byte(`{"jsonrpc":"2.0","id":1,"id":2,"result":{}}`)); err == nil {
		t.Fatal("duplicate response ID should be rejected")
	}
	frame := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
	for index := 0; index < maxMCPUnsolicitedFrames; index++ {
		if err := client.dispatchFrame(frame); err != nil {
			t.Fatalf("notification %d failed early: %v", index, err)
		}
	}
	if err := client.dispatchFrame(frame); err == nil {
		t.Fatal("unsolicited notification flood should fail closed")
	}
}

func TestMCPManualClientUsesBoundedDefaultsWithoutRelaxingExplicitLimits(t *testing.T) {
	client := &MCPClient{}
	if got := client.effectiveMaxFrameBytes(); got != defaultMCPFrameBytes {
		t.Fatalf("zero-value frame limit = %d, want %d", got, defaultMCPFrameBytes)
	}
	if got := client.effectiveMaxJSONDepth(); got != defaultMCPJSONDepth {
		t.Fatalf("zero-value JSON depth = %d, want %d", got, defaultMCPJSONDepth)
	}
	if got := client.effectiveCallTimeout(); got != defaultMCPCallTimeout {
		t.Fatalf("zero-value call timeout = %s, want %s", got, defaultMCPCallTimeout)
	}

	client.maxFrameBytes = 7
	client.maxJSONDepth = 1
	client.callTimeout = time.Millisecond
	if got := client.effectiveMaxFrameBytes(); got != 7 {
		t.Fatalf("explicit frame limit = %d, want 7", got)
	}
	if got := client.effectiveMaxJSONDepth(); got != 1 {
		t.Fatalf("explicit JSON depth = %d, want 1", got)
	}
	if got := client.effectiveCallTimeout(); got != time.Millisecond {
		t.Fatalf("explicit call timeout = %s, want %s", got, time.Millisecond)
	}
	if err := validateMCPJSONDepth([]byte(`{"nested":{}}`), client.effectiveMaxJSONDepth()); err == nil {
		t.Fatal("explicit JSON depth limit was relaxed")
	}
}

func TestMCPPendingLimitAndShutdownCleanup(t *testing.T) {
	client, _, _ := newPipeMCPClient(t, time.Second)
	for id := int64(1); id <= maxMCPPendingCalls; id++ {
		if err := client.addPending(id, make(chan mcpRPCResult, 1)); err != nil {
			t.Fatalf("add pending %d: %v", id, err)
		}
	}
	if err := client.addPending(maxMCPPendingCalls+1, make(chan mcpRPCResult, 1)); err == nil {
		t.Fatal("expected pending call limit")
	}
	client.shutdown(context.Canceled, false)
	client.pendingMu.Lock()
	pending := len(client.pending)
	client.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending after shutdown = %d", pending)
	}
}

func TestMCPMalformedResponseFailsPendingAndClosesGeneration(t *testing.T) {
	client, _, serverWriter := newPipeMCPClient(t, time.Second)
	waiter := make(chan mcpRPCResult, 1)
	if err := client.addPending(1, waiter); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(serverWriter, `{"jsonrpc":"2.0","id":"1","result":{}}`+"\n")
	select {
	case result := <-waiter:
		if result.err == nil {
			t.Fatal("malformed response should fail pending call")
		}
	case <-time.After(time.Second):
		t.Fatal("pending call was not failed")
	}
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("malformed response did not close generation")
	}
}

func TestMCPStderrFloodClosesGeneration(t *testing.T) {
	client, _, _ := newPipeMCPClient(t, time.Second)
	client.drainStderr(strings.NewReader(strings.Repeat("x", maxMCPDiagnosticBytes+1)))
	select {
	case <-client.done:
	default:
		t.Fatal("stderr flood did not close generation")
	}
}

func newPipeMCPClient(t *testing.T, timeout time.Duration) (*MCPClient, *bufio.Reader, io.WriteCloser) {
	t.Helper()
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	client := &MCPClient{
		name: "pipe", stdin: requestWriter, stdout: bufio.NewReader(responseReader), running: true,
		pending: make(map[int64]chan mcpRPCResult), done: make(chan struct{}), callTimeout: timeout,
		maxFrameBytes: defaultMCPFrameBytes, maxJSONDepth: defaultMCPJSONDepth,
	}
	go client.readLoop(client.stdout)
	t.Cleanup(func() {
		client.Close()
		_ = requestReader.Close()
		_ = responseWriter.Close()
	})
	return client, bufio.NewReader(requestReader), responseWriter
}

func rpcTestResult(result json.RawMessage, err error) string {
	if err != nil {
		return "error:" + err.Error()
	}
	var value string
	_ = json.Unmarshal(result, &value)
	return value
}

func jsonNumber(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
