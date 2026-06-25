package observability

import (
	"errors"
	"testing"
	"time"
)

func TestTrackerCreateRegressionCaseFromFailedRun(t *testing.T) {
	dir := t.TempDir()
	tracker := NewTracker(dir)
	started := time.Now().UTC().Add(-2 * time.Second)
	tracker.RecordRun(RunTrace{
		ID:           "run_failed",
		Kind:         "chronos",
		StartedAt:    started,
		EndedAt:      time.Now().UTC(),
		Provider:     "fake",
		Model:        "fake-model",
		WorkDir:      dir,
		WorkstreamID: "ws_failed",
		Status:       "failed",
		Error:        "tests failed",
		Metadata: map[string]string{
			"task":          "fix failing test",
			"verifyCommand": "go test ./...",
		},
		Spans: []RunSpan{{
			ID:        "chronos",
			Name:      "chronos.run",
			StartedAt: started,
			EndedAt:   time.Now().UTC(),
			Status:    "failed",
			Data:      map[string]string{"detail": "tests failed"},
		}},
	})

	c, err := tracker.CreateRegressionCase("run_failed")
	if err != nil {
		t.Fatalf("CreateRegressionCase() error = %v", err)
	}
	if c.TraceID != "run_failed" || c.Kind != "chronos" || !c.Replayable {
		t.Fatalf("unexpected case: %+v", c)
	}
	if c.Failure.Error != "tests failed" || len(c.Failure.FailedSpans) != 1 {
		t.Fatalf("unexpected failure summary: %+v", c.Failure)
	}
	if len(c.Checks) < 2 || c.Checks[0].Expected != "ok" || c.Checks[0].Observed != "failed" {
		t.Fatalf("unexpected checks: %+v", c.Checks)
	}

	again, err := tracker.CreateRegressionCase("run_failed")
	if err != nil {
		t.Fatalf("CreateRegressionCase() second call error = %v", err)
	}
	if again.ID != c.ID || len(tracker.RegressionCases(10)) != 1 {
		t.Fatalf("case creation was not idempotent: first=%+v second=%+v all=%+v", c, again, tracker.RegressionCases(10))
	}

	reloaded := NewTracker(dir)
	cases := reloaded.RegressionCases(10)
	if len(cases) != 1 || cases[0].ID != c.ID || cases[0].Inputs["task"] != "fix failing test" {
		t.Fatalf("regression cases did not persist: %+v", cases)
	}
}

func TestTrackerCreateRegressionCaseRejectsPassingRun(t *testing.T) {
	tracker := NewTracker(t.TempDir())
	tracker.RecordRun(RunTrace{
		ID:        "run_ok",
		Kind:      "agent",
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC(),
		Status:    "ok",
	})

	if _, err := tracker.CreateRegressionCase("run_ok"); !errors.Is(err, ErrRunTraceNotFailed) {
		t.Fatalf("CreateRegressionCase() error = %v, want ErrRunTraceNotFailed", err)
	}
}

func TestTrackerRecordRegressionRunPersistsAndLoads(t *testing.T) {
	dir := t.TempDir()
	tracker := NewTracker(dir)
	c := RegressionCase{
		ID:         "reg_case",
		TraceID:    "run_failed",
		Kind:       "chronos",
		Replayable: true,
		Checks: []RegressionCheck{{
			Name:     "run completes without failure",
			Target:   "run.status",
			Expected: "ok",
			Observed: "failed",
		}},
	}
	run := NewRegressionRun(c, "fake", "fake-model", dir, "run_replay")
	FinishRegressionRun(&run, c, true, "ok", "")
	tracker.RecordRegressionRun(run)

	reloaded := NewTracker(dir)
	runs := reloaded.RegressionRuns(1)
	if len(runs) != 1 {
		t.Fatalf("runs=%d, want 1", len(runs))
	}
	if runs[0].CaseID != "reg_case" || runs[0].RunTraceID != "run_replay" || runs[0].Status != "passed" {
		t.Fatalf("unexpected regression run: %+v", runs[0])
	}
	if len(runs[0].Checks) != 1 || !runs[0].Checks[0].Passed {
		t.Fatalf("unexpected check results: %+v", runs[0].Checks)
	}
}
