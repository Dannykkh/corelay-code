package kairos

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxMemorySize = 25 * 1024 // 25KB

// SafeDirName converts a workspace path to a safe directory name.
// e.g. "D:\git\projectA" → "D--git--projectA"
func SafeDirName(workspace string) string {
	safe := strings.ReplaceAll(workspace, ":", "")
	safe = strings.ReplaceAll(safe, "\\", "--")
	safe = strings.ReplaceAll(safe, "/", "--")
	return safe
}

// Memory represents the AutoDream persistent memory system.
// Each workspace/project gets its own memory directory.
type Memory struct {
	mu       sync.RWMutex
	baseDir  string // <state dir>, see config.BaseDir
	dir      string // current project memory dir
	sessions int
	lastDream time.Time
}

type MemoryEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Category  string    `json:"category"` // "project", "pattern", "gotcha", "preference"
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Source    string    `json:"source"` // "autodream", "user", "session"
}

type MemoryState struct {
	Entries      []MemoryEntry `json:"entries"`
	TotalSize    int           `json:"totalSize"`
	SessionCount int           `json:"sessionCount"`
	LastDream    time.Time     `json:"lastDream"`
}

func NewMemory(baseDir string) *Memory {
	dir := filepath.Join(baseDir, "memory")
	os.MkdirAll(dir, 0755)
	return &Memory{baseDir: baseDir, dir: dir}
}

// SwitchProject changes the memory directory to a project-specific one.
func (m *Memory) SwitchProject(workDir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if workDir == "" {
		m.dir = filepath.Join(m.baseDir, "memory")
	} else {
		m.dir = filepath.Join(m.baseDir, "projects", SafeDirName(workDir), "memory")
	}
	os.MkdirAll(m.dir, 0755)
}

// RecordSession increments session count and checks if dream should trigger.
func (m *Memory) RecordSession() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions++

	// AutoDream trigger: 5+ sessions AND 24h since last dream
	shouldDream := m.sessions >= 5 && time.Since(m.lastDream) > 24*time.Hour
	return shouldDream
}

// Dream runs the 4-phase memory consolidation.
func (m *Memory) Dream(provider interface{ Name() string }) {
	m.mu.Lock()
	defer m.mu.Unlock()

	log.Println("[AutoDream] Starting memory consolidation...")

	// Phase 1: Orient — load current state
	entries := m.loadEntries()
	log.Printf("[AutoDream] Orient: %d entries loaded", len(entries))

	// Phase 2: Gather Signal — read session logs
	logs := m.gatherSessionLogs()
	log.Printf("[AutoDream] Gather: %d log entries found", len(logs))

	// Phase 3: Consolidate — merge new knowledge
	entries = m.consolidate(entries, logs)
	log.Printf("[AutoDream] Consolidate: %d entries after merge", len(entries))

	// Phase 4: Prune — keep under 25KB
	entries = m.prune(entries)
	totalSize := m.calcSize(entries)
	log.Printf("[AutoDream] Prune: %d entries, %d bytes", len(entries), totalSize)

	// Save
	m.saveEntries(entries)
	m.sessions = 0
	m.lastDream = time.Now()
	m.saveMeta()

	log.Println("[AutoDream] Memory consolidation complete")
}

// AddEntry adds a memory entry manually.
func (m *Memory) AddEntry(entry MemoryEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries := m.loadEntries()
	entry.CreatedAt = time.Now()
	entry.UpdatedAt = time.Now()

	// Check for existing key — update instead of duplicate
	for i, e := range entries {
		if e.Key == entry.Key {
			entries[i].Value = entry.Value
			entries[i].UpdatedAt = time.Now()
			m.saveEntries(entries)
			return
		}
	}

	entries = append(entries, entry)
	m.saveEntries(entries)
}

// GetState returns current memory state.
func (m *Memory) GetState() MemoryState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := m.loadEntries()
	return MemoryState{
		Entries:      entries,
		TotalSize:    m.calcSize(entries),
		SessionCount: m.sessions,
		LastDream:    m.lastDream,
	}
}

// Search finds entries matching a query.
func (m *Memory) Search(query string) []MemoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := m.loadEntries()
	query = strings.ToLower(query)

	var results []MemoryEntry
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Key), query) ||
			strings.Contains(strings.ToLower(e.Value), query) ||
			strings.Contains(strings.ToLower(e.Category), query) {
			results = append(results, e)
		}
	}
	return results
}

// ── Internal helpers ──

func (m *Memory) loadEntries() []MemoryEntry {
	path := filepath.Join(m.dir, "entries.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []MemoryEntry
	json.Unmarshal(data, &entries)
	return entries
}

func (m *Memory) saveEntries(entries []MemoryEntry) {
	path := filepath.Join(m.dir, "entries.json")
	data, _ := json.MarshalIndent(entries, "", "  ")
	os.WriteFile(path, data, 0644)
}

func (m *Memory) saveMeta() {
	path := filepath.Join(m.dir, "meta.json")
	data, _ := json.Marshal(map[string]any{
		"sessions":  m.sessions,
		"lastDream": m.lastDream,
	})
	os.WriteFile(path, data, 0644)
}

func (m *Memory) gatherSessionLogs() []LogEntry {
	// Read from daemon logs directory
	logsDir := filepath.Join(m.dir, "..", "logs")
	files, err := os.ReadDir(logsDir)
	if err != nil {
		return nil
	}

	var allLogs []LogEntry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(logsDir, f.Name()))
		if err != nil {
			continue
		}
		var logs []LogEntry
		json.Unmarshal(data, &logs)
		allLogs = append(allLogs, logs...)
	}
	return allLogs
}

func (m *Memory) consolidate(existing []MemoryEntry, logs []LogEntry) []MemoryEntry {
	// Extract patterns from logs
	for _, l := range logs {
		if l.Action == "task-done" && l.Detail != "" {
			key := fmt.Sprintf("session_%s", l.Time.Format("2006-01-02_15-04"))
			existing = append(existing, MemoryEntry{
				Key:       key,
				Value:     l.Detail,
				Category:  "session",
				CreatedAt: l.Time,
				UpdatedAt: l.Time,
				Source:    "autodream",
			})
		}
	}
	return existing
}

func (m *Memory) prune(entries []MemoryEntry) []MemoryEntry {
	// Remove entries until under maxMemorySize
	for m.calcSize(entries) > maxMemorySize && len(entries) > 0 {
		// Remove oldest "session" entries first
		oldestIdx := -1
		var oldestTime time.Time
		for i, e := range entries {
			if e.Category == "session" && (oldestIdx == -1 || e.CreatedAt.Before(oldestTime)) {
				oldestIdx = i
				oldestTime = e.CreatedAt
			}
		}
		if oldestIdx == -1 {
			// No session entries left — remove oldest of any
			oldestIdx = 0
			for i, e := range entries {
				if e.CreatedAt.Before(entries[oldestIdx].CreatedAt) {
					oldestIdx = i
				}
			}
		}
		entries = append(entries[:oldestIdx], entries[oldestIdx+1:]...)
	}
	return entries
}

func (m *Memory) calcSize(entries []MemoryEntry) int {
	data, _ := json.Marshal(entries)
	return len(data)
}
