package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
)

const (
	conformanceCredential = "acp-credential-must-not-leak"
	conformanceMCPSpec    = "acp-mcp-spec-must-not-leak"
)

type advertisedSurfaceFixture struct {
	Upstream struct {
		Repository     string `json:"repository"`
		SnapshotCommit string `json:"snapshotCommit"`
		SchemaV1SHA256 string `json:"schemaV1Sha256"`
		MetaV1SHA256   string `json:"metaV1Sha256"`
		ReleaseCommit  string `json:"releaseCommit"`
	} `json:"upstream"`
	ProtocolVersion int `json:"protocolVersion"`
	Advertised      struct {
		LoadSession           bool `json:"loadSession"`
		SessionList           bool `json:"sessionList"`
		SessionDelete         bool `json:"sessionDelete"`
		SessionResume         bool `json:"sessionResume"`
		SessionClose          bool `json:"sessionClose"`
		AdditionalDirectories bool `json:"additionalDirectories"`
		PromptImage           bool `json:"promptImage"`
		PromptAudio           bool `json:"promptAudio"`
		PromptEmbeddedContext bool `json:"promptEmbeddedContext"`
		MCPHTTP               bool `json:"mcpHttp"`
		MCPSSE                bool `json:"mcpSse"`
		AuthLogout            bool `json:"authLogout"`
	} `json:"advertised"`
	UnadvertisedMethods []string `json:"unadvertisedMethods"`
}

type conformanceFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acp.RPCError   `json:"error,omitempty"`
}

type conformanceLine struct {
	data []byte
	err  error
}

type acpCommandProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   <-chan conformanceLine
	done    <-chan error
	stderr  *lockedBuffer

	wire bytes.Buffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestAdvertisedSurfaceConformanceThroughCommand(t *testing.T) {
	fixture := loadAdvertisedSurfaceFixture(t)
	assertPinnedFixture(t, fixture)

	upstream := newACPConformanceUpstream(t)
	defer upstream.Close()
	configDir := t.TempDir()
	workspace := t.TempDir()
	writeACPConformanceConfig(t, configDir, upstream.URL)
	binary := buildACPCommand(t)
	process := startACPCommand(t, binary, configDir)
	defer process.stop(t)

	initialize := process.request(t, 1, acp.MethodInitialize, map[string]any{
		"protocolVersion": fixture.ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			"session":  map[string]any{"configOptions": map[string]any{}},
		},
		"clientInfo": map[string]string{"name": "repo-conformance", "version": "1"},
	}, nil)
	var initialized acp.InitializeResponse
	decodeConformanceResult(t, initialize, &initialized)
	assertAdvertisedSurface(t, fixture, initialized)

	for index, method := range append(append([]string{}, fixture.UnadvertisedMethods...), "fixture/unknown") {
		response := process.request(t, int64(10+index), method, map[string]any{}, nil)
		if response.Error == nil || response.Error.Code != acp.CodeMethodNotFound {
			t.Fatalf("unadvertised method %q response = %#v", method, response)
		}
	}

	process.sendRaw(t, []byte("{malformed\n"))
	assertParseError(t, process.read(t))
	process.sendRaw(t, append(bytes.Repeat([]byte("x"), 2300), '\n'))
	assertParseError(t, process.read(t))

	createdFrame := process.request(t, 20, acp.MethodSessionNew, map[string]any{
		"cwd": workspace, "mcpServers": []any{},
	}, nil)
	var created acp.NewSessionResponse
	decodeConformanceResult(t, createdFrame, &created)
	if created.SessionID == "" || !hasConfigOption(created.ConfigOptions, "model") || !hasConfigOption(created.ConfigOptions, "reasoning") {
		t.Fatalf("session/new result = %#v", created)
	}

	setConfig := process.request(t, 21, acp.MethodSessionSetConfigOption, map[string]any{
		"sessionId": created.SessionID, "configId": "reasoning", "value": "progress",
	}, nil)
	var configured acp.SetSessionConfigOptionResponse
	decodeConformanceResult(t, setConfig, &configured)
	if !configOptionHasValue(configured.ConfigOptions, "reasoning", "progress") {
		t.Fatalf("session/set_config_option result = %#v", configured)
	}

	normalFrames := process.prompt(t, 22, created.SessionID, "stream-fixture", nil)
	assertPromptResult(t, normalFrames[len(normalFrames)-1], acp.StopEndTurn)
	assertStreamingBeforeTerminal(t, normalFrames, "fixture stream complete")

	listedFrame := process.request(t, 23, acp.MethodSessionList, map[string]any{"cwd": workspace}, nil)
	var listed acp.ListSessionsResponse
	decodeConformanceResult(t, listedFrame, &listed)
	if !containsSession(listed.Sessions, created.SessionID) {
		t.Fatalf("session/list omitted %q: %#v", created.SessionID, listed.Sessions)
	}

	process.mustOK(t, 24, acp.MethodSessionClose, map[string]any{"sessionId": created.SessionID})
	loadedFrames := process.requestFrames(t, 25, acp.MethodSessionLoad, map[string]any{
		"sessionId": created.SessionID, "cwd": workspace, "mcpServers": []any{},
	}, nil)
	decodeConformanceResult(t, loadedFrames[len(loadedFrames)-1], &acp.LoadSessionResponse{})
	if countTranscriptChunks(loadedFrames, created.SessionID) < 2 {
		t.Fatalf("session/load did not replay committed transcript before terminal response: %#v", loadedFrames)
	}
	process.mustOK(t, 251, acp.MethodSessionClose, map[string]any{"sessionId": created.SessionID})
	resumedFrames := process.requestFrames(t, 252, acp.MethodSessionResume, map[string]any{
		"sessionId": created.SessionID, "cwd": workspace, "mcpServers": []any{},
	}, nil)
	decodeConformanceResult(t, resumedFrames[len(resumedFrames)-1], &acp.ResumeSessionResponse{})
	if countTranscriptChunks(resumedFrames, created.SessionID) < 2 {
		t.Fatalf("session/resume did not replay committed transcript: %#v", resumedFrames)
	}

	permissionSeen := false
	permissionFrames := process.prompt(t, 26, created.SessionID, "permission-fixture", func(frame conformanceFrame) {
		if frame.Method != acp.MethodSessionRequestPermission {
			t.Fatalf("unexpected client request during permission prompt: %#v", frame)
		}
		permissionSeen = true
		if bytes.Contains(frame.Params, []byte(conformanceCredential)) || bytes.Contains(frame.Params, []byte("chmod 777")) {
			t.Fatalf("permission payload leaked raw model tool input: %s", frame.Params)
		}
		process.respond(t, frame.ID, map[string]any{
			"outcome": map[string]string{"outcome": "selected", "optionId": "deny"},
		})
	})
	if !permissionSeen {
		t.Fatal("session/request_permission was not observed")
	}
	assertPromptResult(t, permissionFrames[len(permissionFrames)-1], acp.StopEndTurn)
	assertTerminalLast(t, permissionFrames)

	upstream.prepareCancellation()
	process.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 27, "method": acp.MethodSessionPrompt,
		"params": map[string]any{
			"sessionId": created.SessionID,
			"prompt":    []map[string]string{{"type": "text", "text": "cancel-fixture"}},
		},
	})
	upstream.waitForCancellationRequest(t)
	process.send(t, map[string]any{
		"jsonrpc": "2.0", "method": acp.MethodSessionCancel,
		"params": map[string]string{"sessionId": created.SessionID},
	})
	cancelFrames := process.awaitResponse(t, "27", nil)
	assertPromptResult(t, cancelFrames[len(cancelFrames)-1], acp.StopCancelled)
	assertTerminalLast(t, cancelFrames)

	process.mustOK(t, 28, acp.MethodSessionClose, map[string]any{"sessionId": created.SessionID})
	process.mustOK(t, 29, acp.MethodSessionDelete, map[string]any{"sessionId": created.SessionID})

	mcpMarker := filepath.Join(t.TempDir(), "mcp-called")
	mcpExecutable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	mcpCreatedFrame := process.request(t, 40, acp.MethodSessionNew, map[string]any{
		"cwd": workspace,
		"mcpServers": []any{map[string]any{
			"name": "repo-fixture", "command": mcpExecutable,
			"args": []string{"-test.run=^TestAdvertisedSurfaceMCPHelper$"},
			"env": []map[string]string{
				{"name": "CORELAY_ACP_CONFORMANCE_MCP_HELPER", "value": "1"},
				{"name": "CORELAY_ACP_CONFORMANCE_MCP_MARKER", "value": mcpMarker},
				{"name": "CORELAY_ACP_CONFORMANCE_MCP_VALUE", "value": conformanceMCPSpec},
			},
		}},
	}, nil)
	var mcpCreated acp.NewSessionResponse
	decodeConformanceResult(t, mcpCreatedFrame, &mcpCreated)
	assertStateDoesNotContain(t, filepath.Join(configDir, "acp"), conformanceMCPSpec)

	if processsupervisor.NewAutoRunner().Capabilities().ProcessIsolation {
		mcpFrames := process.prompt(t, 41, mcpCreated.SessionID, "mcp-fixture", nil)
		assertPromptResult(t, mcpFrames[len(mcpFrames)-1], acp.StopEndTurn)
		assertStreamingBeforeTerminal(t, mcpFrames, "mcp fixture complete")
		if data, err := os.ReadFile(mcpMarker); err != nil || string(data) != "called" {
			t.Fatalf("stable stdio MCP call marker = %q, error = %v", data, err)
		}
	}
	process.mustOK(t, 42, acp.MethodSessionClose, map[string]any{"sessionId": mcpCreated.SessionID})
	process.mustOK(t, 43, acp.MethodSessionDelete, map[string]any{"sessionId": mcpCreated.SessionID})

	process.finish(t)
	upstream.assertRequests(t)
	assertNoSecret(t, "ACP stdout", process.wire.String(), conformanceCredential, conformanceMCPSpec)
	assertNoSecret(t, "ACP stderr", process.stderr.String(), conformanceCredential, conformanceMCPSpec)
	assertStateDoesNotContain(t, filepath.Join(configDir, "acp"), conformanceMCPSpec)
}

