package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type fakeTUIBackend struct {
	mu sync.Mutex

	calls         []string
	saveResult    sessionSaveResult
	saveExpected  []*uint64
	savedSessions []*agent.Session
	startRequests []agentTurnRequest
	loadedSession *agent.Session
	forkedSession *agent.Session
	forkRequests  []struct {
		id       string
		revision uint64
	}
	approvalDecisions     []string
	cancelCalls           int
	loopWorkDirs          []string
	activeLoopsErr        error
	reconciliationPreview agent.SessionReconciliationAssessment
	reconcileCalls        []struct {
		id             string
		revision       uint64
		evidenceDigest string
		manualAck      bool
	}
}

func (f *fakeTUIBackend) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeTUIBackend) Health(context.Context) (serverHealth, error) {
	f.record("health")
	return serverHealth{Status: "ok", Provider: "test", Model: "model"}, nil
}

func (f *fakeTUIBackend) Config(context.Context) (serverConfig, error) {
	f.record("config")
	return serverConfig{Provider: "test", Model: "model"}, nil
}

func (f *fakeTUIBackend) Root(context.Context) (serverRoot, error) {
	f.record("root")
	return serverRoot{Name: "corelaycode", Version: "test"}, nil
}

func (f *fakeTUIBackend) SetConfig(_ context.Context, provider, model string) (serverConfig, error) {
	f.record("set-config")
	return serverConfig{Provider: provider, Model: model}, nil
}

func (f *fakeTUIBackend) Commands(context.Context, string) ([]agent.SlashCommand, error) {
	f.record("commands")
	return nil, nil
}

func (f *fakeTUIBackend) ActiveLoops(_ context.Context, workDir string) ([]activeLoopInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "active-loops")
	f.loopWorkDirs = append(f.loopWorkDirs, workDir)
	return nil, f.activeLoopsErr
}

func (f *fakeTUIBackend) ListSessions(context.Context, string) ([]agent.SessionSummary, error) {
	f.record("list-sessions")
	return nil, nil
}

func (f *fakeTUIBackend) GetSession(_ context.Context, id string) (*agent.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "get-session")
	if f.loadedSession == nil {
		return nil, nil
	}
	loaded := cloneTUISession(f.loadedSession)
	if loaded.ID == "" {
		loaded.ID = id
	}
	return loaded, nil
}

func (f *fakeTUIBackend) SaveSession(_ context.Context, session *agent.Session, expected *uint64) (sessionSaveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "save")
	if expected == nil {
		f.saveExpected = append(f.saveExpected, nil)
	} else {
		copyRevision := *expected
		f.saveExpected = append(f.saveExpected, &copyRevision)
	}
	f.savedSessions = append(f.savedSessions, cloneTUISession(session))
	return f.saveResult, nil
}

func (f *fakeTUIBackend) ForkSession(_ context.Context, id string, revision uint64) (*agent.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "fork")
	f.forkRequests = append(f.forkRequests, struct {
		id       string
		revision uint64
	}{id: id, revision: revision})
	return cloneTUISession(f.forkedSession), nil
}

func (f *fakeTUIBackend) ReconciliationPreview(_ context.Context, id string, revision uint64) (agent.SessionReconciliationAssessment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "reconciliation-preview")
	assessment := f.reconciliationPreview
	assessment.SessionID = id
	assessment.Revision = revision
	if assessment.EvidenceDigest == "" {
		assessment.EvidenceDigest = "sha256:preview-token"
	}
	return assessment, nil
}

func (f *fakeTUIBackend) ReconcileSession(_ context.Context, id string, revision uint64, evidenceDigest string, manualAck bool) (*agent.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "reconcile")
	f.reconcileCalls = append(f.reconcileCalls, struct {
		id             string
		revision       uint64
		evidenceDigest string
		manualAck      bool
	}{id: id, revision: revision, evidenceDigest: evidenceDigest, manualAck: manualAck})
	return &agent.Session{ID: id, Revision: revision + 1, LifecycleStatus: agent.SessionLifecycleActive}, nil
}

func (f *fakeTUIBackend) CloseSession(context.Context, string, uint64) (*agent.Session, error) {
	f.record("close")
	return nil, nil
}

func (f *fakeTUIBackend) DeleteSession(context.Context, string, uint64) error {
	f.record("delete")
	return nil
}

func (f *fakeTUIBackend) StartTurn(_ context.Context, request agentTurnRequest) <-chan agentStreamItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	copyRequest := request
	copyRequest.Messages = append([]chatMsg(nil), request.Messages...)
	if request.ExpectedRevision != nil {
		revision := *request.ExpectedRevision
		copyRequest.ExpectedRevision = &revision
	}
	f.startRequests = append(f.startRequests, copyRequest)
	return make(chan agentStreamItem)
}

func (f *fakeTUIBackend) ResolveApproval(_ context.Context, _, _, decision string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "approval:"+decision)
	f.approvalDecisions = append(f.approvalDecisions, decision)
	return nil
}

func (f *fakeTUIBackend) CancelRun(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "cancel")
	f.cancelCalls++
	return nil
}

func TestStripTerminalControlsFailClosed(t *testing.T) {
	input := "한글\r새줄\x1b[31mRED\x1b[0m" +
		"\x1b]52;c;c2VjcmV0\x07" +
		"\x1b]0;forged title\x1b\\끝\x00\x7f\t값"
	got := safeTUIText(input, 4096)
	want := "한글\n새줄RED끝\t값"
	if got != want {
		t.Fatalf("safeTUIText() = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"\x1b", "\r", "c2VjcmV0", "forged title", "\x00", "\x7f"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized text still contains %q: %q", forbidden, got)
		}
	}
}

