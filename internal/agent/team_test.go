package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComputeWaves_NoDeps(t *testing.T) {
	team := &Team{
		config: TeamConfig{Name: "test", MaxWaveSize: 5},
		tasks: []*TeamTask{
			{ID: "a", Name: "Task A"},
			{ID: "b", Name: "Task B"},
			{ID: "c", Name: "Task C"},
		},
	}

	waves, err := team.ComputeWaves()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// All independent → 1 wave
	if len(waves) != 1 {
		t.Errorf("Expected 1 wave, got %d: %v", len(waves), waves)
	}
	if len(waves[0]) != 3 {
		t.Errorf("Expected 3 tasks in wave 1, got %d", len(waves[0]))
	}
}

func TestComputeWaves_LinearDeps(t *testing.T) {
	team := &Team{
		config: TeamConfig{Name: "test", MaxWaveSize: 5},
		tasks: []*TeamTask{
			{ID: "a", Name: "Task A"},
			{ID: "b", Name: "Task B", DependsOn: []string{"a"}},
			{ID: "c", Name: "Task C", DependsOn: []string{"b"}},
		},
	}

	waves, err := team.ComputeWaves()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// a → b → c = 3 waves
	if len(waves) != 3 {
		t.Errorf("Expected 3 waves, got %d: %v", len(waves), waves)
	}
}

func TestComputeWaves_DiamondDeps(t *testing.T) {
	//   a
	//  / \
	// b   c
	//  \ /
	//   d
	team := &Team{
		config: TeamConfig{Name: "test", MaxWaveSize: 5},
		tasks: []*TeamTask{
			{ID: "a", Name: "Foundation"},
			{ID: "b", Name: "Frontend", DependsOn: []string{"a"}},
			{ID: "c", Name: "Backend", DependsOn: []string{"a"}},
			{ID: "d", Name: "Integration", DependsOn: []string{"b", "c"}},
		},
	}

	waves, err := team.ComputeWaves()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Wave 1: a, Wave 2: b+c (parallel), Wave 3: d
	if len(waves) != 3 {
		t.Errorf("Expected 3 waves, got %d: %v", len(waves), waves)
	}
	if len(waves[0]) != 1 || waves[0][0] != "a" {
		t.Errorf("Wave 1 should be [a], got %v", waves[0])
	}
	if len(waves[1]) != 2 {
		t.Errorf("Wave 2 should have 2 tasks (b,c), got %d", len(waves[1]))
	}
	if len(waves[2]) != 1 || waves[2][0] != "d" {
		t.Errorf("Wave 3 should be [d], got %v", waves[2])
	}
}

func TestComputeWaves_CircularDeps(t *testing.T) {
	team := &Team{
		config: TeamConfig{Name: "test", MaxWaveSize: 5},
		tasks: []*TeamTask{
			{ID: "a", DependsOn: []string{"b"}},
			{ID: "b", DependsOn: []string{"a"}},
		},
	}

	_, err := team.ComputeWaves()
	if err == nil {
		t.Error("Expected circular dependency error")
	}
}

