package observability

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RequestTrace records a single LLM API request.
type RequestTrace struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	LatencyMs    int64     `json:"latencyMs"`
	InputTokens  int       `json:"inputTokens"`
	OutputTokens int       `json:"outputTokens"`
	Cost         float64   `json:"cost"`
	Status       string    `json:"status"` // "ok", "error"
	Error        string    `json:"error,omitempty"`
	Source       string    `json:"source"` // "web", "claude", "codex", "api"
	WorkDir      string    `json:"workDir,omitempty"`
}

// RunTrace records one agentic loop run and its internal spans.
type RunTrace struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"` // agent, chronos, kairos
	StartedAt    time.Time         `json:"startedAt"`
	EndedAt      time.Time         `json:"endedAt,omitempty"`
	DurationMs   int64             `json:"durationMs,omitempty"`
	Provider     string            `json:"provider"`
	Model        string            `json:"model"`
	WorkDir      string            `json:"workDir,omitempty"`
	WorkstreamID string            `json:"workstreamId,omitempty"`
	Status       string            `json:"status"` // running, ok, failed
	Error        string            `json:"error,omitempty"`
	Spans        []RunSpan         `json:"spans,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// RunSpan records a bounded phase inside an agentic loop.
type RunSpan struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	StartedAt  time.Time         `json:"startedAt"`
	EndedAt    time.Time         `json:"endedAt,omitempty"`
	DurationMs int64             `json:"durationMs,omitempty"`
	Status     string            `json:"status"` // running, ok, failed, skipped
	Data       map[string]string `json:"data,omitempty"`
}

// Metrics is a computed summary over a time window.
type Metrics struct {
	TotalRequests  int                         `json:"totalRequests"`
	TotalTokens    int                         `json:"totalTokens"`
	TotalCost      float64                     `json:"totalCost"`
	AvgLatencyMs   int64                       `json:"avgLatencyMs"`
	P95LatencyMs   int64                       `json:"p95LatencyMs"`
	ErrorRate      float64                     `json:"errorRate"`
	RequestsPerMin float64                     `json:"requestsPerMin"`
	ByProvider     map[string]*ProviderMetrics `json:"byProvider"`
}

// ProviderMetrics is per-provider breakdown.
type ProviderMetrics struct {
	Requests     int     `json:"requests"`
	Tokens       int     `json:"tokens"`
	Cost         float64 `json:"cost"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	Errors       int     `json:"errors"`
}

// Tracker collects and queries request traces.
type Tracker struct {
	mu        sync.RWMutex
	traces    []RequestTrace
	runTraces []RunTrace
	dir       string // persistence directory
	runDir    string
}

func NewTracker(baseDir string) *Tracker {
	dir := filepath.Join(baseDir, "traces")
	runDir := filepath.Join(baseDir, "run-traces")
	os.MkdirAll(dir, 0755)
	os.MkdirAll(runDir, 0755)
	t := &Tracker{
		traces:    make([]RequestTrace, 0, 10000),
		runTraces: make([]RunTrace, 0, 1000),
		dir:       dir,
		runDir:    runDir,
	}
	t.loadToday()
	t.loadRunToday()
	return t
}

// Record adds a new trace.
func (t *Tracker) Record(trace RequestTrace) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.traces = append(t.traces, trace)

	// Keep in-memory limit
	if len(t.traces) > 10000 {
		t.traces = t.traces[len(t.traces)-5000:]
	}

	// Append to daily file
	t.appendToFile(trace)
}

// RecordRun adds a completed run trace.
func (t *Tracker) RecordRun(trace RunTrace) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if trace.ID == "" {
		trace.ID = NewTraceID("run")
	}
	if trace.StartedAt.IsZero() {
		trace.StartedAt = time.Now().UTC()
	}
	if trace.EndedAt.IsZero() {
		trace.EndedAt = time.Now().UTC()
	}
	if trace.DurationMs == 0 {
		trace.DurationMs = trace.EndedAt.Sub(trace.StartedAt).Milliseconds()
	}
	if trace.Status == "" {
		trace.Status = "ok"
	}
	t.runTraces = append(t.runTraces, trace)
	if len(t.runTraces) > 1000 {
		t.runTraces = t.runTraces[len(t.runTraces)-500:]
	}
	t.appendRunToFile(trace)
}

