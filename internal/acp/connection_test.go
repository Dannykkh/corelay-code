package acp

// Golden provenance: stable ACP v1 schema and method registry at commit
// af41b25f57a79c5629b3164e23fb4e8650badeeb.
// https://github.com/agentclientprotocol/agent-client-protocol/blob/af41b25f57a79c5629b3164e23fb4e8650badeeb/schema/v1/schema.json
// https://github.com/agentclientprotocol/agent-client-protocol/blob/af41b25f57a79c5629b3164e23fb4e8650badeeb/schema/v1/meta.json

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type coreTestBackend struct {
	descriptor BackendDescriptor
	newID      string
	prompt     func(context.Context, PromptRequest, Client) (PromptResponse, error)
	cancels    atomic.Int32
}

func newCoreTestBackend() *coreTestBackend {
	return &coreTestBackend{
		descriptor: BackendDescriptor{AgentInfo: Implementation{Name: "corelaycode-test", Version: "1.0.0"}},
		newID:      "session-test",
	}
}

func (b *coreTestBackend) Descriptor() BackendDescriptor { return b.descriptor }
func (b *coreTestBackend) Initialize(context.Context, InitializeRequest, Client) error {
	return nil
}
func (b *coreTestBackend) NewSession(context.Context, NewSessionRequest, Client) (NewSessionResponse, error) {
	return NewSessionResponse{SessionID: b.newID}, nil
}
func (b *coreTestBackend) Prompt(ctx context.Context, request PromptRequest, client Client) (PromptResponse, error) {
	if b.prompt != nil {
		return b.prompt(ctx, request, client)
	}
	return PromptResponse{StopReason: StopEndTurn}, nil
}
func (b *coreTestBackend) CancelSession(context.Context, CancelNotification) error {
	b.cancels.Add(1)
	return nil
}

type fullTestBackend struct{ *coreTestBackend }

func (b *fullTestBackend) Authenticate(context.Context, AuthenticateRequest, Client) (AuthenticateResponse, error) {
	return AuthenticateResponse{}, nil
}
func (b *fullTestBackend) Logout(context.Context, LogoutRequest, Client) (LogoutResponse, error) {
	return LogoutResponse{}, nil
}
func (b *fullTestBackend) LoadSession(context.Context, LoadSessionRequest, Client) (LoadSessionResponse, error) {
	return LoadSessionResponse{}, nil
}
func (b *fullTestBackend) ListSessions(context.Context, ListSessionsRequest, Client) (ListSessionsResponse, error) {
	return ListSessionsResponse{Sessions: []SessionInfo{}}, nil
}
func (b *fullTestBackend) DeleteSession(context.Context, DeleteSessionRequest, Client) (DeleteSessionResponse, error) {
	return DeleteSessionResponse{}, nil
}
func (b *fullTestBackend) ResumeSession(context.Context, ResumeSessionRequest, Client) (ResumeSessionResponse, error) {
	return ResumeSessionResponse{}, nil
}
func (b *fullTestBackend) CloseSession(context.Context, CloseSessionRequest, Client) (CloseSessionResponse, error) {
	return CloseSessionResponse{}, nil
}
func (b *fullTestBackend) SetSessionMode(context.Context, SetSessionModeRequest, Client) (SetSessionModeResponse, error) {
	return SetSessionModeResponse{}, nil
}
func (b *fullTestBackend) SetSessionConfigOption(context.Context, SetSessionConfigOptionRequest, Client) (SetSessionConfigOptionResponse, error) {
	return SetSessionConfigOptionResponse{ConfigOptions: []SessionConfigOption{}}, nil
}

type testHarness struct {
	t        *testing.T
	conn     *Connection
	input    *io.PipeWriter
	output   *io.PipeReader
	cancel   context.CancelFunc
	done     chan error
	frames   chan []byte
	readDone chan struct{}
	writeMu  sync.Mutex
	nextID   int64
}

func newTestHarness(t *testing.T, backend Backend, options Options) *testHarness {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &testHarness{
		t: t, conn: NewConnection(inputReader, outputWriter, backend, options),
		input: inputWriter, output: outputReader, cancel: cancel,
		done: make(chan error, 1), frames: make(chan []byte, 512), readDone: make(chan struct{}), nextID: 1,
	}
	go func() { h.done <- h.conn.Serve(ctx) }()
	go func() {
		defer close(h.readDone)
		scanner := bufio.NewScanner(outputReader)
		scanner.Buffer(make([]byte, 4096), DefaultMaxFrameBytes+1)
		for scanner.Scan() {
			h.frames <- append([]byte(nil), scanner.Bytes()...)
		}
		close(h.frames)
	}()
	t.Cleanup(h.close)
	return h
}

