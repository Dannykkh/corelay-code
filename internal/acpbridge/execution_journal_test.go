package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type journalSpyStore struct {
	*agent.SessionStore
	mu          sync.Mutex
	markCalls   int
	updateCalls int
	commitCalls int
	failMark    error
}

func (store *journalSpyStore) MarkInterrupted(
	id string,
	expected uint64,
	marker agent.SessionInterruption,
) (*agent.Session, error) {
	store.mu.Lock()
	store.markCalls++
	err := store.failMark
	store.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return store.SessionStore.MarkInterrupted(id, expected, marker)
}

func (store *journalSpyStore) UpdateInterruptedRun(
	id string,
	expected uint64,
	expectedMarker agent.SessionInterruption,
	nextMarker agent.SessionInterruption,
) (*agent.Session, error) {
	store.mu.Lock()
	store.updateCalls++
	store.mu.Unlock()
	return store.SessionStore.UpdateInterruptedRun(id, expected, expectedMarker, nextMarker)
}

func (store *journalSpyStore) CommitInterruptedRun(
	session *agent.Session,
	expected uint64,
	marker agent.SessionInterruption,
) error {
	store.mu.Lock()
	store.commitCalls++
	store.mu.Unlock()
	return store.SessionStore.CommitInterruptedRun(session, expected, marker)
}

func (store *journalSpyStore) counts() (int, int, int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.markCalls, store.updateCalls, store.commitCalls
}

func newJournalBackend(
	t *testing.T,
	store SessionStore,
	runner Runner,
	workspace string,
) (*Backend, *recordingClient, string) {
	t.Helper()
	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a", DisplayName: "Model A"}}},
		DefaultModel: "model-a",
		Store:        store,
		Runner:       runner,
		Version:      "journal-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingClient{permissionResponse: acp.RequestPermissionResponse{
		Outcome: acp.PermissionOutcome{Outcome: "selected", OptionID: "allow_once"},
	}}
	response, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: workspace, MCPServers: []acp.MCPServer{},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	return backend, client, response.SessionID
}

func TestACPExecutionJournalAggregatesParallelStartsAndAtomicallyCommitsSuccess(t *testing.T) {
	store := &journalSpyStore{SessionStore: agent.NewSessionStore(t.TempDir())}
	workspace := t.TempDir()
	var sessionID string
	var guarded agent.Session
	runner := RunnerFunc(func(
		_ context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		options agent.RunOptions,
	) (<-chan agent.Event, error) {
		if options.PreExecutionJournal == nil {
			return nil, errors.New("pre-execution journal was not composed")
		}
		entries := []agent.ToolExecutionJournalEntry{
			{ID: "call-b", Name: "Write", InputDigest: "sha256:" + strings.Repeat("b", 64), RunID: "run-parallel"},
			{ID: "call-a", Name: "Read", InputDigest: "sha256:" + strings.Repeat("a", 64), RunID: "run-parallel"},
		}
		errs := make(chan error, len(entries))
		var group sync.WaitGroup
		for _, entry := range entries {
			entry := entry
			group.Add(1)
			go func() {
				defer group.Done()
				errs <- options.PreExecutionJournal(entry)
			}()
		}
		group.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				return nil, err
			}
		}
		persisted, err := store.Get(sessionID)
		if err != nil {
			return nil, err
		}
		guarded = cloneSession(*persisted)

		stream := make(chan agent.Event, 5)
		for _, entry := range entries {
			stream <- agent.Event{Type: "tool_execution_start", Data: map[string]string{
				"id": entry.ID, "name": entry.Name, "inputDigest": entry.InputDigest, "runId": entry.RunID,
			}}
			stream <- agent.Event{Type: "tool_result", Data: map[string]any{
				"id": entry.ID, "name": entry.Name, "result": "ok", "executed": true,
			}}
		}
		stream <- journalSuccessfulDone()
		close(stream)
		return stream, nil
	})
	backend, client, id := newJournalBackend(t, store, runner, workspace)
	sessionID = id

	response, err := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "run two authorized tools"}},
	}, client)
	if err != nil || response.StopReason != acp.StopEndTurn {
		t.Fatalf("Prompt() = (%#v, %v)", response, err)
	}
	if !guarded.ReconcileRequired || guarded.Interruption == nil ||
		guarded.Interruption.ToolName != "multiple_tools" || guarded.Interruption.RunID != "run-parallel" {
		t.Fatalf("parallel journal did not persist an aggregate guard: %#v", guarded)
	}
	final, err := store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if final.ReconcileRequired || final.Interruption != nil || final.LifecycleStatus != agent.SessionLifecycleActive ||
		final.LastRunTerminal == nil || final.LastRunTerminal.BlocksSuccess() {
		t.Fatalf("successful terminal did not atomically clear exact guard: %#v", final)
	}
	mark, update, commit := store.counts()
	if mark != 1 || update != 1 || commit != 1 {
		t.Fatalf("journal CAS counts = mark:%d update:%d commit:%d, want 1/1/1", mark, update, commit)
	}
}