func TestAdvertisedSurfaceMCPHelper(t *testing.T) {
	if os.Getenv("CORELAY_ACP_CONFORMANCE_MCP_HELPER") != "1" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
				"serverInfo": map[string]string{"name": "repo-fixture", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "mcp_surface_echo", "description": "Return deterministic fixture text",
				"inputSchema": map[string]any{
					"type": "object", "properties": map[string]any{
						"value": map[string]string{"type": "string"},
					}, "required": []string{"value"}, "additionalProperties": false,
				},
			}}}
		case "tools/call":
			_ = os.WriteFile(os.Getenv("CORELAY_ACP_CONFORMANCE_MCP_MARKER"), []byte("called"), 0600)
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "mcp-ok"}}}
		default:
			result = map[string]any{}
		}
		if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
			return
		}
	}
}

type acpConformanceUpstream struct {
	*httptest.Server

	mu            sync.Mutex
	requests      int
	badRequests   []string
	cancelStarted chan struct{}
	cancelOnce    sync.Once
}

func newACPConformanceUpstream(t *testing.T) *acpConformanceUpstream {
	t.Helper()
	upstream := &acpConformanceUpstream{cancelStarted: make(chan struct{})}
	upstream.Server = httptest.NewServer(http.HandlerFunc(upstream.serveHTTP))
	return upstream
}

