package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aniclew/aniclew/internal/types"
)

// ── Team: Lead/Worker orchestration with Wave execution ──

// TeamConfig holds team-level settings.
type TeamConfig struct {
	Name              string          `json:"name"`
	MaxWaveSize       int             `json:"maxWaveSize"`   // max agents per wave (default 5)
	CycleTimeout      time.Duration   `json:"cycleTimeout"`  // per-wave timeout
	VerifyCommand     string          `json:"verifyCommand"` // verification command
	Capacity          CapacityConfig  `json:"capacity,omitempty"`
	WorkstreamContext string          `json:"-"`
	ProviderFactory   ProviderFactory `json:"-"`
}

type ProviderFactory func(name string) (types.Provider, error)

// TeamTask represents a task with dependencies.
type TeamTask struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Kind        TaskKind           `json:"kind,omitempty"`
	Role        string             `json:"role,omitempty"`
	Provider    string             `json:"provider,omitempty"`
	Model       string             `json:"model,omitempty"`
	ReadOnly    bool               `json:"readOnly,omitempty"`
	Resources   AgentTaskResources `json:"resources,omitempty"`
	Files       []string           `json:"files"`      // owned files (exclusive)
	DependsOn   []string           `json:"dependsOn"`  // task IDs that must complete first
	Status      string             `json:"status"`     // pending, running, completed, failed
	AssignedTo  string             `json:"assignedTo"` // worker agent ID
	Result      string             `json:"result,omitempty"`
	OutputPath  string             `json:"outputPath,omitempty"`
	ToolCalls   int                `json:"toolCalls"`
	Wave        int                `json:"wave"` // computed wave number
	CreatedAt   time.Time          `json:"createdAt"`
	StartedAt   time.Time          `json:"startedAt,omitempty"`
	FinishedAt  time.Time          `json:"finishedAt,omitempty"`
}

// Team manages the Lead/Worker orchestration.
type Team struct {
	mu       sync.RWMutex
	config   TeamConfig
	tasks    []*TeamTask
	workers  map[string]*workerState
	mailbox  *Mailbox
	provider types.Provider
	model    string
	workDir  string
	baseDir  string
	eventCh  chan<- Event // report progress to caller
}

type workerState struct {
	ID        string
	Name      string
	TaskID    string
	Status    string // running, idle, shutdown
	StartedAt time.Time
	IdleSince time.Time
	cancel    context.CancelFunc
	onIdle    []func(string) // callbacks when worker becomes idle
}

// NewTeam creates a team orchestrator.
func NewTeam(provider types.Provider, model, workDir, baseDir string, cfg TeamConfig) *Team {
	if cfg.MaxWaveSize <= 0 {
		cfg.MaxWaveSize = 5
	}
	if cfg.CycleTimeout <= 0 {
		cfg.CycleTimeout = 5 * time.Minute
	}
	localProvider := false
	if provider != nil {
		localProvider = isLocalProvider(provider.Name())
	}
	cfg.Capacity = cfg.Capacity.Normalized(localProvider)
	return &Team{
		config:   cfg,
		tasks:    make([]*TeamTask, 0),
		workers:  make(map[string]*workerState),
		mailbox:  NewMailbox(filepath.Join(baseDir, "teams", cfg.Name)),
		provider: provider,
		model:    model,
		workDir:  workDir,
		baseDir:  baseDir,
	}
}

// AddTask registers a task with dependencies.
func (t *Team) AddTask(task TeamTask) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task.Status = "pending"
	task.CreatedAt = time.Now()
	t.tasks = append(t.tasks, &task)
}

// ── Wave Computation (Topological Sort) ──

// ComputeWaves assigns wave numbers based on dependency graph.
func (t *Team) ComputeWaves() ([][]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	taskMap := make(map[string]*TeamTask)
	for _, task := range t.tasks {
		taskMap[task.ID] = task
	}

	// Kahn's algorithm
	inDegree := make(map[string]int)
	dependents := make(map[string][]string) // task → tasks that depend on it

	for _, task := range t.tasks {
		if _, ok := inDegree[task.ID]; !ok {
			inDegree[task.ID] = 0
		}
		for _, dep := range task.DependsOn {
			inDegree[task.ID]++
			dependents[dep] = append(dependents[dep], task.ID)
		}
	}

	// Find initial wave (no dependencies)
	var waves [][]string
	wave := 0

	for len(inDegree) > 0 {
		var current []string
		for id, deg := range inDegree {
			if deg == 0 {
				current = append(current, id)
			}
		}

		if len(current) == 0 {
			return nil, fmt.Errorf("circular dependency detected")
		}

		sort.Strings(current)

		// Split if wave too large
		for i := 0; i < len(current); i += t.config.MaxWaveSize {
			end := i + t.config.MaxWaveSize
			if end > len(current) {
				end = len(current)
			}
			subWave := current[i:end]
			waves = append(waves, subWave)

			for _, id := range subWave {
				if task, ok := taskMap[id]; ok {
					task.Wave = wave
				}
			}
			wave++
		}

		// Remove processed and update degrees
		for _, id := range current {
			delete(inDegree, id)
			for _, dep := range dependents[id] {
				inDegree[dep]--
			}
		}
	}

	return waves, nil
}

