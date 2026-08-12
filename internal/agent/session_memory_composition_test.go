package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestSessionMemoryReferenceAndPagedReadsRedactBeforeBounding(t *testing.T) {
	store := openSessionMemoryForTest(t, t.TempDir(), "redact-before-page")
	bearer := "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	apiKey := "api-secret-abcdefghijklmnopqrstuvwxyz"
	pemBody := strings.Repeat("PRIVATE-MATERIAL-", 80)
	content := "Authorization: Bearer " + bearer + "\n" +
		"api_key=" + apiKey + "\n" +
		"-----BEGIN PRIVATE KEY-----\n" + pemBody + "\n-----END PRIVATE KEY-----\n" +
		strings.Repeat("safe-tail/", 400)

	reference, replaced := store.StoreResult("Read", content)
	if !replaced {
		t.Fatal("large result was not stored")
	}
	digest := strings.TrimPrefix(sessionMemoryResultID(content), "result_")
	if !strings.Contains(reference, "tool-result://sha256:"+digest) ||
		strings.Contains(reference, bearer) || strings.Contains(reference, apiKey) ||
		strings.Contains(reference, pemBody) || !strings.Contains(reference, "[REDACTED") {
		t.Fatalf("unsafe or non-canonical reference: %q", reference)
	}

	var rebuilt strings.Builder
	var offset int64
	for calls := 0; calls < 1000; calls++ {
		chunk, err := store.ReadResult("result_"+digest, offset, 11)
		if err != nil {
			t.Fatalf("ReadResult(offset=%d): %v", offset, err)
		}
		if len(chunk.Content) > 11 || chunk.Offset != offset || chunk.NextOffset < chunk.Offset {
			t.Fatalf("invalid bounded chunk: %#v", chunk)
		}
		rebuilt.WriteString(chunk.Content)
		offset = chunk.NextOffset
		if chunk.EOF {
			break
		}
	}
	redacted := rebuilt.String()
	for _, secret := range []string{bearer, apiKey, pemBody, "PRIVATE-MATERIAL"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("paged redacted view leaked %q", secret)
		}
	}
	if !strings.Contains(redacted, "[REDACTED") {
		t.Fatalf("paged view lacks redaction evidence: %q", redacted[:minInt(len(redacted), 300)])
	}

	// This offset pointed into the raw PEM body before redaction. It may now
	// address a later safe byte or be out of range, but it must never recover a
	// fragment of the private material.
	rawPEMMiddle := int64(strings.Index(content, pemBody) + len(pemBody)/2)
	chunk, err := store.ReadResult("result_"+digest, rawPEMMiddle, 128)
	if err == nil && (strings.Contains(chunk.Content, "PRIVATE-MATERIAL") || strings.Contains(chunk.Content, bearer)) {
		t.Fatalf("raw-offset read recovered secret material: %#v", chunk)
	}
}

func TestSessionMemoryCommittedInventoryAndCloneExcludeOrphans(t *testing.T) {
	base := t.TempDir()
	parent := openSessionMemoryForTest(t, base, "parent")
	child := openSessionMemoryForTest(t, base, "child")
	committed := sessionMemoryLargeResult("committed/")
	orphan := sessionMemoryLargeResult("orphan/")
	reference, replaced := parent.StoreResult("Read", committed)
	if !replaced {
		t.Fatal("failed to store committed result")
	}
	orphanReference, replaced := parent.StoreResult("Read", orphan)
	if !replaced {
		t.Fatal("failed to store orphan result")
	}
	messages := []SessionMessage{
		{Role: "tool", Content: reference, ToolResult: reference},
		{Role: "user", Content: "please load " + orphanReference},
		{Role: "assistant", ToolResult: orphanReference},
		{Role: "tool", Content: "untrusted tool output mentioned " + orphanReference},
	}
	references := parent.ReferencesForMessages(messages)
	if len(references) != 1 || references[0].ID != sessionMemoryResultID(committed) {
		t.Fatalf("committed inventory = %#v", references)
	}
	if err := parent.CloneResultsTo(child, committedToolResultDigests(messages)); err != nil {
		t.Fatalf("CloneResultsTo: %v", err)
	}
	if got, err := child.LoadResult(sessionMemoryResultID(committed)); err != nil || got != committed {
		t.Fatalf("child committed result bytes=%d err=%v", len(got), err)
	}
	if _, err := child.LoadResult(sessionMemoryResultID(orphan)); err == nil {
		t.Fatal("child inherited an uncommitted/orphan result")
	}
	if err := parent.CleanupSafe(); err != nil {
		t.Fatal(err)
	}
	if got, err := child.LoadResult(sessionMemoryResultID(committed)); err != nil || got != committed {
		t.Fatalf("child result did not survive parent cleanup: bytes=%d err=%v", len(got), err)
	}
}

