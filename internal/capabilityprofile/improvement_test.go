package capabilityprofile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func revisionProfile(t *testing.T, at time.Time, mutate func(ProbeExecution, *ProbeObservation) error) CapabilityProfile {
	t.Helper()
	p := testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{mutate: mutate}, 48*time.Hour)
	p.config.Clock = func() time.Time { return at }
	profile, err := p.Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func baselineRetries(_ ProbeExecution, o *ProbeObservation) error {
	o.Retries = 1
	return nil
}

func TestImprovementRejectsTieFalseDoneHoldoutAndPerAttemptRegression(t *testing.T) {
	baseline := revisionProfile(t, fixedNow, baselineRetries)
	for _, tc := range []struct {
		name     string
		mutate   func(ProbeExecution, *ProbeObservation) error
		accepted bool
	}{
		{"better", nil, true},
		{"tie", baselineRetries, false},
		{"false-done", func(e ProbeExecution, o *ProbeObservation) error {
			o.FalseDone = e.Case.Stage == StageCalibration
			return nil
		}, false},
		{"holdout", func(e ProbeExecution, o *ProbeObservation) error {
			o.Success = e.Case.Stage != StageHoldout
			return nil
		}, false},
		{"safety", func(e ProbeExecution, o *ProbeObservation) error { o.SafetyPassed = !e.Case.SafetyCritical; return nil }, false},
		{"one-attempt-retry-regression", func(e ProbeExecution, o *ProbeObservation) error {
			if e.Case.ID == "cal-native" && e.Attempt == 1 {
				o.Retries = 2
			}
			return nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := revisionProfile(t, fixedNow.Add(time.Minute), tc.mutate)
			result, err := EvaluateImprovement(baseline, candidate, fixedNow.Add(2*time.Minute))
			if err != nil || (result.Decision == "accepted") != tc.accepted {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
	for _, now := range []time.Time{fixedNow, fixedNow.Add(72 * time.Hour)} {
		candidate := revisionProfile(t, fixedNow.Add(time.Minute), nil)
		result, err := EvaluateImprovement(baseline, candidate, now)
		if err != nil || result.Decision != "rejected" {
			t.Fatalf("invalid time accepted: %+v %v", result, err)
		}
	}
}

func TestImprovePublishesOnlyImprovementAndPersistsRejectedTrials(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "reject", true: "accept"}[accepted], func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			baseline := revisionProfile(t, fixedNow, baselineRetries)
			if _, err := store.Save(baseline); err != nil {
				t.Fatal(err)
			}
			executor := &fakeExecutor{}
			if !accepted {
				executor.mutate = baselineRetries
			}
			profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
			profiler.config.Clock = func() time.Time { return fixedNow.Add(time.Minute) }
			runner, err := NewRunner(profiler, store, testProbePlan(t))
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Improve(context.Background(), testTarget(t), baseline.ID())
			if err != nil || (result.Published != nil) != accepted {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if executor.calls != testProbePlan(t).Attempts() {
				t.Fatal("did not execute complete evaluation")
			}
			// Reopen to verify selection is durable, not an in-memory decision.
			reopened, err := NewStore(store.Root())
			if err != nil {
				t.Fatal(err)
			}
			selected, err := reopened.AutoSelect(testTarget(t), fixedNow.Add(2*time.Minute))
			want := baseline.ID()
			if accepted {
				want = result.Published.ProfileID
			}
			if err != nil || selected.ID() != want {
				t.Fatalf("selected=%s want=%s err=%v", selected.ID(), want, err)
			}
			selection, err := NewAutomaticSelection(testTarget(t), selected, fixedNow.Add(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			_, profileID, ok := selection.RecommendationsFor(testTarget(t).Provider(), testTarget(t).Model(), fixedNow.Add(2*time.Minute))
			if !ok || profileID != want {
				t.Fatal("next run did not receive the selected recommendations")
			}
			trialStore, err := NewStore(filepath.Join(store.Root(), "rsi-trials"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := trialStore.Load(testTarget(t), result.Trial.ProfileID); err != nil {
				t.Fatal(err)
			}
			decision := filepath.Join(trialStore.Root(), baseline.ID()+"-"+result.Trial.ProfileID+".json")
			if _, err := os.Stat(decision); err != nil {
				t.Fatal(err)
			}
			if accepted {
				calls := executor.calls
				if _, err := runner.Improve(context.Background(), testTarget(t), baseline.ID()); err == nil || executor.calls != calls {
					t.Fatal("stale baseline executed")
				}
			}
		})
	}
}

func TestImproveCancellationAndTargetLockDoNotPublish(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseline := revisionProfile(t, fixedNow, baselineRetries)
	if _, err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	executor := &fakeExecutor{mutate: func(_ ProbeExecution, _ *ProbeObservation) error { cancel(); return nil }}
	profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
	profiler.config.Clock = func() time.Time { return fixedNow.Add(time.Minute) }
	runner, _ := NewRunner(profiler, store, testProbePlan(t))
	release, err := store.lockProfiling(testTarget(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Improve(ctx, testTarget(t), baseline.ID()); err == nil || executor.calls != 0 {
		t.Fatal("locked improvement executed")
	}
	if _, err := runner.Run(ctx, testTarget(t)); err == nil || executor.calls != 0 {
		t.Fatal("run bypassed improvement lock")
	}
	release()
	if _, err := runner.Improve(ctx, testTarget(t), baseline.ID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	profiles, err := store.List(testTarget(t))
	if err != nil || len(profiles) != 1 {
		t.Fatal("cancellation published a candidate")
	}
	release, err = store.lockProfiling(testTarget(t))
	if err != nil {
		t.Fatal("canceled run retained target lock")
	}
	release()
}

func TestImproveRejectsChangedPlanBeforeExecuting(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseline := revisionProfile(t, fixedNow, baselineRetries)
	if _, err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{}
	profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
	plan := comparisonPlan(t, HarnessVariantMinimal)
	runner, err := NewRunner(profiler, store, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Improve(context.Background(), testTarget(t), baseline.ID()); !errors.Is(err, ErrIncompatibleProfiles) || executor.calls != 0 {
		t.Fatalf("changed plan ran: calls=%d err=%v", executor.calls, err)
	}
}
