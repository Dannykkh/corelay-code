package acpbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestACPResumeInjectsCommittedResultInventoryAndCloseRetainsBlobs(t *testing.T) {
	var firstOptions agent.RunOptions
	fixture := newBackendFixture(t, RunnerFunc(func(
		_ context.Context,
		_ types.Provider,
		_ string,
		_ []types.Message,
		_ string,
		options agent.RunOptions,
	) (<-chan agent.Event, error) {
		firstOptions = options
		return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
	}))
	sessionID := fixture.newSession(t)
	persisted, err := fixture.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	memory, _, err := fixture.store.OpenToolResultMemory(sessionID, fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	committed := "Authorization: Bearer " + secret + "\n" + strings.Repeat("committed/", 500)
	orphan := strings.Repeat("ACP_CAS_ORPHAN_MUST_NOT_REPLAY/", 300)
	reference, replaced := memory.StoreResult("Read", committed)
	if !replaced {
		t.Fatal("failed to store committed result")
	}
	if _, replaced := memory.StoreResult("Read", orphan); !replaced {
		t.Fatal("failed to seed orphan result")
	}
	persisted.Messages = append(persisted.Messages, agent.SessionMessage{
		Role:       "tool",
		Content:    reference,
		ToolName:   "Read",
		ToolInput:  map[string]string{"toolCallId": "tool-result-history", "inputDigest": "sha256:" + strings.Repeat("a", 64)},
		ToolResult: reference,
	})
	if err := fixture.store.SaveExpected(persisted, persisted.Revision); err != nil {
		t.Fatal(err)
	}
	replayClient := &recordingClient{}
	if _, err := fixture.backend.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID: sessionID, CWD: fixture.workspace, MCPServers: []acp.MCPServer{},
	}, replayClient); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "continue"}},
	}, replayClient); err != nil {
		t.Fatal(err)
	}
	assertACPToolResultOptions(t, firstOptions, reference, orphan)
	updates, _ := replayClient.snapshot()
	wire, _ := json.Marshal(updates)
	if strings.Contains(string(wire), secret) || strings.Contains(string(wire), "ACP_CAS_ORPHAN_MUST_NOT_REPLAY") ||
		strings.Contains(string(wire), "tool-result://sha256:") {
		t.Fatalf("ACP replay crossed raw/reference payload boundary: %s", wire)
	}
	if _, err := fixture.backend.CloseSession(context.Background(), acp.CloseSessionRequest{SessionID: sessionID}, replayClient); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.LoadResult(firstOptions.ToolResultReferences[0].ID); err != nil {
		t.Fatalf("ACP CloseSession removed durable result: %v", err)
	}

	var resumedOptions agent.RunOptions
	resumed, err := New(Options{
		Provider: fixture.backend.provider, DefaultModel: "model-a", Store: fixture.store,
		Runner: RunnerFunc(func(
			_ context.Context,
			_ types.Provider,
			_ string,
			_ []types.Message,
			_ string,
			options agent.RunOptions,
		) (<-chan agent.Event, error) {
			resumedOptions = options
			return scriptedRunner(agent.Event{Type: "done"}).Start(context.Background(), nil, "", nil, "", agent.RunOptions{})
		}),
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeClient := &recordingClient{}
	if _, err := resumed.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionID: sessionID, CWD: fixture.workspace, MCPServers: []acp.MCPServer{},
	}, resumeClient); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Prompt(context.Background(), acp.PromptRequest{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "resume"}},
	}, resumeClient); err != nil {
		t.Fatal(err)
	}
	assertACPToolResultOptions(t, resumedOptions, reference, orphan)
}

func assertACPToolResultOptions(
	t *testing.T,
	options agent.RunOptions,
	committedReference string,
	orphanContent string,
) {
	t.Helper()
	store, storeOK := options.ToolResultStore.(*agent.SessionMemory)
	reader, readerOK := options.ToolResultReader.(*agent.SessionMemory)
	if !storeOK || !readerOK || store != reader {
		t.Fatalf("run did not receive one shared session result memory: store=%T reader=%T", options.ToolResultStore, options.ToolResultReader)
	}
	if len(options.ToolResultReferences) != 1 || !strings.Contains(committedReference, options.ToolResultReferences[0].Digest) {
		t.Fatalf("committed references = %#v", options.ToolResultReferences)
	}
	encoded, _ := json.Marshal(options.ToolResultReferences)
	if strings.Contains(string(encoded), orphanContent) || strings.Contains(string(encoded), "ACP_CAS_ORPHAN_MUST_NOT_REPLAY") {
		t.Fatalf("orphan entered committed inventory: %s", encoded)
	}
	chunk, err := options.ToolResultReader.ReadResult(options.ToolResultReferences[0].ID, 0, 4096)
	if err != nil || strings.Contains(chunk.Content, "sk-proj-") || !strings.Contains(chunk.Content, "[REDACTED]") {
		t.Fatalf("bounded reader result = (%#v, %v)", chunk, err)
	}
}