func TestTUIViewResponsiveBounds(t *testing.T) {
	backend := &fakeTUIBackend{}
	model := newTUIModel(backend, tuiOptions{
		BaseURL: "http://127.0.0.1:7331",
		WorkDir: `D:\git\corelay-code`,
		NoColor: true,
	})
	model.connection = tuiConnected
	model.root = serverRoot{Version: "test"}
	model.config = serverConfig{Provider: "provider-with-a-long-name", Model: "model-with-a-long-name"}
	model.entries = nil
	model.appendEntry(tuiEntryUser, strings.Repeat("긴한글 ", 80)+"\x1b]52;c;hidden\x07")
	model.appendEntry(tuiEntryAssistant, strings.Repeat("response ", 100))

	for _, size := range []struct {
		width  int
		height int
	}{{140, 40}, {100, 30}, {72, 24}, {50, 15}, {39, 9}, {10, 2}, {1, 1}} {
		t.Run(strings.Join([]string{compactNumber(size.width), "x", compactNumber(size.height)}, ""), func(t *testing.T) {
			copyModel := model
			copyModel.resize(size.width, size.height)
			view := copyModel.View()
			if strings.Contains(view, "hidden") || strings.ContainsRune(view, '\x1b') {
				t.Fatalf("rendered view contains terminal-injection payload: %q", view)
			}
			lines := strings.Split(view, "\n")
			if len(lines) > size.height {
				t.Fatalf("view height = %d lines, terminal height = %d", len(lines), size.height)
			}
			for lineNumber, line := range lines {
				if width := lipgloss.Width(line); width > size.width {
					t.Fatalf("line %d width = %d, terminal width = %d: %q", lineNumber+1, width, size.width, line)
				}
			}
		})
	}
}

func TestTUISessionRailUsesPersistedAndFreshSessionContext(t *testing.T) {
	backend := &fakeTUIBackend{}
	model := newTUIModel(backend, tuiOptions{NoColor: true, Mode: agent.ExecutionModeFull})
	model.resize(140, 40)

	loaded := &agent.Session{
		ID: "session-1", Revision: 12, Turns: 2, LifecycleStatus: agent.SessionLifecycleActive,
		WorkstreamID: "workstream-a", PlanID: "plan-b", PlanRevision: 3, StageID: "stage-c",
		ExecutionPolicy: &agent.ExecutionPolicySnapshot{Mode: agent.ExecutionModeWorkspace, Revision: 4},
	}
	model.loadSession(loaded)
	view := model.View()
	for _, want := range []string{"workstream-a", "plan-b@3", "stage-c", "workspace@4"} {
		if !strings.Contains(view, want) {
			t.Fatalf("loaded session rail omitted %q:\n%s", want, view)
		}
	}

	model.applyWireEvent(agentWireEvent{Type: "session", Data: json.RawMessage(`{"sessionId":"run-1","executionPolicy":{"mode":"full","revision":9}}`)})
	if lines := strings.Join(model.sessionRailLines(), "\n"); !strings.Contains(lines, "PERM full@9") {
		t.Fatalf("fresh run policy not shown in session rail: %q", lines)
	}

	model.applyWireEvent(agentWireEvent{Type: "session", Data: json.RawMessage(`{"sessionId":"run-2"}`)})
	if lines := strings.Join(model.sessionRailLines(), "\n"); !strings.Contains(lines, "PERM —") {
		t.Fatalf("missing fresh policy should be shown as unknown: %q", lines)
	}

	synced := cloneTUISession(loaded)
	synced.Revision = 13
	synced.ExecutionPolicy = &agent.ExecutionPolicySnapshot{Mode: agent.ExecutionModeReadOnly, Revision: 6}
	model.syncSessionAfterRun(synced)
	if lines := strings.Join(model.sessionRailLines(), "\n"); !strings.Contains(lines, "PERM read-only@6") {
		t.Fatalf("synced session policy not restored from persisted snapshot: %q", lines)
	}
}

func TestTUISessionRailDoesNotInferMissingPermissionFromCLIOption(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{NoColor: true, Mode: agent.ExecutionModeFull})
	model.resize(140, 40)
	model.loadSession(&agent.Session{
		ID: "session-1", Revision: 1, LifecycleStatus: agent.SessionLifecycleActive,
		ExecutionPolicy: &agent.ExecutionPolicySnapshot{Mode: agent.ExecutionModeWorkspace},
	})
	if lines := strings.Join(model.sessionRailLines(), "\n"); !strings.Contains(lines, "PERM —") {
		t.Fatalf("unknown persisted permission should remain unknown: %q", lines)
	}
	if view := model.View(); !strings.Contains(view, "PERM —") {
		t.Fatalf("unknown permission is not visible in the rendered session rail:\n%s", view)
	}
}

func TestTUIBoundedOverlaysCollapseMultilineServerText(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{NoColor: true})
	model.operationBusy = false
	model.resize(50, 15)
	model.approval = &tuiApproval{
		ToolName:      "Write\nforged-row",
		DangerLevel:   "danger\nforged-risk",
		Scope:         strings.Repeat("scope\n", 20),
		RedactedInput: strings.Repeat("redacted\n", 100),
	}
	view := model.View()
	lines := strings.Split(view, "\n")
	if len(lines) > 15 {
		t.Fatalf("approval view height = %d, want <= 15:\n%s", len(lines), view)
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 50 {
			t.Fatalf("approval line %d width = %d, want <= 50", index+1, width)
		}
	}
}

func TestTUIApprovalKeysAreFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		key      tea.KeyMsg
		decision string
	}{
		{name: "enter denies", key: tea.KeyMsg{Type: tea.KeyEnter}, decision: "deny"},
		{name: "escape denies", key: tea.KeyMsg{Type: tea.KeyEsc}, decision: "deny"},
		{name: "a allows once", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}, decision: "allow_once"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeTUIBackend{}
			model := newTUIModel(backend, tuiOptions{})
			model.resize(100, 30)
			model.runtimeID = "runtime-1"
			model.runState = tuiRunApproval
			model.approval = &tuiApproval{ID: "approval-1", RuntimeID: "runtime-1"}

			next, command := model.handleKey(test.key)
			if command == nil {
				t.Fatal("approval key returned no command")
			}
			updated := next.(tuiModel)
			if updated.approval == nil || !updated.approval.Resolving {
				t.Fatal("approval was not locked while its decision is recorded")
			}
			message := command()
			resolved, ok := message.(tuiApprovalResolvedMsg)
			if !ok {
				t.Fatalf("approval command returned %T", message)
			}
			if resolved.err != nil || resolved.decision != test.decision {
				t.Fatalf("resolution = %#v, want decision %q", resolved, test.decision)
			}
			if len(backend.approvalDecisions) != 1 || backend.approvalDecisions[0] != test.decision {
				t.Fatalf("backend decisions = %v, want [%s]", backend.approvalDecisions, test.decision)
			}
		})
	}
}

func TestTUISmallSecurityReviewDisablesPositiveActions(t *testing.T) {
	tests := []struct {
		name     string
		approval *tuiApproval
		confirm  *tuiConfirmation
		positive rune
		negative rune
		wantDeny bool
	}{
		{
			name: "approval at 40x10", positive: 'a', negative: 'd', wantDeny: true,
			approval: &tuiApproval{ID: "approval-1", RuntimeID: "runtime-1", Generation: 1, ExpiresAt: time.Now().Add(time.Minute)},
		},
		{
			name: "confirmation at 40x10", positive: 'y', negative: 'n',
			confirm: &tuiConfirmation{Action: "delete", Title: "Delete session", Description: "This cannot be undone"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeTUIBackend{}
			model := newTUIModel(backend, tuiOptions{NoColor: true})
			model.operationBusy = false
			model.runtimeID = "runtime-1"
			model.runGeneration = 1
			model.runState = tuiRunApproval
			model.approval = test.approval
			model.confirm = test.confirm
			model.resize(40, 10)

			if !model.securityReviewRestricted() {
				t.Fatal("40x10 security review was not restricted")
			}
			view := model.View()
			if !strings.Contains(view, "SAFETY LOCK") || strings.Contains(view, "allow once") || strings.Contains(view, "Y confirms") {
				t.Fatalf("restricted warning is unsafe:\n%s", view)
			}

			next, command := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{test.positive}})
			if command != nil || len(backend.calls) != 0 {
				t.Fatalf("positive action %q was enabled: command=%v calls=%v", test.positive, command, backend.calls)
			}
			model = next.(tuiModel)
			next, command = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{test.negative}})
			updated := next.(tuiModel)
			if test.wantDeny {
				if command == nil {
					t.Fatal("deny action returned no command")
				}
				_ = command()
				if len(backend.approvalDecisions) != 1 || backend.approvalDecisions[0] != "deny" {
					t.Fatalf("deny decisions = %v", backend.approvalDecisions)
				}
			} else if command != nil || updated.confirm != nil {
				t.Fatalf("cancel action did not close confirmation: command=%v confirm=%#v", command, updated.confirm)
			}
		})
	}
}

func TestTUIAssistantSegmentsPreserveWireEventOrder(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.entries = nil
	model.assistantEntry = -1
	model.assistantText = ""

	model.applyWireEvent(agentWireEvent{Type: "text", Data: json.RawMessage(`"first "`)})
	model.applyWireEvent(agentWireEvent{Type: "text", Data: json.RawMessage(`"segment"`)})
	model.applyWireEvent(agentWireEvent{Type: "tool_start", Data: json.RawMessage(`{"name":"Read"}`)})
	model.applyWireEvent(agentWireEvent{Type: "text", Data: json.RawMessage(`"after tool"`)})
	model.applyWireEvent(agentWireEvent{Type: "status", Data: json.RawMessage(`"checking"`)})
	model.applyWireEvent(agentWireEvent{Type: "text", Data: json.RawMessage(`"after status"`)})
	model.applyWireEvent(agentWireEvent{Type: "diff", Data: json.RawMessage(`{"file":"a.go","diff":"+line"}`)})
	model.applyWireEvent(agentWireEvent{Type: "text", Data: json.RawMessage(`"after diff"`)})

	wantKinds := []tuiEntryKind{
		tuiEntryAssistant, tuiEntryTool, tuiEntryAssistant, tuiEntryStatus,
		tuiEntryAssistant, tuiEntryDiff, tuiEntryAssistant,
	}
	wantTexts := []string{
		"first segment", "Running Read", "after tool", "checking",
		"after status", "a.go\n+line", "after diff",
	}
	if len(model.entries) != len(wantKinds) {
		t.Fatalf("entries = %#v, want %d ordered entries", model.entries, len(wantKinds))
	}
	for index := range wantKinds {
		if model.entries[index].Kind != wantKinds[index] || model.entries[index].Text != wantTexts[index] {
			t.Fatalf("entry %d = %#v, want kind=%q text=%q", index, model.entries[index], wantKinds[index], wantTexts[index])
		}
	}
}

