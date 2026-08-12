package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type resumeContinuationStep struct {
	text      string
	toolID    string
	toolName  string
	toolInput string
}

type resumeContinuationProvider struct {
	mu       sync.Mutex
	steps    []resumeContinuationStep
	calls    int
	requests []types.MessagesRequest
}

func (*resumeContinuationProvider) Name() string              { return "resume-continuation" }
func (*resumeContinuationProvider) DisplayName() string       { return "Resume Continuation" }
func (*resumeContinuationProvider) Models() []types.ModelInfo { return nil }
func (*resumeContinuationProvider) Validate() error           { return nil }

func (p *resumeContinuationProvider) StreamMessage(
	ctx context.Context,
	request *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	index := p.calls
	p.calls++
	if request != nil {
		encoded, _ := json.Marshal(request)
		var snapshot types.MessagesRequest
		_ = json.Unmarshal(encoded, &snapshot)
		p.requests = append(p.requests, snapshot)
	}
	if index >= len(p.steps) {
		p.mu.Unlock()
		return nil, fmt.Errorf("unexpected provider call %d", index+1)
	}
	step := p.steps[index]
	p.mu.Unlock()

	events := make(chan types.SSEEvent, 8)
	if step.text != "" {
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: resumeJSON(map[string]string{"type": "text"})}
		events <- types.SSEEvent{Type: "content_block_delta", Delta: resumeJSON(map[string]string{
			"type": "text_delta",
			"text": step.text,
		})}
		events <- types.SSEEvent{Type: "content_block_stop"}
	}
	if step.toolName != "" {
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: resumeJSON(map[string]string{
			"type": "tool_use",
			"id":   step.toolID,
			"name": step.toolName,
		})}
		events <- types.SSEEvent{Type: "content_block_delta", Delta: resumeJSON(map[string]string{
			"type":         "input_json_delta",
			"partial_json": step.toolInput,
		})}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_delta", Delta: resumeJSON(map[string]string{"stop_reason": "tool_use"})}
	} else {
		events <- types.SSEEvent{Type: "message_delta", Delta: resumeJSON(map[string]string{"stop_reason": "end_turn"})}
	}
	events <- types.SSEEvent{Type: "message_stop"}
	close(events)
	return events, nil
}

func (p *resumeContinuationProvider) requestSnapshots() []types.MessagesRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]types.MessagesRequest, len(p.requests))
	copy(result, p.requests)
	return result
}

type resumeSideEffectRunner struct {
	mu     sync.Mutex
	calls  int
	cancel context.CancelFunc
}

func (*resumeSideEffectRunner) Name() string { return "resume-side-effect" }

func (*resumeSideEffectRunner) Capabilities() sandbox.Capabilities {
	return resumeSandboxCapabilities()
}

func (r *resumeSideEffectRunner) Run(
	_ context.Context,
	policy sandbox.Policy,
	_ sandbox.CommandSpec,
) (sandbox.Result, sandbox.Report) {
	r.mu.Lock()
	r.calls++
	cancel := r.cancel
	r.mu.Unlock()

	// The counter is the external side effect. Cancellation happens only after
	// it has been applied, reproducing the ambiguous commit window exactly.
	if cancel != nil {
		cancel()
	}
	report := sandbox.Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		EffectiveEnforcement: policy.Enforcement,
		Capabilities:         r.Capabilities(),
		AppliedIsolation:     sandbox.Capabilities{ProcessIsolation: true},
		Started:              true,
	}
	return sandbox.Result{Started: true, Stdout: []byte("side effect applied\n"), ExitCode: 0}, report
}

