package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

func TestNormalizeChatOutputPreservesStructuredUndoEvents(t *testing.T) {
	name, value, ok := normalizeChatOutputEvent(agentWireEvent{
		Type: "undo_preview",
		Data: json.RawMessage(`{"entries":[{"id":"abc123456789","path":"safe.txt","status":"restorable"}],"error":""}`),
	}, nil, false, "secret-token", "http://127.0.0.1:1")
	if !ok || name != "undo_preview" {
		t.Fatalf("normalized undo event = name:%q value:%#v ok:%v", name, value, ok)
	}
	payload, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("undo payload type = %T", value)
	}
	entries, ok := payload["entries"].([]map[string]string)
	if !ok || len(entries) != 1 || entries[0]["id"] != "abc123456789" || entries[0]["path"] != "safe.txt" || entries[0]["status"] != "restorable" {
		t.Fatalf("undo entries = %#v", payload["entries"])
	}
}

func TestChatJSONOneShotWritesOneVersionedResultToStdout(t *testing.T) {
	workspace := t.TempDir()
	fixture := &chatSessionAPIFixture{workspace: workspace, sessions: make(map[string]*agent.Session)}
	server := httptest.NewServer(fixture)
	defer server.Close()

	stdout, stderr, err := runChatSubprocessCaptured(t, []string{
		"-url", server.URL, "-workdir", workspace, "-format", "json", "-p", "return structured output",
	})
	if err != nil {
		t.Fatalf("JSON one-shot failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	var result chatOutputResult
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode JSON stdout: %v\n%s", err, stdout)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON stdout has trailing output (%v): %s", err, stdout)
	}
	if result.SchemaVersion != chatOutputSchemaVersion || result.RunID == "" || result.Status != "completed" || result.ExitCode != 0 {
		t.Fatalf("JSON result header = %+v", result)
	}
	if result.Text != "reply-1" || result.SessionID != "session-1" || result.SessionRevision == 0 {
		t.Fatalf("JSON result content = %+v", result)
	}
	if strings.Contains(stdout, "Durable session:") || strings.Contains(stdout, "Execution mode:") || strings.Contains(stdout, "iteration") {
		t.Fatalf("machine stdout contains human progress: %q", stdout)
	}
	if strings.Contains(stderr, "Durable session:") {
		t.Fatalf("machine stderr contains the human session banner: %q", stderr)
	}
}

