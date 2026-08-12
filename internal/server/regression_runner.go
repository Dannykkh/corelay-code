package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func (s *Server) handleRegressionRuns(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	tracker := s.tracker
	s.mu.RUnlock()
	if tracker == nil {
		writeJSON(w, []any{})
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		fmt.Sscanf(q, "%d", &limit)
	}
	writeJSON(w, tracker.RegressionRuns(limit))
}

func (s *Server) handleRunRegressionCase(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	tracker := s.tracker
	provider := s.activeProvider
	model := s.activeModel
	workDir := s.workDir
	s.mu.RUnlock()

	if tracker == nil {
		writeError(w, 500, "Observability tracker not initialized")
		return
	}
	caseID := strings.TrimSpace(r.PathValue("id"))
	c, err := tracker.RegressionCase(caseID)
	if err != nil {
		if errors.Is(err, observability.ErrRegressionNotFound) {
			writeError(w, 404, "Regression case not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}

	var body struct {
		MaxCycles     int    `json:"maxCycles"`
		VerifyCommand string `json:"verifyCommand"`
		WorkDir       string `json:"workDir"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if strings.TrimSpace(body.WorkDir) != "" {
		workDir = strings.TrimSpace(body.WorkDir)
	}
	if workDir == "" {
		workDir = c.WorkDir
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	providerName := ""
	if provider != nil {
		providerName = provider.Name()
	}
	run := observability.NewRegressionRun(c, providerName, model, workDir, "")
	if !c.Replayable {
		observability.MarkRegressionRunUnsupported(&run, c, observability.ErrRegressionNotReplayable.Error())
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}
	if provider == nil && (c.Kind == "chronos" || c.Kind == "team") {
		observability.MarkRegressionRunUnsupported(&run, c, "No provider configured")
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}
	if c.Kind == "team" {
		s.handleRunTeamRegressionCase(w, r, tracker, provider, model, workDir, c, run)
		return
	}
	if c.Kind != "chronos" {
		observability.MarkRegressionRunUnsupported(&run, c, observability.ErrRegressionUnsupported.Error())
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}
	if provider == nil {
		observability.MarkRegressionRunUnsupported(&run, c, "No provider configured")
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}

	task := strings.TrimSpace(c.Inputs["task"])
	if task == "" {
		observability.MarkRegressionRunUnsupported(&run, c, "chronos regression is missing inputs.task")
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	runTraceID := observability.NewTraceID("run")
	run.RunTraceID = runTraceID

	cfg := agent.DefaultChronosConfig()
	if maxCycles := regressionCaseInt(c, "maxCycles"); maxCycles > 0 {
		cfg.MaxCycles = maxCycles
	}
	if body.MaxCycles > 0 {
		cfg.MaxCycles = body.MaxCycles
	}
	if verify := strings.TrimSpace(c.Inputs["verifyCommand"]); verify != "" {
		cfg.VerifyCommand = verify
	}
	if strings.TrimSpace(body.VerifyCommand) != "" {
		cfg.VerifyCommand = strings.TrimSpace(body.VerifyCommand)
	}
	if c.WorkstreamID != "" {
		store := workstream.NewStore(workDir)
		if ws, err := store.Get(c.WorkstreamID); err == nil {
			cfg.WorkstreamContext = workstream.RenderContext(*ws, 2000)
		}
	}

	startedAt := time.Now().UTC()
	run.StartedAt = startedAt
	chronosTrace := observability.RunTrace{
		ID:           runTraceID,
		Kind:         "chronos",
		StartedAt:    startedAt,
		Provider:     provider.Name(),
		Model:        model,
		WorkDir:      workDir,
		WorkstreamID: c.WorkstreamID,
		Status:       "running",
		Metadata: map[string]string{
			"source":          "regression",
			"caseId":          c.ID,
			"originalTraceId": c.TraceID,
			"task":            task,
			"maxCycles":       fmt.Sprintf("%d", cfg.MaxCycles),
			"verifyCommand":   cfg.VerifyCommand,
		},
		Spans: []observability.RunSpan{{
			ID:        "chronos",
			Name:      "chronos.run",
			StartedAt: startedAt,
			Status:    "running",
			Data: map[string]string{
				"caseId": c.ID,
				"task":   task,
			},
		}},
	}

	writeRegressionSSE(w, agent.Event{
		Type: "regression",
		Data: map[string]string{
			"caseId":      c.ID,
			"runId":       run.ID,
			"runTraceId":  runTraceID,
			"originalRun": c.TraceID,
		},
	})

	eventCh := make(chan agent.Event, 64)
	chronosSandboxRunner, chronosSandboxPolicy := agent.DefaultSandboxExecution(workDir)
	cfg.SandboxRunner = chronosSandboxRunner
	cfg.SandboxPolicy = chronosSandboxPolicy
	cfg.CapabilityProfile = selectAutomaticCapabilityProfile(provider, model, time.Now())
	go agent.RunChronos(r.Context(), provider, model, task, workDir, cfg, eventCh)

	passed := false
	detail := ""
	for event := range eventCh {
		text := fmt.Sprint(event.Data)
		switch event.Type {
		case "status", "text":
			if regressionReachedComplete(text) {
				passed = true
			}
			if strings.Contains(strings.ToLower(text), "max cycles") {
				detail = text
			}
		case "error":
			passed = false
			detail = text
		}
		writeRegressionSSE(w, event)
	}

	if !passed && detail == "" {
		detail = "regression replay did not reach COMPLETE"
	}
	if passed {
		finishRunTrace(&chronosTrace, "ok", "")
		observability.FinishRegressionRun(&run, c, true, "ok", "")
	} else {
		finishRunTrace(&chronosTrace, "failed", detail)
		observability.FinishRegressionRun(&run, c, false, "failed", detail)
	}
	tracker.RecordRun(chronosTrace)
	tracker.RecordRegressionRun(run)

	writeRegressionSSE(w, agent.Event{Type: "regression_result", Data: run})
	fmt.Fprintf(w, "data: {\"type\":\"stream_end\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func regressionCaseInt(c observability.RegressionCase, key string) int {
	if c.Inputs == nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(c.Inputs[key]))
	return n
}

func (s *Server) handleRunTeamRegressionCase(w http.ResponseWriter, r *http.Request, tracker *observability.Tracker, provider types.Provider, model, workDir string, c observability.RegressionCase, run observability.RegressionRun) {
	if provider == nil {
		observability.MarkRegressionRunUnsupported(&run, c, "No provider configured")
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}
	receiptPath := strings.TrimSpace(c.Inputs["receipt"])
	receipt, plan, err := loadTeamRegressionPlan(receiptPath)
	if err != nil {
		observability.MarkRegressionRunUnsupported(&run, c, err.Error())
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}
	if plan.VerifyCommand == "" {
		plan.VerifyCommand = strings.TrimSpace(c.Inputs["verifyCommand"])
	}
	if err := agent.ValidateTeamPlan(plan); err != nil {
		observability.MarkRegressionRunUnsupported(&run, c, err.Error())
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}

	workstreamContext := ""
	if c.WorkstreamID != "" {
		store := workstream.NewStore(workDir)
		if ws, err := store.Get(c.WorkstreamID); err == nil {
			workstreamContext = workstream.RenderContext(*ws, 2000)
		}
	}

	baseDir := config.BaseDir()
	teamSandboxRunner, teamSandboxPolicy := agent.DefaultSandboxExecution(workDir)
	team := agent.NewTeam(provider, model, workDir, baseDir, agent.TeamConfig{
		Name:              plan.Name,
		VerifyCommand:     plan.VerifyCommand,
		Capacity:          plan.Capacity,
		WorkstreamContext: workstreamContext,
		ProviderFactory: func(name string) (types.Provider, error) {
			return providers.Create(name, &types.ProviderConfig{})
		},
		SandboxRunner: teamSandboxRunner,
		SandboxPolicy: teamSandboxPolicy,
	})
	for _, task := range plan.ToTeamTasks() {
		team.AddTask(task)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	runTraceID := observability.NewTraceID("run")
	run.RunTraceID = runTraceID
	startedAt := time.Now().UTC()
	run.StartedAt = startedAt
	teamTrace := observability.RunTrace{
		ID:           runTraceID,
		Kind:         "team",
		StartedAt:    startedAt,
		Provider:     provider.Name(),
		Model:        model,
		WorkDir:      workDir,
		WorkstreamID: c.WorkstreamID,
		Status:       "running",
		Metadata: map[string]string{
			"source":          "regression",
			"caseId":          c.ID,
			"originalTraceId": c.TraceID,
			"receipt":         receiptPath,
			"teamName":        receipt.TeamName,
			"objective":       receipt.Objective,
			"taskCount":       fmt.Sprintf("%d", len(plan.Tasks)),
			"verifyCommand":   plan.VerifyCommand,
		},
		Spans: []observability.RunSpan{{
			ID:        "team-run",
			Name:      "team.run",
			StartedAt: startedAt,
			Status:    "running",
			Data: map[string]string{
				"caseId":   c.ID,
				"teamName": receipt.TeamName,
				"tasks":    fmt.Sprintf("%d", len(plan.Tasks)),
			},
		}},
	}

	writeRegressionSSE(w, agent.Event{
		Type: "regression",
		Data: map[string]string{
			"caseId":      c.ID,
			"runId":       run.ID,
			"runTraceId":  runTraceID,
			"originalRun": c.TraceID,
			"kind":        "team",
		},
	})

	eventCh := make(chan agent.Event, 64)
	runStatus := "completed"
	detail := ""
	verification := agent.ReceiptVerification{Status: "not-run", Source: "none"}

	go func() {
		defer close(eventCh)
		if err := team.ExecuteWaves(r.Context(), eventCh); err != nil {
			runStatus = "failed"
			detail = err.Error()
			eventCh <- agent.Event{Type: "error", Data: err.Error()}
			eventCh <- agent.Event{Type: "text", Data: "\n\n" + team.Summary()}
			emitServerTeamReceipt(eventCh, team, plan, baseDir, workDir, runStatus, verification)
			team.Shutdown()
			eventCh <- agent.Event{Type: "done", Data: nil}
			return
		}
		if plan.VerifyCommand != "" {
			passed, verifyDetail := team.Verify(r.Context())
			if passed {
				verification = agent.ReceiptVerification{
					Status:  "passed",
					Source:  "team-verify",
					Command: plan.VerifyCommand,
					Summary: trimEvidenceText(verifyDetail),
					Gate:    agent.EvidenceGateAllow,
					Mode:    agent.EvidenceModeDeep,
					Evidence: []agent.EvidenceRecord{{
						Source:  "team-verify",
						Command: plan.VerifyCommand,
						Status:  "passed",
						Summary: trimEvidenceText(verifyDetail),
					}},
				}
				eventCh <- agent.Event{Type: "status", Data: "Verification PASSED"}
			} else {
				runStatus = "failed"
				detail = verifyDetail
				verification = agent.ReceiptVerification{
					Status:  "failed",
					Source:  "team-verify",
					Command: plan.VerifyCommand,
					Summary: trimEvidenceText(verifyDetail),
					Gate:    agent.EvidenceGateBlock,
					Mode:    agent.EvidenceModeDeep,
					Evidence: []agent.EvidenceRecord{{
						Source:  "team-verify",
						Command: plan.VerifyCommand,
						Status:  "failed",
						Summary: trimEvidenceText(verifyDetail),
					}},
				}
				eventCh <- agent.Event{Type: "status", Data: "Verification FAILED: " + verifyDetail}
			}
		}
		eventCh <- agent.Event{Type: "text", Data: "\n\n" + team.Summary()}
		emitServerTeamReceipt(eventCh, team, plan, baseDir, workDir, runStatus, verification)
		team.Shutdown()
		eventCh <- agent.Event{Type: "done", Data: nil}
	}()

	for event := range eventCh {
		writeRegressionSSE(w, event)
	}

	passed := runStatus != "failed"
	if passed {
		finishRunTrace(&teamTrace, "ok", "")
		observability.FinishRegressionRun(&run, c, true, "ok", "")
	} else {
		if detail == "" {
			detail = "team regression replay failed"
		}
		finishRunTrace(&teamTrace, "failed", detail)
		observability.FinishRegressionRun(&run, c, false, "failed", detail)
	}
	tracker.RecordRun(teamTrace)
	tracker.RecordRegressionRun(run)

	writeRegressionSSE(w, agent.Event{Type: "regression_result", Data: run})
	fmt.Fprintf(w, "data: {\"type\":\"stream_end\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func loadTeamRegressionPlan(receiptPath string) (agent.TeamRunReceipt, agent.TeamPlan, error) {
	receiptPath = strings.TrimSpace(receiptPath)
	if receiptPath == "" {
		return agent.TeamRunReceipt{}, agent.TeamPlan{}, fmt.Errorf("team regression is missing inputs.receipt")
	}
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		return agent.TeamRunReceipt{}, agent.TeamPlan{}, fmt.Errorf("read team receipt: %w", err)
	}
	var receipt agent.TeamRunReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return agent.TeamRunReceipt{}, agent.TeamPlan{}, fmt.Errorf("parse team receipt: %w", err)
	}
	if receipt.Kind != "" && receipt.Kind != "team-run" {
		return agent.TeamRunReceipt{}, agent.TeamPlan{}, fmt.Errorf("receipt kind %q is not a team run", receipt.Kind)
	}
	planName := strings.TrimSpace(receipt.PlanName)
	if planName == "" {
		planName = strings.TrimSpace(receipt.TeamName)
	}
	if planName == "" {
		planName = "team-regression"
	}
	objective := strings.TrimSpace(receipt.Objective)
	if objective == "" {
		objective = planName
	}
	plan := agent.TeamPlan{
		Version:       receipt.PlanVersion,
		Name:          planName,
		Objective:     objective,
		VerifyCommand: receipt.VerifyCommand,
		Capacity:      receipt.Capacity,
		Tasks:         make([]agent.AgentTask, 0, len(receipt.Tasks)),
	}
	if plan.Version <= 0 {
		plan.Version = 1
	}
	for _, task := range receipt.Tasks {
		description := strings.TrimSpace(task.Description)
		if description == "" {
			description = strings.TrimSpace(task.ResultSummary)
		}
		if description == "" {
			description = strings.TrimSpace(task.Name)
		}
		plan.Tasks = append(plan.Tasks, agent.AgentTask{
			ID:          task.ID,
			Name:        task.Name,
			Kind:        task.Kind,
			Role:        task.Role,
			Goal:        description,
			Description: description,
			Files:       append([]string(nil), task.Files...),
			DependsOn:   append([]string(nil), task.DependsOn...),
			ReadOnly:    task.ReadOnly,
			Provider:    task.Provider,
			Model:       task.Model,
			Resources:   task.Resources,
		})
	}
	return receipt, plan, nil
}

func regressionReachedComplete(s string) bool {
	up := strings.ToUpper(s)
	return strings.Contains(up, "COMPLETE") || strings.Contains(up, "VERIFIED")
}

func writeRegressionSSE(w http.ResponseWriter, event agent.Event) {
	data, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