func (u *acpConformanceUpstream) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 8<<20))
	u.mu.Lock()
	u.requests++
	if err != nil {
		u.badRequests = append(u.badRequests, "read request body")
	}
	if request.URL.Path != "/v1/chat/completions" {
		u.badRequests = append(u.badRequests, "path="+request.URL.Path)
	}
	if request.Header.Get("Authorization") != "Bearer "+conformanceCredential {
		u.badRequests = append(u.badRequests, "authorization header mismatch")
	}
	u.mu.Unlock()

	writer.Header().Set("Content-Type", "text/event-stream")
	if bytes.Contains(body, []byte("cancel-fixture")) {
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		u.cancelOnce.Do(func() { close(u.cancelStarted) })
		<-request.Context().Done()
		return
	}
	if bytes.Contains(body, []byte("permission-fixture")) && !bytes.Contains(body, []byte(`"tool_call_id":"permission-call"`)) {
		writeOpenAIToolCall(writer, "permission-call", "Bash", `{"command":"chmod 777 `+conformanceCredential+`"}`)
		return
	}
	if bytes.Contains(body, []byte("mcp-fixture")) && !bytes.Contains(body, []byte(`"tool_call_id":"mcp-call"`)) {
		if !bytes.Contains(body, []byte(`"name":"mcp_surface_echo"`)) {
			u.mu.Lock()
			u.badRequests = append(u.badRequests, "MCP tool absent from first provider request")
			u.mu.Unlock()
		}
		writeOpenAIToolCall(writer, "mcp-call", "mcp_surface_echo", `{"value":"hello"}`)
		return
	}
	switch {
	case bytes.Contains(body, []byte("permission-fixture")):
		writeOpenAIText(writer, "permission denied complete")
	case bytes.Contains(body, []byte("mcp-fixture")):
		writeOpenAIText(writer, "mcp fixture complete")
	default:
		writeOpenAIText(writer, "fixture stream complete")
	}
}

func (u *acpConformanceUpstream) prepareCancellation() {
	u.mu.Lock()
	u.cancelStarted = make(chan struct{})
	u.cancelOnce = sync.Once{}
	u.mu.Unlock()
}

func (u *acpConformanceUpstream) waitForCancellationRequest(t *testing.T) {
	t.Helper()
	u.mu.Lock()
	started := u.cancelStarted
	u.mu.Unlock()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("fake upstream did not receive cancellable request")
	}
}

func (u *acpConformanceUpstream) assertRequests(t *testing.T) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.requests < 4 || len(u.badRequests) != 0 {
		t.Fatalf("fake upstream requests=%d violations=%v", u.requests, u.badRequests)
	}
}

func writeOpenAIText(writer io.Writer, text string) {
	parts := []string{text[:len(text)/2], text[len(text)/2:]}
	for index, part := range parts {
		finish := any(nil)
		if index == len(parts)-1 {
			finish = "stop"
		}
		writeSSEData(writer, map[string]any{
			"id": "chat-fixture", "object": "chat.completion.chunk", "model": "fixture-model",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": part}, "finish_reason": finish,
			}},
		})
	}
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
}

func writeOpenAIToolCall(writer io.Writer, id, name, arguments string) {
	writeSSEData(writer, map[string]any{
		"id": "chat-fixture", "object": "chat.completion.chunk", "model": "fixture-model",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": id, "type": "function",
				"function": map[string]string{"name": name, "arguments": arguments},
			}}},
			"finish_reason": "tool_calls",
		}},
	})
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
}

func writeSSEData(writer io.Writer, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", data)
}