func TestTUIAccumulatesRunDiffsAndListsChangedFiles(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.entries = nil
	model.applyWireEvent(agentWireEvent{Type: "diff", Data: json.RawMessage(`{"file":"one/a.go","diff":"+first"}`)})
	model.applyWireEvent(agentWireEvent{Type: "diff", Data: json.RawMessage(`{"file":"two/a.go","diff":"-second"}`)})
	model.applyWireEvent(agentWireEvent{Type: "diff", Data: json.RawMessage(`{"file":"one/a.go","diff":"+third"}`)})
	if len(model.diffs) != 3 {
		t.Fatalf("diff records = %d, want 3", len(model.diffs))
	}
	summary := formatTUIDiffSummary(model.diffs)
	for _, want := range []string{"Changed files (2): one/a.go, two/a.go", "[1] one/a.go", "[2] two/a.go", "[3] one/a.go", "+third"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("diff summary missing %q: %s", want, summary)
		}
	}
}

func TestTUIApprovalDeadlineIsMandatoryAndFailClosed(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt string
	}{
		{name: "missing"},
		{name: "invalid", expiresAt: "not-a-time"},
		{name: "not future", expiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339Nano)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeTUIBackend{}
			model := newTUIModel(backend, tuiOptions{})
			model.entries = nil
			model.runtimeID = "runtime-1"
			model.runGeneration = 3
			model.runState = tuiRunStreaming
			model.pendingToolInput = `{"secret":"must-not-render"}`
			payload, err := json.Marshal(map[string]string{
				"id": "approval-1", "sessionId": "runtime-1", "toolName": "Write",
				"redactedInput": "safe", "expiresAt": test.expiresAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			commands := model.applyWireEvent(agentWireEvent{Type: "approval_required", Data: payload})
			if model.approval != nil || model.runState != tuiRunCancelling || model.pendingToolInput != "" {
				t.Fatalf("invalid deadline remained actionable: approval=%#v state=%q input=%q", model.approval, model.runState, model.pendingToolInput)
			}
			for _, entry := range model.entries {
				if strings.Contains(entry.Text, "must-not-render") {
					t.Fatalf("raw tool input leaked into transcript: %#v", model.entries)
				}
			}
			if len(commands) != 1 {
				t.Fatalf("cancel commands = %d, want 1", len(commands))
			}
			_ = commands[0]()
			if backend.cancelCalls != 1 {
				t.Fatalf("cancel calls = %d, want 1", backend.cancelCalls)
			}
		})
	}
}

func TestTUIBootstrapModelSwitchFailsClosedWhenRunDiscoveryFails(t *testing.T) {
	backend := &fakeTUIBackend{activeLoopsErr: errors.New("loop service unavailable")}
	model := newTUIModel(backend, tuiOptions{Provider: "provider-next", Model: "model-next"})
	message := model.bootstrapCmd()()
	bootstrap, ok := message.(tuiBootstrapMsg)
	if !ok || bootstrap.err == nil || !strings.Contains(bootstrap.err.Error(), "verify active runs") {
		t.Fatalf("bootstrap result = %#v, want active-run verification failure", message)
	}
	for _, call := range backend.calls {
		if call == "set-config" {
			t.Fatalf("model config changed after active-run discovery failure: %v", backend.calls)
		}
	}
}

func TestTUITypedBlockedDoneNeverBecomesSuccess(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.runState = tuiRunStreaming
	payload := json.RawMessage(`{"kind":"completed","terminalState":"blocked","completionStatus":"incomplete","completionCriteria":2,"completionSatisfied":1}`)
	model.applyWireEvent(agentWireEvent{Type: "done", Data: payload})
	if !model.doneSeen {
		t.Fatal("typed done was not recorded")
	}
	if model.runState != tuiRunBlocked {
		t.Fatalf("run state = %q, want %q", model.runState, tuiRunBlocked)
	}
	for _, entry := range model.entries {
		if entry.Kind == tuiEntrySystem && strings.Contains(strings.ToLower(entry.Text), "completed") {
			t.Fatalf("blocked terminal was rendered as success: %#v", entry)
		}
	}
}

func TestTUIStreamCloseWithoutDoneIsUnknown(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.entries = nil
	model.runState = tuiRunStreaming
	next, _ := model.Update(tuiStreamClosedMsg{})
	updated := next.(tuiModel)
	if updated.runState != tuiRunUnknown {
		t.Fatalf("run state = %q, want %q", updated.runState, tuiRunUnknown)
	}
	if updated.doneSeen {
		t.Fatal("stream close fabricated a done event")
	}
	if len(updated.entries) == 0 || updated.entries[len(updated.entries)-1].Kind != tuiEntryError {
		t.Fatalf("unknown terminal did not render an error: %#v", updated.entries)
	}
}