func TestSessionStoreWorkspaceBoundResultForkCloseAndDelete(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	otherWorkspace := filepath.Join(base, "other")
	store := NewSessionStore(base)
	parent := &Session{Workspace: workspace, Messages: []SessionMessage{{Role: "user", Content: "seed"}}}
	if err := store.Save(parent); err != nil {
		t.Fatal(err)
	}
	memory, refs, err := store.OpenToolResultMemory(parent.ID, workspace)
	if err != nil || len(refs) != 0 {
		t.Fatalf("OpenToolResultMemory = (%#v, %v)", refs, err)
	}
	if _, _, err := store.OpenToolResultMemory(parent.ID, otherWorkspace); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("workspace mismatch = %v, want ErrSessionConflict", err)
	}
	committed := sessionMemoryLargeResult("durable-committed/")
	orphan := sessionMemoryLargeResult("cas-loser-orphan/")
	reference, replaced := memory.StoreResult("Read", committed)
	if !replaced {
		t.Fatal("failed to store committed result")
	}
	if _, replaced := memory.StoreResult("Read", orphan); !replaced {
		t.Fatal("failed to store orphan result")
	}
	parent.Messages = append(parent.Messages, SessionMessage{Role: "tool", Content: reference, ToolResult: reference})
	if err := store.SaveExpected(parent, parent.Revision); err != nil {
		t.Fatal(err)
	}
	child, err := store.Fork(parent.ID, parent.Revision)
	if err != nil {
		t.Fatal(err)
	}
	childMemory, childRefs, err := store.OpenToolResultMemory(child.ID, child.Workspace)
	if err != nil || len(childRefs) != 1 || childRefs[0].ID != sessionMemoryResultID(committed) {
		t.Fatalf("child inventory = (%#v, %v)", childRefs, err)
	}
	if _, err := childMemory.LoadResult(sessionMemoryResultID(orphan)); err == nil {
		t.Fatal("fork cloned CAS-orphan result")
	}
	if _, err := store.DeleteExpected(parent.ID, parent.Revision-1); !errors.Is(err, ErrSessionRevisionConflict) {
		t.Fatalf("stale DeleteExpected = %v", err)
	}
	deleted, err := store.DeleteExpected(parent.ID, parent.Revision)
	if err != nil || deleted.CleanupPending || deleted.ResultCount != 2 {
		t.Fatalf("DeleteExpected = (%#v, %v)", deleted, err)
	}
	if got, err := childMemory.LoadResult(sessionMemoryResultID(committed)); err != nil || got != committed {
		t.Fatalf("fork result did not survive parent delete: bytes=%d err=%v", len(got), err)
	}
	closed, err := store.Close(child.ID, child.Revision)
	if err != nil {
		t.Fatal(err)
	}
	reopened, closedRefs, err := store.OpenToolResultMemory(child.ID, child.Workspace)
	if err != nil || len(closedRefs) != 1 {
		t.Fatalf("closed session result reopen = (%#v, %v)", closedRefs, err)
	}
	if got, err := reopened.LoadResult(sessionMemoryResultID(committed)); err != nil || got != committed {
		t.Fatalf("Close removed durable result: bytes=%d err=%v", len(got), err)
	}
	if _, err := store.DeleteExpected(child.ID, closed.Revision); err != nil {
		t.Fatal(err)
	}
}

type capturingToolResultProvider struct {
	*scriptedLoopProvider
	mu       sync.Mutex
	requests [][]byte
	tools    [][]types.ToolDef
}

func (provider *capturingToolResultProvider) StreamMessage(
	ctx context.Context,
	request *types.MessagesRequest,
	options *types.StreamOptions,
) (<-chan types.SSEEvent, error) {
	encoded, _ := json.Marshal(request)
	provider.mu.Lock()
	provider.requests = append(provider.requests, encoded)
	provider.tools = append(provider.tools, append([]types.ToolDef(nil), request.Tools...))
	provider.mu.Unlock()
	return provider.scriptedLoopProvider.StreamMessage(ctx, request, options)
}