func loadAdvertisedSurfaceFixture(t *testing.T) advertisedSurfaceFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "acp-v1-advertised-surface.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture advertisedSurfaceFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func assertPinnedFixture(t *testing.T, fixture advertisedSurfaceFixture) {
	t.Helper()
	if fixture.Upstream.Repository != "https://github.com/agentclientprotocol/agent-client-protocol" ||
		fixture.Upstream.SnapshotCommit != "af41b25f57a79c5629b3164e23fb4e8650badeeb" ||
		fixture.Upstream.SchemaV1SHA256 != "dce90564fc0d87e16cd9645fa5faba1cb5fb7adac2608bb0b98f6fbda8e951f9" ||
		fixture.Upstream.MetaV1SHA256 != "e94998dd88acca9e53d5a0d7c89587b9b4e6fec4c1d925519da04e70917f797b" ||
		fixture.Upstream.ReleaseCommit != "5e89c71497fe07dd4ae633c181a17224f4a8956d" ||
		fixture.ProtocolVersion != acp.ProtocolVersion {
		t.Fatalf("ACP fixture provenance changed without an explicit pin update: %#v", fixture.Upstream)
	}
}

func assertAdvertisedSurface(t *testing.T, fixture advertisedSurfaceFixture, response acp.InitializeResponse) {
	t.Helper()
	capabilities := response.AgentCapabilities
	actual := []bool{
		capabilities.LoadSession,
		capabilities.SessionCapabilities.List != nil,
		capabilities.SessionCapabilities.Delete != nil,
		capabilities.SessionCapabilities.Resume != nil,
		capabilities.SessionCapabilities.Close != nil,
		capabilities.SessionCapabilities.AdditionalDirectories != nil,
		capabilities.PromptCapabilities.Image,
		capabilities.PromptCapabilities.Audio,
		capabilities.PromptCapabilities.EmbeddedContext,
		capabilities.MCPCapabilities.HTTP,
		capabilities.MCPCapabilities.SSE,
		capabilities.Auth.Logout != nil,
	}
	expected := []bool{
		fixture.Advertised.LoadSession,
		fixture.Advertised.SessionList,
		fixture.Advertised.SessionDelete,
		fixture.Advertised.SessionResume,
		fixture.Advertised.SessionClose,
		fixture.Advertised.AdditionalDirectories,
		fixture.Advertised.PromptImage,
		fixture.Advertised.PromptAudio,
		fixture.Advertised.PromptEmbeddedContext,
		fixture.Advertised.MCPHTTP,
		fixture.Advertised.MCPSSE,
		fixture.Advertised.AuthLogout,
	}
	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf("advertised capability[%d] = %v, want %v: %#v", index, actual[index], expected[index], capabilities)
		}
	}
	if response.ProtocolVersion != fixture.ProtocolVersion || len(response.AuthMethods) != 0 || response.AgentInfo == nil || response.AgentInfo.Name != "corelaycode" {
		t.Fatalf("initialize response = %#v", response)
	}
}

func writeACPConformanceConfig(t *testing.T, dir, baseURL string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"defaultProvider": "openai", "defaultModel": "fixture-model", "responseLang": "en",
		"providers": map[string]any{
			"openai": map[string]string{"apiKey": conformanceCredential, "baseUrl": baseURL},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func buildACPCommand(t *testing.T) string {
	t.Helper()
	name := "corelaycode-acp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build corelaycode-acp: %v\n%s", err, output)
	}
	return binary
}

func startACPCommand(t *testing.T, binary, configDir string) *acpCommandProcess {
	t.Helper()
	command := exec.Command(binary,
		"--provider", "openai", "--model", "fixture-model",
		"--max-frame-bytes", "2048", "--shutdown-timeout", "3s",
	)
	command.Env = conformanceEnvironment(configDir)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	lineChannel := make(chan conformanceLine, 128)
	go func() {
		defer close(lineChannel)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), acp.DefaultMaxFrameBytes+1)
		for scanner.Scan() {
			lineChannel <- conformanceLine{data: append([]byte(nil), scanner.Bytes()...)}
		}
		if err := scanner.Err(); err != nil {
			lineChannel <- conformanceLine{err: err}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return &acpCommandProcess{command: command, stdin: stdin, lines: lineChannel, done: done, stderr: stderr}
}

