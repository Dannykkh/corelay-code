package kairos

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aniclew/aniclew/internal/observability"
	"github.com/aniclew/aniclew/internal/types"
	"github.com/aniclew/aniclew/internal/workstream"
)

// State represents what the daemon is doing.
type State string

const (
	StateIdle     State = "idle"
	StateSleeping State = "sleeping"
	StateWorking  State = "working"
	StateWaiting  State = "waiting"
)

// Task represents a background task the daemon can execute.
type Task struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"` // "pr-review", "test-run", "lint", "custom"
	Description  string    `json:"description"`
	Command      string    `json:"command,omitempty"`
	CronExpr     string    `json:"cron,omitempty"` // "0 0 * * *" = midnight
	WorkstreamID string    `json:"workstreamId,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	LastRun      time.Time `json:"lastRun,omitempty"`
	Enabled      bool      `json:"enabled"`
}

// LogEntry records what the daemon did.
type LogEntry struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
	Cost   float64   `json:"cost,omitempty"`
}

// Config for the daemon.
type DaemonConfig struct {
	Enabled        bool          `json:"enabled"`
	TickInterval   time.Duration `json:"tickInterval"`   // how often to wake up
	BlockingBudget time.Duration `json:"blockingBudget"` // max time for a single action
	CacheExpiry    time.Duration `json:"cacheExpiry"`    // prompt cache TTL
	Autonomy       string        `json:"autonomy"`       // "collaborative", "autonomous", "night"
}

func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{
		Enabled:        false,
		TickInterval:   2 * time.Minute,
		BlockingBudget: 15 * time.Second,
		CacheExpiry:    5 * time.Minute,
		Autonomy:       "collaborative",
	}
}

// Daemon is the KAIROS always-on background agent.
type Daemon struct {
	mu            sync.RWMutex
	config        DaemonConfig
	state         State
	tasks         []Task
	logs          []LogEntry
	provider      types.Provider
	model         string
	workDir       string // current project directory
	baseDir       string // ~/.claude-proxy/
	cancel        context.CancelFunc
	lastGitStatus *GitStatus
	notifier      *Notifier
	tracker       *observability.Tracker
}

func NewDaemon(cfg DaemonConfig) *Daemon {
	return &Daemon{
		config:   cfg,
		state:    StateIdle,
		logs:     make([]LogEntry, 0, 1000),
		notifier: NewNotifier(),
	}
}

// Notifier returns the daemon's notifier.
func (d *Daemon) Notifier() *Notifier {
	return d.notifier
}

// SetBaseDir sets the base storage directory.
func (d *Daemon) SetBaseDir(baseDir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.baseDir = baseDir
}

// SwitchProject changes the daemon to work on a specific project.
func (d *Daemon) SwitchProject(workDir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.workDir = workDir
	// Load project-specific tasks
	d.tasks = d.loadTasks()
}

func (d *Daemon) projectDir() string {
	if d.workDir == "" || d.baseDir == "" {
		return ""
	}
	dir := filepath.Join(d.baseDir, "projects", SafeDirName(d.workDir))
	os.MkdirAll(dir, 0755)
	return dir
}

func (d *Daemon) loadTasks() []Task {
	dir := d.projectDir()
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		return nil
	}
	var tasks []Task
	json.Unmarshal(data, &tasks)
	return tasks
}

func (d *Daemon) saveTasks() {
	dir := d.projectDir()
	if dir == "" {
		return
	}
	data, _ := json.MarshalIndent(d.tasks, "", "  ")
	os.WriteFile(filepath.Join(dir, "tasks.json"), data, 0644)
}

func (d *Daemon) SetProvider(p types.Provider, model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.provider = p
	d.model = model
}

func (d *Daemon) SetTracker(t *observability.Tracker) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tracker = t
}

// Start begins the tick loop.
func (d *Daemon) Start() {
	d.mu.Lock()
	if d.cancel != nil {
		d.mu.Unlock()
		return // already running
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.config.Enabled = true
	d.mu.Unlock()

	d.addLog("daemon-start", "KAIROS daemon started")
	log.Println("[KAIROS] Daemon started")

	go d.tickLoop(ctx)
}

// Stop halts the daemon.
func (d *Daemon) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.config.Enabled = false
	d.state = StateIdle
	d.addLogLocked("daemon-stop", "KAIROS daemon stopped")
	log.Println("[KAIROS] Daemon stopped")
}

// tickLoop is the core KAIROS pattern.
func (d *Daemon) tickLoop(ctx context.Context) {
	ticker := time.NewTicker(d.config.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.onTick(ctx, now)
		}
	}
}

// onTick is called on each wake-up.
func (d *Daemon) onTick(ctx context.Context, now time.Time) {
	d.mu.RLock()
	tasks := d.activeTasks()
	autonomy := d.config.Autonomy
	budget := d.config.BlockingBudget
	d.mu.RUnlock()

	if len(tasks) == 0 {
		// Nothing to do — sleep (cost-aware yielding)
		d.setState(StateSleeping)
		return
	}

	d.setState(StateWorking)

	for _, task := range tasks {
		// Check if task is due
		if !d.isTaskDue(task, now) {
			continue
		}

		// Enforce blocking budget
		taskCtx, cancel := context.WithTimeout(ctx, budget)

		d.addLog("task-start", task.Description)
		log.Printf("[KAIROS] Executing: %s (autonomy=%s)", task.Description, autonomy)

		d.executeTask(taskCtx, task, autonomy)
		cancel()

		// Update last run
		d.mu.Lock()
		for i := range d.tasks {
			if d.tasks[i].ID == task.ID {
				d.tasks[i].LastRun = now
			}
		}
		d.mu.Unlock()
	}

	d.setState(StateIdle)
}

func (d *Daemon) executeTask(ctx context.Context, task Task, autonomy string) {
	// Built-in task types
	switch task.Type {
	case "git-watch":
		d.RunGitWatch()
		return
	}

	d.mu.RLock()
	provider := d.provider
	model := d.model
	workDir := d.workDir
	tracker := d.tracker
	d.mu.RUnlock()

	if provider == nil {
		d.addLog("task-skip", "No provider configured")
		return
	}

	traceID := observability.NewTraceID("run")
	traceStartedAt := time.Now().UTC()
	runTrace := observability.RunTrace{
		ID:           traceID,
		Kind:         "kairos",
		StartedAt:    traceStartedAt,
		Provider:     provider.Name(),
		Model:        model,
		WorkDir:      workDir,
		WorkstreamID: task.WorkstreamID,
		Status:       "running",
		Metadata: map[string]string{
			"taskId":      task.ID,
			"taskType":    task.Type,
			"description": task.Description,
			"autonomy":    autonomy,
		},
		Spans: []observability.RunSpan{{
			ID:        "kairos-task",
			Name:      "kairos.task",
			StartedAt: traceStartedAt,
			Status:    "running",
			Data: map[string]string{
				"taskId":      task.ID,
				"description": task.Description,
			},
		}},
	}

	var wsStore *workstream.Store
	var ws *workstream.Workstream
	workstreamContext := ""
	if task.WorkstreamID != "" {
		if workDir == "" {
			d.addLog("task-error", "No workspace configured for workstream task")
			finishDaemonRunTrace(&runTrace, "failed", "No workspace configured for workstream task")
			if tracker != nil {
				tracker.RecordRun(runTrace)
			}
			return
		}
		wsStore = workstream.NewStore(workDir)
		loaded, err := wsStore.Get(task.WorkstreamID)
		if err != nil {
			d.addLog("task-error", err.Error())
			finishDaemonRunTrace(&runTrace, "failed", err.Error())
			if tracker != nil {
				tracker.RecordRun(runTrace)
			}
			return
		}
		ws = loaded
		workstreamContext = workstream.RenderContext(*ws, 2000)
		if err := wsStore.AppendEvent(ws.ID, workstream.TimelineEvent{
			Type:    "kairos_task_started",
			Message: "KAIROS task started",
			Data: map[string]string{
				"taskId":      task.ID,
				"description": task.Description,
				"provider":    provider.Name(),
				"model":       model,
				"traceId":     traceID,
			},
		}); err != nil {
			d.addLog("task-error", err.Error())
			finishDaemonRunTrace(&runTrace, "failed", err.Error())
			if tracker != nil {
				tracker.RecordRun(runTrace)
			}
			return
		}
	}

	// Build a prompt for the task
	prompt := buildTaskPrompt(task, autonomy, workstreamContext)

	req := &types.MessagesRequest{
		Model: model,
		Messages: []types.Message{
			{Role: "user", Content: mustJSON(prompt)},
		},
		MaxTokens: 4096,
	}

	ch, err := provider.StreamMessage(ctx, req, nil)
	if err != nil {
		d.addLog("task-error", err.Error())
		finishDaemonRunTrace(&runTrace, "failed", err.Error())
		if tracker != nil {
			tracker.RecordRun(runTrace)
		}
		d.recordWorkstreamTaskFailure(wsStore, ws, task, provider.Name(), model, traceID, err.Error())
		return
	}

	// Collect response
	var response string
	for event := range ch {
		if event.Type == "content_block_delta" && event.Delta != nil {
			var delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			json.Unmarshal(event.Delta, &delta)
			if delta.Text != "" {
				response += delta.Text
			}
		}
		if event.Type == "message_stop" {
			break
		}
	}

	if len(response) > 200 {
		d.addLog("task-done", response[:200]+"...")
	} else {
		d.addLog("task-done", response)
	}
	finishDaemonRunTrace(&runTrace, "ok", "")
	if tracker != nil {
		tracker.RecordRun(runTrace)
	}
	d.recordWorkstreamTaskCompletion(wsStore, ws, task, provider.Name(), model, traceID, response)
}

func buildTaskPrompt(task Task, autonomy, workstreamContext string) string {
	mode := "Be collaborative — show choices before acting."
	if autonomy == "autonomous" {
		mode = "Work independently. Only pause for irreversible actions."
	} else if autonomy == "night" {
		mode = "Full autonomy. Complete the task without any user interaction."
	}

	prompt := "You are KAIROS, a background assistant daemon.\n" +
		"Mode: " + mode + "\n" +
		"Task: " + task.Description + "\n" +
		"Execute this task concisely."
	if contextBlock := strings.TrimSpace(workstreamContext); contextBlock != "" {
		prompt += "\n\n" + contextBlock
	}
	return prompt
}

func (d *Daemon) recordWorkstreamTaskCompletion(store *workstream.Store, ws *workstream.Workstream, task Task, provider, model, traceID, response string) {
	if store == nil || ws == nil {
		return
	}
	verification := workstream.VerificationResult{
		Status:  "not-run",
		Source:  "kairos",
		Summary: truncateDetail(response, 200),
	}
	if _, err := store.Patch(ws.ID, workstream.Patch{LastVerification: &verification}); err != nil {
		d.addLog("task-error", "workstream verification: "+err.Error())
	}
	if err := store.AppendEvent(ws.ID, workstream.TimelineEvent{
		Type:    "kairos_task_completed",
		Message: "KAIROS task completed",
		Data: map[string]string{
			"taskId":      task.ID,
			"description": task.Description,
			"provider":    provider,
			"model":       model,
			"traceId":     traceID,
			"summary":     truncateDetail(response, 200),
		},
	}); err != nil {
		d.addLog("task-error", "workstream completion: "+err.Error())
	}
}

func (d *Daemon) recordWorkstreamTaskFailure(store *workstream.Store, ws *workstream.Workstream, task Task, provider, model, traceID, detail string) {
	if store == nil || ws == nil {
		return
	}
	verification := workstream.VerificationResult{
		Status:  "failed",
		Source:  "kairos",
		Summary: truncateDetail(detail, 200),
	}
	if _, err := store.Patch(ws.ID, workstream.Patch{LastVerification: &verification}); err != nil {
		d.addLog("task-error", "workstream verification: "+err.Error())
	}
	if err := store.AppendEvent(ws.ID, workstream.TimelineEvent{
		Type:    "kairos_task_failed",
		Message: "KAIROS task failed",
		Data: map[string]string{
			"taskId":      task.ID,
			"description": task.Description,
			"provider":    provider,
			"model":       model,
			"traceId":     traceID,
			"error":       truncateDetail(detail, 200),
		},
	}); err != nil {
		d.addLog("task-error", "workstream failure: "+err.Error())
	}
}

func truncateDetail(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func finishDaemonRunTrace(trace *observability.RunTrace, status string, detail string) {
	if trace == nil {
		return
	}
	now := time.Now().UTC()
	trace.EndedAt = now
	trace.DurationMs = now.Sub(trace.StartedAt).Milliseconds()
	trace.Status = status
	if detail != "" {
		trace.Error = truncateDetail(detail, 500)
	}
	for i := range trace.Spans {
		if trace.Spans[i].Status != "running" {
			continue
		}
		trace.Spans[i].EndedAt = now
		trace.Spans[i].DurationMs = now.Sub(trace.Spans[i].StartedAt).Milliseconds()
		trace.Spans[i].Status = status
		if detail != "" {
			if trace.Spans[i].Data == nil {
				trace.Spans[i].Data = map[string]string{}
			}
			trace.Spans[i].Data["detail"] = truncateDetail(detail, 500)
		}
	}
}

// ── Task Management ──

func (d *Daemon) AddTask(task Task) {
	d.mu.Lock()
	defer d.mu.Unlock()
	task.CreatedAt = time.Now()
	task.Enabled = true
	d.tasks = append(d.tasks, task)
	d.saveTasks()
	d.addLogLocked("task-added", task.Description)
}

func (d *Daemon) RemoveTask(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, t := range d.tasks {
		if t.ID == id {
			d.tasks = append(d.tasks[:i], d.tasks[i+1:]...)
			d.saveTasks()
			return
		}
	}
}

func (d *Daemon) GetTasks() []Task {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]Task, len(d.tasks))
	copy(result, d.tasks)
	return result
}

func (d *Daemon) activeTasks() []Task {
	var active []Task
	for _, t := range d.tasks {
		if t.Enabled {
			active = append(active, t)
		}
	}
	return active
}

func (d *Daemon) isTaskDue(task Task, now time.Time) bool {
	// Built-in tasks (git-watch): run every tick
	if task.Type == "git-watch" {
		return task.LastRun.IsZero() || now.Sub(task.LastRun) > d.config.TickInterval
	}

	// Cron-based scheduling
	if task.CronExpr != "" {
		return IsDue(task.CronExpr, task.LastRun, now)
	}

	// No cron: run every tick interval
	if task.LastRun.IsZero() {
		return true
	}
	return now.Sub(task.LastRun) > d.config.TickInterval
}

// ── State & Logs ──

func (d *Daemon) setState(s State) {
	d.mu.Lock()
	d.state = s
	d.mu.Unlock()
}

func (d *Daemon) GetState() State {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

func (d *Daemon) GetConfig() DaemonConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.config
}

func (d *Daemon) SetAutonomy(mode string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.Autonomy = mode
}

func (d *Daemon) addLog(action, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addLogLocked(action, detail)
}

func (d *Daemon) addLogLocked(action, detail string) {
	entry := LogEntry{Time: time.Now(), Action: action, Detail: detail}
	d.logs = append(d.logs, entry)
	if len(d.logs) > 1000 {
		d.logs = d.logs[len(d.logs)-500:]
	}
}

func (d *Daemon) GetLogs(limit int) []LogEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if limit <= 0 || limit > len(d.logs) {
		limit = len(d.logs)
	}
	start := len(d.logs) - limit
	result := make([]LogEntry, limit)
	copy(result, d.logs[start:])
	return result
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