func (h *testHarness) close() {
	h.cancel()
	_ = h.input.Close()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.t.Errorf("ACP Serve did not stop")
	}
	_ = h.output.Close()
	select {
	case <-h.readDone:
	case <-time.After(2 * time.Second):
		h.t.Errorf("ACP output reader did not stop")
	}
}

func (h *testHarness) send(value any) {
	h.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		h.t.Fatal(err)
	}
	h.sendRaw(string(data))
}

func (h *testHarness) sendRaw(value string) {
	h.t.Helper()
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if _, err := io.WriteString(h.input, value+"\n"); err != nil {
		h.t.Fatalf("write client frame: %v", err)
	}
}

func (h *testHarness) request(method string, params any) int64 {
	h.t.Helper()
	id := h.nextID
	h.nextID++
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id
}

func (h *testHarness) notify(method string, params any) {
	h.t.Helper()
	h.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (h *testHarness) next() map[string]json.RawMessage {
	h.t.Helper()
	select {
	case raw, ok := <-h.frames:
		if !ok {
			h.t.Fatal("ACP output closed")
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(raw, &frame); err != nil {
			h.t.Fatalf("decode ACP frame %q: %v", raw, err)
		}
		return frame
	case <-time.After(3 * time.Second):
		h.t.Fatal("timed out waiting for ACP frame")
		return nil
	}
}

func (h *testHarness) initialize(capabilities ClientCapabilities) InitializeResponse {
	h.t.Helper()
	id := h.request("initialize", InitializeRequest{ProtocolVersion: 1, ClientCapabilities: capabilities})
	frame := h.next()
	assertFrameID(h.t, frame, id)
	var response InitializeResponse
	decodeFrameResult(h.t, frame, &response)
	return response
}

func (h *testHarness) newSession(id string) {
	h.t.Helper()
	requestID := h.request("session/new", NewSessionRequest{CWD: testAbsolutePath(), MCPServers: []MCPServer{}})
	frame := h.next()
	assertFrameID(h.t, frame, requestID)
	var response NewSessionResponse
	decodeFrameResult(h.t, frame, &response)
	if response.SessionID != id {
		h.t.Fatalf("session id = %q, want %q", response.SessionID, id)
	}
}

func testAbsolutePath() string {
	if runtime.GOOS == "windows" {
		return `C:\workspace`
	}
	return "/workspace"
}

func assertFrameID(t *testing.T, frame map[string]json.RawMessage, want int64) {
	t.Helper()
	var got int64
	if err := json.Unmarshal(frame["id"], &got); err != nil || got != want {
		t.Fatalf("frame id=%s want=%d", frame["id"], want)
	}
}

func decodeFrameResult(t *testing.T, frame map[string]json.RawMessage, target any) {
	t.Helper()
	if rawErr := frame["error"]; len(rawErr) != 0 {
		t.Fatalf("unexpected RPC error: %s", rawErr)
	}
	if err := json.Unmarshal(frame["result"], target); err != nil {
		t.Fatalf("decode result: %v frame=%v", err, frame)
	}
}

func decodeFrameError(t *testing.T, frame map[string]json.RawMessage) RPCError {
	t.Helper()
	var rpcErr RPCError
	if err := json.Unmarshal(frame["error"], &rpcErr); err != nil {
		t.Fatalf("decode error: %v frame=%v", err, frame)
	}
	return rpcErr
}

func TestInitializeOrderVersionAndTruthfulCapabilities(t *testing.T) {
	backend := newCoreTestBackend()
	h := newTestHarness(t, backend, Options{})

	beforeID := h.request("session/new", NewSessionRequest{CWD: testAbsolutePath(), MCPServers: []MCPServer{}})
	before := h.next()
	assertFrameID(t, before, beforeID)
	if rpcErr := decodeFrameError(t, before); rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("pre-initialize code=%d", rpcErr.Code)
	}

	initID := h.request("initialize", InitializeRequest{ProtocolVersion: 42})
	frame := h.next()
	assertFrameID(t, frame, initID)
	var response InitializeResponse
	decodeFrameResult(t, frame, &response)
	if response.ProtocolVersion != 1 {
		t.Fatalf("protocol version=%d", response.ProtocolVersion)
	}
	capabilities := response.AgentCapabilities
	if capabilities.LoadSession || capabilities.SessionCapabilities.List != nil || capabilities.SessionCapabilities.Delete != nil || capabilities.SessionCapabilities.Resume != nil || capabilities.SessionCapabilities.Close != nil || capabilities.Auth.Logout != nil {
		t.Fatalf("unsupported capabilities were advertised: %+v", capabilities)
	}

	duplicateID := h.request("initialize", InitializeRequest{ProtocolVersion: 1})
	duplicate := h.next()
	assertFrameID(t, duplicate, duplicateID)
	if rpcErr := decodeFrameError(t, duplicate); rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("duplicate initialize code=%d", rpcErr.Code)
	}
	listID := h.request("session/list", ListSessionsRequest{})
	list := h.next()
	assertFrameID(t, list, listID)
	if rpcErr := decodeFrameError(t, list); rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("unsupported method code=%d", rpcErr.Code)
	}
}