// ── File Ownership Enforcement ──

// CheckFileOwnership validates that a file isn't owned by another active worker.
func (t *Team) CheckFileOwnership(workerID string, filePath string) (bool, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, task := range t.tasks {
		if task.AssignedTo == workerID || task.Status != "running" {
			continue
		}
		for _, owned := range task.Files {
			if matchesOwnership(filePath, owned) {
				return false, fmt.Sprintf("File '%s' owned by %s (task %s)", filePath, task.AssignedTo, task.ID)
			}
		}
	}
	return true, ""
}

func matchesOwnership(filePath, pattern string) bool {
	if strings.HasSuffix(pattern, "/**") || strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimRight(pattern, "/*")
		return strings.HasPrefix(filePath, prefix)
	}
	return filePath == pattern
}

// ── Wave Execution ──

// ExecuteWaves runs all waves sequentially, with parallel workers within each wave.
func (t *Team) ExecuteWaves(ctx context.Context, eventCh chan<- Event) error {
	t.eventCh = eventCh

	waves, err := t.ComputeWaves()
	if err != nil {
		return err
	}

	t.report(fmt.Sprintf("Team '%s': %d tasks in %d waves", t.config.Name, len(t.tasks), len(waves)))

	for waveIdx, taskIDs := range waves {
		t.report(fmt.Sprintf("Wave %d/%d: %d tasks (%s)", waveIdx+1, len(waves), len(taskIDs), strings.Join(taskIDs, ", ")))

		if err := t.executeWave(ctx, waveIdx, taskIDs); err != nil {
			return fmt.Errorf("wave %d failed: %w", waveIdx+1, err)
		}

		t.report(fmt.Sprintf("Wave %d/%d complete", waveIdx+1, len(waves)))
	}

	return nil
}

func (t *Team) executeWave(ctx context.Context, waveIdx int, taskIDs []string) error {
	waveCtx, cancel := context.WithTimeout(ctx, t.config.CycleTimeout)
	defer cancel()

	pending := make([]*TeamTask, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		task := t.getTask(taskID)
		if task == nil {
			continue
		}
		pending = append(pending, task)
	}

	batchIndex := 0
	for len(pending) > 0 {
		if err := waveCtx.Err(); err != nil {
			return err
		}
		batch := t.selectWaveBatch(pending)
		if len(batch) == 0 {
			return fmt.Errorf("no runnable tasks fit configured capacity in wave %d", waveIdx)
		}
		batchIndex++
		if len(batch) < len(pending) {
			ids := make([]string, 0, len(batch))
			for _, task := range batch {
				ids = append(ids, task.ID)
			}
			t.report(fmt.Sprintf("Wave %d batch %d: %d/%d tasks (%s)", waveIdx+1, batchIndex, len(batch), len(pending), strings.Join(ids, ", ")))
		}

		if err := t.runWaveBatch(waveCtx, batch); err != nil {
			return err
		}

		completedBatch := map[string]bool{}
		for _, task := range batch {
			completedBatch[task.ID] = true
		}
		nextPending := pending[:0]
		for _, task := range pending {
			if !completedBatch[task.ID] {
				nextPending = append(nextPending, task)
			}
		}
		pending = nextPending
	}

	return nil
}

