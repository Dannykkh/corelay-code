package acpbridge

import (
	"context"
	"errors"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestNewSessionDefaultsToWorkspaceAndAdvertisesModes(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(agent.Event{Type: "done"}))
	response, err := fixture.backend.NewSession(context.Background(), acp.NewSessionRequest{
		CWD: fixture.workspace, MCPServers: []acp.MCPServer{},
	}, fixture.client)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if response.Modes == nil || response.Modes.CurrentModeID != string(agent.ExecutionModeWorkspace) || len(response.Modes.AvailableModes) != 3 {
		t.Fatalf("NewSession() modes = %#v", response.Modes)
	}
	for index, mode := range []agent.ExecutionMode{
		agent.ExecutionModeReadOnly,
		agent.ExecutionModeWorkspace,
		agent.ExecutionModeFull,
	} {
		if got := response.Modes.AvailableModes[index].ID; got != string(mode) {
			t.Fatalf("available mode %d = %q, want %q", index, got, mode)
		}
	}
	persisted, err := fixture.store.Get(response.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExecutionPolicy == nil || persisted.ExecutionPolicy.Mode != agent.ExecutionModeWorkspace ||
		persisted.ExecutionPolicy.Source != "default-workspace" || persisted.ExecutionPolicy.FullSelectionRevision != 0 {
		t.Fatalf("new session policy = %#v; full access must require an explicit mode choice", persisted.ExecutionPolicy)
	}
}

func TestSessionModeChangePersistsAndPromptUsesExactFullSnapshot(t *testing.T) {
	var captured agent.RunOptions
	runner := RunnerFunc(func(
		_ context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		options agent.RunOptions,
	) (<-chan agent.Event, error) {
		captured = options
		return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
	})
	fixture := newBackendFixture(t, runner)
	sessionID := fixture.newSession(t)

	if _, err := fixture.backend.SetSessionMode(context.Background(), acp.SetSessionModeRequest{
		SessionID: sessionID, ModeID: string(agent.ExecutionModeFull),
	}, fixture.client); err != nil {
		t.Fatalf("SetSessionMode(full) error = %v", err)
	}
	persisted, err := fixture.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExecutionPolicy == nil {
		t.Fatal("full-mode change was not persisted")
	}
	wantPolicy := *persisted.ExecutionPolicy
	if wantPolicy.Mode != agent.ExecutionModeFull || wantPolicy.Source != "user-selected" ||
		wantPolicy.Revision != 2 || wantPolicy.FullSelectionRevision != wantPolicy.Revision {
		t.Fatalf("persisted full policy = %#v", wantPolicy)
	}
	updates, _ := fixture.client.snapshot()
	if len(updates) != 1 || updates[0].Update.SessionUpdate != "current_mode_update" ||
		updates[0].Update.CurrentModeID != string(agent.ExecutionModeFull) {
		t.Fatalf("session mode updates = %#v", updates)
	}

	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "run with selected mode"}},
	}, fixture.client); err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	assertFullPromptOptions(t, captured, wantPolicy)

	var resumed agent.RunOptions
	resumedRunner := RunnerFunc(func(
		_ context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		options agent.RunOptions,
	) (<-chan agent.Event, error) {
		resumed = options
		return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
	})
	reopened, err := New(Options{
		Provider: fixture.backend.provider, DefaultModel: fixture.backend.defaultModel,
		Store: fixture.store, Runner: resumedRunner,
	})
	if err != nil {
		t.Fatalf("New(reopened backend) error = %v", err)
	}
	load, err := reopened.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID: sessionID, CWD: fixture.workspace, MCPServers: []acp.MCPServer{},
	}, fixture.client)
	if err != nil {
		t.Fatalf("LoadSession() error = %v", err)
	}
	if load.Modes == nil || load.Modes.CurrentModeID != string(agent.ExecutionModeFull) {
		t.Fatalf("reloaded mode state = %#v", load.Modes)
	}
	if _, err := reopened.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "resume selected mode"}},
	}, fixture.client); err != nil {
		t.Fatalf("reloaded Prompt() error = %v", err)
	}
	assertFullPromptOptions(t, resumed, wantPolicy)
}