func TestACPExecutionJournalRejectsStaleAndDifferentRunsWithoutRetry(t *testing.T) {
	t.Run("stale callback after prompt", func(t *testing.T) {
		store := &journalSpyStore{SessionStore: agent.NewSessionStore(t.TempDir())}
		var callback agent.ToolExecutionJournal
		runner := RunnerFunc(func(
			_ context.Context, _ types.Provider, _ string, _ []types.Message, _ string, options agent.RunOptions,
		) (<-chan agent.Event, error) {
			callback = options.PreExecutionJournal
			return scriptedRunner(journalSuccessfulDone()).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
		})
		backend, client, sessionID := newJournalBackend(t, store, runner, t.TempDir())
		if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
			SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "finish"}},
		}, client); err != nil {
			t.Fatal(err)
		}
		if callback == nil {
			t.Fatal("journal callback was not captured")
		}
		if err := callback(agent.ToolExecutionJournalEntry{
			ID: "late", Name: "Write", RunID: "run-late", InputDigest: "sha256:" + strings.Repeat("c", 64),
		}); err == nil {
			t.Fatal("stale callback unexpectedly adopted a completed prompt")
		}
		mark, update, commit := store.counts()
		if mark != 0 || update != 0 || commit != 0 {
			t.Fatalf("stale callback mutated journal: %d/%d/%d", mark, update, commit)
		}
	})

	t.Run("different active run", func(t *testing.T) {
		store := &journalSpyStore{SessionStore: agent.NewSessionStore(t.TempDir())}
		var secondErr error
		runner := RunnerFunc(func(
			_ context.Context, _ types.Provider, _ string, _ []types.Message, _ string, options agent.RunOptions,
		) (<-chan agent.Event, error) {
			first := agent.ToolExecutionJournalEntry{
				ID: "first", Name: "Write", RunID: "run-first", InputDigest: "sha256:" + strings.Repeat("d", 64),
			}
			if err := options.PreExecutionJournal(first); err != nil {
				return nil, err
			}
			secondErr = options.PreExecutionJournal(agent.ToolExecutionJournalEntry{
				ID: "second", Name: "Write", RunID: "run-second", InputDigest: "sha256:" + strings.Repeat("e", 64),
			})
			stream := make(chan agent.Event)
			close(stream)
			return stream, nil
		})
		backend, client, sessionID := newJournalBackend(t, store, runner, t.TempDir())
		if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
			SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "mismatched run"}},
		}, client); err == nil {
			t.Fatal("Prompt() unexpectedly succeeded")
		}
		if secondErr == nil {
			t.Fatal("different run id unexpectedly adopted the active marker")
		}
		persisted, err := store.Get(sessionID)
		if err != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
			persisted.Interruption.RunID != "run-first" {
			t.Fatalf("first exact marker was not preserved: (%#v, %v)", persisted, err)
		}
		mark, update, commit := store.counts()
		if mark != 1 || update != 0 || commit != 0 {
			t.Fatalf("different run retried/adopted revision: %d/%d/%d", mark, update, commit)
		}
	})
}

