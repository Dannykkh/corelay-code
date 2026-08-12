package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestDefaultLocalCapacity(t *testing.T) {
	capacity := CapacityConfig{}.Normalized(true)
	if capacity.ModelSlots != 1 {
		t.Fatalf("local model slots = %d, want 1", capacity.ModelSlots)
	}
	if capacity.ToolSlots < 2 || capacity.WebFetchSlots < 2 {
		t.Fatalf("local tool/web capacity too small: %+v", capacity)
	}
	if !capacity.FileScopeLock {
		t.Fatal("local file scope lock should default on")
	}
}

func TestCapacityConfigPreservesExplicitFileScopeLockFalse(t *testing.T) {
	var capacity CapacityConfig
	if err := json.Unmarshal([]byte(`{"modelSlots":2,"fileScopeLock":false}`), &capacity); err != nil {
		t.Fatalf("unmarshal capacity: %v", err)
	}
	normalized := capacity.Normalized(true)
	if normalized.FileScopeLock {
		t.Fatalf("explicit fileScopeLock=false should be preserved: %+v", normalized)
	}
	if !normalized.FileScopeLockConfigured {
		t.Fatalf("explicit fileScopeLock should be tracked: %+v", normalized)
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("marshal capacity: %v", err)
	}
	if !strings.Contains(string(data), `"fileScopeLock":false`) {
		t.Fatalf("explicit false should remain serializable, got %s", data)
	}

	var omitted CapacityConfig
	if err := json.Unmarshal([]byte(`{"modelSlots":2}`), &omitted); err != nil {
		t.Fatalf("unmarshal omitted capacity: %v", err)
	}
	if !omitted.Normalized(true).FileScopeLock {
		t.Fatalf("omitted fileScopeLock should default on: %+v", omitted.Normalized(true))
	}
}

func TestCapacitySchedulerRespectsDependencies(t *testing.T) {
	tasks := []AgentTask{
		{ID: "research", Kind: TaskKindResearch, Priority: 1, ReadOnly: true},
		{ID: "implement", Kind: TaskKindImplement, DependsOn: []string{"research"}, Priority: 10},
	}
	scheduler := NewCapacityScheduler(CapacityConfig{ModelSlots: 2, MaxParallelTasks: 2, FileScopeLock: true})

	selected := scheduler.SelectRunnable(tasks, map[string]string{}, nil)
	if len(selected) != 1 || selected[0].ID != "research" {
		t.Fatalf("selected before dependency complete = %+v, want research only", selected)
	}

	selected = scheduler.SelectRunnable(tasks, map[string]string{"research": AgentTaskCompleted}, nil)
	if len(selected) != 1 || selected[0].ID != "implement" {
		t.Fatalf("selected after dependency complete = %+v, want implement", selected)
	}
}

func TestCapacitySchedulerRespectsFileScopeLocks(t *testing.T) {
	tasks := []AgentTask{
		{ID: "impl-a", Kind: TaskKindImplement, Files: []string{"src/api/**"}, Priority: 10},
		{ID: "impl-b", Kind: TaskKindImplement, Files: []string{"src/api/handler.go"}, Priority: 9},
		{ID: "research", Kind: TaskKindResearch, ReadOnly: true, Priority: 1},
	}
	running := []AgentTask{
		{ID: "running", Kind: TaskKindImplement, Files: []string{"src/api/**"}},
	}
	scheduler := NewCapacityScheduler(CapacityConfig{ModelSlots: 2, MaxParallelTasks: 2, FileScopeLock: true})

	selected := scheduler.SelectRunnable(tasks, map[string]string{}, running)
	if len(selected) != 1 || selected[0].ID != "research" {
		t.Fatalf("selected = %+v, want only read-only research because api scope is locked", selected)
	}
}

func TestAgentTaskToTeamTaskIncludesContract(t *testing.T) {
	task := AgentTask{
		ID:        "implement-search",
		Name:      "Implement search",
		Kind:      TaskKindImplement,
		Role:      "coder",
		Provider:  "ollama",
		Model:     "qwen3:14b",
		Goal:      "Add provider-neutral search.",
		Files:     []string{"internal/agent/**"},
		DependsOn: []string{"blueprint"},
		Resources: AgentTaskResources{ModelSlots: 2, WebFetchSlots: 1},
		AcceptanceCriteria: []string{
			"search works without provider API keys",
			"go test ./internal/agent passes",
		},
		OutOfScope: []string{"UI redesign"},
	}

	teamTask := task.ToTeamTask()
	if teamTask.ID != task.ID || teamTask.Name != task.Name {
		t.Fatalf("identity mismatch: %+v", teamTask)
	}
	if len(teamTask.DependsOn) != 1 || teamTask.DependsOn[0] != "blueprint" {
		t.Fatalf("dependsOn mismatch: %+v", teamTask.DependsOn)
	}
	if teamTask.Provider != "ollama" || teamTask.Model != "qwen3:14b" || teamTask.Role != "coder" || teamTask.Kind != TaskKindImplement {
		t.Fatalf("runtime metadata mismatch: %+v", teamTask)
	}
	if teamTask.Resources.ModelSlots != 2 || teamTask.Resources.WebFetchSlots != 1 {
		t.Fatalf("resource metadata mismatch: %+v", teamTask.Resources)
	}
	for _, want := range []string{
		"Role: coder",
		"Goal:",
		"Acceptance criteria:",
		"search works without provider API keys",
		"Out of scope:",
		"UI redesign",
	} {
		if !strings.Contains(teamTask.Description, want) {
			t.Fatalf("team task description missing %q:\n%s", want, teamTask.Description)
		}
	}
}

