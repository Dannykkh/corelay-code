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

	"github.com/Dannykkh/corelay-code/internal/agent"
)

func TestExecutionModeCLIHelpers(t *testing.T) {
	for _, test := range []struct {
		value string
		want  agent.ExecutionMode
		bad   bool
	}{
		{value: "", want: ""},
		{value: "read-only", want: agent.ExecutionModeReadOnly},
		{value: "workspace", want: agent.ExecutionModeWorkspace},
		{value: "full", want: agent.ExecutionModeFull},
		{value: "unrestricted", bad: true},
	} {
		got, err := parseExecutionModeFlag(test.value)
		if test.bad {
			if err == nil {
				t.Errorf("parseExecutionModeFlag(%q) = %q, want error", test.value, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("parseExecutionModeFlag(%q) = %q, %v; want %q", test.value, got, err, test.want)
		}
	}

	request, err := json.Marshal(agentTurnRequest{
		ExecutionPolicy: requestedExecutionPolicy(agent.ExecutionModeFull),
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(request, &payload); err != nil {
		t.Fatal(err)
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(payload["executionPolicy"], &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy) != 1 || string(policy["mode"]) != `"full"` {
		t.Fatalf("executionPolicy = %s, want only mode=full", payload["executionPolicy"])
	}

	request, err = json.Marshal(agentTurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	if err := json.Unmarshal(request, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["executionPolicy"]; exists {
		t.Fatalf("default request unexpectedly sent executionPolicy: %s", request)
	}
}

func TestResolveCLIExecutionPolicyUsesWorkspaceDefaultAndExplicitFullRunner(t *testing.T) {
	runner, _, workspace, err := resolveCLIExecutionPolicy(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Mode != agent.ExecutionModeWorkspace || workspace.Source != "default-workspace" {
		t.Fatalf("default snapshot = %#v, want default workspace", workspace)
	}
	if runner == nil {
		t.Fatal("default runner is nil")
	}

	runner, fullPolicy, full, err := resolveCLIExecutionPolicy(t.TempDir(), agent.ExecutionModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if full.Mode != agent.ExecutionModeFull || full.Source != "user-selected" || full.FullSelectionRevision == 0 {
		t.Fatalf("full snapshot = %#v, want explicitly selected full mode", full)
	}
	if runner == nil || runner.Name() != "unconfined" || fullPolicy.Enforcement != "disabled" {
		t.Fatalf("full execution = runner %v, policy %#v; want unconfined + disabled", runner, fullPolicy)
	}
}

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

func TestAgentStreamTransportReconciliationEvidenceRoundTrip(t *testing.T) {
	t.Parallel()

	const token = "reconcile-secret"
	digest := "sha256:" + strings.Repeat("c", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Access-Token"); got != token {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/session-1/reconcile-preview":
			if got := r.URL.Query().Get("expectedRevision"); got != "12" {
				t.Errorf("preview revision = %q", got)
			}
			_, _ = fmt.Fprintf(w, `{"sessionId":"session-1","revision":12,"runId":"run-1","toolName":"Bash","evidenceDigest":%q,"checkpointStatus":"unavailable","sideEffectJudgment":"unknown","recordedExecutionState":"applied","manualConfirmationRequired":true,"manualConfirmationReason":"inspect shell effects","files":[{"path":"<checkpoint>","status":"unavailable"}],"unavailable":1}`, digest)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/session-1/reconcile":
			var body struct {
				ExpectedRevision               uint64 `json:"expectedRevision"`
				EvidenceDigest                 string `json:"evidenceDigest"`
				ManualConfirmationAcknowledged bool   `json:"manualConfirmationAcknowledged"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode reconcile request: %v", err)
			}
			if body.ExpectedRevision != 12 || body.EvidenceDigest != digest || !body.ManualConfirmationAcknowledged {
				t.Errorf("reconcile body = %+v", body)
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"revision":13,"session":{"id":"session-1","revision":13,"lifecycleStatus":"active","lastReconciliation":{"version":1,"at":"2026-09-14T00:00:00Z","runId":"run-1","evidenceDigest":%q,"checkpointStatus":"unavailable","sideEffectJudgment":"unknown","fileCount":1,"unavailable":1,"manualConfirmationRequired":true,"manualConfirmationAcknowledged":true}}}`, digest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	transport := newAgentStreamTransport(server.URL, token, server.Client())
	assessment, err := transport.ReconciliationPreview(context.Background(), "session-1", 12)
	if err != nil || assessment.EvidenceDigest != digest || !assessment.ManualConfirmationRequired {
		t.Fatalf("ReconciliationPreview() = %+v, %v", assessment, err)
	}
	session, err := transport.ReconcileSession(context.Background(), "session-1", 12, assessment.EvidenceDigest, true)
	if err != nil || session == nil || session.Revision != 13 || session.LastReconciliation == nil ||
		!session.LastReconciliation.ManualConfirmationAcknowledged {
		t.Fatalf("ReconcileSession() = %+v, %v", session, err)
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

func TestAgentStreamTransportRecoversCommittedPartialTextWithoutRepost(t *testing.T) {
	const sessionID = "recovery-session"
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			posts++
			var turn agentTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&turn); err != nil || turn.RequestID != "turn_1" {
				t.Errorf("unexpected retry body: %#v err=%v", turn, err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"text\",\"data\":\"hel\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == sessionPath(sessionID):
			_ = json.NewEncoder(w).Encode(agent.Session{
				ID: sessionID, Revision: 2,
				Messages: []agent.SessionMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
				LastAgentRequest: &agent.AgentRequestReceipt{
					ID: "turn_1", ExpectedRevision: 1, CommittedRevision: 2, MessageCount: 1,
					Kind: agent.RunTerminalCompleted,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	revision := uint64(1)
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{
		DurableSessionID: sessionID, ExpectedRevision: &revision, RequestID: "turn_1",
	}))
	if posts != 1 || len(items) != 4 || items[0].Event.Type != "text" ||
		items[1].Event.Type != "text" || items[2].Event.Type != "done" || !items[3].EOF {
		t.Fatalf("posts=%d items=%#v", posts, items)
	}
	var suffix string
	if err := json.Unmarshal(items[1].Event.Data, &suffix); err != nil || suffix != "lo" {
		t.Fatalf("recovered suffix=%q err=%v", suffix, err)
	}
}

func TestAgentStreamTransportRetriesBeforeOutputWithSameRequestID(t *testing.T) {
	const sessionID = "recovery-session"
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			posts++
			var turn agentTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&turn); err != nil || turn.RequestID != "turn_2" ||
				turn.ExpectedRevision == nil || *turn.ExpectedRevision != 1 ||
				len(turn.Messages) != 1 || turn.Messages[0].Content != "original" {
				t.Errorf("retry changed request binding: %#v err=%v", turn, err)
			}
			if posts == 2 {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"text\",\"data\":\"done\"}\n\n")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"done\",\"data\":{\"kind\":\"completed\"}}\n\n")
			}
		case r.Method == http.MethodGet && r.URL.Path == sessionPath(sessionID):
			_ = json.NewEncoder(w).Encode(agent.Session{ID: sessionID, Revision: 1})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	revision := uint64(1)
	messages := []chatMsg{{Role: "user", Content: "original"}}
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	stream := transport.StartTurn(context.Background(), agentTurnRequest{
		Messages: messages, DurableSessionID: sessionID, ExpectedRevision: &revision, RequestID: "turn_2",
	})
	messages[0].Content = "changed after StartTurn"
	items := collectAgentStream(t, stream)
	if posts != 2 || len(items) != 3 || items[0].Event.Type != "text" ||
		items[1].Event.Type != "done" || !items[2].EOF {
		t.Fatalf("posts=%d items=%#v", posts, items)
	}
}

func TestAgentStreamTransportDoesNotRepostAfterUncommittedPartialOutput(t *testing.T) {
	const sessionID = "recovery-session"
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			posts++
			_, _ = fmt.Fprint(w, "data: {\"type\":\"text\",\"data\":\"partial\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == sessionPath(sessionID):
			_ = json.NewEncoder(w).Encode(agent.Session{ID: sessionID, Revision: 1})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	revision := uint64(1)
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{
		DurableSessionID: sessionID, ExpectedRevision: &revision, RequestID: "turn_3",
	}))
	if posts != 1 || len(items) != 2 || items[0].Event.Type != "text" || !items[1].EOF {
		t.Fatalf("posts=%d items=%#v", posts, items)
	}
}

func TestAgentStreamTransportWaitsForActiveRequestReceipt(t *testing.T) {
	const sessionID = "recovery-session"
	var posts, gets int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			posts++
			if posts == 2 {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprint(w, `{"type":"error","error":{"code":"session_run_active"}}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == sessionPath(sessionID):
			gets++
			session := agent.Session{ID: sessionID, Revision: 1}
			if gets >= 2 {
				session.Revision = 2
				session.Messages = []agent.SessionMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "saved"}}
				session.LastAgentRequest = &agent.AgentRequestReceipt{
					ID: "turn_active", ExpectedRevision: 1, CommittedRevision: 2, MessageCount: 1,
					Kind: agent.RunTerminalCompleted,
				}
			}
			_ = json.NewEncoder(w).Encode(session)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	revision := uint64(1)
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{
		DurableSessionID: sessionID, ExpectedRevision: &revision, RequestID: "turn_active",
	}))
	if posts != 2 || len(items) != 3 || items[0].Event.Type != "text" ||
		items[1].Event.Type != "done" || !items[2].EOF {
		t.Fatalf("posts=%d gets=%d items=%#v", posts, gets, items)
	}
}

func TestAgentStreamTransportChecksReceiptAfterRunLeavesRegistry(t *testing.T) {
	const sessionID = "recovery-session"
	var gets int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			_, _ = fmt.Fprint(w, "data: {\"type\":\"session\",\"data\":{\"sessionId\":\"run_1\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"text\",\"data\":\"he\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == sessionPath(sessionID):
			gets++
			session := agent.Session{ID: sessionID, Revision: 1}
			if gets >= 2 {
				session.Revision = 2
				session.Messages = []agent.SessionMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
				session.LastAgentRequest = &agent.AgentRequestReceipt{
					ID: "turn_release", ExpectedRevision: 1, CommittedRevision: 2, MessageCount: 1,
					Kind: agent.RunTerminalCompleted,
				}
			}
			_ = json.NewEncoder(w).Encode(session)
		case r.Method == http.MethodGet && r.URL.Path == "/api/agent/loops":
			_, _ = fmt.Fprint(w, `{"loops":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	revision := uint64(1)
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{
		DurableSessionID: sessionID, ExpectedRevision: &revision, RequestID: "turn_release",
	}))
	if gets != 2 || len(items) != 5 || items[0].Event.Type != "session" ||
		items[1].Event.Type != "text" || items[2].Event.Type != "text" ||
		items[3].Event.Type != "done" || !items[4].EOF {
		t.Fatalf("gets=%d items=%#v", gets, items)
	}
}

func TestAgentStreamTransportRejectsMismatchedSavedText(t *testing.T) {
	const sessionID = "recovery-session"
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			posts++
			_, _ = fmt.Fprint(w, "data: {\"type\":\"text\",\"data\":\"old\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == sessionPath(sessionID):
			_ = json.NewEncoder(w).Encode(agent.Session{
				ID: sessionID, Revision: 2,
				Messages: []agent.SessionMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "different"}},
				LastAgentRequest: &agent.AgentRequestReceipt{
					ID: "turn_mismatch", ExpectedRevision: 1, CommittedRevision: 2, MessageCount: 1,
					Kind: agent.RunTerminalCompleted,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	revision := uint64(1)
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	items := collectAgentStream(t, transport.StartTurn(context.Background(), agentTurnRequest{
		DurableSessionID: sessionID, ExpectedRevision: &revision, RequestID: "turn_mismatch",
	}))
	if posts != 1 || len(items) != 2 || items[0].Event.Type != "text" || !items[1].EOF {
		t.Fatalf("posts=%d items=%#v", posts, items)
	}
}

func TestAgentStreamTransportCancellationStopsRecovery(t *testing.T) {
	getStarted := make(chan struct{})
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
			posts++
		case r.Method == http.MethodGet && r.URL.Path == sessionPath("cancel-session"):
			close(getStarted)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	revision := uint64(1)
	transport := newAgentStreamTransport(server.URL, "", server.Client())
	stream := transport.StartTurn(ctx, agentTurnRequest{
		DurableSessionID: "cancel-session", ExpectedRevision: &revision, RequestID: "cancel_recovery",
	})
	select {
	case <-getStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not inspect the session")
	}
	cancel()
	items := collectAgentStream(t, stream)
	if posts != 1 || len(items) != 1 || !errors.Is(items[0].Err, context.Canceled) {
		t.Fatalf("posts=%d items=%#v", posts, items)
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
	revision := uint64(1)
	stream := transport.StartTurn(ctx, agentTurnRequest{
		DurableSessionID: "cancel-session", ExpectedRevision: &revision, RequestID: "cancel_turn",
	})
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
