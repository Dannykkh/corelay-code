package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrRunTraceNotFound        = errors.New("run trace not found")
	ErrRunTraceNotFailed       = errors.New("run trace is not failed")
	ErrRegressionNotFound      = errors.New("regression case not found")
	ErrRegressionNotReplayable = errors.New("regression case is not replayable")
	ErrRegressionUnsupported   = errors.New("regression kind is not supported")
)

// RegressionCase is a durable fixture generated from a failed agentic run.
type RegressionCase struct {
	ID           string            `json:"id"`
	TraceID      string            `json:"traceId"`
	CreatedAt    time.Time         `json:"createdAt"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Provider     string            `json:"provider"`
	Model        string            `json:"model"`
	WorkDir      string            `json:"workDir,omitempty"`
	WorkstreamID string            `json:"workstreamId,omitempty"`
	Replayable   bool              `json:"replayable"`
	ReplayHint   string            `json:"replayHint,omitempty"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	Failure      RegressionFailure `json:"failure"`
	Checks       []RegressionCheck `json:"checks"`
	Spans        []RegressionSpan  `json:"spans,omitempty"`
}

type RegressionFailure struct {
	Status      string   `json:"status"`
	Error       string   `json:"error,omitempty"`
	FailedSpans []string `json:"failedSpans,omitempty"`
}

type RegressionCheck struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
}