func TestChatJSONLOneShotEmitsAllowlistedVersionedEventsAndOneResult(t *testing.T) {
	workspace := t.TempDir()
	server := newDurableChatStubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/config" {
			writeChatJSON(w, map[string]any{})
			return
		}
		if r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatSSEEvent(w, "session", map[string]any{
			"sessionId":       "server-session",
			"traceId":         "secret-access-token-for-run-id",
			"workDir":         "C:/private/workspace",
			"executionPolicy": map[string]any{"mode": "workspace", "revision": 9},
		})
		writeChatSSEEvent(w, "workstream", map[string]any{
			"id": "workstream-1", "title": "private title", "status": "active", "nextAction": "private action",
		})
		writeChatSSEEvent(w, "tool_input", map[string]any{"name": "shell", "input": "raw-tool-input-secret"})
		writeChatSSEEvent(w, "text", "structured answer")
		writeChatSSEEvent(w, "done", map[string]any{
			"kind": "completed", "stopReason": "end_turn", "durablePolicy": "commit", "terminalState": "unverified",
		})
		_, _ = io.WriteString(w, "data: {\"type\":\"stream_end\"}\n\n")
	}))
	defer server.Close()

	stdout, stderr, err := runChatSubprocessCaptured(t, []string{
		"-url", server.URL, "-token", "secret-access-token-for-run-id", "-workdir", workspace, "-format", "jsonl", "-p", "return structured output",
	})
	if err != nil {
		t.Fatalf("JSONL one-shot failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 3 {
		t.Fatalf("JSONL output has too few records: %q", stdout)
	}
	var names []string
	var resultCount int
	var runID string
	for index, line := range lines {
		var event chatOutputEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("JSONL line %d is invalid: %v: %q", index+1, err, line)
		}
		if index == 0 {
			runID = event.RunID
		}
		if event.SchemaVersion != chatOutputSchemaVersion || event.Seq != uint64(index+1) || event.RunID != runID || event.RunID == "secret-access-token-for-run-id" {
			t.Fatalf("JSONL envelope %d = %+v", index+1, event)
		}
		names = append(names, event.Event)
		if event.Event == "result" {
			resultCount++
			if index != len(lines)-1 {
				t.Fatalf("result record is not last: %q", stdout)
			}
			data, ok := event.Data.(map[string]any)
			if !ok || data["status"] != "completed" || data["text"] != "structured answer" {
				t.Fatalf("result data = %#v", event.Data)
			}
		}
	}
	if resultCount != 1 || names[0] != "session" || names[len(names)-1] != "result" || !containsString(names, "text") {
		t.Fatalf("JSONL event sequence = %v; want session/text/.../one result", names)
	}
	for _, secret := range []string{"raw-tool-input-secret", "tool_input", "C:/private/workspace", "private title", "private action", "secret-access-token-for-run-id"} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("JSONL exposed %q: %s", secret, stdout)
		}
	}
	if strings.Contains(stderr, "Durable session:") || strings.Contains(stdout, "thinking…") || strings.Contains(stdout, "Execution mode:") {
		t.Fatalf("JSONL leaked human progress: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestChatMachineOutputReportsUsageAndStartupFailuresAsStructuredResults(t *testing.T) {
	t.Run("format requires one-shot prompt", func(t *testing.T) {
		stdout, _, err := runChatSubprocessCaptured(t, []string{"-format", "json"})
		assertChatProcessExitCode(t, err, 2)
		var result chatOutputResult
		if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
			t.Fatalf("decode usage result: %v; stdout=%q", decodeErr, stdout)
		}
		if result.ExitCode != 2 || result.Error == nil || result.Error.Code != "usage_error" {
			t.Fatalf("usage result = %+v", result)
		}
	})

	t.Run("unknown flag still writes a structured usage result", func(t *testing.T) {
		stdout, stderr, err := runChatSubprocessCaptured(t, []string{"-unknown", "-format", "json", "-p", "run"})
		assertChatProcessExitCode(t, err, 2)
		var result chatOutputResult
		if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
			t.Fatalf("decode parse-error result: %v; stdout=%q stderr=%q", decodeErr, stdout, stderr)
		}
		if result.ExitCode != 2 || result.Error == nil || result.Error.Code != "usage_error" || strings.TrimSpace(stderr) != "" {
			t.Fatalf("parse-error result = %+v; stderr=%q", result, stderr)
		}
	})

	t.Run("unreachable server is machine-readable", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		stdout, stderr, err := runChatSubprocessCaptured(t, []string{"-url", server.URL, "-format", "json", "-p", "run"})
		assertChatProcessExitCode(t, err, 1)
		var result chatOutputResult
		if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
			t.Fatalf("decode startup result: %v; stdout=%q; stderr=%q", decodeErr, stdout, stderr)
		}
		if result.ExitCode != 1 || result.Status != "failed" || result.Error == nil || result.Error.Code != "server_unavailable" {
			t.Fatalf("startup result = %+v", result)
		}
	})
}

func TestChatOneShotTypedTerminalFailureExitsNonzero(t *testing.T) {
	workspace := t.TempDir()
	server := newDurableChatStubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/config" {
			writeChatJSON(w, map[string]any{})
			return
		}
		if r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatSSEEvent(w, "session", map[string]any{"sessionId": "run-failed", "traceId": "trace-failed"})
		writeChatSSEEvent(w, "text", "partial answer")
		writeChatSSEEvent(w, "done", map[string]any{
			"kind": "failed", "stopReason": "provider_retry_exhausted", "terminalState": "blocked",
		})
		_, _ = io.WriteString(w, "data: {\"type\":\"stream_end\"}\n\n")
	}))
	defer server.Close()

	stdout, stderr, err := runChatSubprocessCaptured(t, []string{
		"-url", server.URL, "-workdir", workspace, "-format", "json", "-p", "must report failed terminal",
	})
	assertChatProcessExitCode(t, err, 1)
	var result chatOutputResult
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatalf("decode failed-terminal JSON: %v; stdout=%q stderr=%q", decodeErr, stdout, stderr)
	}
	if result.Status != "failed" || result.ExitCode != 1 || result.Error == nil || result.Terminal == nil || result.Terminal.Kind != "failed" {
		t.Fatalf("failed-terminal result = %+v", result)
	}
}