func (provider *capturingToolResultProvider) snapshot() ([][]byte, [][]types.ToolDef) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	requests := make([][]byte, len(provider.requests))
	for index := range provider.requests {
		requests[index] = append([]byte(nil), provider.requests[index]...)
	}
	tools := make([][]types.ToolDef, len(provider.tools))
	for index := range provider.tools {
		tools[index] = append([]types.ToolDef(nil), provider.tools[index]...)
	}
	return requests, tools
}

func TestRunLoopOffloadsBeforeEventObserverAndNextModelRequest(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	rawTail := "RAW_TAIL_MUST_NOT_REACH_DURABLE_OR_PROVIDER"
	content := strings.Repeat("safe-prefix/", 300) + rawTail
	path := filepath.Join(workDir, "large.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	memory := openSessionMemoryForTest(t, t.TempDir(), "loop-offload")
	provider := &capturingToolResultProvider{scriptedLoopProvider: &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_large_read", "Read", map[string]string{"file_path": path}),
		textStep("done"),
	}}}
	runner := &fakeBashRunner{name: "result-loop", capabilities: fakeBashCapabilities()}
	events := make(chan Event, 64)
	userContent, _ := json.Marshal("Read the large file once.")
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workDir, RunOptions{
		ToolResultStore:  memory,
		ToolResultReader: memory,
		SandboxRunner:    runner,
		SandboxPolicy:    fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy:   EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, events)
	observer := NewDurableRunObserver("result-run")
	var wire strings.Builder
	for event := range events {
		observer.Observe(event)
		encoded, _ := json.Marshal(event)
		wire.Write(encoded)
	}
	requests, catalogs := provider.snapshot()
	if len(requests) != 2 || len(catalogs) != 2 {
		t.Fatalf("provider requests=%d catalogs=%d", len(requests), len(catalogs))
	}
	if strings.Contains(string(requests[1]), rawTail) || strings.Contains(wire.String(), rawTail) {
		t.Fatal("raw large-result tail crossed provider/event boundary")
	}
	if !strings.Contains(string(requests[1]), "tool-result://sha256:") || !strings.Contains(wire.String(), "tool-result://sha256:") {
		t.Fatal("stored reference missing before provider/event boundary")
	}
	for _, catalog := range catalogs {
		if !toolCatalogContains(catalog, loadToolResultToolName) {
			t.Fatal("reader-bound run did not advertise LoadToolResult")
		}
	}
	persisted := observer.Messages()
	encodedPersisted, _ := json.Marshal(persisted)
	if strings.Contains(string(encodedPersisted), rawTail) || !strings.Contains(string(encodedPersisted), "tool-result://sha256:") {
		t.Fatalf("durable observer persisted unsafe result: %s", encodedPersisted)
	}
	if results := memory.ListResults(); len(results) != 1 {
		t.Fatalf("stored results = %#v", results)
	}
}

func TestLoadToolResultCatalogAndExecutionAreReaderBound(t *testing.T) {
	if toolCatalogContains(staticToolDefsWithResultReader(t.TempDir(), nil), loadToolResultToolName) {
		t.Fatal("LoadToolResult advertised without a reader")
	}
	store := openSessionMemoryForTest(t, t.TempDir(), "load-tool")
	content := "Authorization: Bearer sk-proj-abcdefghijklmnopqrstuvwxyz123456\n" + strings.Repeat("load/", 700)
	if _, replaced := store.StoreResult("Read", content); !replaced {
		t.Fatal("failed to seed load tool")
	}
	withReader := staticToolDefsWithResultReader(t.TempDir(), store)
	if !toolCatalogContains(withReader, loadToolResultToolName) {
		t.Fatal("LoadToolResult missing with a reader")
	}
	builtIn := currentBuiltInToolDefinitions()[loadToolResultToolName]
	if builtIn.Name != loadToolResultToolName || string(builtIn.InputSchema) != string(loadToolResultDefinition().InputSchema) {
		t.Fatalf("immutable built-in definition mismatch: %#v", builtIn)
	}
	result, isError := executeLoadToolResult(json.RawMessage(`{"id":"`+sessionMemoryResultID(content)+`","limit":4096}`), store)
	if isError || strings.Contains(result, "sk-proj-") || !strings.Contains(result, "[REDACTED]") {
		t.Fatalf("LoadToolResult result error=%v content=%q", isError, result)
	}
}