func TestTUIRequiredReloadPreservesLiveTranscript(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.operationBusy = false
	model.entries = []tuiTranscriptEntry{
		{Kind: tuiEntryUser, Text: "live user"},
		{Kind: tuiEntryTool, Text: "live tool"},
		{Kind: tuiEntryDiff, Text: "live diff"},
	}
	model.awaitingReload = true
	model.current = &agent.Session{ID: "session-1", Revision: 1}
	next, _ := model.Update(tuiSessionLoadedMsg{
		required: true,
		session: &agent.Session{
			ID: "session-1", Revision: 2, LifecycleStatus: agent.SessionLifecycleActive,
			Messages: []agent.SessionMessage{
				{Role: "user", Content: "live user"},
				{Role: "assistant", Content: "committed answer"},
			},
		},
	})
	updated := next.(tuiModel)
	if updated.current == nil || updated.current.Revision != 2 || updated.awaitingReload || updated.reloadFailed {
		t.Fatalf("synchronized session state = %#v", updated.current)
	}
	if len(updated.entries) != 3 || updated.entries[1].Text != "live tool" || updated.entries[2].Text != "live diff" {
		t.Fatalf("required reload replaced the live transcript: %#v", updated.entries)
	}
	if len(updated.loadedMessages) != 2 || updated.loadedMessages[1].Content != "committed answer" {
		t.Fatalf("committed messages were not synchronized: %#v", updated.loadedMessages)
	}
}

func TestTUICancelRequestIsSentExactlyOnce(t *testing.T) {
	backend := &fakeTUIBackend{}
	model := newTUIModel(backend, tuiOptions{})
	model.runState = tuiRunStreaming
	model.runtimeID = "runtime-1"

	next, first := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if first == nil {
		t.Fatal("first cancellation returned no command")
	}
	_ = first()
	updated := next.(tuiModel)
	_, second := updated.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if second != nil {
		_ = second()
	}
	if backend.cancelCalls != 1 {
		t.Fatalf("CancelRun calls = %d, want exactly 1", backend.cancelCalls)
	}
}

func TestTUILatePreparedTurnCannotReviveCancelledSubmission(t *testing.T) {
	for _, order := range []string{"prepared-before-cancel-result", "cancel-result-before-prepared"} {
		t.Run(order, func(t *testing.T) {
			cancelCalls := 0
			cancel := onceTUICancel(func() { cancelCalls++ })
			stream := make(chan agentStreamItem)
			defer close(stream)

			model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
			model.operationBusy = false
			model.runGeneration = 17
			model.runState = tuiRunSubmitting
			model.turnCancel = cancel

			next, cancelCommand := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
			if cancelCommand == nil {
				t.Fatal("submitting cancellation returned no command")
			}
			cancelled := next.(tuiModel)
			if cancelled.runState != tuiRunCancelling || !cancelled.cancelRequested || cancelCalls != 1 {
				t.Fatalf("cancel state = %q requested=%v local cancels=%d", cancelled.runState, cancelled.cancelRequested, cancelCalls)
			}

			prepared := tuiTurnPreparedMsg{
				generation: 17,
				session:    &agent.Session{ID: "late-session", Revision: 1},
				stream:     stream,
				cancel:     cancel,
			}
			cancelResult := cancelCommand()
			var waitCommand tea.Cmd
			if order == "prepared-before-cancel-result" {
				next, waitCommand = cancelled.Update(prepared)
				cancelled = next.(tuiModel)
				next, _ = cancelled.Update(cancelResult)
				cancelled = next.(tuiModel)
			} else {
				next, _ = cancelled.Update(cancelResult)
				cancelled = next.(tuiModel)
				next, waitCommand = cancelled.Update(prepared)
				cancelled = next.(tuiModel)
			}

			if waitCommand != nil || cancelled.stream != nil || cancelled.current != nil || cancelled.runState != tuiRunFailed {
				t.Fatalf("late preparation was adopted: wait=%v stream=%v current=%#v state=%q", waitCommand, cancelled.stream, cancelled.current, cancelled.runState)
			}
			if cancelCalls != 1 {
				t.Fatalf("context cancellation calls = %d, want exactly 1", cancelCalls)
			}
		})
	}
}

func TestTUIPreparedTurnRequiresExactCurrentSubmittingGeneration(t *testing.T) {
	tests := []struct {
		name       string
		generation uint64
		state      tuiRunState
		requested  bool
	}{
		{name: "older generation", generation: 8, state: tuiRunSubmitting},
		{name: "newer generation", generation: 10, state: tuiRunSubmitting},
		{name: "current but idle", generation: 9, state: tuiRunIdle},
		{name: "current but cancelled", generation: 9, state: tuiRunSubmitting, requested: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cancelCalls := 0
			stream := make(chan agentStreamItem)
			defer close(stream)
			model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
			model.runGeneration = 9
			model.runState = test.state
			model.cancelRequested = test.requested
			next, command := model.Update(tuiTurnPreparedMsg{
				generation: test.generation,
				session:    &agent.Session{ID: "stale"},
				stream:     stream,
				cancel:     func() { cancelCalls++ },
			})
			updated := next.(tuiModel)
			if command != nil || updated.stream != nil || updated.current != nil || cancelCalls != 1 {
				t.Fatalf("rejected result: command=%v stream=%v current=%#v cancels=%d", command, updated.stream, updated.current, cancelCalls)
			}
		})
	}
}

