package harness

import (
	"math"
	"reflect"
	"testing"
)

func TestResolveProfileDefaultsAndCopiesAliases(t *testing.T) {
	aliases := []string{" Coder ", "CODER", "", ":7B"}
	profile, err := ResolveProfile(ProfileSpec{
		ID:         " local-coder ",
		ToolBudget: 16,
		Aliases:    aliases,
	})
	if err != nil {
		t.Fatalf("ResolveProfile() error = %v", err)
	}

	if !profile.Valid() {
		t.Fatal("resolved profile is not valid")
	}
	if got, want := profile.ID(), "local-coder"; got != want {
		t.Fatalf("ID() = %q, want %q", got, want)
	}
	if got, want := profile.ContextWindow(), DefaultContextWindow; got != want {
		t.Fatalf("ContextWindow() = %d, want %d", got, want)
	}
	if got, want := profile.OutputReserve(), DefaultOutputReserve; got != want {
		t.Fatalf("OutputReserve() = %d, want %d", got, want)
	}
	if got, want := profile.RepeatLimit(), DefaultRepeatLimit; got != want {
		t.Fatalf("RepeatLimit() = %d, want %d", got, want)
	}
	if got, want := profile.MaxIterations(), DefaultMaxIterations; got != want {
		t.Fatalf("MaxIterations() = %d, want %d", got, want)
	}
	if got, want := profile.MaxErrorRounds(), DefaultMaxErrorRounds; got != want {
		t.Fatalf("MaxErrorRounds() = %d, want %d", got, want)
	}
	if !profile.ReadBeforeWrite() {
		t.Fatal("ReadBeforeWrite() = false, want default true")
	}
	if _, ok := profile.Temperature(); ok {
		t.Fatal("Temperature() is configured, want absent")
	}
	if got, want := profile.PlanAnchorMode(), PlanAnchorOff; got != want {
		t.Fatalf("PlanAnchorMode() = %q, want %q", got, want)
	}
	if got, want := profile.WirePolicy(), WireAuto; got != want {
		t.Fatalf("WirePolicy() = %q, want %q", got, want)
	}
	if got, want := profile.ResponsePolicy(), ResponseNativeWithTextRecovery; got != want {
		t.Fatalf("ResponsePolicy() = %q, want %q", got, want)
	}
	if got, want := profile.EditPolicy(), EditCorelayWaterfall; got != want {
		t.Fatalf("EditPolicy() = %q, want %q", got, want)
	}
	if got, want := profile.ToolRouting(), ToolRoutingDirect; got != want {
		t.Fatalf("ToolRouting() = %q, want %q", got, want)
	}

	wantAliases := []string{"coder", ":7b"}
	if got := profile.Aliases(); !reflect.DeepEqual(got, wantAliases) {
		t.Fatalf("Aliases() = %#v, want %#v", got, wantAliases)
	}
	aliases[0] = "mutated-input"
	returned := profile.Aliases()
	returned[0] = "mutated-output"
	if got := profile.Aliases(); !reflect.DeepEqual(got, wantAliases) {
		t.Fatalf("aliases changed through caller-owned slice: %#v", got)
	}

	if !profile.Matches("QWEN-CODER:7B") {
		t.Fatal("Matches() = false for case-insensitive alias")
	}
	if profile.Matches("general-model") {
		t.Fatal("Matches() = true for an unrelated model")
	}
}