func TestStableAgentMethodRoutingAndDerivedCapabilities(t *testing.T) {
	core := newCoreTestBackend()
	core.descriptor.AdditionalDirectories = true
	core.descriptor.AuthMethods = []AuthMethod{{ID: "agent-auth", Name: "Agent auth"}}
	backend := &fullTestBackend{coreTestBackend: core}
	h := newTestHarness(t, backend, Options{})
	initialized := h.initialize(ClientCapabilities{})
	capabilities := initialized.AgentCapabilities
	if !capabilities.LoadSession || capabilities.SessionCapabilities.List == nil || capabilities.SessionCapabilities.Delete == nil || capabilities.SessionCapabilities.AdditionalDirectories == nil || capabilities.SessionCapabilities.Resume == nil || capabilities.SessionCapabilities.Close == nil || capabilities.Auth.Logout == nil {
		t.Fatalf("implemented capabilities were not advertised: %+v", capabilities)
	}
	if len(initialized.AuthMethods) != 1 || initialized.AuthMethods[0].ID != "agent-auth" {
		t.Fatalf("auth methods=%+v", initialized.AuthMethods)
	}

	call := func(method string, params any) map[string]json.RawMessage {
		t.Helper()
		id := h.request(method, params)
		frame := h.next()
		assertFrameID(t, frame, id)
		if rawErr := frame["error"]; len(rawErr) != 0 {
			t.Fatalf("%s failed: %s", method, rawErr)
		}
		return frame
	}
	call("authenticate", AuthenticateRequest{MethodID: "agent-auth"})
	core.newID = "session-route"
	call("session/new", NewSessionRequest{CWD: testAbsolutePath(), MCPServers: []MCPServer{}})
	call("session/set_mode", SetSessionModeRequest{SessionID: "session-route", ModeID: "code"})
	call("session/set_config_option", map[string]any{"sessionId": "session-route", "configId": "safe", "type": "boolean", "value": true})
	call("session/list", ListSessionsRequest{})
	call("session/load", LoadSessionRequest{SessionID: "session-load", CWD: testAbsolutePath(), MCPServers: []MCPServer{}})
	call("session/resume", ResumeSessionRequest{SessionID: "session-resume", CWD: testAbsolutePath()})
	call("session/close", CloseSessionRequest{SessionID: "session-route"})
	call("session/delete", DeleteSessionRequest{SessionID: "session-load"})
	call("logout", LogoutRequest{})
}

func TestNewPromptStreamsStableUpdateThenEndTurn(t *testing.T) {
	backend := newCoreTestBackend()
	backend.prompt = func(ctx context.Context, request PromptRequest, client Client) (PromptResponse, error) {
		text := ContentBlock{Type: "text", Text: "answer"}
		if err := client.SessionUpdate(ctx, SessionNotification{SessionID: request.SessionID, Update: SessionUpdate{SessionUpdate: "agent_message_chunk", Content: &text, MessageID: "message-1"}}); err != nil {
			return PromptResponse{}, err
		}
		return PromptResponse{StopReason: StopEndTurn}, nil
	}
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{})
	h.newSession("session-test")

	promptID := h.request("session/prompt", PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "hello"}}})
	updateFrame := h.next()
	var method string
	_ = json.Unmarshal(updateFrame["method"], &method)
	if method != "session/update" {
		t.Fatalf("method=%q frame=%v", method, updateFrame)
	}
	var notification SessionNotification
	if err := json.Unmarshal(updateFrame["params"], &notification); err != nil {
		t.Fatal(err)
	}
	if notification.Update.SessionUpdate != "agent_message_chunk" || notification.Update.Content == nil || notification.Update.Content.Text != "answer" {
		t.Fatalf("unexpected update: %+v", notification)
	}
	responseFrame := h.next()
	assertFrameID(t, responseFrame, promptID)
	var response PromptResponse
	decodeFrameResult(t, responseFrame, &response)
	if response.StopReason != StopEndTurn {
		t.Fatalf("stop reason=%q", response.StopReason)
	}
}