func TestTUICurrentPrepareFailureCancelsAndFailsWithoutAdoptingState(t *testing.T) {
	cancelCalls := 0
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.runGeneration = 4
	model.runState = tuiRunSubmitting
	model.turnCancel = func() { cancelCalls++ }
	next, command := model.Update(tuiTurnPreparedMsg{
		generation: 4,
		cancel:     model.turnCancel,
		err:        errors.New("save failed"),
	})
	updated := next.(tuiModel)
	if command != nil || updated.runState != tuiRunFailed || updated.stream != nil || updated.current != nil {
		t.Fatalf("prepare failure state: command=%v state=%q stream=%v current=%#v", command, updated.runState, updated.stream, updated.current)
	}
	if cancelCalls != 1 {
		t.Fatalf("prepare failure cancellation calls = %d, want 1", cancelCalls)
	}
}

func TestTUILateApprovalAndResolutionCannotCrossRunGeneration(t *testing.T) {
	backend := &fakeTUIBackend{}
	model := newTUIModel(backend, tuiOptions{})
	model.operationBusy = false
	model.runState = tuiRunCancelling
	model.runtimeID = "run-new"
	model.runGeneration = 2
	model.cancelRequested = true

	commands := model.applyWireEvent(agentWireEvent{
		Type: "approval_required",
		Data: json.RawMessage(`{"id":"late","sessionId":"run-new","toolName":"Write","redactedInput":"safe"}`),
	})
	if model.approval != nil || len(commands) != 0 {
		t.Fatalf("late approval became actionable: approval=%#v commands=%d", model.approval, len(commands))
	}

	model.approval = &tuiApproval{ID: "current", RuntimeID: "run-new", Generation: 2}
	next, command := model.Update(tuiApprovalResolvedMsg{
		approvalID: "old", runtimeID: "run-old", generation: 1,
		decision: "allow_once", err: context.Canceled,
	})
	updated := next.(tuiModel)
	if command != nil || updated.approval == nil || updated.approval.ID != "current" || backend.cancelCalls != 0 {
		t.Fatalf("stale approval result mutated current run: approval=%#v cancelCalls=%d", updated.approval, backend.cancelCalls)
	}
}

func TestTUIBootstrapUsesGlobalRunGateAndLocksInput(t *testing.T) {
	backend := &fakeTUIBackend{}
	model := newTUIModel(backend, tuiOptions{WorkDir: `D:\repo`})
	model.input.SetValue("must wait")
	next, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || next.(tuiModel).input.Value() != "must wait" {
		t.Fatal("input was accepted while bootstrap was in flight")
	}

	message := model.bootstrapCmd()()
	bootstrap, ok := message.(tuiBootstrapMsg)
	if !ok || bootstrap.err != nil {
		t.Fatalf("bootstrap = %#v", message)
	}
	if len(backend.loopWorkDirs) != 1 || backend.loopWorkDirs[0] != "" {
		t.Fatalf("active-loop gate workdirs = %#v, want global empty filter", backend.loopWorkDirs)
	}
}

func TestTUICommandLineForkUsesLoadedRevisionAndAdoptsSessionWorkspace(t *testing.T) {
	backend := &fakeTUIBackend{
		loadedSession: &agent.Session{
			ID: "session-parent", Revision: 14, Workspace: `D:\projects\target`,
			LifecycleStatus: agent.SessionLifecycleActive,
		},
		forkedSession: &agent.Session{
			ID: "session-child", Revision: 1, Workspace: `D:\projects\target`,
			ParentSessionID: "session-parent", ParentRevision: 14,
			LifecycleStatus: agent.SessionLifecycleActive,
		},
	}
	model := newTUIModel(backend, tuiOptions{
		BaseURL: "http://127.0.0.1:4000", WorkDir: `D:\launch-directory`,
		SessionID: "session-parent", ForkSession: true,
	})
	message := model.bootstrapCmd()()
	bootstrap, ok := message.(tuiBootstrapMsg)
	if !ok || bootstrap.err != nil {
		t.Fatalf("bootstrap = %#v", message)
	}
	if len(backend.forkRequests) != 1 || backend.forkRequests[0].id != "session-parent" || backend.forkRequests[0].revision != 14 {
		t.Fatalf("fork request = %+v, want parent revision 14", backend.forkRequests)
	}
	if bootstrap.session == nil || bootstrap.session.ID != "session-child" || bootstrap.workDir != `D:\projects\target` {
		t.Fatalf("bootstrap fork/workspace = session %#v workdir %q", bootstrap.session, bootstrap.workDir)
	}
	model.applyBootstrap(bootstrap)
	if model.opts.SessionID != "session-child" || model.opts.ForkSession || model.opts.WorkDir != `D:\projects\target` {
		t.Fatalf("applied fork state = session %q fork=%v workdir=%q", model.opts.SessionID, model.opts.ForkSession, model.opts.WorkDir)
	}
}