func (r *resumeSideEffectRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestDurableSessionInterruptedRunReconcilesThenContinuesWithoutReplay(t *testing.T) {
	isolateResumeContinuationEnvironment(t)
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	const userText = "Apply the external side effect exactly once, then continue safely."
	requestMessages := []types.Message{{Role: "user", Content: resumeJSON(userText)}}
	session := agent.Session{
		Workspace: workDir,
		Messages:  []agent.SessionMessage{{Role: "user", Content: userText}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	if session.Revision != 1 || session.LastCommittedRevision != 1 {
		t.Fatalf("initial revision = %d/%d, want 1/1", session.Revision, session.LastCommittedRevision)
	}

	interruptedRun, err := prepareDurableAgentRun(
		store, session.ID, session.Revision, workDir, requestMessages,
	)
	if err != nil {
		t.Fatal(err)
	}
	interruptedRun.SetRuntimeRunID("resume-runtime-fallback")
	runCtx, cancelRun := context.WithCancel(context.Background())
	runner := &resumeSideEffectRunner{cancel: cancelRun}
	firstProvider := &resumeContinuationProvider{steps: []resumeContinuationStep{{
		toolID:    "call-side-effect-once",
		toolName:  "Bash",
		toolInput: `{"command":"echo side-effect-once"}`,
	}}}
	firstEvents := runResumeContinuationKernel(
		t, runCtx, firstProvider, requestMessages, workDir, interruptedRun, runner,
	)
	cancelRun()
	if runner.callCount() != 1 {
		t.Fatalf("side-effect executions = %d, want 1", runner.callCount())
	}
	start := exactExecutionStart(t, firstEvents)
	terminal, ok := terminalMetadataFromEvents(firstEvents)
	if !ok || !terminal.BlocksSuccess() {
		t.Fatalf("interrupted terminal = (%#v, %v), want blocking typed done", terminal, ok)
	}

	if committed, finalizeErr := interruptedRun.Finalize(firstProvider.Name(), "resume-model"); committed != nil || !errors.Is(finalizeErr, agent.ErrSessionReconcileRequired) {
		t.Fatalf("interrupted Finalize() = (%#v, %v), want reconciliation", committed, finalizeErr)
	}
	interrupted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Revision != 3 || interrupted.LastCommittedRevision != 1 ||
		!interrupted.ReconcileRequired || interrupted.Interruption == nil {
		t.Fatalf("interrupted session = %#v", interrupted)
	}
	marker := interrupted.Interruption
	if marker.RunID != start["runId"] || marker.ToolName != start["name"] ||
		marker.ToolCallID != start["id"] || marker.InputDigest != start["inputDigest"] ||
		marker.SideEffectState != agent.SessionSideEffectApplied {
		t.Fatalf("interruption marker = %#v, execution start = %#v", marker, start)
	}
	if len(interrupted.Messages) != 1 || interrupted.LastRunTerminal != nil {
		t.Fatalf("interrupted run leaked transcript/terminal: messages=%#v terminal=%#v", interrupted.Messages, interrupted.LastRunTerminal)
	}

	resume, err := store.ResumeState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resume.Revision != 3 || !resume.ReconcileRequired || resume.Interruption == nil {
		t.Fatalf("blocked resume state = %#v", resume)
	}
	if _, err := prepareDurableAgentRun(store, session.ID, resume.Revision, workDir, requestMessages); !errors.Is(err, agent.ErrSessionReconcileRequired) {
		t.Fatalf("pre-reconcile continuation = %v, want reconcile required", err)
	}

	reconciled, err := store.MarkReconciled(session.ID, resume.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Revision != 4 || reconciled.LastCommittedRevision != 4 ||
		reconciled.ReconcileRequired || reconciled.Interruption != nil {
		t.Fatalf("reconciled session = %#v", reconciled)
	}

	continuationRun, err := prepareDurableAgentRun(
		store, session.ID, reconciled.Revision, workDir, requestMessages,
	)
	if err != nil {
		t.Fatal(err)
	}
	staleRun, err := prepareDurableAgentRun(
		store, session.ID, reconciled.Revision, workDir, requestMessages,
	)
	if err != nil {
		t.Fatal(err)
	}
	continuationProvider := &resumeContinuationProvider{steps: []resumeContinuationStep{{
		text: "Continuation completed without replaying the reconciled side effect.",
	}}}
	continuationEvents := runResumeContinuationKernel(
		t, context.Background(), continuationProvider, requestMessages, workDir, continuationRun, runner,
	)
	committed, err := continuationRun.Finalize(continuationProvider.Name(), "resume-model")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 5 || committed.LastCommittedRevision != 5 || committed.ReconcileRequired {
		t.Fatalf("continuation commit = %#v", committed)
	}
	if runner.callCount() != 1 {
		t.Fatalf("reconciled side effect replayed: executions=%d", runner.callCount())
	}
	if len(committed.Messages) != 2 || committed.Messages[0].Content != userText ||
		committed.Messages[1].Role != "assistant" ||
		committed.Messages[1].Content != "Continuation completed without replaying the reconciled side effect." {
		t.Fatalf("committed transcript = %#v", committed.Messages)
	}
	if committed.LastRunTerminal == nil || committed.LastRunTerminal.BlocksSuccess() {
		t.Fatalf("continuation terminal = %#v, want nonblocking", committed.LastRunTerminal)
	}
	if eventTerminal, ok := terminalMetadataFromEvents(continuationEvents); !ok || eventTerminal.BlocksSuccess() {
		t.Fatalf("continuation event terminal = (%#v, %v), want nonblocking", eventTerminal, ok)
	}
	assertSingleCommittedHistory(t, continuationProvider, userText)

	committedTerminal := *committed.LastRunTerminal
	staleRun.Observe(agent.Event{Type: "text", Data: "stale transcript must not persist"})
	staleRun.Observe(agent.Event{Type: "done", Data: agent.DurableRunTerminalMetadata{
		TerminalState: agent.EvidenceTerminalVerified,
	}})
	if staleCommitted, staleErr := staleRun.Finalize("stale-provider", "stale-model"); staleCommitted != nil || !errors.Is(staleErr, agent.ErrSessionRevisionConflict) {
		t.Fatalf("stale Finalize() = (%#v, %v), want revision conflict", staleCommitted, staleErr)
	}
	loaded, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 5 || len(loaded.Messages) != 2 ||
		loaded.Messages[1].Content != committed.Messages[1].Content ||
		loaded.LastRunTerminal == nil || *loaded.LastRunTerminal != committedTerminal {
		t.Fatalf("stale run changed transcript or terminal: %#v", loaded)
	}
}

func TestDurableBlockedTerminalWithoutToolActivityStillCommitsAtomically(t *testing.T) {
	store := agent.NewSessionStore(t.TempDir())
	workDir := t.TempDir()
	session := agent.Session{
		Workspace: workDir,
		Messages:  []agent.SessionMessage{{Role: "user", Content: "blocked request"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, []types.Message{{
		Role: "user", Content: resumeJSON("blocked request"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantTerminal := agent.DurableRunTerminalMetadata{
		TerminalState:       agent.EvidenceTerminalBlocked,
		CompletionStatus:    agent.CompletionStatusIncomplete,
		CompletionRevision:  2,
		CompletionCriteria:  2,
		CompletionSatisfied: 1,
	}
	run.Observe(agent.Event{Type: "text", Data: "bounded blocked answer"})
	run.Observe(agent.Event{Type: "done", Data: wantTerminal})
	committed, err := run.Finalize("provider", "model")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 2 || committed.LastCommittedRevision != 2 || committed.ReconcileRequired ||
		committed.Interruption != nil || len(committed.Messages) != 2 ||
		committed.LastRunTerminal == nil || *committed.LastRunTerminal != wantTerminal {
		t.Fatalf("blocked terminal atomic commit = %#v", committed)
	}
	loaded, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != committed.Revision || len(loaded.Messages) != len(committed.Messages) ||
		loaded.LastRunTerminal == nil || *loaded.LastRunTerminal != wantTerminal {
		t.Fatalf("persisted blocked terminal = %#v", loaded)
	}
}

func runResumeContinuationKernel(
	t *testing.T,
	ctx context.Context,
	provider types.Provider,
	messages []types.Message,
	workDir string,
	durableRun *durableAgentRun,
	runner sandbox.Runner,
) []agent.Event {
	t.Helper()
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "session-resume-continuation-e2e",
		MaxIterations:   4,
		ReadBeforeWrite: harness.SomeBool(false),
		ResponsePolicy:  harness.ResponseNative,
		ToolRouting:     harness.ToolRoutingDirect,
	})
	eventCh := make(chan agent.Event, 128)
	go agent.RunLoopWithOptions(ctx, provider, "resume-model", messages, workDir, agent.RunOptions{
		SessionID:            "resume-kernel-session",
		HarnessProfile:       &profile,
		EvidencePolicy:       agent.EvidencePolicyConfig{Policy: agent.EvidencePolicyOff},
		SandboxRunner:        runner,
		SandboxPolicy:        resumeSandboxPolicy(),
		DisableWorkspaceMCP:  true,
		DisablePlugins:       true,
		PluginDirs:           []string{},
		ToolResultStore:      durableRun.toolResultMemory,
		ToolResultReader:     durableRun.toolResultMemory,
		ToolResultReferences: append([]agent.ToolResultReference(nil), durableRun.toolResultRefs...),
		PreExecutionJournal:  durableRun.JournalToolExecution,
	}, eventCh)
	var events []agent.Event
	for event := range eventCh {
		durableRun.Observe(event)
		events = append(events, event)
	}
	return events
}

func exactExecutionStart(t *testing.T, events []agent.Event) map[string]string {
	t.Helper()
	var start map[string]string
	for _, event := range events {
		if event.Type != "tool_execution_start" {
			continue
		}
		if start != nil {
			t.Fatal("multiple execution-start events observed")
		}
		encoded, _ := json.Marshal(event.Data)
		if err := json.Unmarshal(encoded, &start); err != nil {
			t.Fatal(err)
		}
	}
	if start == nil || start["runId"] == "" || start["name"] != "Bash" ||
		start["id"] != "call-side-effect-once" || !strings.HasPrefix(start["inputDigest"], "sha256:") {
		t.Fatalf("execution start = %#v", start)
	}
	return start
}

func terminalMetadataFromEvents(events []agent.Event) (agent.DurableRunTerminalMetadata, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == "done" {
			return agent.DecodeDurableRunTerminalMetadata(events[index].Data)
		}
	}
	return agent.DurableRunTerminalMetadata{}, false
}

func assertSingleCommittedHistory(t *testing.T, provider *resumeContinuationProvider, userText string) {
	t.Helper()
	requests := provider.requestSnapshots()
	if len(requests) != 1 {
		t.Fatalf("continuation provider requests = %d, want 1", len(requests))
	}
	encoded, _ := json.Marshal(requests[0])
	if strings.Contains(string(encoded), "call-side-effect-once") || strings.Contains(string(encoded), "tool_result") {
		t.Fatalf("interrupted tool transcript replayed into continuation request: %s", encoded)
	}
	userCount := 0
	for _, message := range requests[0].Messages {
		if message.Role != "user" {
			continue
		}
		var content string
		if json.Unmarshal(message.Content, &content) == nil && content == userText {
			userCount++
		}
	}
	if userCount != 1 {
		t.Fatalf("committed user history count = %d, request=%s", userCount, encoded)
	}
}

func isolateResumeContinuationEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("CORELAY_CONFIG_DIR", home)
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
}

func resumeSandboxCapabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		ProcessIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func resumeSandboxPolicy() sandbox.Policy {
	return sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired,
		Required:    resumeSandboxCapabilities(),
	}
}

func resumeJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
