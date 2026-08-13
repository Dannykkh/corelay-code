package capabilityprofile

import (
	"math"
	"testing"
)

func TestTargetIdentityDigestsDeterministicallyWithoutRawInputs(t *testing.T) {
	first, err := NewTargetIdentity(TargetSpec{
		Provider: "openai-compatible", Model: "org/model-v1", Endpoint: "https://one.invalid/v1",
		APIKey: "sk-secret-one", ServingParameters: map[string]any{"max_tokens": 512, "context_tokens": 4096, "temperature": 0.1},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewTargetIdentity(TargetSpec{
		Provider: "openai-compatible", Model: "org/model-v1", Endpoint: "https://one.invalid/v1",
		APIKey: "a-different-key-does-not-change-serving-identity", ServingParameters: map[string]any{"temperature": 0.1, "context_tokens": 4096, "max_tokens": 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() != second.Digest() {
		t.Fatal("map order or API key changed deterministic target digest")
	}
	if first.EndpointDigest() == "https://one.invalid/v1" || first.ServingParametersDigest() == "512" {
		t.Fatal("target retained raw identity inputs")
	}
	drifted, err := NewTargetIdentity(TargetSpec{
		Provider: "openai-compatible", Model: "org/model-v1", Endpoint: "https://one.invalid/v1",
		ServingParameters: map[string]any{"max_tokens": 513, "context_tokens": 4096, "temperature": 0.1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drifted.Digest() == first.Digest() {
		t.Fatal("serving parameter drift did not change target digest")
	}
}

func TestTargetRejectsCredentialKeysLabelsAndNonJSONValues(t *testing.T) {
	tests := []TargetSpec{
		{Provider: "https://endpoint.invalid", Model: "model", Endpoint: "local"},
		{Provider: "provider", Model: "sk-credentialmaterial", Endpoint: "local"},
		{Provider: "provider", Model: "model", Endpoint: "local", ServingParameters: map[string]any{"api_token": "secret"}},
		{Provider: "provider", Model: "model", Endpoint: "local", ServingParameters: map[string]any{"callback": func() {}}},
		{Provider: "provider", Model: "model", Endpoint: "local", ServingParameters: map[string]any{"struct": struct{ Value string }{"x"}}},
		{Provider: "provider", Model: "model", Endpoint: "local", ServingParameters: map[string]any{"nan": math.NaN()}},
	}
	for i, spec := range tests {
		if _, err := NewTargetIdentity(spec); err == nil {
			t.Errorf("case %d accepted unsafe target", i)
		}
	}
	if _, err := NewTargetIdentity(TargetSpec{
		Provider: "provider", Model: "model", Endpoint: "local",
		ServingParameters: map[string]any{"max_tokens": 512, "context_tokens": 4096},
	}); err != nil {
		t.Fatalf("ordinary token-count parameters were rejected: %v", err)
	}
}

func TestProbePlanRequiresHoldoutSafetyAndCopiesCases(t *testing.T) {
	cases := []ProbeCase{
		probe("calibration", StageCalibration, CategoryProtocolNative, 1, 1, 0, 1, true, false),
		probe("holdout", StageHoldout, CategoryProtocolNative, 2, 1, 0, 1, false, false),
	}
	if _, err := NewProbePlan(ProbePlanSpec{Version: "unsafe-v1", Cases: cases}); err == nil {
		t.Fatal("plan without holdout safety case was accepted")
	}
	cases[1].SafetyCritical = true
	plan, err := NewProbePlan(ProbePlanSpec{Version: "safe-v1", Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := plan.Digest()
	cases[0].ID = "mutated"
	copyCases := plan.Cases()
	copyCases[0].ID = "also-mutated"
	if plan.Digest() != originalDigest || plan.Cases()[0].ID != "calibration" {
		t.Fatal("probe plan aliased caller-owned cases")
	}
	second, err := NewProbePlan(ProbePlanSpec{Version: "safe-v1", Cases: plan.Cases()})
	if err != nil || second.Digest() != plan.Digest() {
		t.Fatalf("plan is not deterministic: err=%v first=%s second=%s", err, plan.Digest(), second.Digest())
	}
}

func TestDefaultPlanIsValidAndVersioned(t *testing.T) {
	plan := DefaultProbePlan()
	if !plan.Valid() || plan.Version() != DefaultProbePlanVersion || plan.Attempts() == 0 {
		t.Fatalf("invalid default plan: version=%q attempts=%d", plan.Version(), plan.Attempts())
	}
	if got, want := plan.Version(), "corelay-capability-probes-v2"; got != want {
		t.Fatalf("default plan version=%q want=%q", got, want)
	}
	categories := make(map[ProbeCategory]bool)
	for _, probeCase := range plan.Cases() {
		categories[probeCase.Category] = true
	}
	for _, category := range []ProbeCategory{CategoryFormatCodeblock, CategoryFormatTokenized, CategoryRepositoryMap} {
		if !categories[category] {
			t.Fatalf("default plan omitted %q", category)
		}
	}
}

func TestProbePlanRejectsUnknownHarnessVariant(t *testing.T) {
	if _, err := ProbePlanForVariant(HarnessVariant("unsafe")); err == nil {
		t.Fatal("unknown harness variant was accepted")
	}
	if _, err := ParseHarnessVariant(" MINIMAL "); err != nil {
		t.Fatalf("normalized minimal variant rejected: %v", err)
	}
}