func TestTUIRejectsExplicitWorkspaceMismatchBeforeForkOrAppend(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	parent := &agent.Session{
		ID: "session-parent", Revision: 14, Workspace: workspaceB,
		LifecycleStatus: agent.SessionLifecycleActive,
		Messages:        []agent.SessionMessage{{Role: "assistant", Content: "existing"}},
	}
	backend := &fakeTUIBackend{
		loadedSession: cloneTUISession(parent),
		forkedSession: &agent.Session{
			ID: "session-child", Revision: 1, Workspace: workspaceB,
			ParentSessionID: "session-parent", ParentRevision: 14,
			LifecycleStatus: agent.SessionLifecycleActive,
		},
	}
	model := newTUIModel(backend, tuiOptions{
		BaseURL: "http://127.0.0.1:4000", WorkDir: workspaceA, WorkDirExplicit: true,
		SessionID: "session-parent", ForkSession: true,
	})
	message := model.bootstrapCmd()()
	bootstrap, ok := message.(tuiBootstrapMsg)
	if !ok || bootstrap.err == nil || !strings.Contains(bootstrap.err.Error(), "workspace conflict") {
		t.Fatalf("mismatched bootstrap = %#v, want clear workspace conflict", message)
	}
	if len(backend.forkRequests) != 0 || len(backend.savedSessions) != 0 {
		t.Fatalf("bootstrap mutated durable session: forks=%v saves=%d", backend.forkRequests, len(backend.savedSessions))
	}

	backend.calls = nil
	model.current = cloneTUISession(parent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := model.prepareTurnCmd(ctx, "must not append", cancel)()
	prepared, ok := turn.(tuiTurnPreparedMsg)
	if !ok || prepared.err == nil || !strings.Contains(prepared.err.Error(), "workspace conflict") {
		t.Fatalf("mismatched turn = %#v, want clear workspace conflict", turn)
	}
	if strings.Join(backend.calls, ",") != "" || len(backend.savedSessions) != 0 || len(backend.startRequests) != 0 {
		t.Fatalf("turn mismatch reached mutation: calls=%v saves=%d starts=%d", backend.calls, len(backend.savedSessions), len(backend.startRequests))
	}
}

func TestTUIPreparesDurableTurnBeforeStartingAgent(t *testing.T) {
	tests := []struct {
		name             string
		current          *agent.Session
		saveResult       sessionSaveResult
		wantSaveExpected *uint64
		wantStartRev     uint64
	}{
		{
			name:         "new session",
			saveResult:   sessionSaveResult{ID: "session-new", Version: 1, Revision: 1},
			wantStartRev: 1,
		},
		{
			name: "existing session CAS",
			current: &agent.Session{
				ID: "session-existing", Revision: 7, Version: 1,
				LifecycleStatus: agent.SessionLifecycleActive,
				Messages:        []agent.SessionMessage{{Role: "assistant", Content: "earlier"}},
			},
			saveResult:       sessionSaveResult{ID: "session-existing", Version: 1, Revision: 8},
			wantSaveExpected: revisionPointer(7),
			wantStartRev:     8,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeTUIBackend{saveResult: test.saveResult}
			model := newTUIModel(backend, tuiOptions{WorkDir: `D:\git\corelay-code`, Lang: "ko"})
			model.current = cloneTUISession(test.current)
			model.config = serverConfig{Provider: "provider", Model: "model"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			message := model.prepareTurnCmd(ctx, "검증 요청", cancel)()
			prepared, ok := message.(tuiTurnPreparedMsg)
			if !ok || prepared.err != nil {
				t.Fatalf("prepare command = %#v", message)
			}
			if strings.Join(backend.calls, ",") != "save,start" {
				t.Fatalf("backend order = %v, want [save start]", backend.calls)
			}
			if len(backend.saveExpected) != 1 {
				t.Fatalf("save expectations = %v", backend.saveExpected)
			}
			if test.wantSaveExpected == nil {
				if backend.saveExpected[0] != nil {
					t.Fatalf("new session expected CAS revision %d", *backend.saveExpected[0])
				}
			} else if backend.saveExpected[0] == nil || *backend.saveExpected[0] != *test.wantSaveExpected {
				t.Fatalf("save expected revision = %v, want %d", backend.saveExpected[0], *test.wantSaveExpected)
			}
			if len(backend.startRequests) != 1 {
				t.Fatalf("start requests = %d, want 1", len(backend.startRequests))
			}
			request := backend.startRequests[0]
			if request.DurableSessionID != test.saveResult.ID || request.ExpectedRevision == nil || *request.ExpectedRevision != test.wantStartRev {
				t.Fatalf("durable binding = id %q rev %v, want id %q rev %d", request.DurableSessionID, request.ExpectedRevision, test.saveResult.ID, test.wantStartRev)
			}
			if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1] != (chatMsg{Role: "user", Content: "검증 요청"}) {
				t.Fatalf("start messages = %#v", request.Messages)
			}
		})
	}
}