func (t *Team) selectWaveBatch(tasks []*TeamTask) []*TeamTask {
	candidates := append([]*TeamTask(nil), tasks...)
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})

	capacity := t.config.Capacity
	if capacity.MaxParallelTasks <= 0 {
		capacity = capacity.Normalized(false)
	}
	selected := make([]*TeamTask, 0, capacity.MaxParallelTasks)
	used := AgentTaskResources{}

	for _, task := range candidates {
		if capacity.MaxParallelTasks > 0 && len(selected) >= capacity.MaxParallelTasks {
			break
		}
		need := teamTaskResources(task)
		if !resourcesFit(capacity, used, need) {
			continue
		}
		if capacity.FileScopeLock && conflictsWithSelectedTeamTask(task, selected) {
			continue
		}
		selected = append(selected, task)
		used.ModelSlots += need.ModelSlots
		used.ToolSlots += need.ToolSlots
		used.WebFetchSlots += need.WebFetchSlots
		used.TestSlots += need.TestSlots
	}

	return selected
}

func teamTaskResources(task *TeamTask) AgentTaskResources {
	if task == nil {
		return AgentTaskResources{ModelSlots: 1}
	}
	resources := task.Resources
	if resources.ModelSlots <= 0 {
		resources.ModelSlots = 1
	}
	if resources.TestSlots <= 0 && task.Kind == TaskKindVerify {
		resources.TestSlots = 1
	}
	return resources
}

func resourcesFit(capacity CapacityConfig, used, need AgentTaskResources) bool {
	return resourceSlotFits(capacity.ModelSlots, used.ModelSlots, need.ModelSlots) &&
		resourceSlotFits(capacity.ToolSlots, used.ToolSlots, need.ToolSlots) &&
		resourceSlotFits(capacity.WebFetchSlots, used.WebFetchSlots, need.WebFetchSlots) &&
		resourceSlotFits(capacity.TestSlots, used.TestSlots, need.TestSlots)
}

func resourceSlotFits(limit, used, need int) bool {
	if limit <= 0 {
		return true
	}
	return used+need <= limit
}

func conflictsWithSelectedTeamTask(task *TeamTask, selected []*TeamTask) bool {
	for _, other := range selected {
		if teamTasksConflict(task, other) {
			return true
		}
	}
	return false
}

func teamTasksConflict(a, b *TeamTask) bool {
	if a == nil || b == nil || a.ReadOnly || b.ReadOnly {
		return false
	}
	if len(a.Files) == 0 || len(b.Files) == 0 {
		return true
	}
	for _, left := range a.Files {
		for _, right := range b.Files {
			if ownershipPatternsOverlap(left, right) {
				return true
			}
		}
	}
	return false
}

func (t *Team) runWaveBatch(waveCtx context.Context, batch []*TeamTask) error {
	var wg sync.WaitGroup
	for _, task := range batch {
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.executeTask(waveCtx, task)
		}()
	}
	wg.Wait()

	for _, task := range batch {
		if task.Status != "completed" {
			return fmt.Errorf("task %s did not complete (status: %s)", task.ID, task.Status)
		}
	}
	return nil
}

