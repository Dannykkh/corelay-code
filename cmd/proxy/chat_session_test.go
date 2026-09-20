package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

// newDurableChatStubServer gives focused stream tests the durable session API
// required by the production chat path while leaving their tested handlers
// responsible for the endpoints under test.
func newDurableChatStubServer(t *testing.T, next http.Handler) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var current *agent.Session
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			var request struct {
				agent.Session
				ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid session", http.StatusBadRequest)
				return
			}
			mu.Lock()
			if request.Session.ID == "" {
				if request.ExpectedRevision != nil || current != nil {
					mu.Unlock()
					http.Error(w, "unexpected session create", http.StatusConflict)
					return
				}
				request.Session.ID = "test-chat-session"
				request.Session.Version = 1
				request.Session.Revision = 1
			} else {
				if current == nil || request.Session.ID != current.ID || request.ExpectedRevision == nil || *request.ExpectedRevision != current.Revision {
					mu.Unlock()
					http.Error(w, "stale session revision", http.StatusConflict)
					return
				}
				request.Session.Version = current.Version
				request.Session.Revision = current.Revision + 1
			}
			current = cloneTUISession(&request.Session)
			result := sessionSaveResult{ID: current.ID, Version: current.Version, Revision: current.Revision}
			mu.Unlock()
			writeChatJSON(w, result)
		case r.Method == http.MethodGet && r.URL.Path == sessionPath("test-chat-session"):
			mu.Lock()
			session := cloneTUISession(current)
			mu.Unlock()
			if session == nil {
				http.NotFound(w, r)
				return
			}
			writeChatJSON(w, session)
		default:
			next.ServeHTTP(w, r)
		}
	}))
}

func TestChatSessionSubprocessHelper(t *testing.T) {
	if os.Getenv("CORELAY_CHAT_TEST_CHILD") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("CORELAY_CHAT_TEST_ARGS")), &args); err != nil {
		t.Fatalf("decode child args: %v", err)
	}
	if os.Getenv("CORELAY_MANAGED_SERVER_TEST_MODE") == "1" {
		launchManagedChatServerChild = startManagedServerTestChild
	}
	runChat(args)
	// A real chat executable exits without the Go test runner printing a PASS
	// trailer. Keep subprocess stdout available for machine-output assertions.
	os.Exit(0)
}

