package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteAgentReceiptToDir(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join("D:", "git", "example repo")
	now := time.Date(2026, 6, 23, 1, 2, 3, 4, time.UTC)

	path, err := writeAgentReceiptToDir(baseDir, workDir, AgentReceipt{
		Provider:     "ollama",
		Model:        "qwen3-coder",
		ProjectType:  "go",
		Iterations:   3,
		EditedFiles:  []string{"main.go"},
		Verification: ReceiptVerification{Status: "passed", Source: "auto-verify"},
	}, now)
	if err != nil {
		t.Fatalf("writeAgentReceiptToDir() error = %v", err)
	}

	if filepath.Dir(path) == baseDir {
		t.Fatalf("receipt was not namespaced by workspace: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("receipt file not written: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var got AgentReceipt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("receipt JSON did not unmarshal: %v", err)
	}
	if got.Version != 1 || got.CreatedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("metadata = version %d createdAt %q", got.Version, got.CreatedAt)
	}
	if got.WorkDir != workDir || got.Provider != "ollama" || got.Model != "qwen3-coder" {
		t.Fatalf("identity fields not preserved: %+v", got)
	}
	if got.Verification.Status != "passed" || got.Verification.Source != "auto-verify" {
		t.Fatalf("verification = %+v", got.Verification)
	}
}

func TestReceiptVerification(t *testing.T) {
	tests := []struct {
		in     string
		status string
		source string
	}{
		{"passed", "passed", "auto-verify"},
		{"failed", "failed", "auto-verify"},
		{"", "not-run", "none"},
	}

	for _, tt := range tests {
		got := receiptVerification(tt.in)
		if got.Status != tt.status || got.Source != tt.source {
			t.Fatalf("receiptVerification(%q) = %+v, want %s/%s", tt.in, got, tt.status, tt.source)
		}
	}
}

func TestWriteTeamRunReceiptToDir(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join("D:", "git", "team repo")
	now := time.Date(2026, 6, 25, 4, 5, 6, 7, time.UTC)
	outputPath := filepath.Join(baseDir, "teams", "daedalus", "output", "implement.txt")

	team := &Team{
		config: TeamConfig{
			Name:          "daedalus",
			VerifyCommand: "go test ./...",
			Capacity:      DefaultLocalCapacity(),
		},
		provider: testNamedProvider{name: "ollama"},
		model:    "qwen3:8b",
		workDir:  workDir,
		tasks: []*TeamTask{
			{
				ID:          "implement",
				Name:        "Implement scoped change",
				Kind:        TaskKindImplement,
				Role:        "coder",
				Provider:    "ollama",
				Model:       "qwen3:8b",
				Resources:   AgentTaskResources{ModelSlots: 2, WebFetchSlots: 1},
				Files:       []string{"internal/agent/**"},
				Status:      "completed",
				AssignedTo:  "worker-implement",
				Result:      "implemented the scoped change",
				OutputPath:  outputPath,
				ToolCalls:   2,
				Wave:        1,
				StartedAt:   now.Add(-time.Minute),
				FinishedAt:  now,
				Description: "test task",
			},
		},
	}

	receipt := team.BuildRunReceipt(TeamPlan{
		Version:   1,
		Name:      "daedalus-plan",
		Objective: "Add durable state for team runs",
	}, "completed", ReceiptVerification{Status: "passed", Source: "team-verify"})
	if receipt.WorkDir != workDir {
		t.Fatalf("BuildRunReceipt workDir = %q, want %q", receipt.WorkDir, workDir)
	}

	path, err := writeTeamRunReceiptToDir(baseDir, workDir, receipt, now)
	if err != nil {
		t.Fatalf("writeTeamRunReceiptToDir() error = %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "team-") {
		t.Fatalf("team receipt file should be prefixed: %s", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var got TeamRunReceipt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("team receipt JSON did not unmarshal: %v", err)
	}
	if got.Version != 1 || got.Kind != "team-run" || got.CreatedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("metadata not preserved: %+v", got)
	}
	if got.WorkDir != workDir || got.TeamName != "daedalus" || got.PlanName != "daedalus-plan" {
		t.Fatalf("identity fields not preserved: %+v", got)
	}
	if got.TaskCount != 1 || got.Completed != 1 || got.Failed != 0 || got.ToolCalls != 2 {
		t.Fatalf("counts not preserved: %+v", got)
	}
	if got.Verification.Status != "passed" || got.Verification.Source != "team-verify" {
		t.Fatalf("verification = %+v", got.Verification)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].OutputPath != outputPath || got.Tasks[0].ResultSummary == "" {
		t.Fatalf("task receipt not preserved: %+v", got.Tasks)
	}
	if got.Tasks[0].Resources.ModelSlots != 2 || got.Tasks[0].Resources.WebFetchSlots != 1 {
		t.Fatalf("task resources not preserved: %+v", got.Tasks[0].Resources)
	}
}
