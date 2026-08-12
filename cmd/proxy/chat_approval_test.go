package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

type capturedApprovalDecision struct {
	SessionID string `json:"sessionId"`
	Decision  string `json:"decision"`
}

func TestStreamTurnApprovalDecisions(t *testing.T) {
	tests := []struct {
		name     string
		terminal bool
		input    string
		want     string
	}{
		{name: "non TTY denies", terminal: false, want: "deny"},
		{name: "TTY defaults to deny", terminal: true, input: "\n", want: "deny"},
		{name: "TTY explicit allow once", terminal: true, input: "yes\n", want: "allow_once"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decisions := make(chan capturedApprovalDecision, 1)
			resolved := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/agent":
					w.Header().Set("Content-Type", "text/event-stream")
					writeChatTestEvent(t, w, "session", map[string]string{"sessionId": "run-session"})
					writeChatTestEvent(t, w, "tool_input", map[string]any{
						"name":  "bash_exec",
						"input": map[string]string{"command": "raw-secret-value"},
					})
					writeChatTestEvent(t, w, "approval_required", map[string]string{
						"id":            "apr_test",
						"sessionId":     "run-session",
						"toolName":      "bash_exec",
						"redactedInput": `{"input":"omitted"}`,
						"dangerLevel":   "high",
						"scope":         "process",
						"expiresAt":     "2100-01-01T00:00:00Z",
					})
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					select {
					case <-resolved:
					case <-time.After(2 * time.Second):
						t.Error("approval decision was not posted")
						return
					}
					writeChatTestEvent(t, w, "text", "answer")
					writeChatTestEvent(t, w, "done", map[string]bool{"ok": true})
				case r.URL.Path == "/api/approvals/apr_test/resolve":
					var decision capturedApprovalDecision
					if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
						t.Errorf("decode decision: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					decisions <- decision
					close(resolved)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"apr_test","decision":"`+decision.Decision+`"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			var approvalOutput bytes.Buffer
			client := &chatClient{
				base:        server.URL,
				http:        server.Client(),
				input:       strings.NewReader(tt.input),
				approvalOut: &approvalOutput,
				terminal:    func() bool { return tt.terminal },
				messages:    []chatMsg{{Role: "user", Content: "run"}},
			}
			answer, err := client.streamTurn()
			if err != nil {
				t.Fatalf("streamTurn() error = %v", err)
			}
			if answer != "answer" {
				t.Fatalf("answer = %q, want answer", answer)
			}
			decision := <-decisions
			if decision.SessionID != "run-session" || decision.Decision != tt.want {
				t.Fatalf("decision = %+v, want session run-session and %s", decision, tt.want)
			}
			if strings.Contains(approvalOutput.String(), "raw-secret-value") {
				t.Fatal("approval output exposed raw tool input")
			}
			if !strings.Contains(approvalOutput.String(), `{"input":"omitted"}`) && tt.terminal {
				t.Fatal("TTY prompt did not show the redacted input")
			}

			if tt.want == "allow_once" && !strings.Contains(approvalOutput.String(), "Allowed once") {
				t.Fatalf("output = %q, want allow confirmation", approvalOutput.String())
			}
			if tt.want == "deny" && !strings.Contains(approvalOutput.String(), "Denied") {
				t.Fatalf("output = %q, want deny confirmation", approvalOutput.String())
			}
		})
	}
}

func TestApprovalConflictCancelsTurnFailClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/agent":
			w.Header().Set("Content-Type", "text/event-stream")
			writeChatTestEvent(t, w, "session", map[string]string{"sessionId": "run-session"})
			writeChatTestEvent(t, w, "approval_required", map[string]string{
				"id": "apr_conflict", "sessionId": "run-session", "toolName": "bash_exec",
				"expiresAt": "2100-01-01T00:00:00Z",
			})
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case r.URL.Path == "/api/approvals/apr_conflict/resolve":
			w.WriteHeader(http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var approvalOutput bytes.Buffer
	client := &chatClient{
		base:        server.URL,
		http:        server.Client(),
		input:       strings.NewReader("yes\n"),
		approvalOut: &approvalOutput,
		terminal:    func() bool { return true },
		messages:    []chatMsg{{Role: "user", Content: "run"}},
	}
	if _, err := client.streamTurn(); err == nil {
		t.Fatal("streamTurn() error = nil, want canceled fail-closed turn")
	}
	if !strings.Contains(approvalOutput.String(), "no longer actionable") {
		t.Fatalf("output = %q, want conflict explanation", approvalOutput.String())
	}
}