func TestACPExecutionJournalStoreFailureIsContentFreeAndQuarantinesRuntime(t *testing.T) {
	rawError := `persist C:\private\session.json: access denied`
	store := &journalSpyStore{SessionStore: agent.NewSessionStore(t.TempDir()), failMark: errors.New(rawError)}
	var callbackErr error
	runner := RunnerFunc(func(
		_ context.Context, _ types.Provider, _ string, _ []types.Message, _ string, options agent.RunOptions,
	) (<-chan agent.Event, error) {
		callbackErr = options.PreExecutionJournal(agent.ToolExecutionJournalEntry{
			ID: "call-fail", Name: "Write", RunID: "run-fail", InputDigest: "sha256:" + strings.Repeat("f", 64),
		})
		return scriptedRunner(journalSuccessfulDone()).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
	})
	backend, client, sessionID := newJournalBackend(t, store, runner, t.TempDir())
	_, promptErr := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "attempt tool"}},
	}, client)
	if callbackErr == nil || strings.Contains(callbackErr.Error(), rawError) || strings.Contains(callbackErr.Error(), "session.json") {
		t.Fatalf("journal callback error leaked storage detail: %v", callbackErr)
	}
	if promptErr == nil || strings.Contains(promptErr.Error(), rawError) || strings.Contains(promptErr.Error(), "session.json") {
		t.Fatalf("Prompt error leaked storage detail: %v", promptErr)
	}
	backend.mu.Lock()
	quarantined := backend.sessions[sessionID] != nil && backend.sessions[sessionID].quarantined
	backend.mu.Unlock()
	if !quarantined {
		t.Fatal("ambiguous journal persistence failure did not quarantine the runtime")
	}
	persisted, err := store.Get(sessionID)
	if err != nil || persisted.ReconcileRequired || persisted.Interruption != nil {
		t.Fatalf("failed journal unexpectedly fabricated a persisted marker: (%#v, %v)", persisted, err)
	}
	mark, update, commit := store.counts()
	if mark != 1 || update != 0 || commit != 0 {
		t.Fatalf("failed journal CAS counts = %d/%d/%d", mark, update, commit)
	}
}

func TestACPExecutionJournalEnrichesFailureAndRejectsNonCommittingSuccess(t *testing.T) {
	for _, test := range []struct {
		name string
		done agent.Event
	}{
		{
			name: "graceful failure enriches applied marker",
			done: agent.Event{Type: "done", Data: map[string]any{
				"kind": string(agent.RunTerminalFailed), "stopReason": "completion_blocked",
				"durablePolicy":      string(agent.RunTerminalDurableReconcile),
				"terminalState":      agent.EvidenceTerminalBlocked,
				"completionStatus":   agent.CompletionStatusBlocked,
				"completionCriteria": 1, "completionBlocked": 1,
			}},
		},
		{
			name: "durable none cannot report success while guarded",
			done: agent.Event{Type: "done", Data: map[string]any{
				"kind": string(agent.RunTerminalCommand), "stopReason": "command",
				"durablePolicy": string(agent.RunTerminalDurableNone),
				"terminalState": agent.EvidenceTerminalVerified,
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &journalSpyStore{SessionStore: agent.NewSessionStore(t.TempDir())}
			digest := "sha256:" + strings.Repeat("9", 64)
			runner := RunnerFunc(func(
				_ context.Context, _ types.Provider, _ string, _ []types.Message, _ string, options agent.RunOptions,
			) (<-chan agent.Event, error) {
				entry := agent.ToolExecutionJournalEntry{
					ID: "call-applied", Name: "Write", RunID: "run-applied", InputDigest: digest,
				}
				if err := options.PreExecutionJournal(entry); err != nil {
					return nil, err
				}
				stream := make(chan agent.Event, 3)
				stream <- agent.Event{Type: "tool_execution_start", Data: map[string]string{
					"id": entry.ID, "name": entry.Name, "runId": entry.RunID, "inputDigest": entry.InputDigest,
				}}
				stream <- agent.Event{Type: "tool_result", Data: map[string]any{
					"id": entry.ID, "name": entry.Name, "result": "ok", "executed": true,
				}}
				stream <- test.done
				close(stream)
				return stream, nil
			})
			backend, client, sessionID := newJournalBackend(t, store, runner, t.TempDir())
			_, promptErr := backend.Prompt(context.Background(), acp.PromptRequest{
				SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "non-success tool run"}},
			}, client)
			if !isInvalidRequest(promptErr) {
				t.Fatalf("Prompt() error = %#v, want fail-closed reconciliation", promptErr)
			}
			persisted, err := store.Get(sessionID)
			if err != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
				persisted.Interruption.RunID != "run-applied" ||
				persisted.Interruption.SideEffectState != agent.SessionSideEffectApplied {
				t.Fatalf("enriched failure marker = (%#v, %v)", persisted, err)
			}
			mark, update, commit := store.counts()
			if mark != 1 || update != 1 || commit != 0 {
				t.Fatalf("failure journal CAS counts = %d/%d/%d", mark, update, commit)
			}
		})
	}
}

