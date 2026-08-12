package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type runModeLoopState struct {
	Starts          int `json:"starts"`
	Advances        int `json:"advances"`
	TotalTools      int `json:"totalTools"`
	EstimatedTokens int `json:"estimatedTokens"`
}

type runModeLoopFake struct {
	mu            sync.Mutex
	name          string
	suffix        string
	start         RunModeDirective
	advances      []RunModeDirective
	repeatAdvance *RunModeDirective
	turns         []RunModeTurn
	starts        int
	stopReason    string
}

func (m *runModeLoopFake) Name() string               { return m.name }
func (m *runModeLoopFake) SystemPromptSuffix() string { return m.suffix }
func (m *runModeLoopFake) CurrentStep() string        { return "fake-mode-step" }

func (m *runModeLoopFake) Start(time.Time) (RunModeDirective, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starts++
	return cloneRunModeLoopDirective(m.start), nil
}

func (m *runModeLoopFake) Advance(turn RunModeTurn) (RunModeDirective, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns = append(m.turns, turn)
	index := len(m.turns) - 1
	var directive RunModeDirective
	switch {
	case index < len(m.advances):
		directive = m.advances[index]
	case m.repeatAdvance != nil:
		directive = *m.repeatAdvance
	default:
		directive = RunModeDirective{TerminalText: "fixture exhausted", StopReason: "fixture_exhausted"}
	}
	if !directive.Continue {
		m.stopReason = directive.StopReason
	}
	return cloneRunModeLoopDirective(directive), nil
}

func (m *runModeLoopFake) Snapshot(metrics RunModeMetrics) RunModeSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return RunModeSnapshot{
		Name:       m.name,
		StopReason: m.stopReason,
		State: runModeLoopState{
			Starts:          m.starts,
			Advances:        len(m.turns),
			TotalTools:      metrics.TotalTools,
			EstimatedTokens: metrics.EstimatedTokens,
		},
	}
}

func (m *runModeLoopFake) observedTurns() []RunModeTurn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]RunModeTurn(nil), m.turns...)
}

func cloneRunModeLoopDirective(value RunModeDirective) RunModeDirective {
	value.Status = append([]string(nil), value.Status...)
	return value
}

func TestRunLoopRunModeStartContinueFinishMetadataParity(t *testing.T) {
	mode := &runModeLoopFake{
		name:   "fake-immutable",
		suffix: "FAKE_MODE_SYSTEM_SUFFIX",
		start: RunModeDirective{
			Continue: true, UserPrompt: "FAKE_MODE_START_PROMPT", Status: []string{"fake mode started"},
		},
		advances: []RunModeDirective{
			{Continue: true, UserPrompt: "FAKE_MODE_CONTINUE_PROMPT", Status: []string{"fake mode continued"}},
			{TerminalText: "FAKE_MODE_TERMINAL_TEXT", StopReason: "fake_mode_finished", Status: []string{"fake mode finished"}},
		},
	}
	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("FIRST_NO_TOOL_TEXT"), nil },
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("SECOND_NO_TOOL_TEXT"), nil },
	}}
	recorder := &completionLoopRecorder{}
	events := runModeLoop(t, provider, "original objective", RunOptions{RunMode: mode, Recorder: recorder})

	requests, providerErrs := provider.snapshot()
	if len(providerErrs) != 0 || len(requests) != 2 {
		t.Fatalf("provider requests/errors = %d/%v\n%s", len(requests), providerErrs, eventDump(events))
	}
	for index, request := range requests {
		if !strings.Contains(completionRequestSystemText(request), "FAKE_MODE_SYSTEM_SUFFIX") {
			t.Fatalf("request %d lost run-mode system suffix", index)
		}
		if !requestHasUserTextPrefix(request, "original objective") ||
			!requestContainsText(request, "FAKE_MODE_START_PROMPT") {
			t.Fatalf("request %d lost start prompt: %#v", index, request.Messages)
		}
	}
	if requestContainsText(requests[0], "FAKE_MODE_CONTINUE_PROMPT") ||
		!requestContainsText(requests[1], "FAKE_MODE_CONTINUE_PROMPT") ||
		!requestContainsText(requests[1], "FIRST_NO_TOOL_TEXT") {
		t.Fatalf("provider history did not preserve start/continue turn:\nfirst=%#v\nsecond=%#v", requests[0].Messages, requests[1].Messages)
	}
	turns := mode.observedTurns()
	if len(turns) != 2 || turns[0].Text != "FIRST_NO_TOOL_TEXT" || turns[0].Iteration != 1 ||
		turns[1].Text != "SECOND_NO_TOOL_TEXT" || turns[1].Iteration != 2 {
		t.Fatalf("mode turns = %#v", turns)
	}
	for _, want := range []string{"fake mode started", "fake mode continued", "fake mode finished", "FAKE_MODE_TERMINAL_TEXT"} {
		if !eventTextContains(events, want) {
			t.Fatalf("event %q missing:\n%s", want, eventDump(events))
		}
	}
	done := completionDoneEvent(t, events)
	doneMode, ok := done["runMode"].(RunModeSnapshot)
	if !ok || done["stopReason"] != "fake_mode_finished" || done["iterations"] != 2 || done["planMode"] != false {
		t.Fatalf("done run-mode metadata = %#v", done)
	}
	state, ok := doneMode.State.(runModeLoopState)
	if !ok || state.Starts != 1 || state.Advances != 2 || state.TotalTools != 0 || state.EstimatedTokens <= 0 {
		t.Fatalf("done run-mode snapshot = %#v", doneMode)
	}
	summaries, _, failures := recorder.snapshot()
	if len(failures) != 0 || len(summaries) != 1 || summaries[0].RunMode == nil ||
		!reflect.DeepEqual(*summaries[0].RunMode, doneMode) || summaries[0].Iterations != 2 {
		t.Fatalf("summary parity = summaries:%#v failures:%v done:%#v", summaries, failures, doneMode)
	}
}