func TestChatMachineOutputDoesNotExposeServerTerminalKindOrStopReason(t *testing.T) {
	secret := "access-token-in-terminal-event"
	workspace := t.TempDir()
	server := newDurableChatStubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/config" {
			writeChatJSON(w, map[string]any{})
			return
		}
		if r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatSSEEvent(w, "session", map[string]any{"sessionId": "run-terminal-secret", "traceId": "trace-safe"})
		writeChatSSEEvent(w, "done", map[string]any{"kind": secret, "stopReason": secret})
		_, _ = io.WriteString(w, "data: {\"type\":\"stream_end\"}\n\n")
	}))
	defer server.Close()

	stdout, stderr, err := runChatSubprocessCaptured(t, []string{
		"-url", server.URL, "-token", secret, "-workdir", workspace, "-format", "json", "-p", "check terminal redaction",
	})
	assertChatProcessExitCode(t, err, 1)
	var result chatOutputResult
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatalf("decode terminal redaction result: %v; stdout=%q stderr=%q", decodeErr, stdout, stderr)
	}
	if strings.Contains(stdout, secret) || result.Error == nil || result.Error.Code != "run_failed" {
		t.Fatalf("server terminal secret escaped output contract: %+v; stdout=%q", result, stdout)
	}
	if result.Terminal == nil || result.Terminal.Kind != "" || result.Terminal.StopReason != "[redacted]" {
		t.Fatalf("untrusted terminal fields = %+v", result.Terminal)
	}
}

func TestChatMachineOutputRedactsServerSuppliedRunIdentifiersAndTerminalFields(t *testing.T) {
	secret := "access-token-in-server-event"
	workspace := t.TempDir()
	server := newDurableChatStubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/config" {
			writeChatJSON(w, map[string]any{})
			return
		}
		if r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatSSEEvent(w, "session", map[string]any{"sessionId": "run-secret", "traceId": secret})
		writeChatSSEEvent(w, "done", map[string]any{"kind": secret, "stopReason": secret})
		_, _ = io.WriteString(w, "data: {\"type\":\"stream_end\"}\n\n")
	}))
	defer server.Close()

	stdout, stderr, err := runChatSubprocessCaptured(t, []string{
		"-url", server.URL, "-token", secret, "-workdir", workspace, "-format", "json", "-p", "check redaction",
	})
	assertChatProcessExitCode(t, err, 1)
	var result chatOutputResult
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatalf("decode redacted result: %v; stdout=%q stderr=%q", decodeErr, stdout, stderr)
	}
	if strings.Contains(stdout, secret) || result.RunID == secret || result.Error == nil || result.Error.Code != "run_failed" {
		t.Fatalf("server event secret escaped normalization: %+v; stdout=%q", result, stdout)
	}
	if result.Terminal == nil || result.Terminal.Kind != "" || result.Terminal.StopReason != "[redacted]" {
		t.Fatalf("untrusted terminal fields = %+v", result.Terminal)
	}
}

