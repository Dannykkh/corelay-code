package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MessageType defines structured message categories.
type MessageType string

const (
	MsgText             MessageType = "text"
	MsgResult           MessageType = "result"
	MsgShutdownRequest  MessageType = "shutdown_request"
	MsgShutdownResponse MessageType = "shutdown_response"
	MsgStatus           MessageType = "status"
	MsgPermission       MessageType = "permission"
	MsgIdleNotify       MessageType = "idle_notify"
)

// TeamMessage represents an inter-agent message with structured types.
type TeamMessage struct {
	ID        string      `json:"id"`
	From      string      `json:"from"`
	To        string      `json:"to"`      // agent ID or "*" for broadcast
	Text      string      `json:"text"`
	Type      MessageType `json:"type"`
	Read      bool        `json:"read"`
	Summary   string      `json:"summary,omitempty"` // 5-10 word preview
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data,omitempty"` // structured payload
}

// Mailbox manages message passing between agents with file-based persistence.
type Mailbox struct {
	mu       sync.RWMutex
	dir      string // <state dir>/projects/<safe>/mailbox/
	nextID   int
}

// NewMailbox creates a mailbox with persistence directory.
func NewMailbox(baseDir string) *Mailbox {
	dir := filepath.Join(baseDir, "mailbox")
	os.MkdirAll(dir, 0755)
	return &Mailbox{dir: dir}
}

// Send delivers a message to a specific agent's inbox file.
func (m *Mailbox) Send(msg TeamMessage) TeamMessage {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextID++
	msg.ID = fmt.Sprintf("msg_%d_%d", time.Now().UnixMilli(), m.nextID)
	msg.Timestamp = time.Now()

	if msg.To == "*" {
		// Broadcast: write to all inboxes
		entries, _ := os.ReadDir(m.dir)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			agentName := e.Name()[:len(e.Name())-5] // strip .json
			if agentName != msg.From {
				m.appendToInbox(agentName, msg)
			}
		}
	} else {
		m.appendToInbox(msg.To, msg)
	}
	return msg
}

// Receive returns unread messages for an agent and marks them as read.
func (m *Mailbox) Receive(agentID string) []TeamMessage {
	m.mu.Lock()
	defer m.mu.Unlock()

	messages := m.loadInbox(agentID)
	var unread []TeamMessage

	changed := false
	for i, msg := range messages {
		if !msg.Read {
			unread = append(unread, msg)
			messages[i].Read = true
			changed = true
		}
	}

	if changed {
		m.saveInbox(agentID, messages)
	}
	return unread
}

// Peek returns unread messages without marking them as read.
func (m *Mailbox) Peek(agentID string) []TeamMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()

	messages := m.loadInbox(agentID)
	var unread []TeamMessage
	for _, msg := range messages {
		if !msg.Read {
			unread = append(unread, msg)
		}
	}
	return unread
}

// AllMessages returns all messages for debugging/UI.
func (m *Mailbox) AllMessages() []TeamMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []TeamMessage
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		agentName := e.Name()[:len(e.Name())-5]
		all = append(all, m.loadInbox(agentName)...)
	}
	return all
}

// Clear removes all messages for an agent.
func (m *Mailbox) Clear(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	os.Remove(filepath.Join(m.dir, agentID+".json"))
}

// ClearAll removes all mailbox files.
func (m *Mailbox) ClearAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		os.Remove(filepath.Join(m.dir, e.Name()))
	}
}

// EnsureInbox creates an empty inbox file for an agent (for broadcast discovery).
func (m *Mailbox) EnsureInbox(agentID string) {
	path := filepath.Join(m.dir, agentID+".json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.WriteFile(path, []byte("[]"), 0644)
	}
}

// ── File I/O with simple file-level locking ──

func (m *Mailbox) appendToInbox(agentID string, msg TeamMessage) {
	messages := m.loadInbox(agentID)

	// Collapse idle notifications (keep only latest per sender)
	if msg.Type == MsgIdleNotify {
		filtered := make([]TeamMessage, 0, len(messages))
		for _, existing := range messages {
			if !(existing.Type == MsgIdleNotify && existing.From == msg.From) {
				filtered = append(filtered, existing)
			}
		}
		messages = filtered
	}

	messages = append(messages, msg)
	m.saveInbox(agentID, messages)
}

func (m *Mailbox) loadInbox(agentID string) []TeamMessage {
	path := filepath.Join(m.dir, agentID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var messages []TeamMessage
	json.Unmarshal(data, &messages)
	return messages
}

func (m *Mailbox) saveInbox(agentID string, messages []TeamMessage) {
	path := filepath.Join(m.dir, agentID+".json")
	data, _ := json.MarshalIndent(messages, "", "  ")

	// File lock for cross-process safety
	lockPath := path + ".lock"
	m.acquireFileLock(lockPath)
	defer m.releaseFileLock(lockPath)

	// Atomic write: write to temp then rename
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		os.WriteFile(path, data, 0644) // fallback
		return
	}
	os.Rename(tmp, path)
}

func (m *Mailbox) loadInboxLocked(agentID string) []TeamMessage {
	path := filepath.Join(m.dir, agentID+".json")
	lockPath := path + ".lock"
	m.acquireFileLock(lockPath)
	defer m.releaseFileLock(lockPath)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var messages []TeamMessage
	json.Unmarshal(data, &messages)
	return messages
}

// acquireFileLock creates a lock file with retries.
func (m *Mailbox) acquireFileLock(lockPath string) {
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return
		}
		// Check if lock is stale (older than 5 seconds)
		if info, statErr := os.Stat(lockPath); statErr == nil {
			if time.Since(info.ModTime()) > 5*time.Second {
				os.Remove(lockPath)
				continue
			}
		}
		time.Sleep(time.Duration(5+i*10) * time.Millisecond)
	}
	// Give up — proceed without lock (better than deadlock)
	os.Remove(lockPath)
}

// releaseFileLock removes the lock file.
func (m *Mailbox) releaseFileLock(lockPath string) {
	os.Remove(lockPath)
}
