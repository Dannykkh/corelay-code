package harness

import "testing"

func TestEffectiveUsableInputTokens(t *testing.T) {
	tests := []struct {
		name                string
		contextWindow       int
		outputReserve       int
		reliableInputTokens int
		want                int
	}{
		{
			name:          "output reserve reaches sixteen thousand boundary",
			contextWindow: 20_000,
			outputReserve: 4_000,
			want:          defaultTwoStageRoutingUsableInputTokens,
		},
		{
			name:          "one token above boundary",
			contextWindow: 20_001,
			outputReserve: 4_000,
			want:          defaultTwoStageRoutingUsableInputTokens + 1,
		},
		{
			name:                "reliable ceiling is lower",
			contextWindow:       64_000,
			outputReserve:       8_000,
			reliableInputTokens: 12_000,
			want:                12_000,
		},
		{
			name:                "reliable ceiling cannot increase usable input",
			contextWindow:       20_000,
			outputReserve:       4_000,
			reliableInputTokens: 32_000,
			want:                16_000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := effectiveUsableInputTokens(test.contextWindow, test.outputReserve, test.reliableInputTokens); got != test.want {
				t.Fatalf("effectiveUsableInputTokens() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestComposeProfileDefaultsRoutingFromEffectiveUsableInput(t *testing.T) {
	tests := []struct {
		name               string
		baseRouting        ToolRoutingPolicy
		baseReliableInput  int
		modelContextWindow int
		modelOutputLimit   int
		wantRouting        ToolRoutingPolicy
	}{
		{
			name:               "sixteen thousand boundary uses two stage",
			baseRouting:        ToolRoutingDirect,
			modelContextWindow: 20_000,
			modelOutputLimit:   4_000,
			wantRouting:        ToolRoutingTwoStage,
		},
		{
			name:               "output reserve moves model onto boundary",
			baseRouting:        ToolRoutingDirect,
			modelContextWindow: 20_001,
			modelOutputLimit:   4_001,
			wantRouting:        ToolRoutingTwoStage,
		},
		{
			name:               "one token above boundary preserves direct routing",
			baseRouting:        ToolRoutingDirect,
			modelContextWindow: 20_001,
			modelOutputLimit:   4_000,
			wantRouting:        ToolRoutingDirect,
		},
		{
			name:              "base reliable ceiling is the lower cap",
			baseRouting:       ToolRoutingDirect,
			baseReliableInput: 16_000,
			wantRouting:       ToolRoutingTwoStage,
		},
		{
			name:        "large input preserves non-default base routing",
			baseRouting: ToolRoutingDeterministic,
			wantRouting: ToolRoutingDeterministic,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := MustResolveProfile(ProfileSpec{
				ID:                  "composition-base",
				ContextWindow:       64_000,
				OutputReserve:       4_000,
				ReliableInputTokens: test.baseReliableInput,
				ToolRouting:         test.baseRouting,
			})
			result, err := ComposeProfile(CompositionInput{
				Base:               base,
				ModelContextWindow: test.modelContextWindow,
				ModelOutputLimit:   test.modelOutputLimit,
			})
			if err != nil {
				t.Fatalf("ComposeProfile() error = %v", err)
			}
			if got := result.Profile.ToolRouting(); got != test.wantRouting {
				t.Fatalf("ToolRouting() = %q, want %q", got, test.wantRouting)
			}
		})
	}
}

func TestComposeProfileRetainsModelLimitValidation(t *testing.T) {
	base := MustResolveProfile(ProfileSpec{ID: "composition-base"})
	tests := []struct {
		name  string
		input CompositionInput
	}{
		{
			name:  "negative context window",
			input: CompositionInput{Base: base, ModelContextWindow: -1},
		},
		{
			name:  "negative output limit",
			input: CompositionInput{Base: base, ModelOutputLimit: -1},
		},
		{
			name:  "output limit exhausts context window",
			input: CompositionInput{Base: base, ModelContextWindow: 8_000, ModelOutputLimit: 8_000},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ComposeProfile(test.input); err == nil {
				t.Fatal("ComposeProfile() error = nil, want model-limit validation error")
			}
		})
	}
}

func TestComposeProfileCopiesStopPolicy(t *testing.T) {
	base := MustResolveProfile(ProfileSpec{
		ID:             "bounded-stop-policy",
		MaxIterations:  73,
		MaxErrorRounds: 11,
	})
	result, err := ComposeProfile(CompositionInput{
		Base:               base,
		ModelContextWindow: 64_000,
		ModelOutputLimit:   8_000,
	})
	if err != nil {
		t.Fatalf("ComposeProfile() error = %v", err)
	}
	if got := result.Profile.MaxIterations(); got != base.MaxIterations() {
		t.Fatalf("MaxIterations() = %d, want copy-through %d", got, base.MaxIterations())
	}
	if got := result.Profile.MaxErrorRounds(); got != base.MaxErrorRounds() {
		t.Fatalf("MaxErrorRounds() = %d, want copy-through %d", got, base.MaxErrorRounds())
	}
}
