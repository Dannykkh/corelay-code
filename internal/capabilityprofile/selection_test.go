package capabilityprofile

import (
	"errors"
	"testing"
	"time"
)

func TestAutomaticSelectionBindsExactTargetAndRechecksRunIdentity(t *testing.T) {
	target := testTarget(t)
	profile := verifiedProfile(t, target, 48*time.Hour)
	selection, err := NewAutomaticSelection(target, profile, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("NewAutomaticSelection() error = %v", err)
	}

	recommendation, profileID, ok := selection.RecommendationsFor(
		target.Provider(),
		target.Model(),
		fixedNow.Add(2*time.Hour),
	)
	if !ok || profileID != profile.ID() || recommendation.MaxReliableTools == 0 {
		t.Fatalf("selected recommendation = (%+v, %q, %v)", recommendation, profileID, ok)
	}
	for _, mismatch := range []struct {
		provider string
		model    string
	}{
		{provider: "other-provider", model: target.Model()},
		{provider: target.Provider(), model: "other-model"},
		{provider: "TEST-PROVIDER", model: target.Model()},
	} {
		if _, _, applied := selection.RecommendationsFor(mismatch.provider, mismatch.model, fixedNow.Add(2*time.Hour)); applied {
			t.Fatalf("selection applied to mismatched runtime target %+v", mismatch)
		}
	}

	drifted, err := NewTargetIdentity(TargetSpec{
		Provider: target.Provider(),
		Model:    target.Model(),
		Endpoint: "https://different.invalid/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAutomaticSelection(drifted, profile, fixedNow.Add(time.Hour)); !errors.Is(err, ErrTargetMismatch) {
		t.Fatalf("drifted target error = %v, want ErrTargetMismatch", err)
	}
}

func TestAutomaticSelectionNeverAppliesFutureExpiredQuarantinedOrManualOnly(t *testing.T) {
	target := testTarget(t)
	profile := verifiedProfile(t, target, 2*time.Hour)

	if _, err := NewAutomaticSelection(target, profile, fixedNow.Add(-time.Second)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("future-created selection error = %v", err)
	}
	if _, err := NewAutomaticSelection(target, profile, fixedNow.Add(3*time.Hour)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("expired selection error = %v", err)
	}

	selection, err := NewAutomaticSelection(target, profile, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := selection.RecommendationsFor(target.Provider(), target.Model(), fixedNow.Add(3*time.Hour)); ok {
		t.Fatal("selection remained applicable after expiry")
	}
	if _, _, ok := selection.RecommendationsFor(target.Provider(), target.Model(), fixedNow.Add(-time.Second)); ok {
		t.Fatal("selection applied before profile creation")
	}

	quarantined := quarantinedProfile(t, target, 48*time.Hour)
	if _, err := NewAutomaticSelection(target, quarantined, fixedNow.Add(time.Hour)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("quarantined selection error = %v", err)
	}

	manual, err := profile.WithManualOverride(ManualOverrideSpec{
		ID:        "manual-only",
		Actor:     "test-operator",
		Reason:    "explicit diagnostic selection",
		ExpiresAt: fixedNow.Add(90 * time.Minute),
	}, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAutomaticSelection(target, manual, fixedNow.Add(time.Hour)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("manual-only selection error = %v", err)
	}
}
