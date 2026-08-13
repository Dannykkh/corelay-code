package capabilityprofile

import (
	"context"
	"errors"
	"testing"
	"time"
)

func comparisonPlan(t *testing.T, variant HarnessVariant) ProbePlan {
	t.Helper()
	base := testProbePlan(t)
	plan, err := NewProbePlan(ProbePlanSpec{
		Version: base.Version(), Variant: variant, Cases: base.Cases(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func comparisonProfile(t *testing.T, variant HarnessVariant, mutate func(ProbeExecution, *ProbeObservation) error) CapabilityProfile {
	t.Helper()
	return comparisonProfileWithPlan(t, comparisonPlan(t, variant), mutate)
}

func comparisonProfileWithPlan(t *testing.T, plan ProbePlan, mutate func(ProbeExecution, *ProbeObservation) error) CapabilityProfile {
	t.Helper()
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{mutate: mutate}, 48*time.Hour).
		Run(context.Background(), testTarget(t), plan)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestHarnessVariantsKeepFixtureShapeButChangePlanIdentity(t *testing.T) {
	corelay := comparisonPlan(t, HarnessVariantCorelay)
	minimal := comparisonPlan(t, HarnessVariantMinimal)
	if corelay.Digest() == minimal.Digest() || corelay.FixtureDigest() != minimal.FixtureDigest() ||
		corelay.Variant() != HarnessVariantCorelay || minimal.Variant() != HarnessVariantMinimal {
		t.Fatalf("variant identity was not bound: corelay=%+v minimal=%+v", corelay, minimal)
	}
	left, right := corelay.Cases(), minimal.Cases()
	if len(left) != len(right) {
		t.Fatal("variant plans changed fixture count")
	}
	for index := range left {
		if left[index] != right[index] {
			t.Fatalf("fixture %d differs across variants: left=%+v right=%+v", index, left[index], right[index])
		}
	}
}

func TestCompareProfilesReportsCandidateImprovementWithoutContent(t *testing.T) {
	baseline := comparisonProfile(t, HarnessVariantMinimal, func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.Stage == StageCalibration && execution.Case.Category == CategoryToolCatalog {
			observation.Success = false
			observation.Malformed = true
			observation.FalseDone = true
		}
		return nil
	})
	candidate := comparisonProfile(t, HarnessVariantCorelay, nil)

	report, err := CompareProfiles(baseline, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictCandidate || report.SafetyRegression || report.Delta.Successes <= 0 ||
		report.Baseline.Variant != HarnessVariantMinimal || report.Candidate.Variant != HarnessVariantCorelay {
		t.Fatalf("unexpected comparison: %+v", report)
	}
	if report.TargetDigest == "" || report.FixtureDigest == "" || report.Baseline.ProfileID == "" || len(report.Categories) == 0 {
		t.Fatalf("comparison omitted bounded identity: %+v", report)
	}
}

func TestCompareProfilesMakesSafetyRegressionTerminal(t *testing.T) {
	baseline := comparisonProfile(t, HarnessVariantMinimal, nil)
	candidate := comparisonProfile(t, HarnessVariantCorelay, func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.SafetyCritical {
			observation.Success = false
			observation.SafetyPassed = false
		}
		return nil
	})
	report, err := CompareProfiles(baseline, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictUnsafeRegression || !report.SafetyRegression || report.Delta.SafetyFailures <= 0 {
		t.Fatalf("safety regression was not terminal: %+v", report)
	}
}

func TestCompareProfilesDetectsPerCaseSafetyRegressionWhenCountsTie(t *testing.T) {
	baseline := comparisonProfile(t, HarnessVariantMinimal, func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.ID == "cal-safety" {
			observation.Success = false
			observation.SafetyPassed = false
		}
		return nil
	})
	candidate := comparisonProfile(t, HarnessVariantCorelay, func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.ID == "holdout-safety" {
			observation.Success = false
			observation.SafetyPassed = false
		}
		return nil
	})
	report, err := CompareProfiles(baseline, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if report.Baseline.SafetyFailures != report.Candidate.SafetyFailures ||
		!report.SafetyRegression || report.Verdict != VerdictUnsafeRegression {
		t.Fatalf("per-case safety regression was hidden by equal counts: %+v", report)
	}
}

func TestCompareProfilesRejectsSameVariantAndShapeMismatch(t *testing.T) {
	corelay := comparisonProfile(t, HarnessVariantCorelay, nil)
	secondCorelay := comparisonProfile(t, HarnessVariantCorelay, nil)
	if _, err := CompareProfiles(corelay, secondCorelay); !errors.Is(err, ErrIncompatibleProfiles) {
		t.Fatalf("same variant error=%v", err)
	}

	base := testProbePlan(t)
	cases := base.Cases()
	cases[0].Category = CategoryEditExact
	changedPlan, err := NewProbePlan(ProbePlanSpec{Version: base.Version(), Variant: HarnessVariantMinimal, Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	mutated := comparisonProfileWithPlan(t, changedPlan, nil)
	if _, compareErr := CompareProfiles(corelay, mutated); !errors.Is(compareErr, ErrIncompatibleProfiles) {
		t.Fatalf("shape mismatch error=%v", compareErr)
	}
}

func TestLegacyDefaultPlanProfileWithoutVariantRemainsValid(t *testing.T) {
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{}, 48*time.Hour).
		Run(context.Background(), testTarget(t), DefaultProbePlan())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := profile.Snapshot()
	snapshot.Provenance.PlanVersion = LegacyProbePlanVersion
	snapshot.Provenance.Variant = ""
	snapshot.Provenance.FixtureDigest = ""
	legacy, err := profileFromSnapshot(snapshot, true)
	if err != nil || profileVariant(legacy.Snapshot()) != HarnessVariantCorelay {
		t.Fatalf("legacy profile compatibility failed: profile=%+v err=%v", legacy.Snapshot(), err)
	}
}
