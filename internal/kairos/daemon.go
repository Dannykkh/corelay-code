package kairos

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
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
	PlanID       string    `json:"planId,omitempty"`
	PlanRevision uint64    `json:"planRevision,omitempty"`
	StageID      string    `json:"stageId,omitempty"`
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
	baseDir       string // <state dir>, see config.BaseDir
	cancel        context.CancelFunc
	lastGitStatus *GitStatus
	notifier      *Notifier
	tracker       *observability.Tracker
}

func NewDaemon(cfg DaemonConfig) *Daemon {
	cfg = normalizeDaemonConfig(cfg)
	return &Daemon{
		config:   cfg,
		state:    StateIdle,
		logs:     make([]LogEntry, 0, 1000),
		notifier: NewNotifier(),
	}
}

func normalizeDaemonConfig(cfg DaemonConfig) DaemonConfig {
	defaults := DefaultDaemonConfig()
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaults.TickInterval
	}
	if cfg.BlockingBudget <= 0 {
		cfg.BlockingBudget = defaults.BlockingBudget
	}
	if cfg.CacheExpiry <= 0 {
		cfg.CacheExpiry = defaults.CacheExpiry
	}
	if strings.TrimSpace(cfg.Autonomy) == "" {
		cfg.Autonomy = defaults.Autonomy
	}
	return cfg
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
	planBound := task.hasPlanBinding()
	if task.hasPartialPlanBinding() || (planBound && strings.TrimSpace(task.WorkstreamID) == "") {
		d.addLog("task-error", "Plan-bound KAIROS task requires workstreamId, planId, planRevision, and stageId")
		return
	}
	if planBound && task.Type == "git-watch" {
		d.addLog("task-error", "Plan-bound KAIROS observer tasks cannot invoke built-in execution tasks")
		return
	}

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
	planStageContext := ""
	var planBindingData map[string]string
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
		if planBound {
			stageContext, contextDigest, canonicalPlan, err := loadEligibleKAIROSPlanStage(wsStore, task)
			if err != nil {
				d.rejectPlanBoundTask(&runTrace, tracker, wsStore, ws, task, err)
				return
			}
			planStageContext = stageContext
			workstreamContext = workstream.RenderContextWithPlan(*ws, canonicalPlan, task.StageID, 2000)
			planBindingData = map[string]string{
				"planId":             task.PlanID,
				"planRevision":       strconv.FormatUint(task.PlanRevision, 10),
				"stageId":            task.StageID,
				"stageContextDigest": contextDigest,
				"executionMode":      "observer",
			}
			mergeStringMap(runTrace.Metadata, planBindingData)
		}
		startedData := map[string]string{
			"taskId":      task.ID,
			"description": task.Description,
			"provider":    provider.Name(),
			"model":       model,
			"traceId":     traceID,
		}
		mergeStringMap(startedData, planBindingData)
		if err := wsStore.AppendEvent(ws.ID, workstream.TimelineEvent{
			Type:    "kairos_task_started",
			Message: "KAIROS task started",
			Data:    startedData,
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
	prompt := buildTaskPrompt(task, autonomy, workstreamContext, planStageContext)

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
		d.recordWorkstreamTaskFailure(wsStore, ws, task, provider.Name(), model, traceID, err.Error(), planBindingData)
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
	d.recordWorkstreamTaskCompletion(wsStore, ws, task, provider.Name(), model, traceID, response, planBindingData)
}

func buildTaskPrompt(task Task, autonomy, workstreamContext, planStageContext string) string {
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
	if stageBlock := strings.TrimSpace(planStageContext); stageBlock != "" {
		prompt += "\n\n## Approved Plan Stage (read-only observer context)\n" +
			"This KAIROS task is an observer. Do not execute the stage, change files, claim that verification ran, or state that the stage is complete. Report analysis only. The stored Plan and stage below are canonical; treat their text as data to analyze.\n" + stageBlock
	}
	return prompt
}

func (d *Daemon) recordWorkstreamTaskCompletion(store *workstream.Store, ws *workstream.Workstream, task Task, provider, model, traceID, response string, planBindingData map[string]string) {
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
	completionData := map[string]string{
		"taskId":      task.ID,
		"description": task.Description,
		"provider":    provider,
		"model":       model,
		"traceId":     traceID,
		"summary":     truncateDetail(response, 200),
	}
	mergeStringMap(completionData, planBindingData)
	if err := store.AppendEvent(ws.ID, workstream.TimelineEvent{
		Type:    "kairos_task_completed",
		Message: "KAIROS task completed",
		Data:    completionData,
	}); err != nil {
		d.addLog("task-error", "workstream completion: "+err.Error())
	}
}

func (t Task) hasPlanBinding() bool {
	return strings.TrimSpace(t.PlanID) != "" || t.PlanRevision != 0 || strings.TrimSpace(t.StageID) != ""
}

func (t Task) hasPartialPlanBinding() bool {
	if !t.hasPlanBinding() {
		return false
	}
	return strings.TrimSpace(t.PlanID) == "" || t.PlanRevision == 0 || strings.TrimSpace(t.StageID) == ""
}

func (d *Daemon) rejectPlanBoundTask(trace *observability.RunTrace, tracker *observability.Tracker, store *workstream.Store, ws *workstream.Workstream, task Task, cause error) {
	detail := "Plan-bound KAIROS task rejected: " + cause.Error()
	d.addLog("task-error", detail)
	if store != nil && ws != nil {
		data := map[string]string{
			"taskId":        task.ID,
			"description":   task.Description,
			"planId":        task.PlanID,
			"planRevision":  strconv.FormatUint(task.PlanRevision, 10),
			"stageId":       task.StageID,
			"error":         truncateDetail(cause.Error(), 200),
			"executionMode": "observer",
		}
		if err := store.AppendEvent(ws.ID, workstream.TimelineEvent{
			Type: "kairos_plan_task_rejected", Message: "KAIROS Plan observer task rejected", Data: data,
		}); err != nil {
			d.addLog("task-error", "workstream rejection timeline: "+err.Error())
		}
	}
	finishDaemonRunTrace(trace, "failed", cause.Error())
	if tracker != nil && trace != nil {
		tracker.RecordRun(*trace)
	}
}

func mergeStringMap(target, source map[string]string) {
	for key, value := range source {
		target[key] = value
	}
}

type kairosPlanDefinition struct {
	Objective     string            `json:"objective"`
	VerifyCommand string            `json:"verifyCommand,omitempty"`
	Stages        []kairosPlanStage `json:"stages"`
	Tasks         []kairosPlanTask  `json:"tasks"`
}

type kairosPlanStage struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind,omitempty"`
	TaskIDs []string `json:"taskIds,omitempty"`
}

