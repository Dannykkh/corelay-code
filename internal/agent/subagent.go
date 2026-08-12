package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	subAgentIterationLimit = 10
	subAgentTimeout        = 5 * time.Minute
)

// SubAgentTask represents a task assigned to a sub-agent.
type SubAgentTask struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Instruction string                   `json:"instruction"`
	Files       []string                 `json:"files"`  // files this agent owns
	Status      string                   `json:"status"` // "pending", "running", "completed", "failed"
	Result      string                   `json:"result"`
	StartedAt   time.Time                `json:"startedAt,omitempty"`
	FinishedAt  time.Time                `json:"finishedAt,omitempty"`
	ToolCalls   int                      `json:"toolCalls"`
	Sandbox     []SandboxExecutionRecord `json:"sandbox,omitempty"`
}

type SubAgentManagerOptions struct {
	Context           context.Context
	SessionID         string
	ApprovalRequester approval.Requester
	SandboxRunner     sandbox.Runner
	SandboxPolicy     sandbox.Policy
	PluginDirs        []string
	PluginExecution   *PluginExecutionOptions
	DisablePlugins    bool
	Recorder          RunRecorder
	HarnessProfile    *harness.HarnessProfile
	CapabilityProfile *capabilityprofile.AutomaticSelection
}

// SubAgentManager manages parallel sub-agents.
type SubAgentManager struct {
	mu                sync.RWMutex
	tasks             map[string]*SubAgentTask
	provider          types.Provider
	model             string
	workDir           string
	counter           int
	context           context.Context
	sessionID         string
	approvalRequester approval.Requester
	sandboxRunner     sandbox.Runner
	sandboxPolicy     sandbox.Policy
	pluginDirs        []string
	pluginExecution   *PluginExecutionOptions
	disablePlugins    bool
	recorder          RunRecorder
	harnessProfile    *harness.HarnessProfile
	capabilityProfile *capabilityprofile.AutomaticSelection
}

func NewSubAgentManager(provider types.Provider, model, workDir string) *SubAgentManager {
	return NewSubAgentManagerWithOptions(provider, model, workDir, SubAgentManagerOptions{})
}

func NewSubAgentManagerWithOptions(
	provider types.Provider,
	model string,
	workDir string,
	opts SubAgentManagerOptions,
) *SubAgentManager {
	sandboxRunner, sandboxPolicy := configuredSandboxExecution(
		opts.SandboxRunner,
		opts.SandboxPolicy,
		"sub-agent",
	)
	var pluginDirs []string
	if opts.PluginDirs != nil {
		pluginDirs = make([]string, len(opts.PluginDirs))
		copy(pluginDirs, opts.PluginDirs)
	}
	var pluginExecution *PluginExecutionOptions
	if opts.PluginExecution != nil {
		copy := *opts.PluginExecution
		pluginExecution = &copy
	}
	return &SubAgentManager{
		tasks:             make(map[string]*SubAgentTask),
		provider:          provider,
		model:             model,
		workDir:           workDir,
		context:           opts.Context,
		sessionID:         opts.SessionID,
		approvalRequester: opts.ApprovalRequester,
		sandboxRunner:     sandboxRunner,
		sandboxPolicy:     sandboxPolicy,
		pluginDirs:        pluginDirs,
		pluginExecution:   pluginExecution,
		disablePlugins:    opts.DisablePlugins,
		recorder:          opts.Recorder,
		harnessProfile:    opts.HarnessProfile,
		capabilityProfile: opts.CapabilityProfile,
	}
}

// SessionID returns the immutable approval namespace shared by this manager's
// tasks. Individual tool calls remain independently bound to their run and
// tool-call identities.
func (m *SubAgentManager) SessionID() string {
	if m == nil {
		return ""
	}
	return m.sessionID
}

// Spawn creates and starts a sub-agent in a separate goroutine, then returns a
// detached snapshot of its initial state. Use GetTask to observe later updates.
func (m *SubAgentManager) Spawn(name, instruction string, files []string) *SubAgentTask {
	m.mu.Lock()
	m.counter++
	id := fmt.Sprintf("sub-%d", m.counter)
	task := &SubAgentTask{
		ID:          id,
		Name:        name,
		Instruction: instruction,
		Files:       append([]string(nil), files...),
		Status:      "pending",
	}
	m.tasks[id] = task
	snapshot := cloneSubAgentTask(task)
	m.mu.Unlock()

	// Start in goroutine
	go m.run(task)

	return snapshot
}

