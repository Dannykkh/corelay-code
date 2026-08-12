package harness

import "testing"

func TestHarnessProfileReliableInputCeilingIsDistinctFromContextWindow(t *testing.T) {
	profile, err := ResolveProfile(ProfileSpec{
		ID:                  "empirical-input-ceiling",
		ContextWindow:       64_000,
		OutputReserve:       8_000,
		ReliableInputTokens: 12_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := profile.ReliableInputTokens(); got != 12_000 {
		t.Fatalf("ReliableInputTokens() = %d, want 12000", got)
	}
	if got, err := profile.UsableInputTokens(1_000, 500); err != nil || got != 12_000 {
		t.Fatalf("UsableInputTokens() = (%d, %v), want (12000, nil)", got, err)
	}
	if profile.ContextWindow() != 64_000 || profile.OutputReserve() != 8_000 {
		t.Fatalf("empirical input ceiling changed advertised model limits: window=%d reserve=%d", profile.ContextWindow(), profile.OutputReserve())
	}
}

func TestHarnessProfileReliableInputCeilingDoesNotIncreaseWindowBudget(t *testing.T) {
	profile := MustResolveProfile(ProfileSpec{
		ID:                  "window-is-lower",
		ContextWindow:       16_000,
		OutputReserve:       4_000,
		ReliableInputTokens: 40_000,
	})
	got, err := profile.UsableInputTokens(1_000, 500)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10_500 {
		t.Fatalf("UsableInputTokens() = %d, want window-derived 10500", got)
	}
	if _, err := ResolveProfile(ProfileSpec{ID: "invalid", ReliableInputTokens: -1}); err == nil {
		t.Fatal("ResolveProfile() accepted a negative reliable input ceiling")
	}
}