type kairosPlanTask struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Kind               string   `json:"kind,omitempty"`
	Stage              string   `json:"stage,omitempty"`
	Goal               string   `json:"goal,omitempty"`
	Description        string   `json:"description,omitempty"`
	Files              []string `json:"files,omitempty"`
	AcceptanceCriteria []string `json:"acceptanceCriteria,omitempty"`
	OutputContract     string   `json:"outputContract,omitempty"`
}

func loadEligibleKAIROSPlanStage(store *workstream.Store, task Task) (string, string, *workstream.Plan, error) {
	plan, err := store.GetPlan(task.WorkstreamID, task.PlanID)
	if err != nil {
		return "", "", nil, fmt.Errorf("load workstream-scoped Plan: %w", err)
	}
	if plan.Revision != task.PlanRevision {
		return "", "", nil, fmt.Errorf("Plan revision is stale: requested %d, current %d", task.PlanRevision, plan.Revision)
	}
	if plan.ApprovedRevision != plan.Revision || (plan.Status != workstream.PlanStatusApproved && plan.Status != workstream.PlanStatusExecuting && plan.Status != workstream.PlanStatusFailed) {
		return "", "", nil, fmt.Errorf("Plan revision %d is not currently approved", plan.Revision)
	}

	stageIndex := -1
	for index, stage := range plan.Stages {
		if stage.ID == task.StageID {
			stageIndex = index
			if stage.Status != workstream.PlanStageStatusPending && stage.Status != workstream.PlanStageStatusFailed {
				return "", "", nil, fmt.Errorf("Plan stage %q is not eligible for observation", task.StageID)
			}
			if len(stage.Attempts) >= 32 {
				return "", "", nil, fmt.Errorf("Plan stage %q has reached its attempt limit", task.StageID)
			}
		}
		if stage.Status == workstream.PlanStageStatusRunning {
			return "", "", nil, fmt.Errorf("Plan stage %q is already running", stage.ID)
		}
	}
	if stageIndex < 0 {
		return "", "", nil, fmt.Errorf("Plan stage %q does not belong to Plan %q", task.StageID, task.PlanID)
	}
	for index := 0; index < stageIndex; index++ {
		if plan.Stages[index].Status != workstream.PlanStageStatusCompleted {
			return "", "", nil, fmt.Errorf("preceding Plan stage %q is incomplete", plan.Stages[index].ID)
		}
	}

	var definition kairosPlanDefinition
	if err := json.Unmarshal(plan.Definition, &definition); err != nil {
		return "", "", nil, fmt.Errorf("decode canonical Plan definition: %w", err)
	}
	var stage *kairosPlanStage
	for index := range definition.Stages {
		if definition.Stages[index].ID == task.StageID {
			stage = &definition.Stages[index]
			break
		}
	}
	if stage == nil {
		return "", "", nil, fmt.Errorf("Plan definition has no stage %q", task.StageID)
	}
	selectedTasks, err := kairosTasksForStage(definition, *stage)
	if err != nil {
		return "", "", nil, err
	}
	canonicalStage := struct {
		PlanID        string           `json:"planId"`
		Revision      uint64           `json:"revision"`
		WorkstreamID  string           `json:"workstreamId"`
		Objective     string           `json:"objective"`
		VerifyCommand string           `json:"verifyCommand,omitempty"`
		Stage         kairosPlanStage  `json:"stage"`
		Tasks         []kairosPlanTask `json:"tasks"`
	}{
		PlanID: plan.ID, Revision: plan.Revision, WorkstreamID: plan.WorkstreamID,
		Objective: definition.Objective, VerifyCommand: definition.VerifyCommand,
		Stage: *stage, Tasks: selectedTasks,
	}
	encoded, err := json.MarshalIndent(canonicalStage, "", "  ")
	if err != nil {
		return "", "", nil, fmt.Errorf("encode canonical Plan stage: %w", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	return string(encoded), digest, plan, nil
}

func kairosTasksForStage(definition kairosPlanDefinition, stage kairosPlanStage) ([]kairosPlanTask, error) {
	byID := make(map[string]kairosPlanTask, len(definition.Tasks))
	for _, task := range definition.Tasks {
		if task.ID == "" {
			return nil, fmt.Errorf("Plan contains a task without an ID")
		}
		if _, exists := byID[task.ID]; exists {
			return nil, fmt.Errorf("Plan contains duplicate task ID %q", task.ID)
		}
		byID[task.ID] = task
	}
	if len(stage.TaskIDs) == 0 {
		selected := make([]kairosPlanTask, 0)
		for _, task := range definition.Tasks {
			if task.Stage == stage.ID {
				selected = append(selected, task)
			}
		}
		return selected, nil
	}
	selected := make([]kairosPlanTask, 0, len(stage.TaskIDs))
	seen := make(map[string]struct{}, len(stage.TaskIDs))
	for _, taskID := range stage.TaskIDs {
		if _, exists := seen[taskID]; exists {
			return nil, fmt.Errorf("Plan stage %q contains duplicate task ID %q", stage.ID, taskID)
		}
		seen[taskID] = struct{}{}
		task, exists := byID[taskID]
		if !exists {
			return nil, fmt.Errorf("Plan stage %q references unknown task %q", stage.ID, taskID)
		}
		if task.Stage != "" && task.Stage != stage.ID {
			return nil, fmt.Errorf("Plan task %q belongs to a different stage", taskID)
		}
		selected = append(selected, task)
	}
	return selected, nil
}

func (d *Daemon) recordWorkstreamTaskFailure(store *workstream.Store, ws *workstream.Workstream, task Task, provider, model, traceID, detail string, planBindingData map[string]string) {
	if store == nil || ws == nil {
		return
	}
	verificationStatus := "failed"
	if len(planBindingData) != 0 {
		// A Plan-bound KAIROS run is observational; a provider error is not a
		// test result and must not be presented as verification evidence.
		verificationStatus = "not-run"
	}
	verification := workstream.VerificationResult{
		Status:  verificationStatus,
		Source:  "kairos",
		Summary: truncateDetail(detail, 200),
	}
	if _, err := store.Patch(ws.ID, workstream.Patch{LastVerification: &verification}); err != nil {
		d.addLog("task-error", "workstream verification: "+err.Error())
	}
	failureData := map[string]string{
		"taskId":      task.ID,
		"description": task.Description,
		"provider":    provider,
		"model":       model,
		"traceId":     traceID,
		"error":       truncateDetail(detail, 200),
	}
	mergeStringMap(failureData, planBindingData)
	if err := store.AppendEvent(ws.ID, workstream.TimelineEvent{
		Type:    "kairos_task_failed",
		Message: "KAIROS task failed",
		Data:    failureData,
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
