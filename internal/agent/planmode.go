package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PlanModeState tracks the plan mode lifecycle.
type PlanModeState int

const (
	PlanModeNone     PlanModeState = iota
	PlanModeExplore                // read-only exploration phase
	PlanModeDesign                 // writing the plan
	PlanModePending                // waiting for approval
	PlanModeApproved               // approved, ready to implement
	PlanModeRejected               // rejected, back to explore
)

// PlanMode represents a structured implementation plan.
type PlanMode struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Steps       []PlanModeStep `json:"steps"`
	State       PlanModeState  `json:"state"`
	CreatedAt   time.Time      `json:"createdAt"`
	ApprovedAt  time.Time      `json:"approvedAt,omitempty"`
	FilePath    string         `json:"filePath"` // where plan is saved on disk
}

// PlanModeStep is a single step in the plan.
type PlanModeStep struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Files       []string `json:"files"`  // files to modify
	Status      string   `json:"status"` // pending, in_progress, completed
}

// PlanModeManager manages the plan mode lifecycle.
type PlanModeManager struct {
	current *PlanMode
	workDir string
	planDir string
}

// NewPlanModeManager creates a plan manager for a project.
func NewPlanModeManager(workDir string) *PlanModeManager {
	planDir := filepath.Join(workDir, ".claude", ".plans")
	os.MkdirAll(planDir, 0755)
	return &PlanModeManager{workDir: workDir, planDir: planDir}
}

// EnterPlanModeMode starts the exploration phase.
// In this phase, only read-only tools are allowed.
func (pm *PlanModeManager) EnterPlanModeMode() {
	pm.current = &PlanMode{
		ID:        fmt.Sprintf("plan_%d", time.Now().Unix()),
		State:     PlanModeExplore,
		CreatedAt: time.Now(),
	}
	log.Printf("[PlanModeMode] Entered explore phase")
}

// GetState returns the current plan state.
func (pm *PlanModeManager) GetState() PlanModeState {
	if pm.current == nil {
		return PlanModeNone
	}
	return pm.current.State
}

// GetPlanMode returns the current plan.
func (pm *PlanModeManager) GetPlanMode() *PlanMode {
	return pm.current
}

// IsReadOnlyMode returns true if only read-only tools should be allowed.
func (pm *PlanModeManager) IsReadOnlyMode() bool {
	if pm.current == nil {
		return false
	}
	return pm.current.State == PlanModeExplore || pm.current.State == PlanModeDesign
}

// SubmitPlanMode saves the plan and moves to pending approval.
func (pm *PlanModeManager) SubmitPlanMode(title, description string, steps []PlanModeStep) error {
	if pm.current == nil {
		return fmt.Errorf("not in plan mode")
	}

	pm.current.Title = title
	pm.current.Description = description
	pm.current.Steps = steps
	pm.current.State = PlanModePending

	// Save to disk
	pm.current.FilePath = filepath.Join(pm.planDir, pm.current.ID+".json")
	data, _ := json.MarshalIndent(pm.current, "", "  ")
	os.WriteFile(pm.current.FilePath, data, 0644)

	// Also save readable markdown
	mdPath := filepath.Join(pm.planDir, pm.current.ID+".md")
	md := pm.formatPlanModeMarkdown()
	os.WriteFile(mdPath, []byte(md), 0644)

	log.Printf("[PlanModeMode] PlanMode submitted: %s (%d steps)", title, len(steps))
	return nil
}

// ApprovePlanMode approves the plan and moves to implementation.
func (pm *PlanModeManager) ApprovePlanMode() string {
	if pm.current == nil || pm.current.State != PlanModePending {
		return "No plan pending approval"
	}
	pm.current.State = PlanModeApproved
	pm.current.ApprovedAt = time.Now()
	log.Printf("[PlanModeMode] PlanMode approved: %s", pm.current.Title)
	return fmt.Sprintf("PlanMode '%s' approved. Ready to implement %d steps.", pm.current.Title, len(pm.current.Steps))
}

// RejectPlanMode rejects the plan and goes back to explore.
func (pm *PlanModeManager) RejectPlanMode(reason string) string {
	if pm.current == nil || pm.current.State != PlanModePending {
		return "No plan pending"
	}
	pm.current.State = PlanModeRejected
	log.Printf("[PlanModeMode] PlanMode rejected: %s — %s", pm.current.Title, reason)
	return fmt.Sprintf("PlanMode rejected: %s. Back to exploration.", reason)
}

// ExitPlanModeMode exits plan mode entirely.
func (pm *PlanModeManager) ExitPlanModeMode() {
	pm.current = nil
	log.Printf("[PlanModeMode] Exited")
}

// CompleteStep marks a plan step as completed.
func (pm *PlanModeManager) CompleteStep(stepID string) {
	if pm.current == nil {
		return
	}
	for i, s := range pm.current.Steps {
		if s.ID == stepID {
			pm.current.Steps[i].Status = "completed"
			return
		}
	}
}

// IsToolAllowed checks if a tool is allowed in the current plan state.
func (pm *PlanModeManager) IsToolAllowed(toolName string) (bool, string) {
	if pm.current == nil || pm.current.State == PlanModeApproved {
		return true, "" // no restriction
	}

	// In explore/design phase: only read-only tools
	if pm.current.State == PlanModeExplore || pm.current.State == PlanModeDesign {
		readOnly := map[string]bool{
			"Read": true, "Glob": true, "Grep": true, "LS": true, "RepoMap": true,
			"Bash":  false, // bash needs further check
			"Write": false, "Edit": false,
		}
		if allowed, ok := readOnly[toolName]; ok {
			if !allowed {
				return false, fmt.Sprintf("Tool '%s' not allowed in plan mode (explore phase). Submit plan first.", toolName)
			}
			return true, ""
		}
		// Bash: allow only read-only commands
		if toolName == "Bash" {
			return true, "" // will be checked by IsReadOnlyCommand at execution time
		}
		return true, ""
	}

	// Pending: nothing allowed
	if pm.current.State == PlanModePending {
		return false, "PlanMode is pending approval. Wait for approval before executing tools."
	}

	return true, ""
}

// ListPlanModes returns all saved plans for the project.
func (pm *PlanModeManager) ListPlanModes() []PlanMode {
	var plans []PlanMode
	entries, _ := os.ReadDir(pm.planDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pm.planDir, e.Name()))
		if err != nil {
			continue
		}
		var plan PlanMode
		if json.Unmarshal(data, &plan) == nil {
			plans = append(plans, plan)
		}
	}
	return plans
}

func (pm *PlanModeManager) formatPlanModeMarkdown() string {
	p := pm.current
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# PlanMode: %s\n\n", p.Title))
	sb.WriteString(fmt.Sprintf("Created: %s\n\n", p.CreatedAt.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("## Description\n\n%s\n\n", p.Description))
	sb.WriteString("## Steps\n\n")
	for i, s := range p.Steps {
		files := ""
		if len(s.Files) > 0 {
			files = " (" + strings.Join(s.Files, ", ") + ")"
		}
		sb.WriteString(fmt.Sprintf("%d. %s%s\n", i+1, s.Description, files))
	}
	return sb.String()
}