func TestRunLoopDoesNotReOffloadLoadToolResultChunks(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	store := openSessionMemoryForTest(t, t.TempDir(), "load-no-recursion")
	content := strings.Repeat("bounded-load-content/", 500)
	if _, replaced := store.StoreResult("Read", content); !replaced {
		t.Fatal("failed to seed stored result")
	}
	provider := &capturingToolResultProvider{scriptedLoopProvider: &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_load_result", loadToolResultToolName, map[string]any{
			"id": sessionMemoryResultID(content), "offset": 0, "limit": 4096,
		}),
		textStep("done"),
	}}}
	runner := &fakeBashRunner{name: "load-loop", capabilities: fakeBashCapabilities()}
	events := make(chan Event, 64)
	userContent, _ := json.Marshal("Load the stored result once.")
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workDir, RunOptions{
		ToolResultStore:  store,
		ToolResultReader: store,
		SandboxRunner:    runner,
		SandboxPolicy:    fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy:   EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, events)
	for range events {
	}
	requests, _ := provider.snapshot()
	if len(requests) != 2 || !strings.Contains(string(requests[1]), "nextOffset") {
		t.Fatalf("bounded load chunk missing from next request: %s", requests[len(requests)-1])
	}
	if strings.Contains(string(requests[1]), "Tool result stored") {
		t.Fatal("LoadToolResult chunk was recursively offloaded")
	}
	if results := store.ListResults(); len(results) != 1 {
		t.Fatalf("recursive load created another blob: %#v", results)
	}
}

type failingCheckedToolResultStore struct {
	failure error
}

func (store failingCheckedToolResultStore) StoreResult(string, string) (string, bool) {
	return "", false
}

func (store failingCheckedToolResultStore) StoreResultChecked(string, string) (string, ToolResultStoreDisposition, error) {
	return "", ToolResultInline, store.failure
}

func TestRunLoopFailsClosedWhenLargeResultPersistenceFails(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{name: "quota", err: ErrToolResultPersistence},
		{name: "publish", err: errors.New("injected publish failure with private path and payload")},
	} {
		t.Run(failure.name, func(t *testing.T) {
			isolateEvidenceLoopTest(t)
			workDir := t.TempDir()
			rawTail := "RAW_PERSISTENCE_FAILURE_MUST_NOT_ESCAPE_" + failure.name
			content := strings.Repeat("large-result/", 300) + rawTail
			// Keep the fixture outside RAG's filename/content match surface so a
			// provider can only see the sentinel through the tool-result path we
			// are testing, not through independent project recall.
			path := filepath.Join(workDir, "opaque.bin")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			provider := &capturingToolResultProvider{scriptedLoopProvider: &scriptedLoopProvider{steps: []scriptedLoopStep{
				toolUseStep("toolu_failed_store", "Read", map[string]string{"file_path": path}),
				textStep("done"),
			}}}
			events := make(chan Event, 64)
			observer := NewDurableRunObserver("failed-result-run")
			userContent, _ := json.Marshal("Perform the requested operation once.")
			go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workDir, RunOptions{
				ToolResultStore: failingCheckedToolResultStore{failure: failure.err},
				SandboxRunner:   &fakeBashRunner{name: "failed-result-loop", capabilities: fakeBashCapabilities()},
				SandboxPolicy:   fakeBashPolicy(sandbox.EnforcementRequired),
				EvidencePolicy:  EvidencePolicyConfig{Policy: EvidencePolicyOff},
			}, events)
			var eventWire strings.Builder
			for event := range events {
				observer.Observe(event)
				encoded, _ := json.Marshal(event)
				eventWire.Write(encoded)
			}
			requests, _ := provider.snapshot()
			persisted, _ := json.Marshal(observer.Messages())
			combined := eventWire.String() + string(persisted)
			for _, request := range requests {
				combined += string(request)
			}
			if strings.Contains(combined, rawTail) || strings.Contains(combined, failure.err.Error()) {
				t.Fatalf("persistence failure crossed a public boundary: %s", combined)
			}
			if !strings.Contains(combined, toolResultPersistenceFailureMessage) {
				t.Fatalf("safe persistence failure missing: %s", combined)
			}
		})
	}
}

func toolCatalogContains(catalog []types.ToolDef, name string) bool {
	for _, definition := range catalog {
		if definition.Name == name {
			return true
		}
	}
	return false
}
