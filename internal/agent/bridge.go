package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

const bridgeOutputLimitBytes = 1 << 20

type bridgeRunTerminalReducer struct {
	output    strings.Builder
	truncated bool
	done      bool
	failure   string
}

func (r *bridgeRunTerminalReducer) Observe(event Event) {
	if r == nil || r.done {
		return
	}
	switch event.Type {
	case "text":
		text, _ := event.Data.(string)
		r.appendText(text)
	case "error":
		r.fail(fmt.Sprint(event.Data))
	case "context_blocked":
		r.fail(bridgeContextBlockedMessage(event.Data))
	case "done":
		r.done = true
		if terminal, ok := DecodeDurableRunTerminalMetadata(event.Data); ok && terminal.BlocksSuccess() {
			r.fail("bridge completion is blocked or incomplete")
		}
	}
}

func (r *bridgeRunTerminalReducer) Result(ctxErr error) (string, error) {
	if r == nil {
		return "", fmt.Errorf("bridge run has no terminal observer")
	}
	output := r.output.String()
	if r.truncated {
		output += "\n[output truncated]"
	}
	if r.failure != "" {
		return output, fmt.Errorf("%s", r.failure)
	}
	if r.done {
		return output, nil
	}
	if ctxErr != nil {
		return output, fmt.Errorf("bridge run ended: %s", redactSensitiveString(ctxErr.Error()))
	}
	return output, fmt.Errorf("bridge event stream closed without a done event")
}

func (r *bridgeRunTerminalReducer) appendText(value string) {
	if value == "" || r.truncated {
		return
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	remaining := bridgeOutputLimitBytes - r.output.Len()
	if remaining <= 0 {
		r.truncated = true
		return
	}
	prefix := contextUTF8Prefix(value, remaining)
	r.output.WriteString(prefix)
	r.truncated = len(prefix) < len(value)
}

func (r *bridgeRunTerminalReducer) fail(value string) {
	if r.failure != "" {
		return
	}
	r.failure = sanitizeSnapshotText(redactSensitiveString(value), 1000)
	if r.failure == "" {
		r.failure = "bridge run failed"
	}
}

func bridgeContextBlockedMessage(data any) string {
	switch value := data.(type) {
	case ContextBlockedEvent:
		return value.Message
	case *ContextBlockedEvent:
		if value != nil {
			return value.Message
		}
	}
	return "bridge context planning was blocked"
}

// Bridge provides remote control of the agent via HTTP.
// Allows external tools (IDE extensions, scripts) to send commands
// and receive results without the web UI.
type Bridge struct {
	mu       sync.RWMutex
	sessions map[string]*BridgeSession
	provider types.Provider
	model    string
	workDir  string
}

// BridgeSession is a single remote agent session.
type BridgeSession struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"` // idle, running, done
	Messages   []types.Message `json:"-"`
	LastInput  string          `json:"lastInput"`
	LastOutput string          `json:"lastOutput"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// NewBridge creates a bridge controller.
func NewBridge(provider types.Provider, model, workDir string) *Bridge {
	return &Bridge{
		sessions: make(map[string]*BridgeSession),
		provider: provider,
		model:    model,
		workDir:  workDir,
	}
}

// CreateSession starts a new bridge session.
func (b *Bridge) CreateSession() *BridgeSession {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := fmt.Sprintf("bridge_%d", time.Now().UnixMilli())
	sess := &BridgeSession{
		ID:        id,
		Status:    "idle",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	b.sessions[id] = sess
	log.Printf("[Bridge] Session created: %s", id)
	return sess
}

// Send sends a message to a bridge session and returns the response.
func (b *Bridge) Send(sessionID string, input string) (string, error) {
	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		b.mu.Unlock()
		return "", fmt.Errorf("session %s not found", sessionID)
	}
	if sess.Status == "running" {
		b.mu.Unlock()
		return "", fmt.Errorf("session %s already has an active run", sessionID)
	}
	committedMessageCount := len(sess.Messages)
	sess.Status = "running"
	sess.LastInput = input
	sess.UpdatedAt = time.Now()

	// Build messages
	userContent, _ := json.Marshal(input)
	sess.Messages = append(sess.Messages, types.Message{
		Role: "user", Content: userContent,
	})
	messages := make([]types.Message, len(sess.Messages))
	copy(messages, sess.Messages)
	b.mu.Unlock()

	// Run agent
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	eventCh := make(chan Event, 100)
	go RunLoop(ctx, b.provider, b.model, messages, b.workDir, "auto", eventCh)

	// Collect one truthful terminal outcome. A stream close is not success, and
	// a typed blocked/incomplete done frame remains an error even though its
	// partial transcript is safe to return to the caller.
	var terminal bridgeRunTerminalReducer
	for event := range eventCh {
		terminal.Observe(event)
	}
	result, runErr := terminal.Result(ctx.Err())

	b.mu.Lock()
	sess.Status = "idle"
	sess.LastOutput = result
	sess.UpdatedAt = time.Now()
	if runErr == nil {
		assistantContent, _ := json.Marshal(result)
		sess.Messages = append(sess.Messages, types.Message{
			Role: "assistant", Content: assistantContent,
		})
	} else if committedMessageCount >= 0 && committedMessageCount <= len(sess.Messages) {
		// A failed/blocked run never commits a dangling user half-turn. The next
		// Send therefore cannot accidentally replay an uncompleted request.
		sess.Messages = sess.Messages[:committedMessageCount]
	}
	b.mu.Unlock()

	return result, runErr
}

// GetSession returns a session by ID.
func (b *Bridge) GetSession(id string) *BridgeSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sessions[id]
}

// ListSessions returns all bridge sessions.
func (b *Bridge) ListSessions() []*BridgeSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]*BridgeSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		result = append(result, s)
	}
	return result
}

// CloseSession removes a bridge session.
func (b *Bridge) CloseSession(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, id)
	log.Printf("[Bridge] Session closed: %s", id)
}