func (t *Team) executeTask(ctx context.Context, task *TeamTask) {
	t.mu.Lock()
	task.Status = "running"
	task.StartedAt = time.Now()

	workerID := fmt.Sprintf("worker-%s", task.ID)
	workerCtx, workerCancel := context.WithCancel(ctx)
	t.workers[workerID] = &workerState{
		ID:        workerID,
		Name:      task.Name,
		TaskID:    task.ID,
		Status:    "running",
		StartedAt: time.Now(),
		cancel:    workerCancel,
	}
	task.AssignedTo = workerID
	t.mailbox.EnsureInbox(workerID)
	t.mu.Unlock()

	log.Printf("[Team] Worker %s starting task %s: %s", workerID, task.ID, task.Name)

	provider, model, err := t.resolveTaskRuntime(task)
	if err != nil {
		workerCancel()
		t.failStartedTask(task, workerID, err)
		return
	}

	// Build worker prompt with full context
	prompt := t.buildWorkerPrompt(task)

	// Run worker agent loop
	userContent, _ := json.Marshal(prompt)
	messages := []types.Message{
		{Role: "user", Content: userContent},
	}

	innerEventCh := make(chan Event, 100)
	go RunLoopWithOptions(workerCtx, provider, model, messages, t.workDir, RunOptions{
		ResponseLang:      "auto",
		WorkstreamContext: t.config.WorkstreamContext,
		WorkerID:          workerID,
		OwnershipChecker:  t.CheckFileOwnership,
	}, innerEventCh)

	// Disk-based output: write to file instead of accumulating in memory
	outputDir := filepath.Join(t.baseDir, "teams", t.config.Name, "output")
	os.MkdirAll(outputDir, 0755)
	outputPath := filepath.Join(outputDir, task.ID+".txt")
	t.mu.Lock()
	task.OutputPath = outputPath
	t.mu.Unlock()
	outputFile, _ := os.Create(outputPath)

	var result string
	toolCalls := 0
	for event := range innerEventCh {
		switch event.Type {
		case "text":
			if text, ok := event.Data.(string); ok {
				result += text
				if outputFile != nil {
					outputFile.WriteString(text)
				}
			}
		case "tool_result":
			toolCalls++
			if outputFile != nil {
				if data, ok := event.Data.(map[string]interface{}); ok {
					fmt.Fprintf(outputFile, "\n[tool:%s] %v\n", data["name"], truncateStr(fmt.Sprint(data["result"]), 500))
				}
			}
		}

		// Check mailbox for shutdown requests
		msgs := t.mailbox.Peek(workerID)
		for _, msg := range msgs {
			if msg.Type == MsgShutdownRequest {
				log.Printf("[Team] Worker %s received shutdown request", workerID)
				workerCancel()
			}
		}
	}
	if outputFile != nil {
		outputFile.Close()
	}

	// Keep only summary in memory (not full output)
	if len(result) > 500 {
		result = result[:500] + "... (full output: " + outputPath + ")"
	}

	// Update task status
	t.mu.Lock()
	task.Result = result
	task.ToolCalls = toolCalls
	task.FinishedAt = time.Now()
	if workerCtx.Err() != nil {
		task.Status = "failed"
	} else {
		task.Status = "completed"
	}

	// Mark idle and fire callbacks
	w := t.workers[workerID]
	w.Status = "idle"
	w.IdleSince = time.Now()
	callbacks := make([]func(string), len(w.onIdle))
	copy(callbacks, w.onIdle)
	t.mu.Unlock()

	for _, cb := range callbacks {
		cb(workerID)
	}

	// Send idle notification
	t.mailbox.Send(TeamMessage{
		From: workerID,
		To:   "lead",
		Type: MsgIdleNotify,
		Text: fmt.Sprintf("Task %s %s (%d tool calls, %.1fs)",
			task.ID, task.Status, toolCalls, task.FinishedAt.Sub(task.StartedAt).Seconds()),
		Summary: fmt.Sprintf("%s: %s", task.ID, task.Status),
	})

	t.report(fmt.Sprintf("Worker %s: %s — %s (%d tools, %.1fs)",
		workerID, task.ID, task.Status, toolCalls, task.FinishedAt.Sub(task.StartedAt).Seconds()))
}

func (t *Team) resolveTaskRuntime(task *TeamTask) (types.Provider, string, error) {
	provider := t.provider
	model := t.model

	providerName := strings.TrimSpace(task.Provider)
	if providerName != "" {
		if provider != nil && provider.Name() == providerName {
			// Reuse the team provider.
		} else if t.config.ProviderFactory != nil {
			next, err := t.config.ProviderFactory(providerName)
			if err != nil {
				return nil, "", fmt.Errorf("create task provider %q: %w", providerName, err)
			}
			provider = next
		} else {
			return nil, "", fmt.Errorf("task requested provider %q but no provider factory is configured", providerName)
		}
	}
	if provider == nil {
		return nil, "", fmt.Errorf("no provider configured")
	}

	if taskModel := strings.TrimSpace(task.Model); taskModel != "" {
		model = taskModel
	}
	if strings.TrimSpace(model) == "" {
		return nil, "", fmt.Errorf("no model configured")
	}
	return provider, model, nil
}

func (t *Team) failStartedTask(task *TeamTask, workerID string, err error) {
	t.mu.Lock()
	task.Result = "Runtime error: " + err.Error()
	task.FinishedAt = time.Now()
	task.Status = "failed"
	if w := t.workers[workerID]; w != nil {
		w.Status = "idle"
		w.IdleSince = time.Now()
	}
	t.mu.Unlock()

	t.mailbox.Send(TeamMessage{
		From:    workerID,
		To:      "lead",
		Type:    MsgIdleNotify,
		Text:    fmt.Sprintf("Task %s failed: %s", task.ID, err.Error()),
		Summary: fmt.Sprintf("%s: failed", task.ID),
	})
	t.report(fmt.Sprintf("Worker %s: %s — failed (%s)", workerID, task.ID, err.Error()))
}

