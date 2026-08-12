package capabilityprofile

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerPublishesDeterministicProfileExactlyOnce(t *testing.T) {
	target := testTarget(t)
	plan := testProbePlan(t)
	executor := &fakeExecutor{}
	profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(profiler, store, plan)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := runner.Manifest(target)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TargetDigest != target.Digest() || manifest.PlanDigest != plan.Digest() ||
		manifest.Attempts != plan.Attempts() || manifest.CalibrationAttempts != 8 ||
		manifest.HoldoutAttempts != 4 || manifest.SafetyAttempts != 4 {
		t.Fatalf("manifest = %+v", manifest)
	}

	result, err := runner.Run(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Profile.Verified() || result.Ref.ProfileID != result.Profile.ID() ||
		result.Ref.TargetDigest != target.Digest() || executor.calls != plan.Attempts() {
		t.Fatalf("result=%+v calls=%d", result.Ref, executor.calls)
	}
	if _, err := runner.Run(context.Background(), target); !errors.Is(err, ErrProfileConflict) {
		t.Fatalf("second immutable publication error = %v, want ErrProfileConflict", err)
	}
}

func TestRunnerRejectsInterleavedHoldoutPlan(t *testing.T) {
	plan, err := NewProbePlan(ProbePlanSpec{
		Version: "interleaved-v1",
		Cases: []ProbeCase{
			probe("cal-first", StageCalibration, CategoryProtocolNative, 1, 1, 0, 1, false, false),
			probe("holdout-safety", StageHoldout, CategorySafetyBoundary, 2, 1, 0, 1, true, true),
			probe("cal-after", StageCalibration, CategoryToolCatalog, 3, 1, 0, 1, false, false),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRunner(testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{}, time.Hour), store, plan)
	if !errors.Is(err, ErrInvalidRuntime) {
		t.Fatalf("NewRunner error = %v, want ErrInvalidRuntime", err)
	}
}

func TestStoreListReturnsBoundedNewestFirstInventory(t *testing.T) {
	target := testTarget(t)
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	first := verifiedProfile(t, target, 24*time.Hour)
	if _, err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	secondProfiler, err := NewProfiler(&fakeWorkspaceFactory{}, &fakeExecutor{}, ProfilerConfig{
		ConfidenceThresholdBasisPoints: 8_000,
		MinimumObservations:            8,
		ProfileTTL:                     48 * time.Hour,
		Clock:                          func() time.Time { return fixedNow.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondProfiler.Run(context.Background(), target, testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.List(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].ProfileID != second.ID() || statuses[1].ProfileID != first.ID() {
		t.Fatalf("statuses = %+v", statuses)
	}
	if !statuses[0].Verified || statuses[0].Attempts != testProbePlan(t).Attempts() || statuses[0].ManualOnly {
		t.Fatalf("newest status = %+v", statuses[0])
	}
}