func TestChatStreamRefreshFollowsInterruptContext(t *testing.T) {
	workspace := t.TempDir()
	refreshStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/config":
			writeChatJSON(w, map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			writeChatJSON(w, sessionSaveResult{ID: "refresh-session", Version: 1, Revision: 1})
		case r.Method == http.MethodGet && r.URL.Path == sessionPath("refresh-session"):
			close(refreshStarted)
			<-r.Context().Done()
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			w.Header().Set("Content-Type", "text/event-stream")
			writeChatSSEEvent(w, "session", map[string]any{"sessionId": "run-refresh"})
			writeChatSSEEvent(w, "text", "finished before refresh")
			writeChatSSEEvent(w, "done", map[string]any{"kind": "completed", "terminalState": "unverified"})
			_, _ = io.WriteString(w, "data: {\"type\":\"stream_end\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runCtx, cancel := context.WithCancel(context.Background())
	client := &chatClient{
		base:       server.URL,
		workDir:    workspace,
		http:       server.Client(),
		runCtx:     runCtx,
		format:     chatOutputHuman,
		showStatus: false,
		messages:   nil,
	}
	type result struct {
		answer string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		answer, err := client.runOnce("cancel during canonical session refresh")
		resultCh <- result{answer: answer, err: err}
	}()
	<-refreshStarted
	cancel()
	select {
	case got := <-resultCh:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("runOnce error = %v, want interrupted refresh cancellation", got.err)
		}
		status, exitCode := chatRunStatus(got.err, true)
		if status != "cancelled" || exitCode != 130 {
			t.Fatalf("interrupted refresh status = %s/%d", status, exitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session refresh ignored the one-shot cancellation context")
	}
}

func TestChatJSONLStreamEOFDoesNotRetryOrDuplicateAgentTurn(t *testing.T) {
	workspace := t.TempDir()
	var agentPosts atomic.Int32
	server := newDurableChatStubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/config" {
			writeChatJSON(w, map[string]any{})
			return
		}
		if r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		agentPosts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatSSEEvent(w, "session", map[string]any{"sessionId": "run-1", "traceId": "trace-drop-1"})
		writeChatSSEEvent(w, "text", "partial response before disconnect")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Returning closes the stream without a done event. The client must fail
		// closed and must not POST the mutating agent turn a second time.
	}))
	defer server.Close()

	stdout, stderr, err := runChatSubprocessCaptured(t, []string{
		"-url", server.URL, "-workdir", workspace, "-format", "jsonl", "-p", "run exactly once",
	})
	assertChatProcessExitCode(t, err, 1)
	if got := agentPosts.Load(); got != 1 {
		t.Fatalf("agent POST count = %d, want exactly one; stdout=%q stderr=%q", got, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var final chatOutputEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &final); err != nil {
		t.Fatalf("decode final JSONL result: %v; stdout=%q", err, stdout)
	}
	data, ok := final.Data.(map[string]any)
	if final.Event != "result" || !ok || data["status"] != "failed" || data["exitCode"] != float64(1) {
		t.Fatalf("final stream-drop event = %+v", final)
	}
	if !strings.Contains(stdout, "partial response before disconnect") || !strings.Contains(stdout, "unknown_terminal") {
		t.Fatalf("JSONL did not report partial event and unknown terminal: %s", stdout)
	}
}

func TestChatRunStatusMapsCancellationAndTerminalFailures(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		interrupted bool
		want        string
		code        int
	}{
		{name: "success", want: "completed", code: 0},
		{name: "interrupted", err: context.Canceled, interrupted: true, want: "cancelled", code: 130},
		{name: "interrupted after stream completion", interrupted: true, want: "cancelled", code: 130},
		{name: "internal cancellation", err: &chatTerminalFailureError{kind: "cancelled"}, want: "failed", code: 1},
		{name: "blocked terminal", err: &chatCompletionBlockedError{status: "incomplete"}, want: "blocked", code: 1},
		{name: "failed terminal", err: &chatTerminalFailureError{kind: "failed"}, want: "failed", code: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, code := chatRunStatus(test.err, test.interrupted)
			if status != test.want || code != test.code {
				t.Fatalf("chatRunStatus(%v) = %s/%d, want %s/%d", test.err, status, code, test.want, test.code)
			}
		})
	}
}

func runChatSubprocessCaptured(t *testing.T, args []string) (string, string, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", "", err
	}
	command := exec.Command(os.Args[0], "-test.run=^TestChatSessionSubprocessHelper$")
	command.Env = replaceProcessEnv(os.Environ(), map[string]string{
		"CORELAY_CHAT_TEST_CHILD": "1",
		"CORELAY_CHAT_TEST_ARGS":  string(encoded),
		"CORELAY_CONFIG_DIR":      t.TempDir(),
		"CORELAY_ACCESS_TOKEN":    "",
		"ANICLEW_ACCESS_TOKEN":    "",
		"ANICLEW_CONFIG_DIR":      "",
	})
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	return stdout.String(), stderr.String(), err
}

func assertChatProcessExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("process error = %v, want success", err)
		}
		return
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != want {
		t.Fatalf("process error = %v, want exit code %d", err, want)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