func (t *Team) buildWorkerPrompt(task *TeamTask) string {
	filesStr := "all files"
	if len(task.Files) > 0 {
		filesStr = strings.Join(task.Files, ", ")
	}

	return fmt.Sprintf(`You are a Worker agent in team "%s".

## Your Task
ID: %s
Name: %s
Description: %s

## Your Files (EXCLUSIVE — only modify these)
%s

## Rules
- ONLY modify files listed above
- Do NOT touch files owned by other workers
- Use Read/Grep to understand code before editing
- Complete the task fully, then stop
- Be efficient and precise`, t.config.Name, task.ID, task.Name, task.Description, filesStr)
}

// ── Verification Loop ──

// Verify runs the verification command with retry loop (up to maxRetries).
func (t *Team) Verify(ctx context.Context) (bool, string) {
	if t.config.VerifyCommand == "" {
		return true, "No verification command configured"
	}

	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return false, "Verification cancelled"
		}

		t.report(fmt.Sprintf("Verification attempt %d/%d: %s", attempt, maxRetries, t.config.VerifyCommand))

		input, _ := json.Marshal(map[string]string{"command": t.config.VerifyCommand})
		result := ExecuteBashDeep(input, t.workDir, nil)

		if !result.IsError {
			return true, fmt.Sprintf("Verification PASSED (attempt %d):\n%s", attempt, truncateStr(result.Output, 500))
		}

		t.report(fmt.Sprintf("Verification attempt %d FAILED: %s", attempt, truncateStr(result.Output, 200)))

		if attempt < maxRetries {
			// Give a chance for fixes before retrying
			t.report("Waiting before retry...")
			select {
			case <-ctx.Done():
				return false, "Cancelled during retry wait"
			case <-time.After(2 * time.Second):
			}
		}
	}

	input, _ := json.Marshal(map[string]string{"command": t.config.VerifyCommand})
	result := ExecuteBashDeep(input, t.workDir, nil)
	return false, fmt.Sprintf("Verification FAILED after %d attempts:\n%s", maxRetries, truncateStr(result.Output, 2000))
}

// ── Team Cleanup ──

// Shutdown sends shutdown requests to all workers and cleans up.
func (t *Team) Shutdown() {
	t.mu.Lock()
	workers := make([]string, 0, len(t.workers))
	for id := range t.workers {
		workers = append(workers, id)
	}
	t.mu.Unlock()

	// Send shutdown to all workers
	for _, id := range workers {
		t.mailbox.Send(TeamMessage{
			From: "lead",
			To:   id,
			Type: MsgShutdownRequest,
			Text: "Team shutdown requested",
		})
	}

	// Cancel all worker contexts
	t.mu.Lock()
	for _, w := range t.workers {
		if w.cancel != nil {
			w.cancel()
		}
		w.Status = "shutdown"
	}
	t.mu.Unlock()

	// Clean up mailbox
	t.mailbox.ClearAll()

	log.Printf("[Team] '%s' shutdown complete", t.config.Name)
}

// ── Queries ──

func (t *Team) getTask(id string) *TeamTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, task := range t.tasks {
		if task.ID == id {
			return task
		}
	}
	return nil
}

// GetTasks returns all tasks.
func (t *Team) GetTasks() []*TeamTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]*TeamTask, len(t.tasks))
	copy(result, t.tasks)
	return result
}

// GetWorkers returns all worker states.
func (t *Team) GetWorkers() []workerState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]workerState, 0, len(t.workers))
	for _, w := range t.workers {
		result = append(result, *w)
	}
	return result
}

// Summary returns a text summary of team execution.
func (t *Team) Summary() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Team: %s\n", t.config.Name))

	completed := 0
	failed := 0
	totalTools := 0
	for _, task := range t.tasks {
		switch task.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		}
		totalTools += task.ToolCalls
	}

	sb.WriteString(fmt.Sprintf("Tasks: %d total, %d completed, %d failed\n", len(t.tasks), completed, failed))
	sb.WriteString(fmt.Sprintf("Tool calls: %d\n", totalTools))

	for _, task := range t.tasks {
		icon := "⏳"
		switch task.Status {
		case "completed":
			icon = "✅"
		case "failed":
			icon = "❌"
		case "running":
			icon = "🔄"
		}
		sb.WriteString(fmt.Sprintf("  %s [W%d] %s — %s\n", icon, task.Wave, task.ID, task.Name))
	}

	return sb.String()
}

func (t *Team) report(msg string) {
	log.Printf("[Team/%s] %s", t.config.Name, msg)
	if t.eventCh != nil {
		t.eventCh <- Event{Type: "status", Data: msg}
	}
}