func TestPermissionAllowDenyAndCancel(t *testing.T) {
	tests := []struct {
		name       string
		optionKind PermissionOptionKind
		cancel     bool
		wantStop   StopReason
	}{
		{name: "allow once", optionKind: PermissionAllowOnce, wantStop: StopEndTurn},
		{name: "deny", optionKind: PermissionRejectOnce, wantStop: StopEndTurn},
		{name: "cancel", optionKind: PermissionAllowOnce, cancel: true, wantStop: StopCancelled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newCoreTestBackend()
			outcome := make(chan PermissionOutcome, 1)
			backend.prompt = func(ctx context.Context, request PromptRequest, client Client) (PromptResponse, error) {
				response, err := client.RequestPermission(ctx, RequestPermissionRequest{
					SessionID: request.SessionID,
					ToolCall:  PermissionToolCall{ToolCallID: "tool-1", Title: "run Authorization: Bearer top-secret", Kind: ToolExecute, Status: ToolPending},
					Options:   []PermissionOption{{OptionID: "choice", Name: "choose token=top-secret", Kind: test.optionKind, Meta: Meta(`{"token":"top-secret"}`)}},
					Meta:      Meta(`{"rawInput":"top-secret"}`),
				})
				if err != nil {
					return PromptResponse{}, err
				}
				outcome <- response.Outcome
				return PromptResponse{StopReason: StopEndTurn}, nil
			}
			h := newTestHarness(t, backend, Options{})
			h.initialize(ClientCapabilities{})
			h.newSession("session-test")
			promptID := h.request("session/prompt", PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "go"}}})
			permissionFrame := h.next()
			permissionBytes, _ := json.Marshal(permissionFrame)
			if bytes.Contains(permissionBytes, []byte("top-secret")) || bytes.Contains(permissionBytes, []byte("rawInput")) || bytes.Contains(permissionBytes, []byte("rawOutput")) {
				t.Fatalf("permission frame leaked raw secret/input: %s", permissionBytes)
			}
			var method string
			_ = json.Unmarshal(permissionFrame["method"], &method)
			if method != "session/request_permission" {
				t.Fatalf("method=%q", method)
			}
			if test.cancel {
				h.notify("session/cancel", CancelNotification{SessionID: "session-test"})
			} else {
				h.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(permissionFrame["id"]), "result": RequestPermissionResponse{Outcome: PermissionOutcome{Outcome: "selected", OptionID: "choice"}}})
			}
			var promptResponse PromptResponse
			for attempts := 0; attempts < 4; attempts++ {
				frame := h.next()
				if _, isMethod := frame["method"]; isMethod {
					continue
				}
				assertFrameID(t, frame, promptID)
				decodeFrameResult(t, frame, &promptResponse)
				break
			}
			if promptResponse.StopReason != test.wantStop {
				t.Fatalf("stop=%q want=%q", promptResponse.StopReason, test.wantStop)
			}
			if !test.cancel {
				select {
				case got := <-outcome:
					if got.Outcome != "selected" || got.OptionID != "choice" {
						t.Fatalf("outcome=%+v", got)
					}
				case <-time.After(time.Second):
					t.Fatal("backend did not receive permission outcome")
				}
			}
		})
	}
}

func TestSessionPromptConcurrencyIsolationAndDuplicateIDs(t *testing.T) {
	backend := newCoreTestBackend()
	backend.prompt = func(ctx context.Context, request PromptRequest, _ Client) (PromptResponse, error) {
		if request.SessionID == "session-a" {
			<-ctx.Done()
			return PromptResponse{}, ctx.Err()
		}
		return PromptResponse{StopReason: StopEndTurn}, nil
	}
	backend.newID = "session-a"
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{})
	h.newSession("session-a")
	backend.newID = "session-b"
	h.newSession("session-b")

	h.send(map[string]any{"jsonrpc": "2.0", "id": 40, "method": "session/prompt", "params": PromptRequest{SessionID: "session-a", Prompt: []ContentBlock{{Type: "text", Text: "wait"}}}})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 40, "method": "session/prompt", "params": PromptRequest{SessionID: "session-a", Prompt: []ContentBlock{{Type: "text", Text: "duplicate"}}}})
	duplicate := h.next()
	assertFrameID(t, duplicate, 40)
	if rpcErr := decodeFrameError(t, duplicate); rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("duplicate id code=%d", rpcErr.Code)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 41, "method": "session/prompt", "params": PromptRequest{SessionID: "session-b", Prompt: []ContentBlock{{Type: "text", Text: "fast"}}}})
	fast := h.next()
	assertFrameID(t, fast, 41)
	var fastResponse PromptResponse
	decodeFrameResult(t, fast, &fastResponse)
	if fastResponse.StopReason != StopEndTurn {
		t.Fatalf("session-b stop=%q", fastResponse.StopReason)
	}
	h.notify("session/cancel", CancelNotification{SessionID: "session-a"})
	cancelled := h.next()
	assertFrameID(t, cancelled, 40)
	var cancelledResponse PromptResponse
	decodeFrameResult(t, cancelled, &cancelledResponse)
	if cancelledResponse.StopReason != StopCancelled {
		t.Fatalf("session-a stop=%q", cancelledResponse.StopReason)
	}
}

