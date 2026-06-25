package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TaskKind identifies the execution shape of an agent task.
type TaskKind string

const (
	TaskKindResearch  TaskKind = "research"
	TaskKindPropose   TaskKind = "propose"
	TaskKindBlueprint TaskKind = "blueprint"
	TaskKindImplement TaskKind = "implement"
	TaskKindReview    TaskKind = "review"
	TaskKindVerify    TaskKind = "verify"
)

// AgentTaskStatus values are intentionally aligned with TeamTask.Status.
const (
	AgentTaskPending   = "pending"
	AgentTaskRunning   = "running"
	AgentTaskCompleted = "completed"
	AgentTaskFailed    = "failed"
)

// CapacityConfig limits worker fan-out for local models and shared resources.
type CapacityConfig struct {
	ModelSlots              int  `json:"modelSlots,omitempty"`
	ToolSlots               int  `json:"toolSlots,omitempty"`
	WebFetchSlots           int  `json:"webFetchSlots,omitempty"`
	TestSlots               int  `json:"testSlots,omitempty"`
	MaxParallelTasks        int  `json:"maxParallelTasks,omitempty"`
	FileScopeLock           bool `json:"fileScopeLock,omitempty"`
	FileScopeLockConfigured bool `json:"-"`
}

// AgentTaskResources lets a task reserve more than one capacity slot.
type AgentTaskResources struct {
	ModelSlots    int `json:"modelSlots,omitempty"`
	ToolSlots     int `json:"toolSlots,omitempty"`
	WebFetchSlots int `json:"webFetchSlots,omitempty"`
	TestSlots     int `json:"testSlots,omitempty"`
}

// AgentSpec describes a role that can execute tasks in a team plan.
type AgentSpec struct {
	ID           string   `json:"id"`
	Role         string   `json:"role"`
	Description  string   `json:"description,omitempty"`
	SystemPrompt string   `json:"systemPrompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	ReadOnly     bool     `json:"readOnly,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
}

// AgentTask is the provider-neutral task contract used by Daedalus-style plans.
type AgentTask struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	Kind               TaskKind           `json:"kind,omitempty"`
	Stage              string             `json:"stage,omitempty"`
	Role               string             `json:"role,omitempty"`
	Goal               string             `json:"goal,omitempty"`
	Description        string             `json:"description,omitempty"`
	Files              []string           `json:"files,omitempty"`
	DependsOn          []string           `json:"dependsOn,omitempty"`
	Tools              []string           `json:"tools,omitempty"`
	AcceptanceCriteria []string           `json:"acceptanceCriteria,omitempty"`
	OutOfScope         []string           `json:"outOfScope,omitempty"`
	BlueprintNodes     []string           `json:"blueprintNodes,omitempty"`
	OutputContract     string             `json:"outputContract,omitempty"`
	ReadOnly           bool               `json:"readOnly,omitempty"`
	Provider           string             `json:"provider,omitempty"`
	Model              string             `json:"model,omitempty"`
	Priority           int                `json:"priority,omitempty"`
	Resources          AgentTaskResources `json:"resources,omitempty"`
	CreatedAt          time.Time          `json:"createdAt,omitempty"`
}

// TeamPlanStage groups tasks into Daedalus-style phases.
type TeamPlanStage struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind,omitempty"`
	TaskIDs []string `json:"taskIds,omitempty"`
}

// TeamPlan is the durable execution contract surrounding a task loop.
type TeamPlan struct {
	Version       int             `json:"version"`
	Name          string          `json:"name"`
	Objective     string          `json:"objective"`
	VerifyCommand string          `json:"verifyCommand,omitempty"`
	Agents        []AgentSpec     `json:"agents,omitempty"`
	Stages        []TeamPlanStage `json:"stages,omitempty"`
	Tasks         []AgentTask     `json:"tasks"`
	Capacity      CapacityConfig  `json:"capacity,omitempty"`
}

// TeamPlanValidationError aggregates plan contract problems found before run.
type TeamPlanValidationError struct {
	Problems []string
}

func (e TeamPlanValidationError) Error() string {
	return "invalid TeamPlan: " + strings.Join(e.Problems, "; ")
}

// DefaultLocalCapacity is conservative for Ollama/SGLang. Only one model worker
// runs by default; tools and web fetches may still fan out inside that worker.
func DefaultLocalCapacity() CapacityConfig {
	return CapacityConfig{
		ModelSlots:       1,
		ToolSlots:        3,
		WebFetchSlots:    4,
		TestSlots:        1,
		MaxParallelTasks: 2,
		FileScopeLock:    true,
	}
}