func TestComputeWaves_MaxWaveSize(t *testing.T) {
	team := &Team{
		config: TeamConfig{Name: "test", MaxWaveSize: 2},
		tasks: []*TeamTask{
			{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"},
		},
	}

	waves, err := team.ComputeWaves()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// 5 independent tasks, max 2 per wave → 3 waves (2+2+1)
	if len(waves) != 3 {
		t.Errorf("Expected 3 waves (max 2 per wave), got %d: %v", len(waves), waves)
	}
}

func TestCheckFileOwnership(t *testing.T) {
	team := &Team{
		config: TeamConfig{Name: "test"},
		tasks: []*TeamTask{
			{ID: "t1", AssignedTo: "worker-t1", Status: "running", Files: []string{"src/api/**"}},
			{ID: "t2", AssignedTo: "worker-t2", Status: "running", Files: []string{"src/ui/**"}},
		},
	}

	// Worker 1 can modify its own files
	ok, _ := team.CheckFileOwnership("worker-t1", "src/api/handler.go")
	if !ok {
		t.Error("Worker should be able to modify its own files")
	}

	// Worker 1 cannot modify worker 2's files
	ok, reason := team.CheckFileOwnership("worker-t1", "src/ui/App.tsx")
	if ok {
		t.Error("Worker should NOT be able to modify another worker's files")
	}
	if reason == "" {
		t.Error("Should provide reason for denial")
	}

	// Unowned files are allowed
	ok, _ = team.CheckFileOwnership("worker-t1", "README.md")
	if !ok {
		t.Error("Unowned files should be allowed")
	}
}

func TestSelectWaveBatchRespectsModelResources(t *testing.T) {
	team := &Team{
		config: TeamConfig{
			Name:     "test",
			Capacity: CapacityConfig{ModelSlots: 2, ToolSlots: 2, WebFetchSlots: 2, TestSlots: 1, MaxParallelTasks: 3, FileScopeLock: true},
		},
	}
	batch := team.selectWaveBatch([]*TeamTask{
		{ID: "a", Kind: TaskKindImplement, Files: []string{"src/api/**"}, Resources: AgentTaskResources{ModelSlots: 2}},
		{ID: "b", Kind: TaskKindImplement, Files: []string{"src/ui/**"}, Resources: AgentTaskResources{ModelSlots: 1}},
		{ID: "c", Kind: TaskKindReview, ReadOnly: true, Resources: AgentTaskResources{ModelSlots: 1}},
	})

	if len(batch) != 1 || batch[0].ID != "a" {
		t.Fatalf("batch = %s, want only a because it consumes both model slots", teamTaskIDs(batch))
	}
}

func TestSelectWaveBatchRespectsFileScopeLocks(t *testing.T) {
	team := &Team{
		config: TeamConfig{
			Name:     "test",
			Capacity: CapacityConfig{ModelSlots: 3, ToolSlots: 2, WebFetchSlots: 2, TestSlots: 1, MaxParallelTasks: 3, FileScopeLock: true},
		},
	}
	batch := team.selectWaveBatch([]*TeamTask{
		{ID: "a", Kind: TaskKindImplement, Files: []string{"src/api/**"}},
		{ID: "b", Kind: TaskKindImplement, Files: []string{"src/api/handler.go"}},
		{ID: "c", Kind: TaskKindReview, ReadOnly: true},
	})

	if got := teamTaskIDs(batch); got != "a,c" {
		t.Fatalf("batch = %s, want a,c; b conflicts with a's file scope", got)
	}
}

func TestExecuteToolWithOptionsUsesWorkerScopedOwnership(t *testing.T) {
	oldChecker := FileOwnershipChecker
	oldWorkerID := activeWorkerID
	defer func() {
		FileOwnershipChecker = oldChecker
		activeWorkerID = oldWorkerID
	}()

	globalCalled := false
	FileOwnershipChecker = func(string, string) (bool, string) {
		globalCalled = true
		return true, ""
	}
	activeWorkerID = "worker-t2"

	team := &Team{
		config: TeamConfig{Name: "test"},
		tasks: []*TeamTask{
			{ID: "t1", AssignedTo: "worker-t1", Status: "running", Files: []string{"src/api/**"}},
			{ID: "t2", AssignedTo: "worker-t2", Status: "running", Files: []string{"src/ui/**"}},
		},
	}
	input, _ := json.Marshal(map[string]string{
		"file_path": "src/ui/App.tsx",
		"content":   "blocked",
	})

	out, isErr := ExecuteToolWithOptions("Write", input, t.TempDir(), ToolExecutionOptions{
		WorkerID:         "worker-t1",
		OwnershipChecker: team.CheckFileOwnership,
	})
	if !isErr || !strings.Contains(out, "OWNERSHIP BLOCKED") {
		t.Fatalf("expected ownership block, got isErr=%v out=%q", isErr, out)
	}
	if globalCalled {
		t.Fatal("global ownership checker should not be used when options provide a checker")
	}
}

func teamTaskIDs(tasks []*TeamTask) string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return strings.Join(ids, ",")
}

func TestTeamSummary(t *testing.T) {
	team := &Team{
		config: TeamConfig{Name: "test-team"},
		tasks: []*TeamTask{
			{ID: "t1", Name: "Task 1", Status: "completed", Wave: 0, ToolCalls: 5},
			{ID: "t2", Name: "Task 2", Status: "completed", Wave: 1, ToolCalls: 3},
			{ID: "t3", Name: "Task 3", Status: "failed", Wave: 1, ToolCalls: 1},
		},
	}

	summary := team.Summary()
	if summary == "" {
		t.Error("Summary should not be empty")
	}
	if !contains(summary, "2 completed") {
		t.Errorf("Summary should mention completed count: %s", summary)
	}
	if !contains(summary, "1 failed") {
		t.Errorf("Summary should mention failed count: %s", summary)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