func TestUnknownResponseMalformedOversizeAndDepthFailClosed(t *testing.T) {
	backend := newCoreTestBackend()
	h := newTestHarness(t, backend, Options{MaxFrameBytes: 1024})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 999, "result": map[string]any{}})

	h.sendRaw(`{"jsonrpc":"2.0","id":1,"method":`)
	if rpcErr := decodeFrameError(t, h.next()); rpcErr.Code != CodeParseError {
		t.Fatalf("parse code=%d", rpcErr.Code)
	}
	h.sendRaw(`{"jsonrpc":"2.0","jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if rpcErr := decodeFrameError(t, h.next()); rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("duplicate code=%d", rpcErr.Code)
	}
	h.sendRaw(`{"jsonrpc":"2.0","JSONRPC":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if rpcErr := decodeFrameError(t, h.next()); rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("casefold code=%d", rpcErr.Code)
	}
	deep := strings.Repeat(`{"x":`, 70) + `0` + strings.Repeat(`}`, 70)
	h.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"_meta":` + deep + `}}`)
	if rpcErr := decodeFrameError(t, h.next()); rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("depth code=%d", rpcErr.Code)
	}
	h.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"padding":"` + strings.Repeat("x", 1200) + `"}}`)
	if rpcErr := decodeFrameError(t, h.next()); rpcErr.Code != CodeParseError {
		t.Fatalf("oversize code=%d", rpcErr.Code)
	}

	response := h.initialize(ClientCapabilities{})
	if response.ProtocolVersion != 1 {
		t.Fatal("connection did not recover after invalid frames")
	}
}

func TestConcurrentRoutingSoakAndUnknownLateResponses(t *testing.T) {
	backend := newCoreTestBackend()
	h := newTestHarness(t, backend, Options{MaxInflight: 256, WriteQueue: 256})
	h.initialize(ClientCapabilities{})
	const requests = 100
	for id := int64(1000); id < 1000+requests; id++ {
		h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "unknown/stable", "params": map[string]any{}})
	}
	seen := make(map[int64]struct{}, requests)
	for len(seen) < requests {
		frame := h.next()
		var id int64
		if err := json.Unmarshal(frame["id"], &id); err != nil {
			t.Fatal(err)
		}
		if rpcErr := decodeFrameError(t, frame); rpcErr.Code != CodeMethodNotFound {
			t.Fatalf("id=%d code=%d", id, rpcErr.Code)
		}
		seen[id] = struct{}{}
	}
	// A response to an already unknown request id is ignored and cannot poison
	// routing for a subsequent valid request.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
	id := h.request("session/new", NewSessionRequest{CWD: testAbsolutePath(), MCPServers: []MCPServer{}})
	frame := h.next()
	assertFrameID(t, frame, id)
	var session NewSessionResponse
	decodeFrameResult(t, frame, &session)
}

func TestLateClientResponseAfterSessionCancelIsIgnored(t *testing.T) {
	backend := newCoreTestBackend()
	var prompts atomic.Int32
	backend.prompt = func(ctx context.Context, request PromptRequest, client Client) (PromptResponse, error) {
		if prompts.Add(1) > 1 {
			return PromptResponse{StopReason: StopEndTurn}, nil
		}
		_, err := client.RequestPermission(ctx, RequestPermissionRequest{
			SessionID: request.SessionID,
			ToolCall:  PermissionToolCall{ToolCallID: "late-call", Title: "Run", Kind: ToolExecute, Status: ToolPending},
			Options:   []PermissionOption{{OptionID: "allow", Name: "Allow", Kind: PermissionAllowOnce}},
		})
		return PromptResponse{}, err
	}
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{})
	h.newSession("session-test")
	firstPromptID := h.request(MethodSessionPrompt, PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "first"}}})
	permission := h.next()
	permissionID := append(json.RawMessage(nil), permission["id"]...)
	h.notify(MethodSessionCancel, CancelNotification{SessionID: "session-test"})
	for {
		frame := h.next()
		if _, notification := frame["method"]; notification {
			continue
		}
		assertFrameID(t, frame, firstPromptID)
		var response PromptResponse
		decodeFrameResult(t, frame, &response)
		if response.StopReason != StopCancelled {
			t.Fatalf("stop=%q", response.StopReason)
		}
		break
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": permissionID, "result": RequestPermissionResponse{Outcome: PermissionOutcome{Outcome: "selected", OptionID: "allow"}}})
	secondPromptID := h.request(MethodSessionPrompt, PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "second"}}})
	second := h.next()
	assertFrameID(t, second, secondPromptID)
	var secondResponse PromptResponse
	decodeFrameResult(t, second, &secondResponse)
	if secondResponse.StopReason != StopEndTurn {
		t.Fatalf("second stop=%q", secondResponse.StopReason)
	}
}