func TestSessionModeRejectsInvalidSelectionWithoutChangingPolicy(t *testing.T) {
	fixture := newBackendFixture(t, scriptedRunner(agent.Event{Type: "done"}))
	sessionID := fixture.newSession(t)
	before, err := fixture.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.backend.SetSessionMode(context.Background(), acp.SetSessionModeRequest{
		SessionID: sessionID, ModeID: "full-access",
	}, fixture.client)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeInvalidParams {
		t.Fatalf("SetSessionMode(invalid) error = %#v, want invalid params", err)
	}
	after, err := fixture.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.ExecutionPolicy == nil || before.ExecutionPolicy == nil ||
		*after.ExecutionPolicy != *before.ExecutionPolicy {
		t.Fatalf("invalid mode changed durable session: before=%#v after=%#v", before, after)
	}
}

func TestLoadOldSessionDefaultsToWorkspaceAndPersistsSnapshotOnPrompt(t *testing.T) {
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
	legacy := agent.Session{
		Workspace: fixture.workspace, Messages: []agent.SessionMessage{},
		Provider: fixture.backend.provider.Name(), Model: fixture.backend.defaultModel,
	}
	if err := fixture.store.SaveExpected(&legacy, 0); err != nil {
		t.Fatal(err)
	}
	if persisted, err := fixture.store.Get(legacy.ID); err != nil || persisted.ExecutionPolicy != nil {
		t.Fatalf("test setup is not a pre-mode session: session=%#v err=%v", persisted, err)
	}
	loaded, err := fixture.backend.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID: legacy.ID, CWD: fixture.workspace, MCPServers: []acp.MCPServer{},
	}, fixture.client)
	if err != nil {
		t.Fatalf("LoadSession(legacy) error = %v", err)
	}
	if loaded.Modes == nil || loaded.Modes.CurrentModeID != string(agent.ExecutionModeWorkspace) {
		t.Fatalf("legacy session mode = %#v", loaded.Modes)
	}
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: legacy.ID, Prompt: []acp.ContentBlock{{Type: "text", Text: "continue"}},
	}, fixture.client); err != nil {
		t.Fatalf("Prompt(legacy) error = %v", err)
	}
	if captured.ExecutionPolicy == nil || captured.ExecutionPolicy.Mode != agent.ExecutionModeWorkspace ||
		captured.ExecutionPolicy.Source != "default-workspace" {
		t.Fatalf("legacy prompt policy = %#v", captured.ExecutionPolicy)
	}
	persisted, err := fixture.store.Get(legacy.ID)
	if err != nil || persisted.ExecutionPolicy == nil || persisted.ExecutionPolicy.Mode != agent.ExecutionModeWorkspace {
		t.Fatalf("legacy policy was not persisted with the prompt: session=%#v err=%v", persisted, err)
	}
}

func assertFullPromptOptions(t *testing.T, options agent.RunOptions, want agent.ExecutionPolicySnapshot) {
	t.Helper()
	if options.SessionRevision == 0 {
		t.Fatal("prompt did not bind the run to its durable session revision")
	}
	if options.ExecutionPolicy == nil || *options.ExecutionPolicy != want {
		t.Fatalf("prompt policy = %#v, want %#v", options.ExecutionPolicy, want)
	}
	if options.SandboxRunner == nil || options.SandboxRunner.Name() != "unconfined" ||
		options.SandboxPolicy.Enforcement != sandbox.EnforcementDisabled {
		t.Fatalf("full mode did not select the explicit current-user host runner: runner=%v policy=%#v", options.SandboxRunner, options.SandboxPolicy)
	}
	if options.ExecutionPolicy.RuntimeCapabilities != options.SandboxRunner.Capabilities() {
		t.Fatalf("snapshot capabilities %#v do not match selected runner %#v",
			options.ExecutionPolicy.RuntimeCapabilities, options.SandboxRunner.Capabilities())
	}
}
