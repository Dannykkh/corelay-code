package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aniclew/aniclew/internal/config"
)

// AgentReceipt is the machine-readable proof that an agent run completed with
// observable state, not just a prose claim.
type AgentReceipt struct {
	Version      int                 `json:"version"`
	CreatedAt    string              `json:"createdAt"`
	WorkDir      string              `json:"workDir"`
	Provider     string              `json:"provider"`
	Model        string              `json:"model"`
	ProjectType  string              `json:"projectType"`
	PlanMode     bool                `json:"planMode"`
	Iterations   int                 `json:"iterations"`
	EditedFiles  []string            `json:"editedFiles"`
	Verification ReceiptVerification `json:"verification"`
}

// TeamRunReceipt is the durable state snapshot for a TeamPlan or worker run.
// It stores compact summaries and file pointers, never raw prompts or full
// model/tool output.
type TeamRunReceipt struct {
	Version       int                 `json:"version"`
	Kind          string              `json:"kind"`
	CreatedAt     string              `json:"createdAt"`
	WorkDir       string              `json:"workDir"`
	Status        string              `json:"status"` // completed, failed, cancelled
	TeamName      string              `json:"teamName"`
	PlanName      string              `json:"planName,omitempty"`
	PlanVersion   int                 `json:"planVersion,omitempty"`
	Objective     string              `json:"objective,omitempty"`
	Provider      string              `json:"provider"`
	Model         string              `json:"model"`
	Capacity      CapacityConfig      `json:"capacity"`
	VerifyCommand string              `json:"verifyCommand,omitempty"`
	TaskCount     int                 `json:"taskCount"`
	Completed     int                 `json:"completed"`
	Failed        int                 `json:"failed"`
	ToolCalls     int                 `json:"toolCalls"`
	Verification  ReceiptVerification `json:"verification"`
	Tasks         []TeamTaskReceipt   `json:"tasks"`
}

type TeamTaskReceipt struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Description   string             `json:"description,omitempty"`
	Kind          TaskKind           `json:"kind,omitempty"`
	Role          string             `json:"role,omitempty"`
	Provider      string             `json:"provider,omitempty"`
	Model         string             `json:"model,omitempty"`
	ReadOnly      bool               `json:"readOnly,omitempty"`
	Resources     AgentTaskResources `json:"resources,omitempty"`
	Status        string             `json:"status"`
	AssignedTo    string             `json:"assignedTo,omitempty"`
	Files         []string           `json:"files,omitempty"`
	DependsOn     []string           `json:"dependsOn,omitempty"`
	Wave          int                `json:"wave"`
	ToolCalls     int                `json:"toolCalls"`
	OutputPath    string             `json:"outputPath,omitempty"`
	ResultSummary string             `json:"resultSummary,omitempty"`
	StartedAt     string             `json:"startedAt,omitempty"`
	FinishedAt    string             `json:"finishedAt,omitempty"`
}

type ReceiptVerification struct {
	Status string `json:"status"` // passed, failed, not-run
	Source string `json:"source"` // auto-verify, none
}

func writeAgentReceipt(workDir string, receipt AgentReceipt) (string, error) {
	baseDir := filepath.Join(filepath.Dir(config.ConfigPath()), "receipts")
	return writeAgentReceiptToDir(baseDir, workDir, receipt, time.Now().UTC())
}

func writeAgentReceiptToDir(baseDir, workDir string, receipt AgentReceipt, now time.Time) (string, error) {
	receipt.Version = 1
	receipt.CreatedAt = now.Format(time.RFC3339Nano)
	receipt.WorkDir = workDir

	dir := filepath.Join(baseDir, safeReceiptDir(workDir))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", err
	}

	name := now.Format("20060102-150405.000000000") + ".json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

func WriteTeamRunReceipt(baseDir, workDir string, receipt TeamRunReceipt) (string, error) {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = filepath.Dir(config.ConfigPath())
	}
	receiptBaseDir := filepath.Join(baseDir, "receipts")
	return writeTeamRunReceiptToDir(receiptBaseDir, workDir, receipt, time.Now().UTC())
}