func TestPlainStreamRejectsOversizedSSE(t *testing.T) {
	tests := []struct {
		name    string
		write   func(io.Writer)
		wantErr error
	}{
		{
			name: "line",
			write: func(w io.Writer) {
				_, _ = fmt.Fprint(w, "data: ", strings.Repeat("x", maxAgentSSELineBytes), "\n\n")
			},
			wantErr: errAgentSSELineTooLong,
		},
		{
			name: "multiline event",
			write: func(w io.Writer) {
				chunk := strings.Repeat("x", maxAgentSSEEventBytes/2+1)
				_, _ = fmt.Fprintf(w, "data: %s\ndata: %s\n\n", chunk, chunk)
			},
			wantErr: errAgentSSEEventTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				tt.write(w)
			}))
			defer server.Close()

			client := &chatClient{
				base:     server.URL,
				http:     server.Client(),
				messages: []chatMsg{{Role: "user", Content: "run"}},
			}
			_, err := client.streamTurn()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("streamTurn() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPlainStreamReturnsBoundedSanitizedHTTPError(t *testing.T) {
	const token = "plain-stream-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("\x1b[31mdenied\x1b[0m\x00\xff token=" + token + " " + strings.Repeat("z", 6000)))
	}))
	defer server.Close()

	client := &chatClient{
		base:        server.URL,
		accessToken: token,
		http:        server.Client(),
		messages:    []chatMsg{{Role: "user", Content: "run"}},
	}
	_, err := client.streamTurn()
	if err == nil {
		t.Fatal("streamTurn() error = nil")
	}
	message := err.Error()
	if strings.Contains(message, token) {
		t.Fatal("plain stream error exposed the access token")
	}
	if len(message) > maxAgentHTTPErrorBodyBytes {
		t.Fatalf("plain stream error length = %d, want <= %d", len(message), maxAgentHTTPErrorBodyBytes)
	}
	if !utf8.ValidString(message) {
		t.Fatalf("plain stream error is not valid UTF-8: %q", message)
	}
	for _, char := range message {
		if char != '\n' && char != '\t' && unicode.IsControl(char) {
			t.Fatalf("plain stream error retained control character %U", char)
		}
	}
}

func TestPlainStreamSanitizesTextEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatTestEvent(t, w, "text", "한글\x1b]52;c;secret\x07 안전\x00")
		writeChatTestEvent(t, w, "done", map[string]bool{"ok": true})
	}))
	defer server.Close()

	client := &chatClient{
		base:     server.URL,
		http:     server.Client(),
		messages: []chatMsg{{Role: "user", Content: "run"}},
	}
	answer, err := client.streamTurn()
	if err != nil {
		t.Fatalf("streamTurn() error = %v", err)
	}
	if answer != "한글 안전" {
		t.Fatalf("answer = %q, want sanitized CJK text", answer)
	}
	if !utf8.ValidString(answer) || strings.ContainsRune(answer, '\x1b') || strings.ContainsRune(answer, '\x00') {
		t.Fatalf("answer retained unsafe terminal content: %q", answer)
	}
}

func TestPlainStreamTextOnlyEOFIsUnknownTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatTestEvent(t, w, "text", "partial answer")
	}))
	defer server.Close()

	client := &chatClient{
		base:     server.URL,
		http:     server.Client(),
		messages: []chatMsg{{Role: "user", Content: "run"}},
	}
	answer, err := client.streamTurn()
	if answer != "partial answer" {
		t.Fatalf("streamTurn() answer = %q, want retained partial answer for diagnostics", answer)
	}
	if !errors.Is(err, errChatUnknownTerminal) {
		t.Fatalf("streamTurn() error = %v, want unknown-terminal error", err)
	}
	if strings.Contains(err.Error(), answer) {
		t.Fatalf("unknown-terminal error exposed streamed content: %q", err)
	}
}

func TestPlainStreamDoneReturnsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatTestEvent(t, w, "text", "complete answer")
		writeChatTestEvent(t, w, "done", map[string]any{
			"terminalState":    "verified",
			"completionStatus": "complete",
		})
		writeChatTestEvent(t, w, "stream_end", nil)
	}))
	defer server.Close()

	client := &chatClient{
		base:     server.URL,
		http:     server.Client(),
		messages: []chatMsg{{Role: "user", Content: "run"}},
	}
	answer, err := client.streamTurn()
	if err != nil {
		t.Fatalf("streamTurn() error = %v", err)
	}
	if answer != "complete answer" {
		t.Fatalf("streamTurn() answer = %q, want complete answer", answer)
	}
}

func TestResolveAccessTokenPrecedence(t *testing.T) {
	t.Setenv("CORELAY_ACCESS_TOKEN", "current-token")
	t.Setenv("ANICLEW_ACCESS_TOKEN", "legacy-token")

	if got := resolveAccessToken("flag-token"); got != "flag-token" {
		t.Fatalf("resolveAccessToken(flag) = %q, want flag-token", got)
	}
	if got := resolveAccessToken(""); got != "current-token" {
		t.Fatalf("resolveAccessToken(current env) = %q, want current-token", got)
	}
	t.Setenv("CORELAY_ACCESS_TOKEN", "")
	if got := resolveAccessToken(""); got != "legacy-token" {
		t.Fatalf("resolveAccessToken(legacy env) = %q, want legacy-token", got)
	}
}