func conformanceEnvironment(configDir string) []string {
	blocked := map[string]struct{}{
		"CORELAY_CONFIG_DIR": {}, "CORELAY_MEMORY": {}, "CORELAY_AUTOSKILL": {},
		"CORELAY_AUTOVERIFY": {}, "OPENAI_API_KEY": {},
	}
	environment := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		name := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			name = item[:index]
		}
		if _, remove := blocked[strings.ToUpper(name)]; !remove {
			environment = append(environment, item)
		}
	}
	return append(environment,
		"CORELAY_CONFIG_DIR="+configDir,
		"CORELAY_MEMORY=off",
		"CORELAY_AUTOSKILL=off",
		"CORELAY_AUTOVERIFY=off",
	)
}

func (p *acpCommandProcess) send(t *testing.T, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	p.sendRaw(t, append(data, '\n'))
}

func (p *acpCommandProcess) sendRaw(t *testing.T, data []byte) {
	t.Helper()
	if _, err := p.stdin.Write(data); err != nil {
		t.Fatalf("write ACP command stdin: %v; stderr=%q", err, p.stderr.String())
	}
}

func (p *acpCommandProcess) read(t *testing.T) conformanceFrame {
	t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			t.Fatalf("ACP command stdout closed; stderr=%q", p.stderr.String())
		}
		if line.err != nil {
			t.Fatalf("read ACP command stdout: %v", line.err)
		}
		p.wire.Write(line.data)
		p.wire.WriteByte('\n')
		var frame conformanceFrame
		if err := json.Unmarshal(line.data, &frame); err != nil || frame.JSONRPC != "2.0" {
			t.Fatalf("ACP command emitted non-JSON-RPC stdout %q: %v", line.data, err)
		}
		return frame
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out reading ACP command; stderr=%q", p.stderr.String())
	}
	return conformanceFrame{}
}

func (p *acpCommandProcess) request(t *testing.T, id int64, method string, params any, clientRequest func(conformanceFrame)) conformanceFrame {
	t.Helper()
	frames := p.requestFrames(t, id, method, params, clientRequest)
	return frames[len(frames)-1]
}

func (p *acpCommandProcess) requestFrames(t *testing.T, id int64, method string, params any, clientRequest func(conformanceFrame)) []conformanceFrame {
	t.Helper()
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return p.awaitResponse(t, fmt.Sprint(id), clientRequest)
}

func (p *acpCommandProcess) awaitResponse(t *testing.T, id string, clientRequest func(conformanceFrame)) []conformanceFrame {
	t.Helper()
	var frames []conformanceFrame
	for {
		frame := p.read(t)
		frames = append(frames, frame)
		if frame.Method != "" && len(frame.ID) != 0 {
			if clientRequest == nil {
				t.Fatalf("unexpected ACP client request: %#v", frame)
			}
			clientRequest(frame)
			continue
		}
		if frame.Method == "" && string(frame.ID) == id {
			return frames
		}
	}
}

func (p *acpCommandProcess) prompt(t *testing.T, id int64, sessionID, text string, clientRequest func(conformanceFrame)) []conformanceFrame {
	t.Helper()
	return p.requestFrames(t, id, acp.MethodSessionPrompt, map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	}, clientRequest)
}

func (p *acpCommandProcess) respond(t *testing.T, id json.RawMessage, result any) {
	t.Helper()
	p.send(t, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: result})
}

func (p *acpCommandProcess) mustOK(t *testing.T, id int64, method string, params any) {
	t.Helper()
	frame := p.request(t, id, method, params, nil)
	if frame.Error != nil {
		t.Fatalf("%s error = %#v", method, frame.Error)
	}
}

