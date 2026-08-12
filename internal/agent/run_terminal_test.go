package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type terminalLifecycleRecorder struct {
	mu        sync.Mutex
	started   int
	completed int
	failed    int
	receipts  int
	failures  []string
	order     []string
	observe   func(string)
}

func (r *terminalLifecycleRecorder) RunStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
	r.order = append(r.order, "recorder-start")
	if r.observe != nil {
		r.observe("recorder-start")
	}
}

func (r *terminalLifecycleRecorder) ReceiptWritten(string, AgentReceipt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts++
	r.order = append(r.order, "receipt")
	if r.observe != nil {
		r.observe("receipt")
	}
}

func (r *terminalLifecycleRecorder) RunCompleted(RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed++
	r.order = append(r.order, "recorder-completed")
	if r.observe != nil {
		r.observe("recorder-completed")
	}
}

func (r *terminalLifecycleRecorder) RunFailed(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed++
	r.failures = append(r.failures, message)
	r.order = append(r.order, "recorder-failed")
	if r.observe != nil {
		r.observe("recorder-failed")
	}
}

func (r *terminalLifecycleRecorder) snapshot() terminalLifecycleRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return terminalLifecycleRecorder{
		started: r.started, completed: r.completed, failed: r.failed,
		receipts: r.receipts, failures: append([]string(nil), r.failures...),
		order: append([]string(nil), r.order...),
	}
}

func TestRunTerminalFinalizerIdempotentOrderingAndReceiptBound(t *testing.T) {
	isolateEvidenceLoopTest(t)
	var sequenceMu sync.Mutex
	var sequence []string
	observe := func(value string) {
		sequenceMu.Lock()
		defer sequenceMu.Unlock()
		sequence = append(sequence, value)
	}
	recorder := &terminalLifecycleRecorder{observe: observe}
	events := make(chan Event)
	received := make(chan Event, 2)
	go func() {
		for event := range events {
			observe("event-" + event.Type)
			received <- event
		}
		close(received)
	}()

	var lifecycle []string
	finalizer := newRunTerminalFinalizer(events, recorder)
	finalizer.BeginHookSession(
		func() {
			lifecycle = append(lifecycle, "hook-start")
			observe("hook-start")
		},
		func() {
			lifecycle = append(lifecycle, "hook-end")
			observe("hook-end")
		},
		func() []hooks.HookResult { return nil },
	)
	receipt := AgentReceipt{Provider: "fixture", Model: "fixture", ProjectType: "fixture"}
	if _, err := finalizer.WriteReceipt(t.TempDir(), receipt); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	if _, err := finalizer.WriteReceipt(t.TempDir(), receipt); !errors.Is(err, errRunReceiptAlreadyAttempted) {
		t.Fatalf("second receipt error = %v, want %v", err, errRunReceiptAlreadyAttempted)
	}
	finalizer.Complete(
		RunTerminalCompleted,
		"end_turn",
		RunTerminalDurableCommit,
		map[string]interface{}{"terminalState": EvidenceTerminalUnverified},
		RunSummary{},
	)
	finalizer.Finalize()
	finalizer.Finalize()

	var terminalEvents []Event
	for event := range received {
		terminalEvents = append(terminalEvents, event)
	}
	if len(terminalEvents) != 1 || terminalEvents[0].Type != "done" {
		t.Fatalf("terminal events = %#v", terminalEvents)
	}
	if got := lifecycle; fmt.Sprint(got) != "[hook-start hook-end]" {
		t.Fatalf("hook lifecycle = %v", got)
	}
	snapshot := recorder.snapshot()
	if snapshot.started != 1 || snapshot.completed != 1 || snapshot.failed != 0 || snapshot.receipts != 1 {
		t.Fatalf("recorder counts = start:%d complete:%d fail:%d receipt:%d", snapshot.started, snapshot.completed, snapshot.failed, snapshot.receipts)
	}
	if fmt.Sprint(snapshot.order) != "[recorder-start receipt recorder-completed]" {
		t.Fatalf("recorder order = %v", snapshot.order)
	}
	sequenceMu.Lock()
	defer sequenceMu.Unlock()
	if fmt.Sprint(sequence) != "[recorder-start hook-start receipt hook-end recorder-completed event-done]" {
		t.Fatalf("terminal ordering = %v", sequence)
	}
}