func writeTeamRunReceiptToDir(baseDir, workDir string, receipt TeamRunReceipt, now time.Time) (string, error) {
	receipt.Version = 1
	receipt.Kind = "team-run"
	receipt.CreatedAt = now.Format(time.RFC3339Nano)
	receipt.WorkDir = workDir
	if receipt.Verification.Status == "" {
		receipt.Verification = ReceiptVerification{Status: "not-run", Source: "none"}
	}

	dir := filepath.Join(baseDir, safeReceiptDir(workDir))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", err
	}

	name := "team-" + now.Format("20060102-150405.000000000") + ".json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

func (t *Team) BuildRunReceipt(plan TeamPlan, status string, verification ReceiptVerification) TeamRunReceipt {
	t.mu.RLock()
	defer t.mu.RUnlock()

	providerName := ""
	if t.provider != nil {
		providerName = t.provider.Name()
	}
	planName := strings.TrimSpace(plan.Name)
	if planName == "" {
		planName = t.config.Name
	}
	objective := strings.TrimSpace(plan.Objective)
	if objective == "" {
		objective = t.config.Name
	}
	if verification.Status == "" {
		verification = ReceiptVerification{Status: "not-run", Source: "none"}
	}

	receipt := TeamRunReceipt{
		Kind:          "team-run",
		WorkDir:       t.workDir,
		Status:        strings.TrimSpace(status),
		TeamName:      t.config.Name,
		PlanName:      planName,
		PlanVersion:   plan.Version,
		Objective:     objective,
		Provider:      providerName,
		Model:         t.model,
		Capacity:      t.config.Capacity,
		VerifyCommand: t.config.VerifyCommand,
		Verification:  verification,
		Tasks:         make([]TeamTaskReceipt, 0, len(t.tasks)),
	}

	for _, task := range t.tasks {
		taskProvider := task.Provider
		if strings.TrimSpace(taskProvider) == "" {
			taskProvider = providerName
		}
		taskModel := task.Model
		if strings.TrimSpace(taskModel) == "" {
			taskModel = t.model
		}
		taskReceipt := TeamTaskReceipt{
			ID:            task.ID,
			Name:          task.Name,
			Description:   task.Description,
			Kind:          task.Kind,
			Role:          task.Role,
			Provider:      taskProvider,
			Model:         taskModel,
			ReadOnly:      task.ReadOnly,
			Resources:     task.Resources,
			Status:        task.Status,
			AssignedTo:    task.AssignedTo,
			Files:         append([]string(nil), task.Files...),
			DependsOn:     append([]string(nil), task.DependsOn...),
			Wave:          task.Wave,
			ToolCalls:     task.ToolCalls,
			OutputPath:    task.OutputPath,
			ResultSummary: truncateStr(strings.TrimSpace(task.Result), 500),
			StartedAt:     receiptTime(task.StartedAt),
			FinishedAt:    receiptTime(task.FinishedAt),
		}
		receipt.Tasks = append(receipt.Tasks, taskReceipt)
		receipt.TaskCount++
		receipt.ToolCalls += task.ToolCalls
		switch task.Status {
		case "completed":
			receipt.Completed++
		case "failed":
			receipt.Failed++
		}
	}

	if receipt.Status == "" {
		if receipt.Failed > 0 {
			receipt.Status = "failed"
		} else {
			receipt.Status = "completed"
		}
	}
	return receipt
}

func receiptVerification(testResult string) ReceiptVerification {
	switch testResult {
	case "passed":
		return ReceiptVerification{Status: "passed", Source: "auto-verify"}
	case "failed":
		return ReceiptVerification{Status: "failed", Source: "auto-verify"}
	default:
		return ReceiptVerification{Status: "not-run", Source: "none"}
	}
}

func receiptTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

var receiptDirUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeReceiptDir(workDir string) string {
	clean := strings.TrimSpace(filepath.Clean(workDir))
	clean = strings.Trim(clean, string(filepath.Separator))
	clean = receiptDirUnsafe.ReplaceAllString(clean, "_")
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		return "workspace"
	}
	if len(clean) > 120 {
		return clean[:120]
	}
	return clean
}