func (p *acpCommandProcess) finish(t *testing.T) {
	t.Helper()
	if p.stdin == nil {
		return
	}
	_ = p.stdin.Close()
	p.stdin = nil
	select {
	case err := <-p.done:
		p.done = nil
		if err != nil {
			t.Fatalf("corelaycode-acp exit: %v; stderr=%q", err, p.stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = p.command.Process.Kill()
		t.Fatalf("corelaycode-acp did not exit after stdin EOF; stderr=%q", p.stderr.String())
	}
}

func (p *acpCommandProcess) stop(t *testing.T) {
	t.Helper()
	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	if p.done == nil {
		return
	}
	select {
	case <-p.done:
		p.done = nil
	case <-time.After(3 * time.Second):
		_ = p.command.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			t.Log("corelaycode-acp cleanup wait timed out")
		}
		p.done = nil
	}
}

func assertParseError(t *testing.T, frame conformanceFrame) {
	t.Helper()
	if string(frame.ID) != "null" || frame.Error == nil || frame.Error.Code != acp.CodeParseError {
		t.Fatalf("parse error frame = %#v", frame)
	}
}

func decodeConformanceResult(t *testing.T, frame conformanceFrame, target any) {
	t.Helper()
	if frame.Error != nil {
		t.Fatalf("RPC error = %#v", frame.Error)
	}
	if err := json.Unmarshal(frame.Result, target); err != nil {
		t.Fatalf("decode result %s: %v", frame.Result, err)
	}
}

func hasConfigOption(options []acp.SessionConfigOption, id string) bool {
	for _, option := range options {
		if option.ID == id {
			return true
		}
	}
	return false
}

func configOptionHasValue(options []acp.SessionConfigOption, id, expected string) bool {
	for _, option := range options {
		if option.ID == id {
			value, _ := option.CurrentValue.(string)
			return value == expected
		}
	}
	return false
}

func containsSession(sessions []acp.SessionInfo, id string) bool {
	for _, session := range sessions {
		if session.SessionID == id {
			return true
		}
	}
	return false
}

func countTranscriptChunks(frames []conformanceFrame, sessionID string) int {
	count := 0
	for _, frame := range frames[:len(frames)-1] {
		if frame.Method != acp.MethodSessionUpdate || !bytes.Contains(frame.Params, []byte(sessionID)) {
			continue
		}
		if bytes.Contains(frame.Params, []byte(`"sessionUpdate":"user_message_chunk"`)) ||
			bytes.Contains(frame.Params, []byte(`"sessionUpdate":"agent_message_chunk"`)) {
			count++
		}
	}
	return count
}

func assertPromptResult(t *testing.T, frame conformanceFrame, expected acp.StopReason) {
	t.Helper()
	var result acp.PromptResponse
	decodeConformanceResult(t, frame, &result)
	if result.StopReason != expected {
		t.Fatalf("prompt stopReason = %q, want %q", result.StopReason, expected)
	}
}

func assertStreamingBeforeTerminal(t *testing.T, frames []conformanceFrame, expectedText string) {
	t.Helper()
	assertTerminalLast(t, frames)
	var text strings.Builder
	for _, frame := range frames[:len(frames)-1] {
		if frame.Method != acp.MethodSessionUpdate {
			continue
		}
		var notification acp.SessionNotification
		if json.Unmarshal(frame.Params, &notification) == nil && notification.Update.SessionUpdate == "agent_message_chunk" && notification.Update.Content != nil {
			text.WriteString(notification.Update.Content.Text)
		}
	}
	if text.String() != expectedText {
		t.Fatalf("streamed text = %q, want %q; frames=%#v", text.String(), expectedText, frames)
	}
}

func assertTerminalLast(t *testing.T, frames []conformanceFrame) {
	t.Helper()
	if len(frames) == 0 || frames[len(frames)-1].Method != "" || len(frames[len(frames)-1].ID) == 0 {
		t.Fatalf("terminal response was not last: %#v", frames)
	}
}

func assertNoSecret(t *testing.T, label, value string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(value, secret) {
			t.Fatalf("%s leaked secret %q", label, secret)
		}
	}
}

func assertStateDoesNotContain(t *testing.T, root string, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() == ".instance.lock" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			return fmt.Errorf("secret found in %s", path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