func TestInflightBoundFailsClosed(t *testing.T) {
	backend := newCoreTestBackend()
	started := make(chan struct{})
	backend.prompt = func(ctx context.Context, _ PromptRequest, _ Client) (PromptResponse, error) {
		close(started)
		<-ctx.Done()
		return PromptResponse{}, ctx.Err()
	}
	h := newTestHarness(t, backend, Options{MaxInflight: 1})
	h.initialize(ClientCapabilities{})
	h.newSession("session-test")
	h.send(map[string]any{"jsonrpc": "2.0", "id": 70, "method": MethodSessionPrompt, "params": PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "hold"}}}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 71, "method": "unknown", "params": map[string]any{}})
	bounded := h.next()
	assertFrameID(t, bounded, 71)
	if rpcErr := decodeFrameError(t, bounded); rpcErr.Code != CodeInternalError {
		t.Fatalf("inflight bound code=%d", rpcErr.Code)
	}
	h.notify(MethodSessionCancel, CancelNotification{SessionID: "session-test"})
	frame := h.next()
	assertFrameID(t, frame, 70)
}

type blockingWriteCloser struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return nil
}

func TestWriterBackpressureCancellationJoinsTransportGoroutines(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	output := newBlockingWriteCloser()
	backend := newCoreTestBackend()
	connection := NewConnection(inputReader, output, backend, Options{WriteQueue: 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- connection.Serve(ctx) }()
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": InitializeRequest{ProtocolVersion: 1}})
	if _, err := inputWriter.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach backpressure fixture")
	}
	cancel()
	_ = inputWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve leaked a blocked writer/request goroutine")
	}
}

func TestRequestCancellationUsesJSONRPCErrorWhileSessionCancelUsesStopReason(t *testing.T) {
	backend := newCoreTestBackend()
	started := make(chan struct{})
	backend.prompt = func(ctx context.Context, _ PromptRequest, _ Client) (PromptResponse, error) {
		close(started)
		<-ctx.Done()
		return PromptResponse{}, ctx.Err()
	}
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{})
	h.newSession("session-test")
	h.send(map[string]any{"jsonrpc": "2.0", "id": "prompt-string-id", "method": MethodSessionPrompt, "params": PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "wait"}}}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	h.notify(MethodCancelRequest, map[string]any{"requestId": "prompt-string-id"})
	frame := h.next()
	var id string
	if err := json.Unmarshal(frame["id"], &id); err != nil || id != "prompt-string-id" {
		t.Fatalf("id=%s", frame["id"])
	}
	if rpcErr := decodeFrameError(t, frame); rpcErr.Code != CodeRequestCancelled {
		t.Fatalf("cancel code=%d", rpcErr.Code)
	}
}

func TestEOFAndExplicitShutdownCleanup(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(*Connection, *io.PipeWriter)
	}{
		{name: "EOF", stop: func(_ *Connection, input *io.PipeWriter) { _ = input.Close() }},
		{name: "Shutdown", stop: func(connection *Connection, _ *io.PipeWriter) { connection.Shutdown() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputReader, inputWriter := io.Pipe()
			outputReader, outputWriter := io.Pipe()
			connection := NewConnection(inputReader, outputWriter, newCoreTestBackend(), Options{})
			done := make(chan error, 1)
			go func() { done <- connection.Serve(context.Background()) }()
			test.stop(connection, inputWriter)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Serve error: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("connection cleanup did not complete")
			}
			_ = inputWriter.Close()
			_ = outputReader.Close()
		})
	}
}