func TestTUIPropagatesRequestedModeAndShowsServerConfirmedMode(t *testing.T) {
	backend := &fakeTUIBackend{saveResult: sessionSaveResult{ID: "session-mode", Version: 1, Revision: 1}}
	model := newTUIModel(backend, tuiOptions{
		WorkDir: `D:\repo`,
		Mode:    agent.ExecutionModeFull,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared, ok := model.prepareTurnCmd(ctx, "run with full mode", cancel)().(tuiTurnPreparedMsg)
	if !ok || prepared.err != nil {
		t.Fatalf("prepare turn = %#v", prepared)
	}
	if len(backend.startRequests) != 1 || backend.startRequests[0].ExecutionPolicy == nil ||
		backend.startRequests[0].ExecutionPolicy.Mode != agent.ExecutionModeFull ||
		backend.startRequests[0].ExecutionPolicy.Revision != 0 {
		t.Fatalf("turn policy = %#v, want only requested mode", backend.startRequests)
	}

	model.applyWireEvent(agentWireEvent{
		Type: "session",
		Data: json.RawMessage(`{"sessionId":"run-1","executionPolicy":{"mode":"workspace","revision":7}}`),
	})
	if model.runtimeID != "run-1" {
		t.Fatalf("runtime ID = %q, want run-1", model.runtimeID)
	}
	if model.lastStatus != "Execution mode: workspace (revision 7)" {
		t.Fatalf("effective mode status = %q", model.lastStatus)
	}
}

func TestTUIInterruptedSessionRequiresExplicitReconcile(t *testing.T) {
	backend := &fakeTUIBackend{reconciliationPreview: agent.SessionReconciliationAssessment{
		EvidenceDigest:             "sha256:preview-token",
		CheckpointStatus:           "unavailable",
		SideEffectJudgment:         agent.ReconciliationJudgmentUnknown,
		ManualConfirmationRequired: true,
		ManualConfirmationReason:   "confirm shell effects",
		Files:                      []agent.ReconciliationFileEvidence{{Path: "<checkpoint>", Status: agent.ReconciliationFileUnavailable}},
	}}
	model := newTUIModel(backend, tuiOptions{})
	model.resize(100, 30)
	model.entries = nil
	model.current = &agent.Session{
		ID:                "session-1",
		Revision:          12,
		LifecycleStatus:   agent.SessionLifecycleInterrupted,
		ReconcileRequired: true,
		Interruption: &agent.SessionInterruption{
			Summary:         "authorized command may have changed files",
			ToolName:        "shell",
			InputDigest:     "sha256:bounded",
			SideEffectState: agent.SessionSideEffectMayHaveApplied,
		},
	}
	if model.sessionWritable() {
		t.Fatal("interrupted session was writable")
	}

	model.input.SetValue("must not send")
	next, command := model.submitInput()
	blocked := next.(tuiModel)
	if command != nil {
		t.Fatal("read-only session returned a turn command")
	}
	if len(backend.calls) != 0 {
		t.Fatalf("read-only send reached backend: %v", backend.calls)
	}
	if len(blocked.entries) == 0 || blocked.entries[len(blocked.entries)-1].Kind != tuiEntryError {
		t.Fatalf("read-only send did not explain the block: %#v", blocked.entries)
	}

	blocked.input.SetValue("/reconcile")
	next, command = blocked.submitInput()
	checking := next.(tuiModel)
	if command == nil || checking.confirm != nil {
		t.Fatalf("reconcile did not check evidence first: command=%v confirm=%#v", command, checking.confirm)
	}
	preview, ok := command().(tuiReconciliationPreviewMsg)
	if !ok || preview.err != nil || preview.revision != 12 {
		t.Fatalf("reconciliation preview = %#v", preview)
	}
	updated, nextCommand := checking.Update(preview)
	confirming := updated.(tuiModel)
	if nextCommand != nil || confirming.confirm == nil || confirming.confirm.Action != "reconcile" {
		t.Fatalf("reconcile preview did not open confirmation: command=%v confirm=%#v", nextCommand, confirming.confirm)
	}
	for _, expected := range []string{"checkpoint=unavailable", "judgment=unknown", "manual confirmation required", "sha256:preview-token"} {
		if !strings.Contains(confirming.confirm.Description, expected) {
			t.Fatalf("confirmation omits %q: %q", expected, confirming.confirm.Description)
		}
	}

	afterYes, reconcileCommand := confirming.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if reconcileCommand == nil || afterYes.(tuiModel).confirm != nil {
		t.Fatal("Y did not consume confirmation and schedule reconciliation")
	}
	operation, ok := reconcileCommand().(tuiSessionOperationMsg)
	if !ok || operation.err != nil || operation.action != "reconcile" {
		t.Fatalf("reconcile operation = %#v", operation)
	}
	if len(backend.reconcileCalls) != 1 || backend.reconcileCalls[0].id != "session-1" ||
		backend.reconcileCalls[0].revision != 12 || backend.reconcileCalls[0].evidenceDigest != "sha256:preview-token" ||
		!backend.reconcileCalls[0].manualAck {
		t.Fatalf("reconcile CAS calls = %#v", backend.reconcileCalls)
	}
}

func TestTUIConfirmationEnterCancels(t *testing.T) {
	backend := &fakeTUIBackend{}
	model := newTUIModel(backend, tuiOptions{})
	model.confirm = &tuiConfirmation{Action: "delete", Title: "Delete?"}
	next, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated := next.(tuiModel)
	if command != nil || updated.confirm != nil {
		t.Fatalf("Enter must cancel confirmation: command=%v confirm=%#v", command, updated.confirm)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("Enter executed a destructive action: %v", backend.calls)
	}
}

func TestTUICommandPalettePrefersSkillCollision(t *testing.T) {
	model := newTUIModel(&fakeTUIBackend{}, tuiOptions{})
	model.dynamicCommands = []tuiCommand{{Name: "plan", Description: "workspace skill", Skill: true}}
	model.input.SetValue("/plan")
	model.openPalette()
	if len(model.paletteMatches) == 0 || model.paletteMatches[0].Name != "plan" || !model.paletteMatches[0].Skill {
		t.Fatalf("palette collision winner = %#v, want dynamic skill", model.paletteMatches)
	}
	command, ok := model.commandNamed("plan")
	if !ok || !command.Skill || command.Local {
		t.Fatalf("command collision winner = %#v, %v", command, ok)
	}
}

func revisionPointer(value uint64) *uint64 {
	return &value
}