func TestRunLoopCommonTerminalTable(t *testing.T) {
	type fixture struct {
		ctx      context.Context
		provider types.Provider
		input    string
		opts     RunOptions
		afterRun func()
	}
	type testCase struct {
		name         string
		setup        func(*testing.T, string) fixture
		wantKind     RunTerminalKind
		wantReason   string
		wantPolicy   RunTerminalDurablePolicy
		wantSuccess  bool
		wantHooks    int
		wantReceipts int
	}

	validProfile := func(t *testing.T, id string, maxIterations int, response harness.ResponsePolicy, routing harness.ToolRoutingPolicy) harness.HarnessProfile {
		t.Helper()
		return harness.MustResolveProfile(harness.ProfileSpec{
			ID:              id,
			MaxIterations:   maxIterations,
			MaxErrorRounds:  3,
			ReadBeforeWrite: harness.SomeBool(false),
			ResponsePolicy:  response,
			ToolRouting:     routing,
		})
	}
	normalProfile := func(t *testing.T, id string) harness.HarnessProfile {
		return validProfile(t, id, 6, harness.ResponseNative, harness.ToolRoutingDirect)
	}
	ambiguous := `<tool_call>{"name":"Read","arguments":{"file_path":"notes.txt"}}</tool_call>` + "\n" +
		"```json\n{\"name\":\"Write\",\"arguments\":{\"file_path\":\"out.txt\",\"content\":\"x\"}}\n```"

	cases := []testCase{
		{
			name: "preflight failure",
			setup: func(_ *testing.T, _ string) fixture {
				invalid := harness.HarnessProfile{}
				return fixture{ctx: context.Background(), provider: &phase2NoCallProvider{}, input: "run", opts: RunOptions{HarnessProfile: &invalid}}
			},
			wantKind: RunTerminalFailed, wantReason: "harness_resolution_failed", wantPolicy: RunTerminalDurableReconcile, wantHooks: 0,
		},
		{
			name: "help command",
			setup: func(t *testing.T, _ string) fixture {
				profile := normalProfile(t, "terminal-help")
				return fixture{ctx: context.Background(), provider: &phase2NoCallProvider{}, input: "/help", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalCommand, wantReason: "help", wantPolicy: RunTerminalDurableNone, wantSuccess: true, wantHooks: 2,
		},
		{
			name: "ui command",
			setup: func(t *testing.T, _ string) fixture {
				profile := normalProfile(t, "terminal-ui")
				return fixture{ctx: context.Background(), provider: &phase2NoCallProvider{}, input: "/clear", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalCommand, wantReason: "ui_command", wantPolicy: RunTerminalDurableNone, wantSuccess: true, wantHooks: 2,
		},
		{
			name: "context blocked",
			setup: func(t *testing.T, _ string) fixture {
				profile := phase2Profile("terminal-context", 16_384, 2_048)
				estimator := &phase2TokenEstimator{estimate: func(ContextEstimateRequest) TokenEstimate {
					return TokenEstimate{InputTokens: 1_000_000, Source: "terminal-test", Confidence: "exact"}
				}}
				return fixture{ctx: context.Background(), provider: &phase2NoCallProvider{}, input: "implement", opts: RunOptions{HarnessProfile: &profile, TokenEstimator: estimator}}
			},
			wantKind: RunTerminalContextBlocked, wantReason: "context_overflow_after_compaction", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
		{
			name: "provider retry failure",
			setup: func(t *testing.T, _ string) fixture {
				profile := normalProfile(t, "terminal-provider-failure")
				return fixture{ctx: context.Background(), provider: &terminalRetryProvider{}, input: "run", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalFailed, wantReason: "provider_retry_exhausted", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
		{
			name: "cancel",
			setup: func(t *testing.T, _ string) fixture {
				profile := normalProfile(t, "terminal-cancel")
				ctx, cancel := context.WithCancel(context.Background())
				provider := newTerminalCancelProvider()
				return fixture{
					ctx: ctx, provider: provider, input: "run", opts: RunOptions{HarnessProfile: &profile},
					afterRun: func() { <-provider.started; cancel() },
				}
			},
			wantKind: RunTerminalCancelled, wantReason: "cancelled", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
		{
			name: "parser exhaustion",
			setup: func(t *testing.T, _ string) fixture {
				profile := validProfile(t, "terminal-parser", 6, harness.ResponseMultiFormat, harness.ToolRoutingDirect)
				provider := &responsePolicyTestProvider{steps: []responsePolicyStep{{visible: ambiguous}, {visible: ambiguous}, {visible: ambiguous}}}
				return fixture{ctx: context.Background(), provider: provider, input: "run", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalFailed, wantReason: "parser_correction_exhausted", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
		{
			name: "selector exhaustion",
			setup: func(t *testing.T, _ string) fixture {
				profile := validProfile(t, "terminal-selector", 6, harness.ResponseNative, harness.ToolRoutingTwoStage)
				steps := make([]responsePolicyStep, defaultToolResponseCorrectionLimit+1)
				for index := range steps {
					steps[index] = responsePolicyStep{nativeName: "SelectToolCategory", nativeID: fmt.Sprintf("selector-%d", index), nativeJSON: `{"category":"unknown"}`}
				}
				return fixture{ctx: context.Background(), provider: &responsePolicyTestProvider{steps: steps}, input: "read", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalFailed, wantReason: "selector_correction_exhausted", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
		{
			name: "normal no edit",
			setup: func(t *testing.T, _ string) fixture {
				profile := normalProfile(t, "terminal-no-edit")
				return fixture{ctx: context.Background(), provider: &scriptedLoopProvider{steps: []scriptedLoopStep{textStep("done")}}, input: "answer", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalCompleted, wantReason: "end_turn", wantPolicy: RunTerminalDurableCommit, wantSuccess: true, wantHooks: 2,
		},
		{
			name: "normal edit",
			setup: func(t *testing.T, workDir string) fixture {
				profile := normalProfile(t, "terminal-edit")
				provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
					toolUseStep("write-1", "Write", map[string]string{"file_path": "result.txt", "content": "ok\n"}),
					textStep("done"),
				}}
				_ = workDir
				return fixture{ctx: context.Background(), provider: provider, input: "write result.txt", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalCompleted, wantReason: "end_turn", wantPolicy: RunTerminalDurableCommit, wantSuccess: true, wantHooks: 2, wantReceipts: 1,
		},
		{
			name: "max iterations",
			setup: func(t *testing.T, workDir string) fixture {
				profile := validProfile(t, "terminal-max", 1, harness.ResponseNative, harness.ToolRoutingDirect)
				return fixture{
					ctx:      context.Background(),
					provider: &scriptedLoopProvider{steps: []scriptedLoopStep{toolUseStep("read-1", "Read", map[string]string{"file_path": "missing.txt"})}},
					input:    "read missing.txt", opts: RunOptions{HarnessProfile: &profile},
				}
			},
			wantKind: RunTerminalMaxIterations, wantReason: "max_iterations", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
		{
			name: "channel close without terminal",
			setup: func(t *testing.T, _ string) fixture {
				profile := normalProfile(t, "terminal-empty-stream")
				return fixture{ctx: context.Background(), provider: &phase2NoCallProvider{}, input: "run", opts: RunOptions{HarnessProfile: &profile}}
			},
			wantKind: RunTerminalNoTerminal, wantReason: "provider_stream_closed", wantPolicy: RunTerminalDurableReconcile, wantHooks: 2,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			isolateEvidenceLoopTest(t)
			t.Setenv("CORELAY_MAX_TOOLS", "30")
			workDir := t.TempDir()
			writeLoopHookConfig(t, workDir, map[string][]map[string]any{
				"session_start": {{"command": "session-start"}},
				"session_end":   {{"command": "session-end"}},
			})
			hookRunner := &loopHookRunner{}
			hookRegistry := hooks.NewRegistryWithOptions(hooks.RegistryOptions{Runner: hookRunner, ShellPath: "hook-shell"})
			recorder := &terminalLifecycleRecorder{}
			fixture := test.setup(t, workDir)
			fixture.opts.HookRegistry = hookRegistry
			fixture.opts.Recorder = recorder
			fixture.opts.DisablePlugins = true
			fixture.opts.DisableWorkspaceMCP = true
			fixture.opts.EvidencePolicy = EvidencePolicyConfig{Policy: EvidencePolicyOff}
			if fixture.ctx == nil {
				fixture.ctx = context.Background()
			}

			eventCh := make(chan Event, 512)
			done := make(chan struct{})
			go func() {
				RunLoopWithOptions(
					fixture.ctx,
					fixture.provider,
					"terminal-model",
					[]types.Message{{Role: "user", Content: mustJSON(fixture.input)}},
					workDir,
					fixture.opts,
					eventCh,
				)
				close(done)
			}()
			if fixture.afterRun != nil {
				fixture.afterRun()
			}
			var events []Event
			for event := range eventCh {
				events = append(events, event)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("RunLoop did not return after closing the event channel")
			}

			if len(events) == 0 || events[len(events)-1].Type != "done" {
				t.Fatalf("last event = %#v", events)
			}
			doneCount := 0
			for _, event := range events {
				if event.Type == "done" {
					doneCount++
				}
			}
			if doneCount != 1 {
				t.Fatalf("done count = %d, events=%s", doneCount, eventDump(events))
			}
			data, ok := events[len(events)-1].Data.(map[string]interface{})
			if !ok {
				t.Fatalf("done data type = %T", events[len(events)-1].Data)
			}
			if data["kind"] != string(test.wantKind) || data["stopReason"] != test.wantReason || data["durablePolicy"] != string(test.wantPolicy) {
				t.Fatalf("terminal metadata = %#v", data)
			}
			metadata, typed := DecodeDurableRunTerminalMetadata(data)
			if test.wantSuccess {
				if typed && metadata.BlocksSuccess() {
					t.Fatalf("successful terminal blocks success: %#v", metadata)
				}
			} else if !typed || !metadata.BlocksSuccess() {
				t.Fatalf("failure terminal is not fail-closed: typed=%v metadata=%#v", typed, metadata)
			}

			recorded := recorder.snapshot()
			wantCompleted, wantFailed := 0, 1
			if test.wantSuccess {
				wantCompleted, wantFailed = 1, 0
			}
			if recorded.started != 1 || recorded.completed != wantCompleted || recorded.failed != wantFailed || recorded.receipts != test.wantReceipts {
				t.Fatalf("recorder = start:%d complete:%d fail:%d receipt:%d failures:%v", recorded.started, recorded.completed, recorded.failed, recorded.receipts, recorded.failures)
			}
			if calls := hookRunner.snapshot(); len(calls) != test.wantHooks {
				t.Fatalf("hook count = %d, want %d: %#v", len(calls), test.wantHooks, calls)
			} else if len(calls) == 2 && (calls[0].command.Args[1] != "session-start" || calls[1].command.Args[1] != "session-end") {
				t.Fatalf("hook order = %#v", calls)
			}
		})
	}
}

type terminalRetryProvider struct {
	mu    sync.Mutex
	calls int
}

func (*terminalRetryProvider) Name() string              { return "terminal-retry" }
func (*terminalRetryProvider) DisplayName() string       { return "terminal-retry" }
func (*terminalRetryProvider) Models() []types.ModelInfo { return nil }
func (*terminalRetryProvider) Validate() error           { return nil }
func (p *terminalRetryProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return nil, errors.New("fixture provider failure")
}

type terminalCancelProvider struct {
	started chan struct{}
	once    sync.Once
}

func newTerminalCancelProvider() *terminalCancelProvider {
	return &terminalCancelProvider{started: make(chan struct{})}
}

func (*terminalCancelProvider) Name() string              { return "terminal-cancel" }
func (*terminalCancelProvider) DisplayName() string       { return "terminal-cancel" }
func (*terminalCancelProvider) Models() []types.ModelInfo { return nil }
func (*terminalCancelProvider) Validate() error           { return nil }
func (p *terminalCancelProvider) StreamMessage(ctx context.Context, _ *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.once.Do(func() { close(p.started) })
	events := make(chan types.SSEEvent)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events, nil
}