// SpawnMultiple creates multiple sub-agents and runs them in parallel.
func (m *SubAgentManager) SpawnMultiple(tasks []struct {
	Name        string
	Instruction string
	Files       []string
}) []*SubAgentTask {
	var results []*SubAgentTask
	for _, t := range tasks {
		results = append(results, m.Spawn(t.Name, t.Instruction, t.Files))
	}
	return results
}

// Wait blocks until all sub-agents are done.
func (m *SubAgentManager) Wait(timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			log.Println("[SubAgent] Timeout waiting for agents")
			return
		default:
			if m.AllDone() {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// AllDone returns true if all tasks are completed or failed.
func (m *SubAgentManager) AllDone() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.Status == "pending" || t.Status == "running" {
			return false
		}
	}
	return true
}

// GetTasks returns detached snapshots of all sub-agent tasks. The manager keeps
// mutable task state private so callers can safely inspect or encode the result
// while a sub-agent is still running.
func (m *SubAgentManager) GetTasks() []*SubAgentTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*SubAgentTask
	for _, t := range m.tasks {
		result = append(result, cloneSubAgentTask(t))
	}
	return result
}

// GetTask returns a detached snapshot of a specific task.
func (m *SubAgentManager) GetTask(id string) *SubAgentTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneSubAgentTask(m.tasks[id])
}

func cloneSubAgentTask(task *SubAgentTask) *SubAgentTask {
	if task == nil {
		return nil
	}
	clone := *task
	clone.Files = append([]string(nil), task.Files...)
	clone.Sandbox = append([]SandboxExecutionRecord(nil), task.Sandbox...)
	return &clone
}

// run adapts one sub-agent task to the common Agent Kernel. The run mode owns
// only the task-local terminal decision; RunLoopWithOptions remains the sole
// owner of provider calls, parsing, tools, safety, evidence, and recording.
func (m *SubAgentManager) run(task *SubAgentTask) {
	if task == nil {
		return
	}

	startedAt := time.Now()
	m.mu.Lock()
	task.Status = "running"
	task.StartedAt = startedAt
	m.mu.Unlock()

	log.Printf("[SubAgent] %s started: %s", task.ID, task.Name)

	parentContext := m.context
	if parentContext == nil {
		parentContext = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentContext, subAgentTimeout)
	defer cancel()

	mode := newSubAgentRunMode(task)
	anchor := defaultSubAgentPlanAnchor(task)
	reducer := newSubAgentEventReducer(task.ID, mode)
	events := make(chan Event, 64)
	go RunLoopWithOptions(
		ctx,
		m.provider,
		m.model,
		[]types.Message{{Role: "user", Content: mustJSON(task.Instruction)}},
		m.workDir,
		RunOptions{
			SessionID:         m.sessionID,
			ApprovalRequester: m.approvalRequester,
			Recorder:          m.recorder,
			WorkerID:          task.ID,
			OwnershipChecker:  m.ownershipChecker(task),
			RunMode:           mode,
			IterationLimit:    subAgentIterationLimit,
			HarnessProfile:    m.harnessProfile,
			CapabilityProfile: m.capabilityProfile,
			PlanAnchor:        anchor,
			SandboxRunner:     m.sandboxRunner,
			SandboxPolicy:     m.sandboxPolicy,
			PluginDirs:        cloneStringsPreserveNil(m.pluginDirs),
			PluginExecution:   clonePluginExecutionOptions(m.pluginExecution),
			DisablePlugins:    m.disablePlugins,
		},
		events,
	)
	for event := range events {
		reducer.Observe(event)
	}

	result := reducer.Result(ctx.Err())
	finishedAt := time.Now()
	m.mu.Lock()
	if result.Success {
		task.Status = "completed"
	} else {
		task.Status = "failed"
	}
	task.Result = result.Text
	task.ToolCalls = result.ToolCalls
	task.Sandbox = append([]SandboxExecutionRecord(nil), result.Sandbox...)
	task.FinishedAt = finishedAt
	m.mu.Unlock()

	if result.Success {
		log.Printf("[SubAgent] %s completed: %d tool calls, %.1fs",
			task.ID, result.ToolCalls, finishedAt.Sub(startedAt).Seconds())
		return
	}
	log.Printf("[SubAgent] %s failed after %d tool calls: %s",
		task.ID, result.ToolCalls, result.Text)
}

