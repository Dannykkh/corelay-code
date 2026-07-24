package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aniclew/aniclew/internal/agent"
	"github.com/aniclew/aniclew/internal/config"
)

type evidencePolicyResponse struct {
	Policy        string `json:"policy"`
	MaxStopBlocks int    `json:"maxStopBlocks"`
}

type evidenceRecentResponse struct {
	BaseDir string               `json:"baseDir"`
	Scope   string               `json:"scope"`
	WorkDir string               `json:"workDir,omitempty"`
	Items   []evidenceRecentItem `json:"items"`
}

type evidenceRecentItem struct {
	Kind            string                 `json:"kind"`
	CreatedAt       string                 `json:"createdAt"`
	WorkDir         string                 `json:"workDir,omitempty"`
	Workspace       string                 `json:"workspace,omitempty"`
	Provider        string                 `json:"provider,omitempty"`
	Model           string                 `json:"model,omitempty"`
	Status          string                 `json:"status"`
	Source          string                 `json:"source"`
	Command         string                 `json:"command,omitempty"`
	Gate            string                 `json:"gate,omitempty"`
	Mode            string                 `json:"mode,omitempty"`
	Summary         string                 `json:"summary,omitempty"`
	EditedFiles     []string               `json:"editedFiles,omitempty"`
	Evidence        []agent.EvidenceRecord `json:"evidence,omitempty"`
	EvidenceCount   int                    `json:"evidenceCount"`
	EditedFileCount int                    `json:"editedFileCount,omitempty"`
	TaskCount       int                    `json:"taskCount,omitempty"`
	Completed       int                    `json:"completed,omitempty"`
	Failed          int                    `json:"failed,omitempty"`
	ReceiptPath     string                 `json:"receiptPath"`
	modifiedTime    time.Time
}

func (s *Server) currentEvidencePolicy() agent.EvidencePolicyConfig {
	if s == nil {
		return agent.DefaultEvidencePolicyConfig()
	}
	s.evidenceMu.RLock()
	defer s.evidenceMu.RUnlock()
	return agent.NormalizeEvidencePolicyConfig(s.evidencePolicy)
}

func (s *Server) setEvidencePolicy(cfg agent.EvidencePolicyConfig) agent.EvidencePolicyConfig {
	cfg = agent.NormalizeEvidencePolicyConfig(cfg)
	s.evidenceMu.Lock()
	defer s.evidenceMu.Unlock()
	s.evidencePolicy = cfg
	return cfg
}

func (s *Server) SetEvidencePolicy(cfg agent.EvidencePolicyConfig) {
	s.setEvidencePolicy(cfg)
}

func (s *Server) handleEvidencePolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, evidencePolicyPayload(s.currentEvidencePolicy()))
}

func (s *Server) handleEvidencePolicyUpdate(w http.ResponseWriter, r *http.Request) {
	var body agent.EvidencePolicyConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg := agent.NormalizeEvidencePolicyConfig(body)
	saved := config.Load()
	saved.EvidencePolicy = cfg.Policy
	saved.EvidenceMaxStopBlocks = cfg.MaxStopBlocks
	if err := config.Save(saved); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save evidence policy: "+err.Error())
		return
	}
	cfg = s.setEvidencePolicy(cfg)
	writeJSON(w, evidencePolicyPayload(cfg))
}

func (s *Server) handleEvidenceRecent(w http.ResponseWriter, r *http.Request) {
	limit := 12
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 50 {
		limit = 50
	}
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	if scope == "" {
		scope = "current"
	}
	workDir := ""
	if scope != "all" {
		s.mu.RLock()
		workDir = s.workDir
		s.mu.RUnlock()
		if strings.TrimSpace(workDir) == "" {
			scope = "all"
		}
	}

	baseDir := filepath.Join(filepath.Dir(config.ConfigPath()), "receipts")
	items := readRecentEvidenceReceiptsForWorkspace(baseDir, limit, workDir)
	writeJSON(w, evidenceRecentResponse{
		BaseDir: baseDir,
		Scope:   scope,
		WorkDir: workDir,
		Items:   items,
	})
}

func evidencePolicyPayload(cfg agent.EvidencePolicyConfig) evidencePolicyResponse {
	cfg = agent.NormalizeEvidencePolicyConfig(cfg)
	return evidencePolicyResponse{Policy: cfg.Policy, MaxStopBlocks: cfg.MaxStopBlocks}
}

func readRecentEvidenceReceipts(baseDir string, limit int) []evidenceRecentItem {
	return readRecentEvidenceReceiptsForWorkspace(baseDir, limit, "")
}