func TestStableV1GoldenWireShapes(t *testing.T) {
	wantAgentMethods := []string{
		"initialize", "authenticate", "session/new", "session/load", "session/set_mode",
		"session/set_config_option", "session/prompt", "session/cancel", "session/list",
		"session/delete", "session/resume", "session/close", "logout",
	}
	wantClientMethods := []string{
		"session/request_permission", "session/update", "fs/write_text_file", "fs/read_text_file",
		"terminal/create", "terminal/output", "terminal/release", "terminal/wait_for_exit",
		"terminal/kill", "elicitation/create", "elicitation/complete",
	}
	if got := StableAgentMethodNames(); !slices.Equal(got, wantAgentMethods) {
		t.Fatalf("stable agent methods=%v", got)
	}
	if got := StableClientMethodNames(); !slices.Equal(got, wantClientMethods) {
		t.Fatalf("stable client methods=%v", got)
	}
	title := "Run tests"
	text := ContentBlock{Type: "text", Text: "ok"}
	notification := SessionNotification{SessionID: "session-1", Update: SessionUpdate{
		SessionUpdate: "tool_call", ToolCallID: "call-1", Title: &title, Kind: ToolExecute, Status: ToolPending,
		ToolContent: []ToolCallContent{{Type: "content", Content: &text}},
	}}
	data, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sessionId":"session-1","update":{"content":[{"type":"content","content":{"type":"text","text":"ok"}}],"kind":"execute","sessionUpdate":"tool_call","status":"pending","title":"Run tests","toolCallId":"call-1"}}`
	assertJSONEqual(t, data, []byte(want))
	response, err := json.Marshal(PromptResponse{StopReason: StopCancelled})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, response, []byte(`{"stopReason":"cancelled"}`))
	permission, err := json.Marshal(PermissionOption{OptionID: "once", Name: "Allow once", Kind: PermissionAllowOnce})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, permission, []byte(`{"optionId":"once","name":"Allow once","kind":"allow_once"}`))
	plan, err := json.Marshal(SessionUpdate{SessionUpdate: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, plan, []byte(`{"sessionUpdate":"plan","entries":[]}`))
	initialize, err := json.Marshal(InitializeResponse{ProtocolVersion: 1, AuthMethods: []AuthMethod{}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(initialize, []byte(`"authMethods":[]`)) {
		t.Fatalf("initialize authMethods must be an array: %s", initialize)
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	gotCanonical, _ := json.Marshal(gotValue)
	wantCanonical, _ := json.Marshal(wantValue)
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", gotCanonical, wantCanonical)
	}
}

func FuzzDecodeEnvelope(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":"x","result":{}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32800,"message":"cancelled"}}`,
		`not json`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > DefaultMaxFrameBytes {
			t.Skip()
		}
		_, _ = decodeEnvelope([]byte(value))
	})
}

func TestRequestIDStringIntegerAndNull(t *testing.T) {
	for _, test := range []struct {
		raw  string
		key  string
		null bool
	}{
		{raw: `"abc"`, key: "s:abc"},
		{raw: `42`, key: "i:42"},
		{raw: `null`, key: "n:", null: true},
	} {
		id, err := parseRequestID(json.RawMessage(test.raw))
		if err != nil {
			t.Fatalf("parse %s: %v", test.raw, err)
		}
		if id.key() != test.key || id.IsNull() != test.null {
			t.Fatalf("id=%+v", id)
		}
	}
	if _, err := parseRequestID(json.RawMessage(`1.5`)); err == nil {
		t.Fatal("fractional request id accepted")
	}
}