func defaultSubAgentPlanAnchor(task *SubAgentTask) *PlanAnchor {
	if task == nil {
		return nil
	}
	objective := strings.TrimSpace(task.Instruction)
	if objective == "" {
		objective = strings.TrimSpace(task.Name)
	}
	if objective == "" {
		objective = strings.TrimSpace(task.ID)
	}
	if objective == "" {
		objective = "Complete the assigned sub-agent task."
	}
	currentStep := strings.TrimSpace(task.ID)
	if currentStep == "" {
		currentStep = "sub-agent task"
	}
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:   objective,
		CurrentStep: currentStep,
		DefinitionOfDone: []string{
			"The assigned task is completed within its ownership scope and the final response reports the outcome or a blocking condition.",
		},
	})
	if err != nil {
		return nil
	}
	return &anchor
}

func (m *SubAgentManager) ownershipChecker(task *SubAgentTask) func(workerID, filePath string) (bool, string) {
	taskID := ""
	var files []string
	if task != nil {
		taskID = task.ID
		files = append([]string(nil), task.Files...)
	}
	workDir := m.workDir
	return func(workerID, filePath string) (bool, string) {
		if workerID != taskID || taskID == "" {
			return false, "Sub-agent worker identity does not match the assigned task"
		}
		if len(files) == 0 {
			return true, ""
		}
		for _, allowed := range files {
			if matchesOwnedPath(filePath, allowed, workDir) {
				return true, ""
			}
		}
		return false, "File target is outside the sub-agent's assigned scope"
	}
}

// isAllowedFile checks if a tool call targets files within the agent's scope.
func (m *SubAgentManager) isAllowedFile(task *SubAgentTask, toolName string, input json.RawMessage) bool {
	if len(task.Files) == 0 {
		return true // no restriction
	}

	// Only check file-modifying tools
	if toolName != "Write" && toolName != "Edit" {
		return true
	}

	var args struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(input, &args) != nil {
		return false
	}
	if args.FilePath == "" {
		return false
	}

	// Check if file matches any allowed pattern
	for _, allowed := range task.Files {
		if matchesOwnedPath(args.FilePath, allowed, m.workDir) {
			return true
		}
	}

	return false
}

func matchesOwnedPath(target, allowed, workDir string) bool {
	target = strings.TrimSpace(target)
	allowed = strings.TrimSpace(allowed)
	if target == "" || allowed == "" {
		return false
	}

	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return false
	}
	targetPath, err := canonicalPathWithinWorkspace(
		target,
		workDir,
		workspace,
		PermissionConfig{},
	)
	if err != nil {
		return false
	}

	allowedSlash := filepath.ToSlash(allowed)
	doubleStar := strings.HasSuffix(allowedSlash, "**")
	singleStar := !doubleStar && strings.HasSuffix(allowedSlash, "*")
	if doubleStar {
		baseRaw := strings.TrimRight(strings.TrimSuffix(allowedSlash, "**"), "/")
		if baseRaw == "" {
			baseRaw = "."
		}
		base, baseErr := canonicalPathWithinWorkspace(
			filepath.FromSlash(baseRaw),
			workDir,
			workspace,
			PermissionConfig{},
		)
		if baseErr != nil {
			return false
		}
		return pathWithin(targetPath, base)
	}
	if singleStar {
		prefixRaw := strings.TrimSuffix(allowedSlash, "*")
		directoryPattern := strings.HasSuffix(prefixRaw, "/")
		prefixPath := filepath.FromSlash(strings.TrimRight(prefixRaw, "/"))
		directoryRaw := filepath.Dir(prefixPath)
		namePrefix := filepath.Base(prefixPath)
		if directoryPattern {
			directoryRaw = prefixPath
			namePrefix = ""
		}
		if prefixPath == "" || prefixPath == "." {
			directoryRaw = "."
			namePrefix = ""
		}
		directory, directoryErr := canonicalPathWithinWorkspace(
			directoryRaw,
			workDir,
			workspace,
			PermissionConfig{},
		)
		if directoryErr != nil {
			return false
		}
		relative, relErr := filepath.Rel(directory, targetPath)
		return relErr == nil && relative != "." && filepath.Dir(relative) == "." &&
			strings.HasPrefix(filepath.Base(relative), namePrefix)
	}
	ownedPath, ownedErr := canonicalPathWithinWorkspace(
		allowed,
		workDir,
		workspace,
		PermissionConfig{},
	)
	if ownedErr != nil {
		return false
	}
	relative, relErr := filepath.Rel(ownedPath, targetPath)
	return relErr == nil && relative == "."
}
