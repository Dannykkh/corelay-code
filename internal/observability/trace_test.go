package observability

import (
	"testing"
	"time"
)

func TestTrackerRecordRunPersistsAndLoads(t *testing.T) {
	dir := t.TempDir()
	tracker := NewTracker(dir)
	started := time.Now().UTC().Add(-time.Second)
	tracker.RecordRun(RunTrace{
		ID:        "run_test",
		Kind:      "agent",
		StartedAt: started,
		EndedAt:   time.Now().UTC(),
		Provider:  "fake",
		Model:     "fake-model",
		WorkDir:   dir,
		Status:    "ok",
		Spans: []RunSpan{{
			ID:        "maker_01",
			Name:      "maker.llm_call",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "ok",
		}},
	})

	reloaded := NewTracker(dir)
	runs := reloaded.RecentRuns(1)
	if len(runs) != 1 {
		t.Fatalf("runs=%d, want 1", len(runs))
	}
	if runs[0].ID != "run_test" || runs[0].Kind != "agent" || runs[0].Status != "ok" {
		t.Fatalf("unexpected run trace: %+v", runs[0])
	}
	if len(runs[0].Spans) != 1 || runs[0].Spans[0].Name != "maker.llm_call" {
		t.Fatalf("unexpected spans: %+v", runs[0].Spans)
	}
}

func TestNewTraceIDUsesPrefix(t *testing.T) {
	id := NewTraceID("run")
	if len(id) <= len("run_") || id[:4] != "run_" {
		t.Fatalf("trace id %q does not use prefix", id)
	}
}