func TestChatOneShotPlainResumeAndForkUseDurableSessionContract(t *testing.T) {
	workspace := t.TempDir()
	fixture := &chatSessionAPIFixture{
		workspace: workspace,
		sessions:  make(map[string]*agent.Session),
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	firstOutput := runChatSubprocess(t, []string{
		"-url", server.URL, "-workdir", workspace, "-quiet", "-p", "first one-shot turn",
	}, "")
	if !strings.Contains(firstOutput, "Durable session: session-1") {
		t.Fatalf("one-shot did not expose its durable session ID: %s", firstOutput)
	}

	secondOutput := runChatSubprocess(t, []string{
		"-url", server.URL, "-session", "session-1", "-quiet", "-p", "resume through one-shot",
	}, "")
	if !strings.Contains(secondOutput, "reply-2") {
		t.Fatalf("one-shot resume did not complete: %s", secondOutput)
	}

	plainOutput := runChatSubprocess(t, []string{
		"-url", server.URL, "-session", "session-1", "-plain", "-quiet",
	}, "resume through plain mode\nexit\n")
	if !strings.Contains(plainOutput, "reply-3") {
		t.Fatalf("plain session resume did not complete: %s", plainOutput)
	}

	forkOutput := runChatSubprocess(t, []string{
		"-url", server.URL, "-session", "session-1", "-fork", "-quiet", "-p", "forked one-shot",
	}, "")
	if !strings.Contains(forkOutput, "Durable session: session-fork-1") {
		t.Fatalf("one-shot fork did not expose the child session: %s", forkOutput)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.failure != "" {
		t.Fatal(fixture.failure)
	}
	if len(fixture.turns) != 4 {
		t.Fatalf("durable agent turns = %d, want 4: %+v", len(fixture.turns), fixture.turns)
	}
	wantPrompts := []string{
		"first one-shot turn", "resume through one-shot", "resume through plain mode", "forked one-shot",
	}
	wantIDs := []string{"session-1", "session-1", "session-1", "session-fork-1"}
	wantRevisions := []uint64{1, 3, 5, 2}
	for i, turn := range fixture.turns {
		if turn.sessionID != wantIDs[i] || turn.revision != wantRevisions[i] || turn.workDir != workspace {
			t.Fatalf("turn %d durable binding = %+v, want session=%s revision=%d workspace=%s", i+1, turn, wantIDs[i], wantRevisions[i], workspace)
		}
		if len(turn.messages) == 0 || turn.messages[len(turn.messages)-1] != (chatMsg{Role: "user", Content: wantPrompts[i]}) {
			t.Fatalf("turn %d latest prompt = %#v, want %q", i+1, turn.messages, wantPrompts[i])
		}
	}
	forked := fixture.sessions["session-fork-1"]
	if forked == nil || forked.ParentSessionID != "session-1" || forked.ParentRevision != 6 {
		t.Fatalf("fork lineage = %+v, want parent session-1 revision 6", forked)
	}
}

func TestChatRejectsExplicitWorkspaceMismatchBeforeSessionMutation(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	fixture := &chatSessionAPIFixture{
		workspace: workspaceA,
		sessions: map[string]*agent.Session{
			"session-parent": {
				ID: "session-parent", Version: 1, Revision: 9, Workspace: workspaceB,
				LifecycleStatus: agent.SessionLifecycleActive,
				Messages:        []agent.SessionMessage{{Role: "assistant", Content: "existing"}},
			},
		},
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{
			name: "one-shot resume",
			args: []string{"-url", server.URL, "-workdir", workspaceA, "-session", "session-parent", "-quiet", "-p", "must not append"},
		},
		{
			name:  "plain resume",
			args:  []string{"-url", server.URL, "-workdir", workspaceA, "-session", "session-parent", "-plain", "-quiet"},
			stdin: "must not append\nexit\n",
		},
		{
			name: "fork preflight",
			args: []string{"-url", server.URL, "-workdir", workspaceA, "-session", "session-parent", "-fork", "-quiet", "-p", "must not fork"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := runChatSubprocessResult(t, test.args, test.stdin)
			if test.name == "plain resume" {
				if err != nil {
					t.Fatalf("plain client exit = %v, output = %s", err, output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("one-shot error = %v, output = %s; want exit 1", err, output)
				}
			}
			if !strings.Contains(output, "workspace conflict") {
				t.Fatalf("output = %q, want clear workspace conflict", output)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			parent := fixture.sessions["session-parent"]
			if parent == nil || parent.Revision != 9 || len(parent.Messages) != 1 || len(fixture.sessions) != 1 || len(fixture.turns) != 0 || fixture.forkN != 0 {
				t.Fatalf("workspace preflight mutated session state: parent=%+v sessions=%d turns=%d forks=%d", parent, len(fixture.sessions), len(fixture.turns), fixture.forkN)
			}
		})
	}
}

func TestChatReloadsSessionAfterCASConflictBeforeNextTurn(t *testing.T) {
	workspace := t.TempDir()
	current := &agent.Session{
		ID: "session-race", Version: 1, Revision: 1, Workspace: workspace,
		LifecycleStatus: agent.SessionLifecycleActive,
		Messages:        []agent.SessionMessage{{Role: "user", Content: "earlier"}},
	}
	getCalls := 0
	var saveRevisions []uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == sessionPath("session-race"):
			getCalls++
			writeChatJSON(w, current)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			var request struct {
				agent.Session
				ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode session save: %v", err)
				http.Error(w, "invalid save", http.StatusBadRequest)
				return
			}
			if request.ExpectedRevision == nil {
				t.Error("session save omitted expected revision")
				http.Error(w, "missing expected revision", http.StatusBadRequest)
				return
			}
			saveRevisions = append(saveRevisions, *request.ExpectedRevision)
			if len(saveRevisions) == 1 {
				current.Revision = 2 // Simulate another client winning the first CAS.
				current.Messages = append(current.Messages, agent.SessionMessage{Role: "assistant", Content: "other client"})
				http.Error(w, "stale session revision", http.StatusConflict)
				return
			}
			if len(request.Messages) != 3 || request.Messages[1].Content != "other client" || request.Messages[2].Content != "retry" {
				t.Errorf("retry transcript = %#v, want canonical concurrent update followed by retry", request.Messages)
				http.Error(w, "stale transcript", http.StatusConflict)
				return
			}
			current = cloneTUISession(&request.Session)
			current.Revision = 3
			writeChatJSON(w, sessionSaveResult{ID: current.ID, Version: current.Version, Revision: current.Revision})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &chatClient{
		base:      server.URL,
		http:      server.Client(),
		workDir:   workspace,
		sessionID: "session-race",
	}
	transport := newAgentStreamTransport(client.base, "", client.http)
	if _, _, err := client.prepareDurableTurn(context.Background(), transport, "first try"); err == nil || !isChatHTTPStatus(err, http.StatusConflict) {
		t.Fatalf("first durable append error = %v, want HTTP 409", err)
	}
	if client.session != nil {
		t.Fatalf("conflicted session cache = %+v, want invalidated", client.session)
	}
	session, revision, err := client.prepareDurableTurn(context.Background(), transport, "retry")
	if err != nil {
		t.Fatalf("retry durable append: %v", err)
	}
	if getCalls != 2 || len(saveRevisions) != 2 || saveRevisions[0] != 1 || saveRevisions[1] != 2 {
		t.Fatalf("reload/CAS sequence = gets %d saves %v, want gets=2 revisions=[1 2]", getCalls, saveRevisions)
	}
	if session == nil || revision != 3 || session.Revision != 3 || len(session.Messages) != 3 || session.Messages[1].Content != "other client" || session.Messages[2].Content != "retry" {
		t.Fatalf("retried session = %+v revision=%d, want canonical revision 3 with concurrent update", session, revision)
	}
}

type chatDurableTurnRecord struct {
	sessionID string
	revision  uint64
	workDir   string
	messages  []chatMsg
}

type chatSessionAPIFixture struct {
	mu        sync.Mutex
	workspace string
	sessions  map[string]*agent.Session
	createN   int
	forkN     int
	turnN     int
	turns     []chatDurableTurnRecord
	failure   string
}

func (f *chatSessionAPIFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/config":
		writeChatJSON(w, map[string]any{"provider": "fixture", "model": "fixture-model"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
		f.saveSession(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/sessions/"):
		f.getSession(w, strings.TrimPrefix(r.URL.Path, "/api/sessions/"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork"):
		f.forkSession(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/agent":
		f.startAgentTurn(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *chatSessionAPIFixture) saveSession(w http.ResponseWriter, r *http.Request) {
	var request struct {
		agent.Session
		ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid session", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.Session.ID == "" {
		if request.ExpectedRevision != nil {
			http.Error(w, "unexpected create revision", http.StatusBadRequest)
			return
		}
		f.createN++
		request.Session.ID = fmt.Sprintf("session-%d", f.createN)
		request.Session.Workspace = f.workspace
		request.Session.Provider = "fixture"
		request.Session.Model = "fixture-model"
		request.Session.Version = 1
		request.Session.Revision = 1
	} else {
		current := f.sessions[request.Session.ID]
		if current == nil || request.ExpectedRevision == nil || *request.ExpectedRevision != current.Revision {
			http.Error(w, "stale session revision", http.StatusConflict)
			return
		}
		if request.Session.Workspace != current.Workspace {
			http.Error(w, "workspace mismatch", http.StatusConflict)
			return
		}
		request.Session.Version = current.Version
		request.Session.Revision = current.Revision + 1
	}
	if request.Session.CreatedAt.IsZero() {
		request.Session.CreatedAt = time.Now().UTC()
	}
	request.Session.UpdatedAt = time.Now().UTC()
	f.sessions[request.Session.ID] = cloneTUISession(&request.Session)
	writeChatJSON(w, sessionSaveResult{ID: request.Session.ID, Version: request.Session.Version, Revision: request.Session.Revision})
}

func (f *chatSessionAPIFixture) getSession(w http.ResponseWriter, id string) {
	f.mu.Lock()
	session := cloneTUISession(f.sessions[id])
	f.mu.Unlock()
	if session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeChatJSON(w, session)
}

func (f *chatSessionAPIFixture) forkSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/fork")
	var request struct {
		ExpectedRevision uint64 `json:"expectedRevision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid fork", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.sessions[id]
	if parent == nil || request.ExpectedRevision != parent.Revision {
		http.Error(w, "stale fork revision", http.StatusConflict)
		return
	}
	f.forkN++
	forked := cloneTUISession(parent)
	forked.ID = fmt.Sprintf("session-fork-%d", f.forkN)
	forked.ParentSessionID = parent.ID
	forked.ParentRevision = parent.Revision
	forked.Revision = 1
	forked.Version = 1
	forked.CreatedAt = time.Now().UTC()
	forked.UpdatedAt = forked.CreatedAt
	f.sessions[forked.ID] = forked
	writeChatJSON(w, map[string]any{"session": forked})
}

func (f *chatSessionAPIFixture) startAgentTurn(w http.ResponseWriter, r *http.Request) {
	var request agentTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid agent turn", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	session := f.sessions[request.DurableSessionID]
	if session == nil || request.ExpectedRevision == nil || *request.ExpectedRevision != session.Revision {
		f.failure = fmt.Sprintf("agent durable binding = id %q revision %v", request.DurableSessionID, request.ExpectedRevision)
		f.mu.Unlock()
		http.Error(w, "invalid durable binding", http.StatusConflict)
		return
	}
	wantMessages := wireMessagesFromSession(session.Messages)
	if request.WorkDir != session.Workspace || !reflect.DeepEqual(request.Messages, wantMessages) {
		f.failure = fmt.Sprintf("agent run used wrong workspace/transcript: workdir=%q messages=%#v want=%#v", request.WorkDir, request.Messages, wantMessages)
		f.mu.Unlock()
		http.Error(w, "session transcript mismatch", http.StatusConflict)
		return
	}
	f.turnN++
	turnNumber := f.turnN
	f.turns = append(f.turns, chatDurableTurnRecord{
		sessionID: request.DurableSessionID,
		revision:  *request.ExpectedRevision,
		workDir:   request.WorkDir,
		messages:  append([]chatMsg(nil), request.Messages...),
	})
	reply := fmt.Sprintf("reply-%d", turnNumber)
	session.Messages = append(session.Messages, agent.SessionMessage{Role: "assistant", Content: reply, Timestamp: time.Now().UTC()})
	session.Revision++
	session.UpdatedAt = time.Now().UTC()
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	writeChatSSEEvent(w, "session", map[string]any{"sessionId": fmt.Sprintf("run-%d", turnNumber)})
	writeChatSSEEvent(w, "text", reply)
	writeChatSSEEvent(w, "done", map[string]any{"iterations": 1, "stopReason": "end_turn", "terminalState": "unverified"})
	_, _ = fmt.Fprint(w, "data: {\"type\":\"stream_end\"}\n\n")
}

func writeChatJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeChatSSEEvent(w http.ResponseWriter, eventType string, data any) {
	payload, _ := json.Marshal(map[string]any{"type": eventType, "data": data})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}

func runChatSubprocess(t *testing.T, args []string, stdin string) string {
	t.Helper()
	output, err := runChatSubprocessResult(t, args, stdin)
	if err != nil {
		t.Fatalf("chat subprocess failed: %v\n%s", err, output)
	}
	return output
}

func runChatSubprocessResult(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	return runChatSubprocessEnvResult(t, args, stdin, nil)
}

func runChatSubprocessEnvResult(t *testing.T, args []string, stdin string, envOverrides map[string]string) (string, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	command := exec.Command(os.Args[0], "-test.run=^TestChatSessionSubprocessHelper$")
	overrides := map[string]string{
		"CORELAY_CHAT_TEST_CHILD": "1",
		"CORELAY_CHAT_TEST_ARGS":  string(encoded),
		"CORELAY_CONFIG_DIR":      t.TempDir(),
		"CORELAY_ACCESS_TOKEN":    "",
		"ANICLEW_ACCESS_TOKEN":    "",
		"ANICLEW_CONFIG_DIR":      "",
	}
	for key, value := range envOverrides {
		overrides[key] = value
	}
	command.Env = replaceProcessEnv(os.Environ(), overrides)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}