func TestResolveTaskRuntimeUsesProviderAndModelOverride(t *testing.T) {
	team := NewTeam(testNamedProvider{name: "ollama"}, "qwen3:8b", t.TempDir(), t.TempDir(), TeamConfig{
		Name: "runtime-test",
		ProviderFactory: func(name string) (types.Provider, error) {
			return testNamedProvider{name: name}, nil
		},
	})

	provider, model, err := team.resolveTaskRuntime(&TeamTask{
		Provider: "openai",
		Model:    "gpt-test",
	})
	if err != nil {
		t.Fatalf("resolveTaskRuntime returned error: %v", err)
	}
	if provider.Name() != "openai" || model != "gpt-test" {
		t.Fatalf("runtime = %s/%s, want openai/gpt-test", provider.Name(), model)
	}
}

func TestResolveTaskRuntimeRejectsProviderOverrideWithoutFactory(t *testing.T) {
	team := NewTeam(testNamedProvider{name: "ollama"}, "qwen3:8b", t.TempDir(), t.TempDir(), TeamConfig{Name: "runtime-test"})
	_, _, err := team.resolveTaskRuntime(&TeamTask{Provider: "openai"})
	if err == nil || !strings.Contains(err.Error(), "no provider factory") {
		t.Fatalf("expected provider factory error, got %v", err)
	}
}

type testNamedProvider struct {
	name string
}

func (p testNamedProvider) Name() string              { return p.name }
func (p testNamedProvider) DisplayName() string       { return p.name }
func (p testNamedProvider) Models() []types.ModelInfo { return nil }
func (p testNamedProvider) Validate() error           { return nil }
func (p testNamedProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	ch := make(chan types.SSEEvent)
	close(ch)
	return ch, nil
}

func TestNewDaedalusTeamPlanHasOrderedPhases(t *testing.T) {
	plan := NewDaedalusTeamPlan("local-loop", "Implement an Ollama worker harness.")
	if plan.Version != 1 || plan.Name != "local-loop" {
		t.Fatalf("unexpected plan identity: %+v", plan)
	}
	if len(plan.Tasks) != 5 {
		t.Fatalf("tasks = %d, want 5", len(plan.Tasks))
	}
	if plan.Tasks[2].ID != "implement" || len(plan.Tasks[2].DependsOn) != 1 || plan.Tasks[2].DependsOn[0] != "blueprint" {
		t.Fatalf("implement task dependency mismatch: %+v", plan.Tasks[2])
	}
	if plan.Capacity.ModelSlots != 1 {
		t.Fatalf("daedalus local plan should default to one model slot: %+v", plan.Capacity)
	}
	if err := ValidateTeamPlan(plan); err != nil {
		t.Fatalf("generated daedalus plan should validate: %v", err)
	}
}

func TestValidateTeamPlanRejectsMalformedPlans(t *testing.T) {
	tests := []struct {
		name string
		plan TeamPlan
		want string
	}{
		{
			name: "duplicate task",
			plan: TeamPlan{
				Objective: "test duplicate tasks",
				Tasks: []AgentTask{
					{ID: "a", Kind: TaskKindResearch, Goal: "read", ReadOnly: true},
					{ID: "a", Kind: TaskKindResearch, Goal: "read again", ReadOnly: true},
				},
			},
			want: `duplicate task id "a"`,
		},
		{
			name: "unknown dependency",
			plan: TeamPlan{
				Objective: "test dependency",
				Tasks: []AgentTask{
					{ID: "a", Kind: TaskKindResearch, Goal: "read", DependsOn: []string{"missing"}, ReadOnly: true},
				},
			},
			want: `depends on unknown task "missing"`,
		},
		{
			name: "cycle",
			plan: TeamPlan{
				Objective: "test cycle",
				Tasks: []AgentTask{
					{ID: "a", Kind: TaskKindResearch, Goal: "a", DependsOn: []string{"b"}, ReadOnly: true},
					{ID: "b", Kind: TaskKindResearch, Goal: "b", DependsOn: []string{"a"}, ReadOnly: true},
				},
			},
			want: "dependency graph has a cycle",
		},
		{
			name: "missing file scope",
			plan: TeamPlan{
				Objective: "test scope",
				Tasks: []AgentTask{
					{ID: "write", Kind: TaskKindImplement, Goal: "write code"},
				},
			},
			want: `write-capable task "write" needs file scope`,
		},
		{
			name: "stage references missing task",
			plan: TeamPlan{
				Objective: "test stage",
				Stages:    []TeamPlanStage{{ID: "s1", TaskIDs: []string{"missing"}}},
				Tasks:     []AgentTask{{ID: "a", Kind: TaskKindResearch, Goal: "read", ReadOnly: true}},
			},
			want: `stage "s1" references unknown task "missing"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTeamPlan(tt.plan)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateTeamPlan error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateTeamPlanAcceptsValidPlan(t *testing.T) {
	plan := TeamPlan{
		Objective: "ship a validated plan",
		Stages: []TeamPlanStage{
			{ID: "research", TaskIDs: []string{"research"}},
			{ID: "implement", TaskIDs: []string{"implement"}},
		},
		Tasks: []AgentTask{
			{ID: "research", Kind: TaskKindResearch, Stage: "research", Goal: "inspect", ReadOnly: true},
			{ID: "implement", Kind: TaskKindImplement, Stage: "implement", Goal: "change code", DependsOn: []string{"research"}, Files: []string{"internal/agent/**"}},
		},
	}
	if err := ValidateTeamPlan(plan); err != nil {
		t.Fatalf("ValidateTeamPlan returned error: %v", err)
	}
}