func journalSuccessfulDone() agent.Event {
	return agent.Event{Type: "done", Data: map[string]any{
		"kind":                string(agent.RunTerminalCompleted),
		"stopReason":          "end_turn",
		"durablePolicy":       string(agent.RunTerminalDurableCommit),
		"terminalState":       agent.EvidenceTerminalVerified,
		"completionStatus":    agent.CompletionStatusComplete,
		"completionRevision":  1,
		"completionCriteria":  1,
		"completionSatisfied": 1,
	}}
}

const hardCrashChildEnv = "CORELAY_ACP_HARD_CRASH_CHILD"

func TestACPExecutionJournalSurvivesHardProcessExit(t *testing.T) {
	if os.Getenv(hardCrashChildEnv) == "1" {
		runACPExecutionJournalCrashChild(t)
		return
	}

	root := t.TempDir()
	storage := filepath.Join(root, "store")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(root, "session-id")
	canary := filepath.Join(workspace, "hard-crash-canary.txt")
	command := exec.Command(os.Args[0], "-test.run=^TestACPExecutionJournalSurvivesHardProcessExit$", "-test.count=1")
	command.Env = append(os.Environ(),
		hardCrashChildEnv+"=1",
		"CORELAY_ACP_CRASH_STORAGE="+storage,
		"CORELAY_ACP_CRASH_WORKSPACE="+workspace,
		"CORELAY_ACP_CRASH_SESSION_FILE="+sessionFile,
		"CORELAY_CONFIG_DIR="+filepath.Join(root, "config"),
		"CORELAY_MEMORY=off",
		"CORELAY_AUTOSKILL=off",
		"CORELAY_AUTOVERIFY=off",
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 91 {
		t.Fatalf("hard-crash child = %v, output:\n%s", err, output)
	}

	sessionBytes, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.TrimSpace(string(sessionBytes))
	canaryBefore, err := os.ReadFile(canary)
	if err != nil || string(canaryBefore) != "1\n" {
		t.Fatalf("hard-crash canary = %q, %v", canaryBefore, err)
	}
	store := agent.NewSessionStore(storage)
	persisted, err := store.Get(sessionID)
	if err != nil || !persisted.ReconcileRequired || persisted.Interruption == nil ||
		persisted.Interruption.SideEffectState != agent.SessionSideEffectStarted {
		t.Fatalf("fresh process did not observe durable pre-execution guard: (%#v, %v)", persisted, err)
	}

	backend, err := New(Options{
		Provider:     &fakeProvider{models: []types.ModelInfo{{ID: "model-a"}}},
		DefaultModel: "model-a",
		Store:        store,
		Runner: scriptedRunner(
			agent.Event{Type: "text", Data: "continued without a tool"}, journalSuccessfulDone(),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingClient{}
	load := acp.LoadSessionRequest{SessionID: sessionID, CWD: workspace, MCPServers: []acp.MCPServer{}}
	if _, err := backend.LoadSession(context.Background(), load, client); !isInvalidRequest(err) {
		t.Fatalf("fresh LoadSession() error = %#v, want reconciliation block", err)
	}
	if _, err := backend.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionID: sessionID, CWD: workspace, MCPServers: []acp.MCPServer{},
	}, client); !isInvalidRequest(err) {
		t.Fatalf("fresh ResumeSession() error = %#v, want reconciliation block", err)
	}
	reconciled, err := store.MarkReconciled(sessionID, persisted.Revision)
	if err != nil || reconciled.ReconcileRequired {
		t.Fatalf("MarkReconciled() = (%#v, %v)", reconciled, err)
	}
	if _, err := backend.LoadSession(context.Background(), load, client); err != nil {
		t.Fatalf("LoadSession() after explicit reconciliation: %v", err)
	}
	if _, err := backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "continue with text only"}},
	}, client); err != nil {
		t.Fatalf("text-only continuation: %v", err)
	}
	canaryAfter, err := os.ReadFile(canary)
	if err != nil || string(canaryAfter) != "1\n" {
		t.Fatalf("side effect was repeated after recovery: %q, %v", canaryAfter, err)
	}
}