func readRecentEvidenceReceiptsForWorkspace(baseDir string, limit int, workDir string) []evidenceRecentItem {
	var items []evidenceRecentItem
	_ = filepath.WalkDir(baseDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		item, ok := readEvidenceReceipt(path)
		if ok {
			if strings.TrimSpace(workDir) != "" && !sameWorkspace(item.WorkDir, workDir) {
				return nil
			}
			items = append(items, item)
		}
		return nil
	})
	sort.Slice(items, func(i, j int) bool {
		return evidenceItemTime(items[i]).After(evidenceItemTime(items[j]))
	})
	if len(items) > limit {
		items = items[:limit]
	}
	for i := range items {
		items[i].modifiedTime = time.Time{}
	}
	return items
}

func readEvidenceReceipt(path string) (evidenceRecentItem, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evidenceRecentItem{}, false
	}
	info, _ := os.Stat(path)

	var probe struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(data, &probe)
	if probe.Kind == "team-run" {
		var receipt agent.TeamRunReceipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			return evidenceRecentItem{}, false
		}
		verification := receipt.Verification
		command := emptyAs(verification.Command, receipt.VerifyCommand)
		status := emptyAs(verification.Status, "not-run")
		source := emptyAs(verification.Source, "none")
		summary := teamEvidenceSummary(receipt, command)
		return evidenceRecentItem{
			Kind:          "team-run",
			CreatedAt:     receipt.CreatedAt,
			WorkDir:       receipt.WorkDir,
			Workspace:     workspaceName(receipt.WorkDir),
			Provider:      receipt.Provider,
			Model:         receipt.Model,
			Status:        status,
			Source:        source,
			Command:       command,
			Gate:          verification.Gate,
			Mode:          verification.Mode,
			Summary:       summary,
			Evidence:      verification.Evidence,
			EvidenceCount: len(verification.Evidence),
			TaskCount:     receipt.TaskCount,
			Completed:     receipt.Completed,
			Failed:        receipt.Failed,
			ReceiptPath:   path,
			modifiedTime:  fileModTime(info),
		}, true
	}

	var receipt agent.AgentReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return evidenceRecentItem{}, false
	}
	verification := receipt.Verification
	status := emptyAs(verification.Status, "not-run")
	source := emptyAs(verification.Source, "none")
	summary := agentEvidenceSummary(receipt)
	return evidenceRecentItem{
		Kind:            "agent",
		CreatedAt:       receipt.CreatedAt,
		WorkDir:         receipt.WorkDir,
		Workspace:       workspaceName(receipt.WorkDir),
		Provider:        receipt.Provider,
		Model:           receipt.Model,
		Status:          status,
		Source:          source,
		Command:         verification.Command,
		Gate:            verification.Gate,
		Mode:            verification.Mode,
		Summary:         summary,
		EditedFiles:     receipt.EditedFiles,
		Evidence:        verification.Evidence,
		EvidenceCount:   len(verification.Evidence),
		EditedFileCount: len(receipt.EditedFiles),
		ReceiptPath:     path,
		modifiedTime:    fileModTime(info),
	}, true
}

func sameWorkspace(a, b string) bool {
	a = cleanWorkspacePath(a)
	b = cleanWorkspacePath(b)
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

func cleanWorkspacePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func workspaceName(workDir string) string {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(workDir))
}

func agentEvidenceSummary(receipt agent.AgentReceipt) string {
	if summary := trimEvidenceText(receipt.Verification.Summary); summary != "" {
		return summary
	}
	if receipt.Verification.Command != "" {
		return "Verification command recorded"
	}
	if len(receipt.Verification.Evidence) > 0 {
		return fmt.Sprintf("%d evidence record(s) captured", len(receipt.Verification.Evidence))
	}
	if len(receipt.EditedFiles) > 0 {
		return fmt.Sprintf("%d edited file(s), no successful verification evidence", len(receipt.EditedFiles))
	}
	return "No verification evidence recorded"
}

func teamEvidenceSummary(receipt agent.TeamRunReceipt, command string) string {
	if summary := trimEvidenceText(receipt.Verification.Summary); summary != "" {
		return summary
	}
	if len(receipt.Verification.Evidence) > 0 {
		return fmt.Sprintf("%d evidence record(s) captured", len(receipt.Verification.Evidence))
	}
	if command != "" {
		return "Verification command configured but no result recorded"
	}
	if receipt.TaskCount > 0 {
		return fmt.Sprintf("%d/%d tasks completed, no team verification command", receipt.Completed, receipt.TaskCount)
	}
	return "No team verification evidence recorded"
}

func evidenceItemTime(item evidenceRecentItem) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, item.CreatedAt); err == nil {
		return parsed
	}
	return item.modifiedTime
}

func fileModTime(info os.FileInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	return info.ModTime()
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func trimEvidenceText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 240 {
		return value
	}
	return value[:240] + "..."
}
