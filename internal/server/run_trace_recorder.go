package server

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aniclew/aniclew/internal/agent"
	"github.com/aniclew/aniclew/internal/observability"
)

type compositeRunRecorder struct {
	recorders []agent.RunRecorder
}

func (r compositeRunRecorder) RunStarted() {
	for _, recorder := range r.recorders {
		recorder.RunStarted()
	}
}

func (r compositeRunRecorder) ReceiptWritten(path string, receipt agent.AgentReceipt) {
	for _, recorder := range r.recorders {
		recorder.ReceiptWritten(path, receipt)
	}
}

func (r compositeRunRecorder) RunCompleted(summary agent.RunSummary) {
	for _, recorder := range r.recorders {
		recorder.RunCompleted(summary)
	}
}

func (r compositeRunRecorder) RunFailed(message string) {
	for _, recorder := range r.recorders {
		if failureRecorder, ok := recorder.(agent.RunFailureRecorder); ok {
			failureRecorder.RunFailed(message)
		}
	}
}

func (r compositeRunRecorder) RunSpanStarted(id string, name string, data map[string]string) {
	for _, recorder := range r.recorders {
		if spanRecorder, ok := recorder.(agent.RunSpanRecorder); ok {
			spanRecorder.RunSpanStarted(id, name, data)
		}
	}
}

func (r compositeRunRecorder) RunSpanCompleted(id string, status string, data map[string]string) {
	for _, recorder := range r.recorders {
		if spanRecorder, ok := recorder.(agent.RunSpanRecorder); ok {
			spanRecorder.RunSpanCompleted(id, status, data)
		}
	}
}

type observabilityRunRecorder struct {
	mu          sync.Mutex
	tracker     *observability.Tracker
	trace       observability.RunTrace
	spanIndexes map[string]int
}

func newObservabilityRunRecorder(tracker *observability.Tracker, trace observability.RunTrace) *observabilityRunRecorder {
	if trace.ID == "" {
		trace.ID = observability.NewTraceID("run")
	}
	if trace.StartedAt.IsZero() {
		trace.StartedAt = time.Now().UTC()
	}
	if trace.Status == "" {
		trace.Status = "running"
	}
	return &observabilityRunRecorder{
		tracker:     tracker,
		trace:       trace,
		spanIndexes: map[string]int{},
	}
}

func (r *observabilityRunRecorder) RunStarted() {
	r.RunSpanStarted("run", r.trace.Kind+".run", map[string]string{
		"provider": r.trace.Provider,
		"model":    r.trace.Model,
	})
}

func (r *observabilityRunRecorder) ReceiptWritten(path string, receipt agent.AgentReceipt) {
	r.RunSpanStarted("receipt", "agent.receipt", map[string]string{"path": path})
	r.RunSpanCompleted("receipt", "ok", map[string]string{
		"provider":     receipt.Provider,
		"model":        receipt.Model,
		"verification": receipt.Verification.Status,
	})
}

func (r *observabilityRunRecorder) RunCompleted(summary agent.RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.completeSpanLocked("run", "ok", map[string]string{
		"iterations":   fmt.Sprintf("%d", summary.Iterations),
		"verification": summary.Verification.Status,
	})
	r.trace.EndedAt = now
	r.trace.DurationMs = now.Sub(r.trace.StartedAt).Milliseconds()
	r.trace.Status = "ok"
	if strings.EqualFold(summary.Verification.Status, "failed") {
		r.trace.Status = "failed"
	}
	if r.trace.Metadata == nil {
		r.trace.Metadata = map[string]string{}
	}
	r.trace.Metadata["projectType"] = summary.ProjectType
	r.trace.Metadata["planMode"] = fmt.Sprintf("%v", summary.PlanMode)
	r.trace.Metadata["iterations"] = fmt.Sprintf("%d", summary.Iterations)
	r.trace.Metadata["editedFiles"] = strings.Join(summary.EditedFiles, ",")
	r.trace.Metadata["verification"] = summary.Verification.Status
	r.trace.Metadata["receipt"] = summary.ReceiptPath
	if r.tracker != nil {
		r.tracker.RecordRun(r.trace)
	}
}

func (r *observabilityRunRecorder) RunFailed(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.completeSpanLocked("run", "failed", map[string]string{"error": message})
	r.trace.EndedAt = now
	r.trace.DurationMs = now.Sub(r.trace.StartedAt).Milliseconds()
	r.trace.Status = "failed"
	r.trace.Error = message
	if r.tracker != nil {
		r.tracker.RecordRun(r.trace)
	}
}

func (r *observabilityRunRecorder) RunSpanStarted(id string, name string, data map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spanIndexes == nil {
		r.spanIndexes = map[string]int{}
	}
	if _, exists := r.spanIndexes[id]; exists {
		return
	}
	span := observability.RunSpan{
		ID:        id,
		Name:      name,
		StartedAt: time.Now().UTC(),
		Status:    "running",
		Data:      copyStringMap(data),
	}
	r.trace.Spans = append(r.trace.Spans, span)
	r.spanIndexes[id] = len(r.trace.Spans) - 1
}

func (r *observabilityRunRecorder) RunSpanCompleted(id string, status string, data map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completeSpanLocked(id, status, data)
}

func (r *observabilityRunRecorder) completeSpanLocked(id string, status string, data map[string]string) {
	idx, ok := r.spanIndexes[id]
	now := time.Now().UTC()
	if !ok {
		r.trace.Spans = append(r.trace.Spans, observability.RunSpan{
			ID:        id,
			Name:      id,
			StartedAt: now,
			Status:    "running",
			Data:      map[string]string{},
		})
		idx = len(r.trace.Spans) - 1
		r.spanIndexes[id] = idx
	}
	span := &r.trace.Spans[idx]
	span.EndedAt = now
	span.DurationMs = now.Sub(span.StartedAt).Milliseconds()
	span.Status = status
	if span.Status == "" {
		span.Status = "ok"
	}
	if span.Data == nil {
		span.Data = map[string]string{}
	}
	for k, v := range data {
		span.Data[k] = v
	}
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

func finishRunTrace(trace *observability.RunTrace, status string, detail string) {
	if trace == nil {
		return
	}
	now := time.Now().UTC()
	trace.EndedAt = now
	trace.DurationMs = now.Sub(trace.StartedAt).Milliseconds()
	trace.Status = status
	if detail != "" {
		trace.Error = detail
	}
	for i := range trace.Spans {
		if trace.Spans[i].Status != "running" {
			continue
		}
		trace.Spans[i].EndedAt = now
		trace.Spans[i].DurationMs = now.Sub(trace.Spans[i].StartedAt).Milliseconds()
		trace.Spans[i].Status = status
		if detail != "" {
			if trace.Spans[i].Data == nil {
				trace.Spans[i].Data = map[string]string{}
			}
			trace.Spans[i].Data["detail"] = detail
		}
	}
}
