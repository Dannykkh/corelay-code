package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentStreamTransportAuthAndMetadataEndpoints(t *testing.T) {
	t.Parallel()

	const token = "transport-secret"
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("X-Access-Token")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok","provider":"local","model":"m"}`))
		case "/api/config":
			if r.Method == http.MethodPut {
				_, _ = w.Write([]byte(`{"provider":"next","model":"m2","routerEnabled":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"provider":"local","model":"m","responseLang":"ko","routerEnabled":false}`))
		case "/":
			_, _ = w.Write([]byte(`{"name":"corelaycode","version":"1.0.0","provider":"local","model":"m","router":false}`))
		case "/api/commands":
			if got := r.URL.Query().Get("workDir"); got != `D:\workspace 한글` {
				t.Errorf("workDir query = %q", got)
			}
			_, _ = w.Write([]byte(`[{"name":"plan","description":"Plan","skillName":"plan","skillPath":"skill.md"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL+"/", token, server.Client())
	ctx := context.Background()
	health, err := transport.Health(ctx)
	if err != nil || health.Status != "ok" {
		t.Fatalf("Health() = %#v, %v", health, err)
	}
	config, err := transport.Config(ctx)
	if err != nil || config.ResponseLang != "ko" {
		t.Fatalf("Config() = %#v, %v", config, err)
	}
	updated, err := transport.SetConfig(ctx, "next", "m2")
	if err != nil || updated.Provider != "next" || !updated.RouterEnabled {
		t.Fatalf("SetConfig() = %#v, %v", updated, err)
	}
	root, err := transport.Root(ctx)
	if err != nil || root.Name != "corelaycode" {
		t.Fatalf("Root() = %#v, %v", root, err)
	}
	commands, err := transport.Commands(ctx, `D:\workspace 한글`)
	if err != nil || len(commands) != 1 || commands[0].Name != "plan" {
		t.Fatalf("Commands() = %#v, %v", commands, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := seen["/health"]; got != "" {
		t.Fatalf("health auth header = %q, want empty", got)
	}
	for _, path := range []string{"/api/config", "/", "/api/commands"} {
		if got := seen[path]; got != token {
			t.Errorf("%s auth header = %q, want %q", path, got, token)
		}
	}
}

func TestAgentStreamTransportStartTurnPreservesChunkedCJKAndOrder(t *testing.T) {
	t.Parallel()

	const token = "stream-secret"
	frames := "data: {\"type\":\"session\",\"data\":{\"sessionId\":\"run-1\"}}\r\n\r\n" +
		": heartbeat comment\n" +
		"data: {\"type\":\"text\",\"data\":\"한글과 日本語\"}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Access-Token"); got != token {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		var request agentTurnRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.WorkDir != `D:\repo` || len(request.Messages) != 1 {
			t.Errorf("request = %#v", request)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for index := 0; index < len(frames); index++ {
			_, _ = w.Write([]byte{frames[index]})
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL, token, server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{
		Messages: []chatMsg{{Role: "user", Content: "테스트"}},
		WorkDir:  `D:\repo`,
	}))
	if len(items) != 3 {
		t.Fatalf("stream item count = %d, items = %#v", len(items), items)
	}
	if items[0].Event.Type != "session" || items[1].Event.Type != "text" {
		t.Fatalf("event order = %q, %q", items[0].Event.Type, items[1].Event.Type)
	}
	var text string
	if err := json.Unmarshal(items[1].Event.Data, &text); err != nil {
		t.Fatalf("decode text event: %v", err)
	}
	if text != "한글과 日本語" {
		t.Fatalf("text event = %q", text)
	}
	if !items[2].EOF || items[2].Err != nil {
		t.Fatalf("terminal item = %#v, want EOF", items[2])
	}
}

func TestAgentStreamTransportRejectsOversizeSSELine(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: ", strings.Repeat("x", maxAgentSSELineBytes), "\n\n")
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{}))
	if len(items) != 1 || !errors.Is(items[0].Err, errAgentSSELineTooLong) || items[0].EOF {
		t.Fatalf("oversize stream items = %#v", items)
	}
}

func TestAgentStreamTransportRejectsOversizeMultilineSSEEvent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := strings.Repeat("x", maxAgentSSEEventBytes/2+1)
		_, _ = fmt.Fprintf(w, "data: %s\ndata: %s\n\n", chunk, chunk)
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{}))
	if len(items) != 1 || !errors.Is(items[0].Err, errAgentSSEEventTooLong) || items[0].EOF {
		t.Fatalf("oversize multiline event items = %#v", items)
	}
}

func TestAgentStreamTransportSafeHTTPError(t *testing.T) {
	t.Parallel()

	const token = "do-not-render-this-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "\x1b[31mdenied\x00\u009b token=%s %s", token, strings.Repeat("z", 5000))
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL, token, server.Client())
	_, err := transport.Config(context.Background())
	var httpErr *agentHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("Config() error = %T %v, want *agentHTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d", httpErr.StatusCode)
	}
	if strings.Contains(httpErr.Body, token) {
		t.Fatal("safe error body exposed the access token")
	}
	if len(httpErr.Body) > maxAgentHTTPErrorBodyBytes {
		t.Fatalf("safe error body length = %d", len(httpErr.Body))
	}
	for _, char := range httpErr.Body {
		if char != '\n' && char != '\t' && (char < 0x20 || char == 0x7f || char >= 0x80 && char <= 0x9f) {
			t.Fatalf("safe error body retained control character %U", char)
		}
	}
}

func TestAgentStreamTransportDeleteSessionSendsRevisionBody(t *testing.T) {
	t.Parallel()

	const token = "delete-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/sessions/session-1" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Access-Token"); got != token {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var body struct {
			ExpectedRevision *uint64 `json:"expectedRevision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode delete body: %v", err)
		}
		if body.ExpectedRevision == nil || *body.ExpectedRevision != 7 {
			t.Errorf("expectedRevision = %v", body.ExpectedRevision)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"cleanup":{"resultCount":2,"totalBytes":17,"cleanupPending":false}}`))
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL, token, server.Client())
	if err := transport.DeleteSession(context.Background(), "session-1", 7); err != nil {
		t.Fatalf("DeleteSession(): %v", err)
	}
}

func TestAgentStreamTransportStartTurnCanBeCanceled(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseHandler) })

	ctx, cancel := context.WithCancel(context.Background())
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	stream := transport.StartTurn(ctx, agentTurnRequest{})
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("agent request did not start")
	}
	cancel()
	items := collectAgentStream(t, stream)
	if len(items) != 1 || !errors.Is(items[0].Err, context.Canceled) || items[0].EOF {
		t.Fatalf("cancel stream items = %#v", items)
	}
}

func collectAgentStream(t *testing.T, stream <-chan agentStreamItem) []agentStreamItem {
	t.Helper()
	var items []agentStreamItem
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case item, ok := <-stream:
			if !ok {
				return items
			}
			items = append(items, item)
		case <-timer.C:
			t.Fatalf("timed out waiting for agent stream; items = %#v", items)
		}
	}
}