func runACPExecutionJournalCrashChild(t *testing.T) {
	storage := os.Getenv("CORELAY_ACP_CRASH_STORAGE")
	workspace := os.Getenv("CORELAY_ACP_CRASH_WORKSPACE")
	sessionFile := os.Getenv("CORELAY_ACP_CRASH_SESSION_FILE")
	store := agent.NewSessionStore(storage)
	provider := &hardCrashWriteProvider{}
	backend, err := New(Options{
		Provider: provider, DefaultModel: "model-a", Store: store, Runner: AgentRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingClient{permissionResponse: acp.RequestPermissionResponse{
		Outcome: acp.PermissionOutcome{Outcome: "selected", OptionID: "allow_once"},
	}}
	created, err := backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: workspace, MCPServers: []acp.MCPServer{},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte(created.SessionID), 0o600); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(workspace, "hard-crash-canary.txt")
	go func() {
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if data, readErr := os.ReadFile(canary); readErr == nil && string(data) == "1\n" {
					os.Exit(91)
				}
			case <-deadline.C:
				os.Exit(92)
			}
		}
	}()
	_, _ = backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: created.SessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "write the crash canary exactly once"}},
	}, client)
	os.Exit(93)
}

type hardCrashWriteProvider struct {
	mu    sync.Mutex
	calls int
}

func (*hardCrashWriteProvider) Name() string        { return "fake" }
func (*hardCrashWriteProvider) DisplayName() string { return "Hard Crash Test Provider" }
func (*hardCrashWriteProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{{ID: "model-a", DisplayName: "Model A"}}
}
func (*hardCrashWriteProvider) Validate() error { return nil }

func (provider *hardCrashWriteProvider) StreamMessage(
	ctx context.Context,
	_ *types.MessagesRequest,
	_ *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	provider.mu.Lock()
	call := provider.calls
	provider.calls++
	provider.mu.Unlock()
	if call != 0 {
		stream := make(chan types.SSEEvent)
		go func() {
			<-ctx.Done()
			close(stream)
		}()
		return stream, nil
	}
	stream := make(chan types.SSEEvent, 5)
	stream <- types.SSEEvent{Type: "content_block_start", ContentBlock: journalJSON(map[string]string{
		"type": "tool_use", "id": "hard-crash-write", "name": "Write",
	})}
	stream <- types.SSEEvent{Type: "content_block_delta", Delta: journalJSON(map[string]string{
		"type": "input_json_delta", "partial_json": `{"file_path":"hard-crash-canary.txt","content":"1\n"}`,
	})}
	stream <- types.SSEEvent{Type: "content_block_stop"}
	stream <- types.SSEEvent{Type: "message_delta", Delta: journalJSON(map[string]string{"stop_reason": "tool_use"})}
	stream <- types.SSEEvent{Type: "message_stop"}
	close(stream)
	return stream, nil
}

func journalJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func isInvalidRequest(err error) bool {
	var rpcErr *acp.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == acp.CodeInvalidRequest
}