func TestRunLoopIterationLimitCapsHarnessLimit(t *testing.T) {
	repeat := RunModeDirective{Continue: true, UserPrompt: "continue until kernel limit"}
	mode := &runModeLoopFake{
		name: "bounded-fake", start: RunModeDirective{Continue: true, UserPrompt: "start bounded mode"},
		repeatAdvance: &repeat,
	}
	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("iteration one"), nil },
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("iteration two"), nil },
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("must not execute"), nil },
	}}
	recorder := &completionLoopRecorder{}
	events := runModeLoop(t, provider, "bounded objective", RunOptions{
		RunMode: mode, IterationLimit: 2, Recorder: recorder,
	})
	requests, providerErrs := provider.snapshot()
	if len(providerErrs) != 0 || len(requests) != 2 || !eventTextContains(events, "Max iterations reached") {
		t.Fatalf("iteration override requests/errors = %d/%v\n%s", len(requests), providerErrs, eventDump(events))
	}
	if turns := mode.observedTurns(); len(turns) != 2 || turns[1].Iteration != 2 {
		t.Fatalf("iteration-limited turns = %#v", turns)
	}
	summaries, _, failures := recorder.snapshot()
	if len(summaries) != 0 || len(failures) != 1 || failures[0] != "Max iterations reached" {
		t.Fatalf("iteration-limit recorder = summaries:%#v failures:%v", summaries, failures)
	}
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		t.Fatalf("iteration exhaustion did not end with typed done: %s", eventDump(events))
	}
	done, ok := events[len(events)-1].Data.(map[string]interface{})
	if !ok || done["kind"] != string(RunTerminalMaxIterations) || done["stopReason"] != "max_iterations" {
		t.Fatalf("iteration terminal = %#v", events[len(events)-1])
	}
	metadata, typed := DecodeDurableRunTerminalMetadata(done)
	if !typed || !metadata.BlocksSuccess() {
		t.Fatalf("iteration terminal is not fail-closed: typed=%v metadata=%#v", typed, metadata)
	}
}

func TestEffectiveRunIterationLimitNeverWeakensHarness(t *testing.T) {
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID: "run-mode-limit-test", MaxIterations: 4,
	})
	for _, test := range []struct {
		name     string
		override int
		want     int
		wantErr  bool
	}{
		{name: "profile default", override: 0, want: 4},
		{name: "stricter mode cap", override: 2, want: 2},
		{name: "looser mode cap preserves harness", override: 10, want: 4},
		{name: "negative", override: -1, wantErr: true},
		{name: "over maximum", override: harness.MaxIterationsLimit + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := effectiveRunIterationLimit(profile, test.override)
			if (err != nil) != test.wantErr || (!test.wantErr && got != test.want) {
				t.Fatalf("effectiveRunIterationLimit(%d) = %d, %v; want %d, error=%v", test.override, got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestRunLoopInvalidAdvanceDirectiveFailsBeforeNextProviderCall(t *testing.T) {
	mode := &runModeLoopFake{
		name: "invalid-advance", start: RunModeDirective{Continue: true, UserPrompt: "start validly"},
		advances: []RunModeDirective{{Continue: true}},
	}
	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("first response"), nil },
		func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("must not execute"), nil },
	}}
	recorder := &completionLoopRecorder{}
	events := runModeLoop(t, provider, "invalid transition objective", RunOptions{RunMode: mode, Recorder: recorder})
	requests, providerErrs := provider.snapshot()
	if len(providerErrs) != 0 || len(requests) != 1 || !eventTextContains(events, "Run mode transition failed") {
		t.Fatalf("invalid transition requests/errors = %d/%v\n%s", len(requests), providerErrs, eventDump(events))
	}
	summaries, _, failures := recorder.snapshot()
	if len(summaries) != 0 || len(failures) != 1 || failures[0] != "Run mode transition failed" {
		t.Fatalf("invalid transition recorder = summaries:%#v failures:%v", summaries, failures)
	}
}