func TestMetaIsBoundedOpaqueAndCopied(t *testing.T) {
	original := []byte(`{"Vendor":{"CaseSensitive":1}}`)
	var meta Meta
	if err := json.Unmarshal(original, &meta); err != nil {
		t.Fatal(err)
	}
	original[2] = 'X'
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, encoded, []byte(`{"Vendor":{"CaseSensitive":1}}`))
	update, err := json.Marshal(SessionUpdate{SessionUpdate: "session_info_update", Meta: Meta(`{"largeInteger":9007199254740993}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(update, []byte(`9007199254740993`)) {
		t.Fatalf("session update changed opaque _meta: %s", update)
	}
	oversized := []byte(`{"value":"` + strings.Repeat("x", MaxMetaBytes) + `"}`)
	if err := json.Unmarshal(oversized, &meta); err == nil {
		t.Fatal("oversized _meta was accepted")
	}
}

func TestBackendErrorNeverLeaksRawMessage(t *testing.T) {
	backend := newCoreTestBackend()
	backend.prompt = func(context.Context, PromptRequest, Client) (PromptResponse, error) {
		return PromptResponse{}, fmt.Errorf("Authorization: Bearer should-never-leak")
	}
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{})
	h.newSession("session-test")
	id := h.request("session/prompt", PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "hello"}}})
	frame := h.next()
	assertFrameID(t, frame, id)
	raw, _ := json.Marshal(frame)
	if bytes.Contains(raw, []byte("should-never-leak")) {
		t.Fatalf("backend error leaked: %s", raw)
	}
	if rpcErr := decodeFrameError(t, frame); rpcErr.Code != CodeInternalError {
		t.Fatalf("code=%d", rpcErr.Code)
	}
}

func TestNullRequestIDReceivesNullResponse(t *testing.T) {
	backend := newCoreTestBackend()
	h := newTestHarness(t, backend, Options{})
	h.sendRaw(`{"jsonrpc":"2.0","id":null,"method":"initialize","params":{"protocolVersion":1}}`)
	frame := h.next()
	if string(frame["id"]) != "null" {
		t.Fatalf("id=%s", frame["id"])
	}
	var response InitializeResponse
	decodeFrameResult(t, frame, &response)
}

func TestClientCapabilityGatingBeforeWrite(t *testing.T) {
	backend := newCoreTestBackend()
	backend.prompt = func(ctx context.Context, request PromptRequest, client Client) (PromptResponse, error) {
		_, err := client.ReadTextFile(ctx, ReadTextFileRequest{SessionID: request.SessionID, Path: testAbsolutePath()})
		if err == nil {
			return PromptResponse{}, errors.New("unsupported fs call succeeded")
		}
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
			return PromptResponse{}, errors.New("unexpected capability error")
		}
		return PromptResponse{StopReason: StopEndTurn}, nil
	}
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{})
	h.newSession("session-test")
	id := h.request("session/prompt", PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "hello"}}})
	frame := h.next()
	assertFrameID(t, frame, id)
	var response PromptResponse
	decodeFrameResult(t, frame, &response)
}

func TestStableAgentToClientMethodSeam(t *testing.T) {
	backend := newCoreTestBackend()
	backend.prompt = func(ctx context.Context, request PromptRequest, client Client) (PromptResponse, error) {
		if _, err := client.ReadTextFile(ctx, ReadTextFileRequest{SessionID: request.SessionID, Path: testAbsolutePath()}); err != nil {
			return PromptResponse{}, err
		}
		if _, err := client.WriteTextFile(ctx, WriteTextFileRequest{SessionID: request.SessionID, Path: testAbsolutePath(), Content: "safe"}); err != nil {
			return PromptResponse{}, err
		}
		terminal, err := client.CreateTerminal(ctx, CreateTerminalRequest{SessionID: request.SessionID, Command: "go", Args: []string{"version"}})
		if err != nil {
			return PromptResponse{}, err
		}
		terminalRequest := TerminalRequest{SessionID: request.SessionID, TerminalID: terminal.TerminalID}
		if _, err := client.TerminalOutput(ctx, terminalRequest); err != nil {
			return PromptResponse{}, err
		}
		if _, err := client.WaitForTerminalExit(ctx, terminalRequest); err != nil {
			return PromptResponse{}, err
		}
		if _, err := client.KillTerminal(ctx, terminalRequest); err != nil {
			return PromptResponse{}, err
		}
		if _, err := client.ReleaseTerminal(ctx, terminalRequest); err != nil {
			return PromptResponse{}, err
		}
		if _, err := client.CreateElicitation(ctx, CreateElicitationRequest{
			Mode: "form", Message: "Choose", SessionID: request.SessionID,
			RequestedSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}); err != nil {
			return PromptResponse{}, err
		}
		if err := client.CompleteElicitation(ctx, CompleteElicitationNotification{ElicitationID: "elicitation-1"}); err != nil {
			return PromptResponse{}, err
		}
		return PromptResponse{StopReason: StopEndTurn}, nil
	}
	h := newTestHarness(t, backend, Options{})
	h.initialize(ClientCapabilities{
		FS: FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: true,
		Elicitation: &ElicitationCapabilities{Form: &EmptyCapability{}, URL: &EmptyCapability{}},
	})
	h.newSession("session-test")
	promptID := h.request("session/prompt", PromptRequest{SessionID: "session-test", Prompt: []ContentBlock{{Type: "text", Text: "run"}}})
	wantMethods := []string{
		"fs/read_text_file", "fs/write_text_file", "terminal/create", "terminal/output",
		"terminal/wait_for_exit", "terminal/kill", "terminal/release", "elicitation/create", "elicitation/complete",
	}
	for _, wantMethod := range wantMethods {
		frame := h.next()
		var method string
		if err := json.Unmarshal(frame["method"], &method); err != nil || method != wantMethod {
			t.Fatalf("method=%q want=%q frame=%v", method, wantMethod, frame)
		}
		if wantMethod == "elicitation/complete" {
			if _, hasID := frame["id"]; hasID {
				t.Fatalf("elicitation/complete must be a notification: %v", frame)
			}
			continue
		}
		var result any = map[string]any{}
		switch wantMethod {
		case "fs/read_text_file":
			result = ReadTextFileResponse{Content: "file"}
		case "terminal/create":
			result = CreateTerminalResponse{TerminalID: "terminal-1"}
		case "terminal/output":
			result = TerminalOutputResponse{Output: "ok", Truncated: false}
		case "terminal/wait_for_exit":
			zero := uint32(0)
			result = WaitForTerminalExitResponse{ExitCode: &zero}
		case "elicitation/create":
			result = CreateElicitationResponse{Action: "decline"}
		}
		h.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(frame["id"]), "result": result})
	}
	responseFrame := h.next()
	assertFrameID(t, responseFrame, promptID)
	var response PromptResponse
	decodeFrameResult(t, responseFrame, &response)
	if response.StopReason != StopEndTurn {
		t.Fatalf("stop=%q", response.StopReason)
	}
}