func TestPlainOneShotUnknownTerminalExitsNonzero(t *testing.T) {
	if os.Getenv("CORELAY_PLAIN_ONESHOT_HELPER") == "1" {
		runChat([]string{
			"-url", os.Getenv("CORELAY_PLAIN_ONESHOT_URL"),
			"-p", "run",
			"-no-color",
		})
		return
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		case "/api/agent":
			w.Header().Set("Content-Type", "text/event-stream")
			writeChatTestEvent(t, w, "text", "partial one-shot answer")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestPlainOneShotUnknownTerminalExitsNonzero$")
	command.Env = append(os.Environ(),
		"CORELAY_PLAIN_ONESHOT_HELPER=1",
		"CORELAY_PLAIN_ONESHOT_URL="+server.URL,
		"CORELAY_ACCESS_TOKEN=",
		"ANICLEW_ACCESS_TOKEN=",
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("one-shot process error = %v, output = %q; want exit code 1", err, output)
	}
	text := string(output)
	if !strings.Contains(text, errChatUnknownTerminal.Error()) {
		t.Fatalf("one-shot output = %q, want unknown-terminal error", text)
	}
}

func TestApprovalRequiresFutureExpiry(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt string
	}{
		{name: "missing"},
		{name: "malformed", expiresAt: "not-a-deadline-secret"},
		{name: "expired", expiresAt: "2000-01-01T00:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resolveCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/agent":
					w.Header().Set("Content-Type", "text/event-stream")
					writeChatTestEvent(t, w, "session", map[string]string{"sessionId": "run-session"})
					writeChatTestEvent(t, w, "approval_required", map[string]string{
						"id":        "apr_invalid_expiry",
						"sessionId": "run-session",
						"toolName":  "bash_exec",
						"expiresAt": tt.expiresAt,
					})
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					<-r.Context().Done()
				case strings.HasPrefix(r.URL.Path, "/api/approvals/"):
					resolveCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			var output bytes.Buffer
			client := &chatClient{
				base:        server.URL,
				http:        server.Client(),
				input:       strings.NewReader("yes\n"),
				approvalOut: &output,
				terminal:    func() bool { return true },
				messages:    []chatMsg{{Role: "user", Content: "run"}},
			}
			if _, err := client.streamTurn(); err == nil {
				t.Fatal("streamTurn() error = nil, want fail-closed cancellation")
			}
			if got := resolveCalls.Load(); got != 0 {
				t.Fatalf("approval resolve calls = %d, want 0", got)
			}
			if strings.Contains(output.String(), tt.expiresAt) && tt.expiresAt != "" {
				t.Fatalf("approval output exposed untrusted expiry: %q", output.String())
			}
			if !strings.Contains(output.String(), "canceling the run without approval") {
				t.Fatalf("output = %q, want fail-closed explanation", output.String())
			}
		})
	}
}

func TestApprovalResolutionDeadlineUsesEarlierBound(t *testing.T) {
	now := time.Date(2026, time.August, 12, 9, 30, 0, 0, time.UTC)
	if got, want := approvalResolutionDeadline(now.Add(time.Minute), now), now.Add(15*time.Second); !got.Equal(want) {
		t.Fatalf("long expiry deadline = %s, want %s", got, want)
	}
	if got, want := approvalResolutionDeadline(now.Add(2*time.Second), now), now.Add(2*time.Second); !got.Equal(want) {
		t.Fatalf("short expiry deadline = %s, want %s", got, want)
	}
}

func TestStreamTurnReturnsBlockedCompletionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatTestEvent(t, w, "text", "bounded blocked answer")
		writeChatTestEvent(t, w, "done", map[string]any{
			"terminalState":      "blocked",
			"completionStatus":   "incomplete",
			"completionRevision": uint64(2),
			"completionCriteria": 1,
		})
		writeChatTestEvent(t, w, "stream_end", nil)
	}))
	defer server.Close()

	client := &chatClient{
		base:     server.URL,
		http:     server.Client(),
		messages: []chatMsg{{Role: "user", Content: "run"}},
	}
	answer, err := client.streamTurn()
	var blocked *chatCompletionBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("streamTurn() error = %v, want chatCompletionBlockedError", err)
	}
	if answer != "bounded blocked answer" || blocked.status != "incomplete" {
		t.Fatalf("answer/status = %q/%q", answer, blocked.status)
	}
}

func TestCanceledConsoleReadDoesNotStealNextLine(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	client := &chatClient{input: reader}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.readConsoleLine(canceled); err != context.Canceled {
		t.Fatalf("readConsoleLine() error = %v, want context.Canceled", err)
	}

	go func() { _, _ = fmt.Fprintln(writer, "next command") }()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	line, err := client.readConsoleLine(ctx)
	if err != nil {
		t.Fatalf("next read error = %v", err)
	}
	if line != "next command" {
		t.Fatalf("next line = %q", line)
	}
}

func TestTerminalSafeRemovesControlSequences(t *testing.T) {
	got := terminalSafe("safe\x1b[31m\x00text")
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\x00') {
		t.Fatalf("terminalSafe() retained control characters: %q", got)
	}
}

func writeChatTestEvent(t *testing.T, w io.Writer, eventType string, data any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"type": eventType, "data": data})
	if err != nil {
		t.Fatalf("marshal SSE event: %v", err)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}
