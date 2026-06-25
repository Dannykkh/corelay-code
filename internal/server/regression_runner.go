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

	"github.com/aniclew/aniclew/internal/agent"
	"github.com/aniclew/aniclew/internal/observability"
	"github.com/aniclew/aniclew/internal/workstream"
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
	if c.Kind != "chronos" {
		observability.MarkRegressionRunUnsupported(&run, c, observability.ErrRegressionUnsupported.Error())
		tracker.RecordRegressionRun(run)
		writeJSON(w, run)
		return
	}
	if provider == nil {
		writeError(w, 500, "No provider configured")
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