type RegressionSpan struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	DurationMs int64             `json:"durationMs,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

// RegressionRun records a replay attempt for a regression case.
type RegressionRun struct {
	ID         string                  `json:"id"`
	CaseID     string                  `json:"caseId"`
	TraceID    string                  `json:"traceId"`
	RunTraceID string                  `json:"runTraceId,omitempty"`
	StartedAt  time.Time               `json:"startedAt"`
	EndedAt    time.Time               `json:"endedAt,omitempty"`
	DurationMs int64                   `json:"durationMs,omitempty"`
	Kind       string                  `json:"kind"`
	Provider   string                  `json:"provider"`
	Model      string                  `json:"model"`
	WorkDir    string                  `json:"workDir,omitempty"`
	Status     string                  `json:"status"` // running, passed, failed, unsupported
	Error      string                  `json:"error,omitempty"`
	Checks     []RegressionCheckResult `json:"checks,omitempty"`
	Metadata   map[string]string       `json:"metadata,omitempty"`
}

type RegressionCheckResult struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	Passed   bool   `json:"passed"`
}

// CreateRegressionCase turns a failed run trace into a durable regression fixture.
func (t *Tracker) CreateRegressionCase(traceID string) (RegressionCase, error) {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		return RegressionCase{}, ErrRunTraceNotFound
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, existing := range t.regressionCases {
		if existing.TraceID == traceID {
			return existing, nil
		}
	}

	var trace RunTrace
	found := false
	for i := len(t.runTraces) - 1; i >= 0; i-- {
		if t.runTraces[i].ID == traceID {
			trace = t.runTraces[i]
			found = true
			break
		}
	}
	if !found {
		return RegressionCase{}, ErrRunTraceNotFound
	}
	if trace.Status != "failed" {
		return RegressionCase{}, ErrRunTraceNotFailed
	}

	c := buildRegressionCase(trace)
	if err := t.writeRegressionCase(c); err != nil {
		return RegressionCase{}, err
	}
	t.regressionCases = append(t.regressionCases, c)
	t.regressionCases = trimRegressionCases(t.regressionCases)
	return c, nil
}

func (t *Tracker) RegressionCases(limit int) []RegressionCase {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if limit <= 0 || limit > len(t.regressionCases) {
		limit = len(t.regressionCases)
	}
	start := len(t.regressionCases) - limit
	result := make([]RegressionCase, limit)
	copy(result, t.regressionCases[start:])
	return result
}

func (t *Tracker) RegressionCase(id string) (RegressionCase, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, c := range t.regressionCases {
		if c.ID == id {
			return c, nil
		}
	}
	return RegressionCase{}, ErrRegressionNotFound
}

func NewRegressionRun(c RegressionCase, provider, model, workDir, runTraceID string) RegressionRun {
	if provider == "" {
		provider = c.Provider
	}
	if model == "" {
		model = c.Model
	}
	if workDir == "" {
		workDir = c.WorkDir
	}
	now := time.Now().UTC()
	return RegressionRun{
		ID:         NewTraceID("regrun"),
		CaseID:     c.ID,
		TraceID:    c.TraceID,
		RunTraceID: runTraceID,
		StartedAt:  now,
		Kind:       c.Kind,
		Provider:   provider,
		Model:      model,
		WorkDir:    workDir,
		Status:     "running",
		Metadata: map[string]string{
			"replayable": fmt.Sprintf("%v", c.Replayable),
		},
	}
}

func FinishRegressionRun(run *RegressionRun, c RegressionCase, passed bool, observed string, detail string) {
	if run == nil {
		return
	}
	now := time.Now().UTC()
	run.EndedAt = now
	run.DurationMs = now.Sub(run.StartedAt).Milliseconds()
	if observed == "" {
		if passed {
			observed = "ok"
		} else {
			observed = "failed"
		}
	}
	if passed {
		run.Status = "passed"
	} else if run.Status == "unsupported" {
		// Keep explicit unsupported status.
	} else {
		run.Status = "failed"
	}
	if detail != "" {
		run.Error = detail
	}
	run.Checks = evaluateRegressionChecks(c, observed)
}

func MarkRegressionRunUnsupported(run *RegressionRun, c RegressionCase, detail string) {
	if run == nil {
		return
	}
	run.Status = "unsupported"
	FinishRegressionRun(run, c, false, "unsupported", detail)
}

func (t *Tracker) RecordRegressionRun(run RegressionRun) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if run.ID == "" {
		run.ID = NewTraceID("regrun")
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	if run.EndedAt.IsZero() {
		run.EndedAt = time.Now().UTC()
	}
	if run.DurationMs == 0 {
		run.DurationMs = run.EndedAt.Sub(run.StartedAt).Milliseconds()
	}
	if run.Status == "" {
		run.Status = "failed"
	}
	t.regressionRuns = append(t.regressionRuns, run)
	t.regressionRuns = trimRegressionRuns(t.regressionRuns)
	t.appendRegressionRunToFile(run)
}

func (t *Tracker) RegressionRuns(limit int) []RegressionRun {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if limit <= 0 || limit > len(t.regressionRuns) {
		limit = len(t.regressionRuns)
	}
	start := len(t.regressionRuns) - limit
	result := make([]RegressionRun, limit)
	copy(result, t.regressionRuns[start:])
	return result
}

func buildRegressionCase(trace RunTrace) RegressionCase {
	inputs := copyStringMap(trace.Metadata)
	if inputs == nil {
		inputs = map[string]string{}
	}
	if trace.WorkstreamID != "" {
		inputs["workstreamId"] = trace.WorkstreamID
	}

	failedSpans := make([]string, 0)
	spans := make([]RegressionSpan, 0, len(trace.Spans))
	checks := []RegressionCheck{{
		Name:     "run completes without failure",
		Target:   "run.status",
		Expected: "ok",
		Observed: trace.Status,
	}}
	for _, span := range trace.Spans {
		spans = append(spans, RegressionSpan{
			Name:       span.Name,
			Status:     span.Status,
			DurationMs: span.DurationMs,
			Data:       copyStringMap(span.Data),
		})
		if span.Status == "failed" {
			failedSpans = append(failedSpans, span.Name)
			checks = append(checks, RegressionCheck{
				Name:     fmt.Sprintf("span %s completes", span.Name),
				Target:   "span." + span.Name + ".status",
				Expected: "ok",
				Observed: span.Status,
			})
		}
	}

	replayable, replayHint := regressionReplay(trace, inputs)
	name := strings.TrimSpace(trace.Kind)
	if name == "" {
		name = "run"
	}
	name += " regression for " + trace.ID

	return RegressionCase{
		ID:           "reg_" + sanitizeRegressionID(trace.ID),
		TraceID:      trace.ID,
		CreatedAt:    time.Now().UTC(),
		Name:         name,
		Kind:         trace.Kind,
		Provider:     trace.Provider,
		Model:        trace.Model,
		WorkDir:      trace.WorkDir,
		WorkstreamID: trace.WorkstreamID,
		Replayable:   replayable,
		ReplayHint:   replayHint,
		Inputs:       inputs,
		Failure: RegressionFailure{
			Status:      trace.Status,
			Error:       trace.Error,
			FailedSpans: failedSpans,
		},
		Checks: checks,
		Spans:  spans,
	}
}

func regressionReplay(trace RunTrace, inputs map[string]string) (bool, string) {
	switch trace.Kind {
	case "chronos":
		if strings.TrimSpace(inputs["task"]) != "" {
			return true, "Replay with POST /api/chronos using inputs.task and the recorded verifyCommand."
		}
	case "kairos":
		if strings.TrimSpace(inputs["description"]) != "" {
			return true, "Replay by enqueueing a KAIROS task using inputs.description, taskType, and autonomy."
		}
	case "agent":
		if strings.TrimSpace(inputs["prompt"]) != "" {
			return true, "Replay with POST /api/agent using inputs.prompt."
		}
		return false, "Agent prompt bodies are not captured in run traces; this case preserves failure checks only."
	case "team":
		if strings.TrimSpace(inputs["receipt"]) != "" {
			return true, "Replay with POST /api/regressions/{id}/run using the recorded Team receipt."
		}
		return false, "Team replay needs the recorded receipt path to reconstruct the task graph."
	}
	return false, "No replay input is available for this run kind."
}

func evaluateRegressionChecks(c RegressionCase, observed string) []RegressionCheckResult {
	results := make([]RegressionCheckResult, 0, len(c.Checks))
	for _, check := range c.Checks {
		got := observed
		if check.Target != "run.status" && observed == "ok" {
			got = "ok"
		}
		results = append(results, RegressionCheckResult{
			Name:     check.Name,
			Target:   check.Target,
			Expected: check.Expected,
			Observed: got,
			Passed:   got == check.Expected,
		})
	}
	return results
}

func (t *Tracker) writeRegressionCase(c RegressionCase) error {
	path := filepath.Join(t.regressionDir, sanitizeRegressionID(c.ID)+".json")
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("observability: marshal regression case: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("observability: ensure regression dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("observability: write regression case: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("observability: save regression case: %w", err)
	}
	return nil
}

func (t *Tracker) loadRegressionCases() {
	entries, err := os.ReadDir(t.regressionDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(t.regressionDir, entry.Name()))
		if err != nil {
			continue
		}
		var c RegressionCase
		if json.Unmarshal(data, &c) == nil && c.ID != "" {
			t.regressionCases = append(t.regressionCases, c)
		}
	}
	sort.SliceStable(t.regressionCases, func(i, j int) bool {
		return t.regressionCases[i].CreatedAt.Before(t.regressionCases[j].CreatedAt)
	})
	t.regressionCases = trimRegressionCases(t.regressionCases)
}

func (t *Tracker) regressionRunTodayFile() string {
	return filepath.Join(t.regressionRunDir, time.Now().Format("2006-01-02")+".jsonl")
}

func (t *Tracker) appendRegressionRunToFile(run RegressionRun) {
	f, err := os.OpenFile(t.regressionRunTodayFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(run)
	f.Write(data)
	f.Write([]byte("\n"))
}

func (t *Tracker) loadRegressionRunsToday() {
	scanJSONL(t.regressionRunTodayFile(), func(line []byte) {
		var run RegressionRun
		if json.Unmarshal(line, &run) == nil {
			t.regressionRuns = append(t.regressionRuns, run)
			t.regressionRuns = trimRegressionRuns(t.regressionRuns)
		}
	})
}

func sanitizeRegressionID(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "regression"
	}
	if len(out) > 96 {
		return strings.TrimRight(out[:96], ".-")
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
