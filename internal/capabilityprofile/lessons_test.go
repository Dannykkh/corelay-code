package capabilityprofile

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLessonsUseRepeatedCalibrationFailuresOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   ProbeStage
		repeats bool
		want    LessonPolicy
	}{
		{"calibration", StageCalibration, true, LessonToolContract},
		{"holdout", StageHoldout, true, 0},
		{"single", StageCalibration, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := revisionProfile(t, fixedNow, func(e ProbeExecution, o *ProbeObservation) error {
				if e.Case.Stage == tc.stage && e.Case.Category == CategoryProtocolNative && (tc.repeats || e.Attempt == 1) {
					o.Malformed = true
				}
				return nil
			})
			got, err := ProposeLessons(profile)
			if err != nil || got != tc.want {
				t.Fatalf("lessons=%v want=%v err=%v", got, tc.want, err)
			}
		})
	}
}

func TestLearnRunsFrozenControlAndCandidateBeforeAdoption(t *testing.T) {
	for _, helps := range []bool{true, false} {
		t.Run(map[bool]string{true: "improves", false: "does-not-improve"}[helps], func(t *testing.T) {
			source := revisionProfile(t, fixedNow, func(e ProbeExecution, o *ProbeObservation) error {
				if e.Case.Category == CategoryProtocolNative && e.Case.Stage == StageCalibration {
					o.Malformed = true
				}
				return nil
			})
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Save(source); err != nil {
				t.Fatal(err)
			}
			var applied []LessonPolicy
			executor := &fakeExecutor{mutate: func(e ProbeExecution, o *ProbeObservation) error {
				applied = append(applied, e.Lessons)
				if !e.LessonEvaluation {
					t.Fatal("evaluation omitted lesson binding")
				}
				o.LessonDigest = e.Lessons.Digest()
				if e.Case.Stage == StageCalibration && e.Case.Category == CategoryProtocolNative && (!helps || e.Lessons&LessonToolContract == 0) {
					o.Malformed = true
				}
				return nil
			}}
			profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
			clock := fixedNow
			profiler.config.Clock = func() time.Time { clock = clock.Add(time.Minute); return clock }
			runner, err := NewRunner(profiler, store, testProbePlan(t))
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Learn(context.Background(), testTarget(t), source.ID())
			if err != nil {
				t.Fatal(err)
			}
			if (result.Published != nil) != helps || result.Control == nil || result.SourceProfileID != source.ID() {
				t.Fatalf("result=%+v", result)
			}
			attempts := testProbePlan(t).Attempts()
			if len(applied) != 2*attempts {
				t.Fatalf("attempts=%d", len(applied))
			}
			for i, policy := range applied {
				want := LessonPolicy(0)
				if i >= attempts {
					want = LessonToolContract
				}
				if policy != want {
					t.Fatalf("candidate changed during evaluation: %d %v", i, policy)
				}
			}
			trials, err := NewStore(filepath.Join(store.Root(), "rsi-trials"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := trials.Load(testTarget(t), result.Control.ProfileID); err != nil {
				t.Fatal(err)
			}
			if helps {
				reloaded, _ := NewStore(store.Root())
				selected, err := reloaded.AutoSelect(testTarget(t), clock)
				if err != nil {
					t.Fatal(err)
				}
				selection, err := NewAutomaticSelection(testTarget(t), selected, clock)
				if err != nil || selection.LessonsFor(testTarget(t).Provider(), testTarget(t).Model(), clock) != LessonToolContract {
					t.Fatalf("learned policy was not selected: %v", err)
				}
				if selection.LessonsFor("wrong-provider", testTarget(t).Model(), clock) != 0 {
					t.Fatal("lessons crossed target boundary")
				}
			} else if _, err := store.Load(testTarget(t), result.Trial.ProfileID); err == nil {
				t.Fatal("rejected candidate became selectable")
			}
		})
	}
}

func TestLearnNoNewLessonDoesNotExecuteAndMissingAttestationFails(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	source := revisionProfile(t, fixedNow, nil)
	if _, err := store.Save(source); err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{}
	profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
	runner, _ := NewRunner(profiler, store, testProbePlan(t))
	result, err := runner.Learn(context.Background(), testTarget(t), source.ID())
	if err != nil || result.Decision != "rejected" || executor.calls != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := profiler.RunWithLessons(context.Background(), testTarget(t), testProbePlan(t), LessonToolContract); err == nil {
		t.Fatal("executor silently ignored lesson injection")
	}
	if _, err := profiler.RunWithLessons(context.Background(), testTarget(t), testProbePlan(t), LessonPolicy(128)); err == nil {
		t.Fatal("unknown lesson accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	executor.mutate = func(_ ProbeExecution, _ *ProbeObservation) error { cancel(); return nil }
	if _, err := profiler.RunWithLessons(ctx, testTarget(t), testProbePlan(t), LessonToolContract); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
}

func TestLessonProfileRejectsChangedTemplateOrMissingEvidence(t *testing.T) {
	executor := &fakeExecutor{mutate: func(e ProbeExecution, o *ProbeObservation) error { o.LessonDigest = e.Lessons.Digest(); return nil }}
	profiler := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour)
	profile, err := profiler.RunWithLessons(context.Background(), testTarget(t), testProbePlan(t), LessonToolContract)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := profile.Snapshot()
	snapshot.Provenance.Lessons = LessonVerifyCompletion
	if _, err := profileFromSnapshot(snapshot, true); err == nil {
		t.Fatal("changed template accepted with old evidence")
	}
	snapshot = profile.Snapshot()
	snapshot.Observations[0].LessonDigest = ""
	if _, err := profileFromSnapshot(snapshot, true); err == nil {
		t.Fatal("missing lesson execution evidence accepted")
	}
}