// Recent returns the last N traces.
func (t *Tracker) Recent(limit int) []RequestTrace {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if limit <= 0 || limit > len(t.traces) {
		limit = len(t.traces)
	}
	start := len(t.traces) - limit
	result := make([]RequestTrace, limit)
	copy(result, t.traces[start:])
	return result
}

// RecentRuns returns the last N agentic run traces.
func (t *Tracker) RecentRuns(limit int) []RunTrace {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if limit <= 0 || limit > len(t.runTraces) {
		limit = len(t.runTraces)
	}
	start := len(t.runTraces) - limit
	result := make([]RunTrace, limit)
	copy(result, t.runTraces[start:])
	return result
}

// Compute calculates metrics over recent traces.
func (t *Tracker) Compute(windowMinutes int) Metrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	cutoff := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
	m := Metrics{
		ByProvider: make(map[string]*ProviderMetrics),
	}

	var totalLatency int64
	var latencies []int64
	var errors int

	for _, tr := range t.traces {
		if tr.Timestamp.Before(cutoff) {
			continue
		}
		m.TotalRequests++
		m.TotalTokens += tr.InputTokens + tr.OutputTokens
		m.TotalCost += tr.Cost
		totalLatency += tr.LatencyMs
		latencies = append(latencies, tr.LatencyMs)

		if tr.Status == "error" {
			errors++
		}

		pm, ok := m.ByProvider[tr.Provider]
		if !ok {
			pm = &ProviderMetrics{}
			m.ByProvider[tr.Provider] = pm
		}
		pm.Requests++
		pm.Tokens += tr.InputTokens + tr.OutputTokens
		pm.Cost += tr.Cost
		pm.AvgLatencyMs += tr.LatencyMs
		if tr.Status == "error" {
			pm.Errors++
		}
	}

	if m.TotalRequests > 0 {
		m.AvgLatencyMs = totalLatency / int64(m.TotalRequests)
		m.ErrorRate = float64(errors) / float64(m.TotalRequests)
		elapsed := float64(windowMinutes)
		if elapsed > 0 {
			m.RequestsPerMin = float64(m.TotalRequests) / elapsed
		}

		// P95
		if len(latencies) > 0 {
			sortInt64s(latencies)
			idx := int(float64(len(latencies)) * 0.95)
			if idx >= len(latencies) {
				idx = len(latencies) - 1
			}
			m.P95LatencyMs = latencies[idx]
		}

		// Provider averages
		for _, pm := range m.ByProvider {
			if pm.Requests > 0 {
				pm.AvgLatencyMs = pm.AvgLatencyMs / int64(pm.Requests)
			}
		}
	}

	return m
}

// ── Persistence ──

func (t *Tracker) todayFile() string {
	return filepath.Join(t.dir, time.Now().Format("2006-01-02")+".jsonl")
}

func (t *Tracker) runTodayFile() string {
	return filepath.Join(t.runDir, time.Now().Format("2006-01-02")+".jsonl")
}

func (t *Tracker) appendToFile(trace RequestTrace) {
	f, err := os.OpenFile(t.todayFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(trace)
	f.Write(data)
	f.Write([]byte("\n"))
}

func (t *Tracker) appendRunToFile(trace RunTrace) {
	f, err := os.OpenFile(t.runTodayFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(trace)
	f.Write(data)
	f.Write([]byte("\n"))
}

func (t *Tracker) loadToday() {
	data, err := os.ReadFile(t.todayFile())
	if err != nil {
		return
	}
	for _, line := range splitLines(data) {
		var trace RequestTrace
		if json.Unmarshal(line, &trace) == nil {
			t.traces = append(t.traces, trace)
		}
	}
}

func (t *Tracker) loadRunToday() {
	data, err := os.ReadFile(t.runTodayFile())
	if err != nil {
		return
	}
	for _, line := range splitLines(data) {
		var trace RunTrace
		if json.Unmarshal(line, &trace) == nil {
			t.runTraces = append(t.runTraces, trace)
		}
	}
}

func NewTraceID(prefix string) string {
	if prefix == "" {
		prefix = "trace"
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func sortInt64s(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