func TestRunLoopGeneratedInteractiveSyntaxIsInertForRunModeOrWorker(t *testing.T) {
	for _, boundary := range []string{"run-mode", "worker"} {
		for _, input := range []string{"/plan @proof.txt", "/help", "@proof.txt"} {
			t.Run(boundary+"/"+strings.ReplaceAll(input, " ", "_"), func(t *testing.T) {
				workDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(workDir, "proof.txt"), []byte("EXPANSION_CONTENT_SENTINEL"), 0o600); err != nil {
					t.Fatal(err)
				}
				provider := &completionLoopProvider{steps: []completionLoopStep{
					func(*types.MessagesRequest) (scriptedLoopStep, error) { return textStep("SAFE_PROVIDER_SENTINEL"), nil },
				}}
				opts := RunOptions{}
				if boundary == "run-mode" {
					opts.RunMode = &runModeLoopFake{
						name: "guard-mode", start: RunModeDirective{Continue: true, UserPrompt: "guard mode start"},
						advances: []RunModeDirective{{StopReason: "guard_finished"}},
					}
				} else {
					opts.WorkerID = "worker-generated"
				}
				events := runModeLoopAt(t, provider, input, workDir, opts)
				requests, providerErrs := provider.snapshot()
				if len(providerErrs) != 0 || len(requests) != 1 || !eventTextContains(events, "SAFE_PROVIDER_SENTINEL") {
					t.Fatalf("guarded provider requests/errors = %d/%v\n%s", len(requests), providerErrs, eventDump(events))
				}
				request := requests[0]
				if !requestHasUserTextPrefix(request, input) {
					t.Fatalf("generated input was rewritten: %#v", request.Messages)
				}
				if strings.Contains(completionRequestSystemText(request), "## PLAN MODE") ||
					requestContainsText(request, "EXPANSION_CONTENT_SENTINEL") {
					t.Fatalf("interactive syntax escaped guard: system=%q messages=%#v", completionRequestSystemText(request), request.Messages)
				}
				if strings.HasPrefix(input, "/plan") && !requestHasTool(request, "Write") {
					t.Fatalf("generated /plan incorrectly reduced tool catalog: %v", requestToolNames(request))
				}
				done := completionDoneEvent(t, events)
				if done["planMode"] != false {
					t.Fatalf("generated syntax enabled plan mode: %#v", done)
				}
			})
		}
	}
}

func runModeLoop(t *testing.T, provider types.Provider, prompt string, opts RunOptions) []Event {
	t.Helper()
	return runModeLoopAt(t, provider, prompt, t.TempDir(), opts)
}

func runModeLoopAt(t *testing.T, provider types.Provider, prompt, workDir string, opts RunOptions) []Event {
	t.Helper()
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("CORELAY_OFFLINE", "1")
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID: "run-mode-loop-test", ContextWindow: 65_536, OutputReserve: 4_096,
		MaxIterations: 7, MaxErrorRounds: 3, ToolRouting: harness.ToolRoutingDirect,
		ReadBeforeWrite: harness.SomeBool(false),
	})
	opts.HarnessProfile = &profile
	opts.DisablePlugins = true
	opts.PluginDirs = []string{}
	opts.DisableWorkspaceMCP = true
	opts.EvidencePolicy = EvidencePolicyConfig{Policy: EvidencePolicyOff}
	events := make(chan Event, 256)
	go RunLoopWithOptions(context.Background(), provider, "run-mode-model", []types.Message{{
		Role: "user", Content: mustJSON(prompt),
	}}, workDir, opts, events)
	var collected []Event
	for event := range events {
		collected = append(collected, event)
	}
	return collected
}

func requestHasUserTextPrefix(request *types.MessagesRequest, want string) bool {
	if request == nil {
		return false
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil &&
			(text == want || strings.HasPrefix(text, want+"\n\n")) {
			return true
		}
	}
	return false
}

func requestContainsText(request *types.MessagesRequest, want string) bool {
	if request == nil {
		return false
	}
	for _, message := range request.Messages {
		if strings.Contains(string(message.Content), want) {
			return true
		}
	}
	return false
}