func TestResolveProfileAcceptsLegacyAniClewWaterfallAsCorelayPolicy(t *testing.T) {
	profile, err := ResolveProfile(ProfileSpec{
		ID:         "legacy-edit-policy",
		EditPolicy: EditPolicy("aniclew-waterfall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := profile.EditPolicy(); got != EditCorelayWaterfall {
		t.Fatalf("EditPolicy() = %q, want canonical %q", got, EditCorelayWaterfall)
	}
}

func TestResolveProfileExplicitPolicies(t *testing.T) {
	profile, err := ResolveProfile(ProfileSpec{
		ID:              "remote-coding",
		ToolBudget:      24,
		Temperature:     SomeFloat64(0.25),
		ContextWindow:   32_768,
		OutputReserve:   4_096,
		Aliases:         []string{"remote-code"},
		ReadBeforeWrite: SomeBool(false),
		RepeatLimit:     5,
		MaxIterations:   40,
		MaxErrorRounds:  7,
		PromptSuffix:    "  follow the repository contract  ",
		PlanAnchorMode:  PlanAnchorStrict,
		WirePolicy:      WireOpenAIResponses,
		ResponsePolicy:  ResponseMultiFormat,
		EditPolicy:      EditPatchFirst,
		ToolRouting:     ToolRoutingTwoStage,
	})
	if err != nil {
		t.Fatalf("ResolveProfile() error = %v", err)
	}

	if got, want := profile.ToolBudget(), 24; got != want {
		t.Fatalf("ToolBudget() = %d, want %d", got, want)
	}
	if got, ok := profile.Temperature(); !ok || got != 0.25 {
		t.Fatalf("Temperature() = (%v, %v), want (0.25, true)", got, ok)
	}
	if profile.ReadBeforeWrite() {
		t.Fatal("ReadBeforeWrite() = true, want explicit false")
	}
	if got, want := profile.MaxIterations(), 40; got != want {
		t.Fatalf("MaxIterations() = %d, want %d", got, want)
	}
	if got, want := profile.MaxErrorRounds(), 7; got != want {
		t.Fatalf("MaxErrorRounds() = %d, want %d", got, want)
	}
	if got, want := profile.PromptSuffix(), "follow the repository contract"; got != want {
		t.Fatalf("PromptSuffix() = %q, want %q", got, want)
	}
	if got, want := profile.PlanAnchorMode(), PlanAnchorStrict; got != want {
		t.Fatalf("PlanAnchorMode() = %q, want %q", got, want)
	}
	if got, want := profile.WirePolicy(), WireOpenAIResponses; got != want {
		t.Fatalf("WirePolicy() = %q, want %q", got, want)
	}
	if got, want := profile.ResponsePolicy(), ResponseMultiFormat; got != want {
		t.Fatalf("ResponsePolicy() = %q, want %q", got, want)
	}
	if got, want := profile.EditPolicy(), EditPatchFirst; got != want {
		t.Fatalf("EditPolicy() = %q, want %q", got, want)
	}
	if got, want := profile.ToolRouting(), ToolRoutingTwoStage; got != want {
		t.Fatalf("ToolRouting() = %q, want %q", got, want)
	}
}

func TestResolveProfileRejectsInvalidSpecs(t *testing.T) {
	tests := []struct {
		name string
		spec ProfileSpec
	}{
		{name: "missing ID", spec: ProfileSpec{}},
		{name: "negative tool budget", spec: ProfileSpec{ID: "x", ToolBudget: -1}},
		{name: "negative temperature", spec: ProfileSpec{ID: "x", Temperature: SomeFloat64(-0.1)}},
		{name: "NaN temperature", spec: ProfileSpec{ID: "x", Temperature: SomeFloat64(math.NaN())}},
		{name: "infinite temperature", spec: ProfileSpec{ID: "x", Temperature: SomeFloat64(math.Inf(1))}},
		{name: "negative context", spec: ProfileSpec{ID: "x", ContextWindow: -1}},
		{
			name: "negative output reserve",
			spec: ProfileSpec{ID: "x", ContextWindow: 10_000, OutputReserve: -1},
		},
		{
			name: "reserve exhausts context",
			spec: ProfileSpec{ID: "x", ContextWindow: 10_000, OutputReserve: 10_000},
		},
		{name: "negative repeat limit", spec: ProfileSpec{ID: "x", RepeatLimit: -1}},
		{name: "negative max iterations", spec: ProfileSpec{ID: "x", MaxIterations: -1}},
		{
			name: "max iterations exceeds upper bound",
			spec: ProfileSpec{ID: "x", MaxIterations: MaxIterationsLimit + 1},
		},
		{name: "negative max error rounds", spec: ProfileSpec{ID: "x", MaxErrorRounds: -1}},
		{
			name: "max error rounds exceeds upper bound",
			spec: ProfileSpec{ID: "x", MaxErrorRounds: MaxErrorRoundsLimit + 1},
		},
		{name: "invalid plan anchor", spec: ProfileSpec{ID: "x", PlanAnchorMode: "sometimes"}},
		{name: "invalid wire policy", spec: ProfileSpec{ID: "x", WirePolicy: "custom"}},
		{name: "invalid response policy", spec: ProfileSpec{ID: "x", ResponsePolicy: "custom"}},
		{name: "invalid edit policy", spec: ProfileSpec{ID: "x", EditPolicy: "custom"}},
		{name: "invalid tool routing", spec: ProfileSpec{ID: "x", ToolRouting: "custom"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ResolveProfile(tt.spec); err == nil {
				t.Fatal("ResolveProfile() error = nil, want validation error")
			}
		})
	}
}

func TestResolveProfileAcceptsStopPolicyUpperBounds(t *testing.T) {
	profile, err := ResolveProfile(ProfileSpec{
		ID:             "bounded-stop-policy",
		MaxIterations:  MaxIterationsLimit,
		MaxErrorRounds: MaxErrorRoundsLimit,
	})
	if err != nil {
		t.Fatalf("ResolveProfile() error = %v", err)
	}
	if got := profile.MaxIterations(); got != MaxIterationsLimit {
		t.Fatalf("MaxIterations() = %d, want %d", got, MaxIterationsLimit)
	}
	if got := profile.MaxErrorRounds(); got != MaxErrorRoundsLimit {
		t.Fatalf("MaxErrorRounds() = %d, want %d", got, MaxErrorRoundsLimit)
	}
}

func TestHarnessProfileUsableInputTokens(t *testing.T) {
	profile, err := ResolveProfile(ProfileSpec{
		ID:            "budgeted",
		ContextWindow: 32_768,
		OutputReserve: 4_096,
	})
	if err != nil {
		t.Fatalf("ResolveProfile() error = %v", err)
	}

	got, err := profile.UsableInputTokens(1_024, 512)
	if err != nil {
		t.Fatalf("UsableInputTokens() error = %v", err)
	}
	if want := 27_136; got != want {
		t.Fatalf("UsableInputTokens() = %d, want %d", got, want)
	}
	if _, err := profile.UsableInputTokens(-1, 0); err == nil {
		t.Fatal("UsableInputTokens() accepted a negative reservation")
	}
	if _, err := profile.UsableInputTokens(30_000, 0); err == nil {
		t.Fatal("UsableInputTokens() accepted exhausted context")
	}
	if _, err := (HarnessProfile{}).UsableInputTokens(0, 0); err == nil {
		t.Fatal("UsableInputTokens() accepted an unresolved profile")
	}
}