// DefaultCloudCapacity keeps existing wave-style fan-out for stronger providers.
func DefaultCloudCapacity() CapacityConfig {
	return CapacityConfig{
		ModelSlots:       4,
		ToolSlots:        8,
		WebFetchSlots:    8,
		TestSlots:        2,
		MaxParallelTasks: 4,
		FileScopeLock:    true,
	}
}

func (c *CapacityConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		ModelSlots       int   `json:"modelSlots,omitempty"`
		ToolSlots        int   `json:"toolSlots,omitempty"`
		WebFetchSlots    int   `json:"webFetchSlots,omitempty"`
		TestSlots        int   `json:"testSlots,omitempty"`
		MaxParallelTasks int   `json:"maxParallelTasks,omitempty"`
		FileScopeLock    *bool `json:"fileScopeLock,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ModelSlots = raw.ModelSlots
	c.ToolSlots = raw.ToolSlots
	c.WebFetchSlots = raw.WebFetchSlots
	c.TestSlots = raw.TestSlots
	c.MaxParallelTasks = raw.MaxParallelTasks
	c.FileScopeLockConfigured = raw.FileScopeLock != nil
	if raw.FileScopeLock != nil {
		c.FileScopeLock = *raw.FileScopeLock
	}
	return nil
}

func (c CapacityConfig) MarshalJSON() ([]byte, error) {
	var fileScopeLock *bool
	if c.FileScopeLock || c.FileScopeLockConfigured {
		value := c.FileScopeLock
		fileScopeLock = &value
	}
	return json.Marshal(struct {
		ModelSlots       int   `json:"modelSlots,omitempty"`
		ToolSlots        int   `json:"toolSlots,omitempty"`
		WebFetchSlots    int   `json:"webFetchSlots,omitempty"`
		TestSlots        int   `json:"testSlots,omitempty"`
		MaxParallelTasks int   `json:"maxParallelTasks,omitempty"`
		FileScopeLock    *bool `json:"fileScopeLock,omitempty"`
	}{
		ModelSlots:       c.ModelSlots,
		ToolSlots:        c.ToolSlots,
		WebFetchSlots:    c.WebFetchSlots,
		TestSlots:        c.TestSlots,
		MaxParallelTasks: c.MaxParallelTasks,
		FileScopeLock:    fileScopeLock,
	})
}

// Normalized fills defaults while preserving explicit non-zero settings.
func (c CapacityConfig) Normalized(local bool) CapacityConfig {
	defaults := DefaultCloudCapacity()
	if local {
		defaults = DefaultLocalCapacity()
	}
	if c.ModelSlots <= 0 {
		c.ModelSlots = defaults.ModelSlots
	}
	if c.ToolSlots <= 0 {
		c.ToolSlots = defaults.ToolSlots
	}
	if c.WebFetchSlots <= 0 {
		c.WebFetchSlots = defaults.WebFetchSlots
	}
	if c.TestSlots <= 0 {
		c.TestSlots = defaults.TestSlots
	}
	if c.MaxParallelTasks <= 0 {
		c.MaxParallelTasks = defaults.MaxParallelTasks
	}
	if !c.FileScopeLock && !c.FileScopeLockConfigured {
		c.FileScopeLock = defaults.FileScopeLock
	}
	return c
}

// ModelTaskLimit returns the runnable model-backed task cap.
func (c CapacityConfig) ModelTaskLimit(fallback int) int {
	if fallback <= 0 {
		fallback = 1
	}
	limit := fallback
	if c.MaxParallelTasks > 0 && c.MaxParallelTasks < limit {
		limit = c.MaxParallelTasks
	}
	if c.ModelSlots > 0 && c.ModelSlots < limit {
		limit = c.ModelSlots
	}
	if limit <= 0 {
		return 1
	}
	return limit
}

// NewDaedalusTeamPlan creates a minimal PM/Worker plan skeleton from one goal.
func NewDaedalusTeamPlan(name, objective string) TeamPlan {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "daedalus-plan"
	}
	return TeamPlan{
		Version:   1,
		Name:      name,
		Objective: strings.TrimSpace(objective),
		Capacity:  DefaultLocalCapacity(),
		Agents: []AgentSpec{
			{ID: "planner", Role: "planner", ReadOnly: true},
			{ID: "implementer", Role: "coder"},
			{ID: "reviewer", Role: "reviewer", ReadOnly: true},
		},
		Stages: []TeamPlanStage{
			{ID: "research", Name: "Research and proposal", Kind: string(TaskKindResearch), TaskIDs: []string{"research"}},
			{ID: "blueprint", Name: "Blueprint", Kind: string(TaskKindBlueprint), TaskIDs: []string{"blueprint"}},
			{ID: "implement", Name: "Implementation", Kind: string(TaskKindImplement), TaskIDs: []string{"implement"}},
			{ID: "verify", Name: "Review and verification", Kind: string(TaskKindVerify), TaskIDs: []string{"review", "verify"}},
		},
		Tasks: []AgentTask{
			{
				ID:          "research",
				Name:        "Research approach",
				Kind:        TaskKindResearch,
				Stage:       "research",
				Role:        "planner",
				Goal:        "Inspect the project and propose the smallest viable implementation plan.",
				Description: objective,
				ReadOnly:    true,
			},
			{
				ID:          "blueprint",
				Name:        "Define execution blueprint",
				Kind:        TaskKindBlueprint,
				Stage:       "blueprint",
				Role:        "planner",
				Goal:        "Turn the approved approach into explicit task boundaries, dependencies, and verification evidence.",
				DependsOn:   []string{"research"},
				Description: objective,
				ReadOnly:    true,
			},
			{
				ID:          "implement",
				Name:        "Implement scoped change",
				Kind:        TaskKindImplement,
				Stage:       "implement",
				Role:        "coder",
				Goal:        "Implement the scoped change with minimal edits.",
				DependsOn:   []string{"blueprint"},
				Description: objective,
				Files:       []string{"**"},
			},
			{
				ID:          "review",
				Name:        "Review implementation",
				Kind:        TaskKindReview,
				Stage:       "verify",
				Role:        "reviewer",
				Goal:        "Check correctness, scope drift, safety, and missing tests.",
				DependsOn:   []string{"implement"},
				Description: objective,
				ReadOnly:    true,
			},
			{
				ID:          "verify",
				Name:        "Run verification",
				Kind:        TaskKindVerify,
				Stage:       "verify",
				Role:        "tester",
				Goal:        "Run the configured verification command and report the exact result.",
				DependsOn:   []string{"review"},
				Description: objective,
				ReadOnly:    true,
			},
		},
	}
}

// LoadTeamPlan reads a TeamPlan JSON document from disk.
func LoadTeamPlan(path string) (TeamPlan, error) {
	var plan TeamPlan
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return plan, err
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, err
	}
	if plan.Version == 0 {
		plan.Version = 1
	}
	return plan.Normalized(), nil
}

// SaveTeamPlan writes a TeamPlan JSON document to disk.
func SaveTeamPlan(path string, plan TeamPlan) error {
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(plan.Normalized(), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// Normalized fills plan defaults and normalizes all task contracts.
func (p TeamPlan) Normalized() TeamPlan {
	if p.Version == 0 {
		p.Version = 1
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = "team-plan"
	}
	p.Objective = strings.TrimSpace(p.Objective)
	p.Capacity = p.Capacity.Normalized(true)
	for i := range p.Agents {
		p.Agents[i].ID = strings.TrimSpace(p.Agents[i].ID)
		p.Agents[i].Role = strings.TrimSpace(p.Agents[i].Role)
	}
	for i := range p.Stages {
		p.Stages[i].ID = strings.TrimSpace(p.Stages[i].ID)
		p.Stages[i].Name = strings.TrimSpace(p.Stages[i].Name)
	}
	for i := range p.Tasks {
		p.Tasks[i] = p.Tasks[i].Normalized()
	}
	return p
}

// ValidateTeamPlan rejects malformed plans before they spawn workers.
func ValidateTeamPlan(plan TeamPlan) error {
	plan = plan.Normalized()
	var problems []string
	if strings.TrimSpace(plan.Objective) == "" {
		problems = append(problems, "objective is required")
	}
	if len(plan.Tasks) == 0 {
		problems = append(problems, "at least one task is required")
	}

	taskIDs := map[string]bool{}
	for _, task := range plan.Tasks {
		if task.ID == "" {
			problems = append(problems, "task id is required")
			continue
		}
		if taskIDs[task.ID] {
			problems = append(problems, fmt.Sprintf("duplicate task id %q", task.ID))
			continue
		}
		taskIDs[task.ID] = true
		if strings.TrimSpace(task.Goal) == "" && strings.TrimSpace(task.Description) == "" {
			problems = append(problems, fmt.Sprintf("task %q needs goal or description", task.ID))
		}
		if !task.IsReadOnly() && len(task.Files) == 0 {
			problems = append(problems, fmt.Sprintf("write-capable task %q needs file scope", task.ID))
		}
	}

	for _, task := range plan.Tasks {
		for _, dep := range task.DependsOn {
			if !taskIDs[dep] {
				problems = append(problems, fmt.Sprintf("task %q depends on unknown task %q", task.ID, dep))
			}
			if dep == task.ID {
				problems = append(problems, fmt.Sprintf("task %q depends on itself", task.ID))
			}
		}
	}

	stageIDs := map[string]bool{}
	for _, stage := range plan.Stages {
		if stage.ID == "" {
			problems = append(problems, "stage id is required")
			continue
		}
		if stageIDs[stage.ID] {
			problems = append(problems, fmt.Sprintf("duplicate stage id %q", stage.ID))
		}
		stageIDs[stage.ID] = true
		for _, taskID := range stage.TaskIDs {
			if !taskIDs[taskID] {
				problems = append(problems, fmt.Sprintf("stage %q references unknown task %q", stage.ID, taskID))
			}
		}
	}
	for _, task := range plan.Tasks {
		if task.Stage != "" && len(stageIDs) > 0 && !stageIDs[task.Stage] {
			problems = append(problems, fmt.Sprintf("task %q references unknown stage %q", task.ID, task.Stage))
		}
	}

	if hasTaskDependencyCycle(plan.Tasks) {
		problems = append(problems, "task dependency graph has a cycle")
	}
	if len(problems) > 0 {
		return TeamPlanValidationError{Problems: problems}
	}
	return nil
}

func hasTaskDependencyCycle(tasks []AgentTask) bool {
	graph := map[string][]string{}
	for _, task := range tasks {
		graph[task.ID] = append([]string(nil), task.DependsOn...)
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, dep := range graph[id] {
			if _, ok := graph[dep]; !ok {
				continue
			}
			if visit(dep) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range graph {
		if visit(id) {
			return true
		}
	}
	return false
}

// LoadAgentTask reads one AgentTask JSON document from disk.
func LoadAgentTask(path string) (AgentTask, error) {
	var task AgentTask
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return task, err
	}
	if err := json.Unmarshal(data, &task); err != nil {
		return task, err
	}
	return task.Normalized(), nil
}

// Normalized fills task fields needed by the runner.
func (t AgentTask) Normalized() AgentTask {
	t.ID = strings.TrimSpace(t.ID)
	t.Name = strings.TrimSpace(t.Name)
	if t.ID == "" {
		t.ID = "task"
	}
	if t.Name == "" {
		t.Name = t.ID
	}
	if t.Kind == "" {
		t.Kind = TaskKindImplement
	}
	if t.Role == "" {
		t.Role = defaultRoleForTaskKind(t.Kind)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	return t
}

// ToTeamTask converts the provider-neutral task to the existing Team runner.
func (t AgentTask) ToTeamTask() TeamTask {
	t = t.Normalized()
	return TeamTask{
		ID:          t.ID,
		Name:        t.Name,
		Description: t.WorkerPrompt(),
		Kind:        t.Kind,
		Role:        t.Role,
		Provider:    t.Provider,
		Model:       t.Model,
		ReadOnly:    t.IsReadOnly(),
		Resources:   t.Resources,
		Files:       append([]string(nil), t.Files...),
		DependsOn:   append([]string(nil), t.DependsOn...),
		CreatedAt:   t.CreatedAt,
	}
}

// ToTeamTasks converts every task in the plan to TeamTask values.
func (p TeamPlan) ToTeamTasks() []TeamTask {
	tasks := make([]TeamTask, 0, len(p.Tasks))
	for _, task := range p.Tasks {
		tasks = append(tasks, task.ToTeamTask())
	}
	return tasks
}

// WorkerPrompt renders the exact contract handed to a worker loop.
func (t AgentTask) WorkerPrompt() string {
	t = t.Normalized()
	var b strings.Builder
	fmt.Fprintf(&b, "Role: %s\n", t.Role)
	fmt.Fprintf(&b, "Kind: %s\n", t.Kind)
	if t.Stage != "" {
		fmt.Fprintf(&b, "Stage: %s\n", t.Stage)
	}
	if t.Goal != "" {
		fmt.Fprintf(&b, "\nGoal:\n%s\n", strings.TrimSpace(t.Goal))
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "\nDetails:\n%s\n", strings.TrimSpace(t.Description))
	}
	writeList := func(title string, values []string) {
		if len(values) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				fmt.Fprintf(&b, "- %s\n", value)
			}
		}
	}
	writeList("Acceptance criteria", t.AcceptanceCriteria)
	writeList("Out of scope", t.OutOfScope)
	writeList("Blueprint nodes", t.BlueprintNodes)
	writeList("Allowed tools", t.Tools)
	if t.OutputContract != "" {
		fmt.Fprintf(&b, "\nOutput contract:\n%s\n", strings.TrimSpace(t.OutputContract))
	}
	if t.IsReadOnly() {
		b.WriteString("\nThis task is read-only. Do not modify files.\n")
	}
	return strings.TrimSpace(b.String())
}

// IsReadOnly reports whether a task should avoid file mutations.
func (t AgentTask) IsReadOnly() bool {
	if t.ReadOnly {
		return true
	}
	switch t.Kind {
	case TaskKindResearch, TaskKindPropose, TaskKindBlueprint, TaskKindReview:
		return true
	default:
		return false
	}
}

func defaultRoleForTaskKind(kind TaskKind) string {
	switch kind {
	case TaskKindResearch, TaskKindPropose, TaskKindBlueprint:
		return "planner"
	case TaskKindReview:
		return "reviewer"
	case TaskKindVerify:
		return "tester"
	default:
		return "coder"
	}
}

// CapacityScheduler selects ready tasks without exceeding local-model capacity.
type CapacityScheduler struct {
	Capacity CapacityConfig
}

func NewCapacityScheduler(capacity CapacityConfig) CapacityScheduler {
	return CapacityScheduler{Capacity: capacity}
}

// SelectRunnable returns pending tasks whose dependencies and locks allow them
// to run now. Status values use AgentTaskStatus constants.
func (s CapacityScheduler) SelectRunnable(tasks []AgentTask, statuses map[string]string, running []AgentTask) []AgentTask {
	capacity := s.Capacity.Normalized(true)
	modelSlotsUsed := 0
	for _, task := range running {
		modelSlotsUsed += taskModelSlots(task)
	}
	availableModelSlots := capacity.ModelSlots - modelSlotsUsed
	if availableModelSlots <= 0 {
		return nil
	}

	candidates := make([]AgentTask, 0, len(tasks))
	for _, task := range tasks {
		task = task.Normalized()
		status := statuses[task.ID]
		if status != "" && status != AgentTaskPending {
			continue
		}
		if !dependenciesComplete(task, statuses) {
			continue
		}
		candidates = append(candidates, task)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].ID < candidates[j].ID
	})

	limit := capacity.ModelTaskLimit(capacity.MaxParallelTasks)
	if len(running) >= limit {
		return nil
	}
	availableTasks := limit - len(running)

	selected := make([]AgentTask, 0, availableTasks)
	for _, task := range candidates {
		if len(selected) >= availableTasks {
			break
		}
		needed := taskModelSlots(task)
		if needed > availableModelSlots {
			continue
		}
		if capacity.FileScopeLock && conflictsWithAny(task, append(running, selected...)) {
			continue
		}
		selected = append(selected, task)
		availableModelSlots -= needed
	}
	return selected
}

func dependenciesComplete(task AgentTask, statuses map[string]string) bool {
	for _, dep := range task.DependsOn {
		if statuses[dep] != AgentTaskCompleted {
			return false
		}
	}
	return true
}

func taskModelSlots(task AgentTask) int {
	if task.Resources.ModelSlots > 0 {
		return task.Resources.ModelSlots
	}
	return 1
}

func conflictsWithAny(task AgentTask, others []AgentTask) bool {
	for _, other := range others {
		if fileScopesConflict(task, other) {
			return true
		}
	}
	return false
}

func fileScopesConflict(a, b AgentTask) bool {
	if a.IsReadOnly() || b.IsReadOnly() {
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

func ownershipPatternsOverlap(a, b string) bool {
	a = strings.TrimSpace(filepath.ToSlash(a))
	b = strings.TrimSpace(filepath.ToSlash(b))
	if a == "" || b == "" {
		return true
	}
	if a == b {
		return true
	}
	ap := ownershipPrefix(a)
	bp := ownershipPrefix(b)
	return strings.HasPrefix(ap, bp) || strings.HasPrefix(bp, ap)
}

func ownershipPrefix(pattern string) string {
	pattern = strings.TrimSuffix(pattern, "/**")
	pattern = strings.TrimSuffix(pattern, "/*")
	pattern = strings.TrimSuffix(pattern, "*")
	return strings.TrimRight(pattern, "/")
}
