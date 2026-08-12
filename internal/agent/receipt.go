package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
)

// AgentReceipt is the machine-readable proof that an agent run completed with
// observable state, not just a prose claim.
type AgentReceipt struct {
	Version      int                         `json:"version"`
	CreatedAt    string                      `json:"createdAt"`
	WorkDir      string                      `json:"workDir"`
	Provider     string                      `json:"provider"`
	Model        string                      `json:"model"`
	ProjectType  string                      `json:"projectType"`
	PlanMode     bool                        `json:"planMode"`
	Iterations   int                         `json:"iterations"`
	EditedFiles  []string                    `json:"editedFiles"`
	Artifacts    []ReceiptArtifact           `json:"artifacts,omitempty"`
	Verification ReceiptVerification         `json:"verification"`
	Completion   *CompletionContractSnapshot `json:"completion,omitempty"`
	Recovery     *RunGuardSnapshot           `json:"recovery,omitempty"`
}

// TeamRunReceipt is the durable state snapshot for a TeamPlan or worker run.
// It stores compact summaries and file pointers, never raw prompts or full
// model/tool output.
type TeamRunReceipt struct {
	Version             int                 `json:"version"`
	Kind                string              `json:"kind"`
	CreatedAt           string              `json:"createdAt"`
	WorkDir             string              `json:"workDir"`
	Status              string              `json:"status"` // completed, failed, cancelled
	TeamName            string              `json:"teamName"`
	PlanName            string              `json:"planName,omitempty"`
	PlanVersion         int                 `json:"planVersion,omitempty"`
	Objective           string              `json:"objective,omitempty"`
	Provider            string              `json:"provider"`
	Model               string              `json:"model"`
	Capacity            CapacityConfig      `json:"capacity"`
	VerifyCommand       string              `json:"verifyCommand,omitempty"`
	VerifyCommandDigest string              `json:"verifyCommandDigest,omitempty"` // digest of the pre-redaction command
	TaskCount           int                 `json:"taskCount"`
	Completed           int                 `json:"completed"`
	Failed              int                 `json:"failed"`
	ToolCalls           int                 `json:"toolCalls"`
	Verification        ReceiptVerification `json:"verification"`
	Tasks               []TeamTaskReceipt   `json:"tasks"`
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
	Artifacts     []ReceiptArtifact  `json:"artifacts,omitempty"`
	DependsOn     []string           `json:"dependsOn,omitempty"`
	Wave          int                `json:"wave"`
	ToolCalls     int                `json:"toolCalls"`
	OutputPath    string             `json:"outputPath,omitempty"`
	ResultSummary string             `json:"resultSummary,omitempty"`
	StartedAt     string             `json:"startedAt,omitempty"`
	FinishedAt    string             `json:"finishedAt,omitempty"`
}

type ReceiptVerification struct {
	Status        string           `json:"status"` // passed, failed, not-run
	TerminalState string           `json:"terminalState"`
	Source        string           `json:"source"` // auto-verify, team-verify, none
	Command       string           `json:"command,omitempty"`
	CommandDigest string           `json:"commandDigest,omitempty"` // digest of the pre-redaction command
	Summary       string           `json:"summary,omitempty"`
	Gate          string           `json:"gate,omitempty"`
	Mode          string           `json:"mode,omitempty"`
	Evidence      []EvidenceRecord `json:"evidence,omitempty"`
}

func writeAgentReceipt(workDir string, receipt AgentReceipt) (string, error) {
	baseDir := filepath.Join(filepath.Dir(config.ConfigPath()), "receipts")
	return writeAgentReceiptToDir(baseDir, workDir, receipt, time.Now().UTC())
}

func writeAgentReceiptToDir(baseDir, workDir string, receipt AgentReceipt, now time.Time) (string, error) {
	receipt.Version = 1
	receipt.CreatedAt = now.Format(time.RFC3339Nano)
	receipt.WorkDir = workDir

	dir, err := receiptWorkspaceDir(baseDir, workDir)
	if err != nil {
		return "", err
	}

	data, err := marshalSanitizedReceipt(workDir, receipt)
	if err != nil {
		return "", err
	}
	return writeReceiptAtomically(dir, "", now, data)
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

	dir, err := receiptWorkspaceDir(baseDir, workDir)
	if err != nil {
		return "", err
	}

	data, err := marshalSanitizedReceipt(workDir, receipt)
	if err != nil {
		return "", err
	}
	return writeReceiptAtomically(dir, "team-", now, data)
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

func safeReceiptDir(workDir string) string {
	if key, _, err := workspaceStorageKey(workDir); err == nil {
		return key
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(workDir)))
	return "ws_" + hex.EncodeToString(sum[:])
}

func receiptWorkspaceDir(baseDir, workDir string) (string, error) {
	if strings.TrimSpace(baseDir) == "" {
		return "", fmt.Errorf("receipt base directory is empty")
	}
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return "", fmt.Errorf("create receipt base directory: %w", err)
	}
	dir := filepath.Join(baseDir, safeReceiptDir(workDir))
	if err := ensureLexicallyContained(baseDir, dir); err != nil {
		return "", fmt.Errorf("receipt workspace directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create receipt workspace directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect receipt workspace directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("receipt workspace path is not a regular directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("secure receipt workspace directory: %w", err)
	}
	return dir, nil
}

func writeReceiptAtomically(dir, prefix string, now time.Time, data []byte) (path string, err error) {
	temp, err := os.CreateTemp(dir, ".receipt-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create receipt temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0600); err != nil {
		return "", fmt.Errorf("secure receipt temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return "", fmt.Errorf("write receipt temp file: %w", err)
	}
	if _, err := io.WriteString(temp, "\n"); err != nil {
		return "", fmt.Errorf("terminate receipt temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("sync receipt temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close receipt temp file: %w", err)
	}

	for attempt := 0; attempt < 16; attempt++ {
		suffix := make([]byte, 8)
		if _, err := rand.Read(suffix); err != nil {
			return "", fmt.Errorf("generate receipt filename: %w", err)
		}
		name := prefix + now.Format("20060102-150405.000000000") + "-" + hex.EncodeToString(suffix) + ".json"
		path = filepath.Join(dir, name)
		if _, statErr := os.Lstat(path); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect receipt target: %w", statErr)
		}
		if err := os.Rename(tempPath, path); err != nil {
			return "", fmt.Errorf("replace receipt atomically: %w", err)
		}
		dirFile, openErr := os.Open(dir)
		if openErr != nil {
			return "", fmt.Errorf("open receipt directory for sync: %w", openErr)
		}
		syncErr := dirFile.Sync()
		closeErr := dirFile.Close()
		if runtime.GOOS != "windows" && syncErr != nil {
			return "", fmt.Errorf("sync receipt directory: %w", syncErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close receipt directory: %w", closeErr)
		}
		return path, nil
	}
	return "", fmt.Errorf("could not allocate a unique receipt filename")
}
